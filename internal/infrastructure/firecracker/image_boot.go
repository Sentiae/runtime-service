package firecracker

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// imgSubnetBase is the /16 the image-boot path carves per-workload /30s out of.
// Each workload index N gets the /30 based at N*4 within 10.201.0.0/16:
//
//	network = 10.201.<(N*4)>>8>.<(N*4)&0xff>
//	host    = network + 1   (the gateway the guest sees)
//	guest   = network + 2
//
// N in [1,imgMaxIndex] keeps every octet valid and every /30 aligned, and stays
// well inside 10.201.0.0/16. (This realizes the CP3 spec's "unique /30 per
// workload, ~4000 capacity" intent with valid octets — see image_boot notes.)
const (
	imgMaxIndex = 4000
	imgNetmask  = "255.255.255.252"
	imgSubnet16 = "10.201.0.0/16"
)

// ImageBooter boots microVMs from a materialized OCI ext4 rootfs (runtime-fleet
// CP3). It reuses the sibling Provider for the low-level Firecracker API dance
// (socket handling, machine/boot/drive config, InstanceStart) and its config
// (kernel path, binary path, socket dir), but owns its own per-workload routed
// /30 networking so it never touches the warm-pool netns/bridge machinery.
type ImageBooter struct {
	p             *Provider
	advertiseHost string
	hostPortMin   int
	hostPortMax   int

	natOnce sync.Once
	natErr  error

	mu        sync.Mutex
	usedIndex map[int]bool
	usedPort  map[int]bool
}

var _ usecase.ImageBooter = (*ImageBooter)(nil)

// NewImageBooter constructs an ImageBooter over an existing Provider.
func NewImageBooter(p *Provider, advertiseHost string, hostPortMin, hostPortMax int) *ImageBooter {
	if hostPortMin <= 0 || hostPortMax < hostPortMin {
		hostPortMin, hostPortMax = 20000, 20999
	}
	return &ImageBooter{
		p:             p,
		advertiseHost: advertiseHost,
		hostPortMin:   hostPortMin,
		hostPortMax:   hostPortMax,
		usedIndex:     make(map[int]bool),
		usedPort:      make(map[int]bool),
	}
}

// Seed marks indices and ports already in use by active workloads so the
// allocator (re)started from a persisted set never double-allocates.
func (b *ImageBooter) Seed(indices []int, ports []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range indices {
		if n > 0 {
			b.usedIndex[n] = true
		}
	}
	for _, p := range ports {
		if p > 0 {
			b.usedPort[p] = true
		}
	}
}

func (b *ImageBooter) allocIndex() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for n := 1; n <= imgMaxIndex; n++ {
		if !b.usedIndex[n] {
			b.usedIndex[n] = true
			return n, nil
		}
	}
	return 0, fmt.Errorf("image-boot network index space exhausted (max %d)", imgMaxIndex)
}

func (b *ImageBooter) freeIndex(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	delete(b.usedIndex, n)
	b.mu.Unlock()
}

func (b *ImageBooter) allocPort() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for p := b.hostPortMin; p <= b.hostPortMax; p++ {
		if !b.usedPort[p] {
			b.usedPort[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("image-boot host port range [%d,%d] exhausted", b.hostPortMin, b.hostPortMax)
}

func (b *ImageBooter) freePort(p int) {
	if p <= 0 {
		return
	}
	b.mu.Lock()
	delete(b.usedPort, p)
	b.mu.Unlock()
}

// imgNet holds the derived addressing for a workload index.
type imgNet struct {
	index   int
	tapName string
	hostIP  string
	guestIP string
}

// deriveNet computes the /30 addressing for index n.
func deriveNet(n int) imgNet {
	base := n * 4
	o3 := (base >> 8) & 0xff
	o4 := base & 0xff
	return imgNet{
		index:   n,
		tapName: fmt.Sprintf("img%d", n),
		hostIP:  fmt.Sprintf("10.201.%d.%d", o3, o4+1),
		guestIP: fmt.Sprintf("10.201.%d.%d", o3, o4+2),
	}
}

// ensureNAT enables IP forwarding and installs an idempotent MASQUERADE for the
// image-boot /16 so guests reach external destinations. Runs once.
func (b *ImageBooter) ensureNAT() error {
	b.natOnce.Do(func() {
		_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
		if exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", imgSubnet16, "-j", "MASQUERADE").Run() != nil {
			if out, err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", imgSubnet16, "-j", "MASQUERADE").CombinedOutput(); err != nil {
				b.natErr = fmt.Errorf("install image-boot MASQUERADE: %s: %w", string(out), err)
			}
		}
		// Docker sets the FORWARD policy to DROP — DNAT'd ingress to a workload
		// and workload egress both traverse FORWARD, so accept the img subnet.
		for _, rule := range [][]string{
			{"-d", imgSubnet16, "-j", "ACCEPT"},
			{"-s", imgSubnet16, "-j", "ACCEPT"},
		} {
			check := append([]string{"-C", "FORWARD"}, rule...)
			if exec.Command("iptables", check...).Run() != nil {
				insert := append([]string{"-I", "FORWARD", "1"}, rule...)
				if out, err := exec.Command("iptables", insert...).CombinedOutput(); err != nil && b.natErr == nil {
					b.natErr = fmt.Errorf("install image-boot FORWARD accept: %s: %w", string(out), err)
				}
			}
		}
		// Tenant isolation: the two accepts above would also permit guest→guest
		// traffic (both endpoints in the flat /16, one host root netns). Deny any
		// flow whose source AND destination are both in the img subnet so one
		// resident tenant's microVM cannot reach another's. Inserted at the TOP
		// (after the accepts, so -I 1 lands it above them) → evaluated first;
		// ingress (src=host/external) and egress (dst=external) each have only
		// one side in the subnet and are unaffected. (Mirrors the 172.16/24 rule.)
		denyRule := []string{"-s", imgSubnet16, "-d", imgSubnet16, "-j", "DROP"}
		check := append([]string{"-C", "FORWARD"}, denyRule...)
		if exec.Command("iptables", check...).Run() != nil {
			insert := append([]string{"-I", "FORWARD", "1"}, denyRule...)
			if out, err := exec.Command("iptables", insert...).CombinedOutput(); err != nil && b.natErr == nil {
				b.natErr = fmt.Errorf("install image-boot cross-tenant deny: %s: %w", string(out), err)
			}
		}
	})
	return b.natErr
}

// createTap creates the routed /30 TAP for a workload.
func (b *ImageBooter) createTap(nw imgNet) error {
	// A stale device from a crashed workload blocks TUNSETIFF — delete first.
	_ = exec.Command("ip", "link", "del", nw.tapName).Run()
	if err := b.ensureNAT(); err != nil {
		return err
	}
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", nw.tapName, "mode", "tap"},
		{"ip", "addr", "add", nw.hostIP + "/30", "dev", nw.tapName},
		{"ip", "link", "set", nw.tapName, "up"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = exec.Command("ip", "link", "del", nw.tapName).Run()
			return fmt.Errorf("create img tap %v: %s: %w", args, string(out), err)
		}
	}
	return nil
}

// destroyTap removes the TAP device (best-effort).
func (b *ImageBooter) destroyTap(tapName string) {
	if tapName == "" {
		return
	}
	if out, err := exec.Command("ip", "link", "del", tapName).CombinedOutput(); err != nil {
		log.Printf("image-boot: warning: delete tap %s: %s: %v", tapName, string(out), err)
	}
}

// installDNAT publishes hostPort → guestIP:guestPort for a resident workload,
// on both the PREROUTING (external) and OUTPUT (host-local) chains.
func (b *ImageBooter) installDNAT(hostPort int, guestIP string, guestPort int) error {
	dest := fmt.Sprintf("%s:%d", guestIP, guestPort)
	rules := b.dnatRuleSpecs(hostPort, dest)
	for _, r := range rules {
		args := append([]string{"-t", "nat", "-A"}, r...)
		if out, err := exec.Command("iptables", args...).CombinedOutput(); err != nil {
			b.removeDNAT(hostPort, guestIP, guestPort) // roll back partial install
			return fmt.Errorf("install DNAT %v: %s: %w", args, string(out), err)
		}
	}
	return nil
}

// removeDNAT deletes the DNAT rules for a resident workload (best-effort).
func (b *ImageBooter) removeDNAT(hostPort int, guestIP string, guestPort int) {
	dest := fmt.Sprintf("%s:%d", guestIP, guestPort)
	for _, r := range b.dnatRuleSpecs(hostPort, dest) {
		args := append([]string{"-t", "nat", "-D"}, r...)
		_ = exec.Command("iptables", args...).Run()
	}
}

// dnatRuleSpecs returns the iptables rule bodies (chain + match + target) for a
// host-port → guest publish, used for both -A (install) and -D (remove).
func (b *ImageBooter) dnatRuleSpecs(hostPort int, dest string) [][]string {
	portStr := fmt.Sprintf("%d", hostPort)
	return [][]string{
		{"PREROUTING", "-p", "tcp", "--dport", portStr, "-j", "DNAT", "--to-destination", dest},
		{"OUTPUT", "-p", "tcp", "-o", "lo", "--dport", portStr, "-j", "DNAT", "--to-destination", dest},
	}
}

// bootArgs builds the kernel command line: init=/sentiae/init plus the proven
// ip= static-network arg (same mechanism as the warm path).
func imageBootArgs(guestIP, hostIP string) string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off init=/sentiae/init ip=%s::%s:%s::eth0:off",
		guestIP, hostIP, imgNetmask,
	)
}

// startVM starts a Firecracker process configured to boot the given rootfs on
// the given TAP. It returns the running process and socket path. The caller is
// responsible for killing the process and cleaning the socket.
func (b *ImageBooter) startVM(ctx context.Context, vmID uuid.UUID, rootfsPath string, nw imgNet, vcpu, memMB int) (*exec.Cmd, string, error) {
	socketPath := b.p.socketPath(vmID)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return nil, "", fmt.Errorf("create socket dir: %w", err)
	}

	// Deliberately NOT CommandContext: the VM must outlive the Provision RPC's
	// request context (a resident VM killed on gRPC return was the CP3 bring-up
	// bug). Lifecycle is owned by killVM/Decommission and the test timeout.
	cmd := exec.Command(b.p.cfg.BinaryPath, "--api-sock", socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The guest serial console (console=ttyS0) arrives on the firecracker
	// process's stdout — capture it next to the rootfs so a dead workload is
	// diagnosable (the only place the image's own crash output ever appears).
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(rootfsPath), "console.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start firecracker: %w", err)
	}

	if err := b.p.waitForSocket(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("firecracker socket not ready: %w", err)
	}

	client := b.p.unixHTTPClient(socketPath)
	if err := b.p.apiPut(ctx, client, "/machine-config", map[string]any{
		"vcpu_count":   vcpu,
		"mem_size_mib": memMB,
	}); err != nil {
		b.killVM(cmd, socketPath)
		return nil, "", fmt.Errorf("configure machine: %w", err)
	}
	if err := b.p.apiPut(ctx, client, "/boot-source", map[string]any{
		"kernel_image_path": b.p.cfg.KernelPath,
		"boot_args":         imageBootArgs(nw.guestIP, nw.hostIP),
	}); err != nil {
		b.killVM(cmd, socketPath)
		return nil, "", fmt.Errorf("configure boot source: %w", err)
	}
	if err := b.p.apiPut(ctx, client, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		b.killVM(cmd, socketPath)
		return nil, "", fmt.Errorf("configure rootfs: %w", err)
	}
	if err := b.p.apiPut(ctx, client, "/network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"guest_mac":     generateMAC(vmID),
		"host_dev_name": nw.tapName,
	}); err != nil {
		b.killVM(cmd, socketPath)
		return nil, "", fmt.Errorf("configure network: %w", err)
	}
	if err := b.p.startInstance(ctx, socketPath); err != nil {
		b.killVM(cmd, socketPath)
		return nil, "", fmt.Errorf("start instance: %w", err)
	}
	return cmd, socketPath, nil
}

// killVM force-kills a firecracker process and removes its socket.
func (b *ImageBooter) killVM(cmd *exec.Cmd, socketPath string) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	if socketPath != "" {
		_ = os.Remove(socketPath)
		_ = os.Remove(socketPath + ".vsock")
	}
}

// BootTest boots a single-shot VM, waits for it to power off (bounded by the
// timeout), reads /sentiae/out/* from the rootfs, and tears everything down.
func (b *ImageBooter) BootTest(ctx context.Context, in usecase.ImageBootInput) (usecase.ImageTestResult, error) {
	nw, cleanupNet, err := b.setupNet(false, 0, in.Port)
	if err != nil {
		return usecase.ImageTestResult{}, err
	}
	defer cleanupNet()
	defer func() { _ = os.Remove(in.RootfsPath) }()

	vcpu, memMB := normalizeResources(in.VCPU, in.MemoryMB)
	cmd, socketPath, err := b.startVM(ctx, in.WorkloadID, in.RootfsPath, nw, vcpu, memMB)
	if err != nil {
		return usecase.ImageTestResult{}, err
	}
	defer func() { _ = os.Remove(socketPath) }()

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	waitDone := make(chan error, 1)
	go func() {
		defer func() { _ = recover() }()
		waitDone <- cmd.Wait()
	}()

	timedOut := false
	select {
	case <-waitDone:
	case <-time.After(timeout):
		timedOut = true
		_ = cmd.Process.Kill()
		<-waitDone
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waitDone
		return usecase.ImageTestResult{}, ctx.Err()
	}

	stdout, stderr, exitCode := b.readTestOutput(in.WorkloadID, in.RootfsPath)
	if timedOut {
		return usecase.ImageTestResult{
			ExitCode: 124,
			Stdout:   stdout,
			Stderr:   "test run timed out",
			TimedOut: true,
		}, nil
	}
	return usecase.ImageTestResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr}, nil
}

// readTestOutput loop-mounts the rootfs read-only and reads the /sentiae/out
// well-known files (mirrors Provider.readResults).
func (b *ImageBooter) readTestOutput(vmID uuid.UUID, rootfsPath string) (stdout, stderr string, exitCode int) {
	mountDir := filepath.Join(os.TempDir(), "img-out-"+vmID.String())
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", fmt.Sprintf("image-boot: mkdir mount: %v", err), 1
	}
	defer os.RemoveAll(mountDir)

	// noload: the guest powers off without a clean unmount, so the ext4 journal
	// is dirty; a plain ro mount refuses it (EROFS journal replay).
	if out, err := exec.Command("mount", "-o", "loop,ro,noload", rootfsPath, mountDir).CombinedOutput(); err != nil {
		return "", fmt.Sprintf("image-boot: mount rootfs: %s: %v", string(out), err), 1
	}
	defer func() { _ = exec.Command("umount", mountDir).Run() }()

	outBytes, _ := os.ReadFile(filepath.Join(mountDir, "sentiae", "out", "stdout.txt"))
	errBytes, _ := os.ReadFile(filepath.Join(mountDir, "sentiae", "out", "stderr.txt"))
	codeBytes, _ := os.ReadFile(filepath.Join(mountDir, "sentiae", "out", "exit_code.txt"))

	exitCode = parseExitCode(string(codeBytes))
	return string(outBytes), string(errBytes), exitCode
}

// BootResident boots a long-lived VM and returns once its port answers a TCP
// dial (retried up to 60s). The VM keeps running; Decommission tears it down.
func (b *ImageBooter) BootResident(ctx context.Context, in usecase.ImageBootInput) (usecase.ImageResidentResult, error) {
	hostPort, err := b.allocPort()
	if err != nil {
		return usecase.ImageResidentResult{}, err
	}
	nw, cleanupNet, err := b.setupNet(true, hostPort, in.Port)
	if err != nil {
		b.freePort(hostPort)
		return usecase.ImageResidentResult{}, err
	}

	vcpu, memMB := normalizeResources(in.VCPU, in.MemoryMB)
	cmd, socketPath, err := b.startVM(ctx, in.WorkloadID, in.RootfsPath, nw, vcpu, memMB)
	if err != nil {
		cleanupNet()
		b.freePort(hostPort)
		return usecase.ImageResidentResult{}, err
	}

	// Reap the process if it dies while we wait (defensive — a resident VM that
	// exits during boot must not become a zombie).
	go func() {
		defer func() { _ = recover() }()
		_ = cmd.Wait()
	}()

	if err := waitForTCP(ctx, nw.guestIP, in.Port, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(socketPath)
		cleanupNet()
		b.freePort(hostPort)
		return usecase.ImageResidentResult{}, fmt.Errorf("resident workload did not serve %s:%d: %w", nw.guestIP, in.Port, err)
	}

	return usecase.ImageResidentResult{
		PID:        cmd.Process.Pid,
		GuestIP:    nw.guestIP,
		HostPort:   hostPort,
		NetIndex:   nw.index,
		TapName:    nw.tapName,
		SocketPath: socketPath,
	}, nil
}

// Decommission tears down a resident workload: kill process, remove DNAT + TAP,
// free the index/port, delete the rootfs.
func (b *ImageBooter) Decommission(_ context.Context, in usecase.ImageDecommissionInput) error {
	if in.PID > 0 {
		if proc, err := os.FindProcess(in.PID); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				defer func() { _ = recover() }()
				_, _ = proc.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = proc.Kill()
			}
		}
	}
	if in.HostPort > 0 && in.GuestIP != "" {
		b.removeDNAT(in.HostPort, in.GuestIP, in.Port)
	}
	b.destroyTap(in.TapName)
	b.freeIndex(in.NetIndex)
	b.freePort(in.HostPort)
	if in.SocketPath != "" {
		_ = os.Remove(in.SocketPath)
		_ = os.Remove(in.SocketPath + ".vsock")
	}
	if in.RootfsPath != "" {
		_ = os.Remove(in.RootfsPath)
	}
	return nil
}

// setupNet allocates an index, creates the TAP, and (resident only) installs
// the DNAT. It returns the derived addressing plus a cleanup func that reverses
// exactly what it did.
func (b *ImageBooter) setupNet(resident bool, hostPort, guestPort int) (imgNet, func(), error) {
	idx, err := b.allocIndex()
	if err != nil {
		return imgNet{}, func() {}, err
	}
	nw := deriveNet(idx)
	if err := b.createTap(nw); err != nil {
		b.freeIndex(idx)
		return imgNet{}, func() {}, err
	}
	if resident {
		if err := b.installDNAT(hostPort, nw.guestIP, guestPort); err != nil {
			b.destroyTap(nw.tapName)
			b.freeIndex(idx)
			return imgNet{}, func() {}, err
		}
	}
	cleanup := func() {
		if resident {
			b.removeDNAT(hostPort, nw.guestIP, guestPort)
		}
		b.destroyTap(nw.tapName)
		b.freeIndex(idx)
	}
	return nw, cleanup, nil
}

// waitForTCP dials host:port until it connects or the deadline passes.
func waitForTCP(ctx context.Context, host string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s", timeout)
		}
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// normalizeResources applies the CP3 defaults (1 vcpu, 512 MB) for zero values.
func normalizeResources(vcpu, memMB int) (int, int) {
	if vcpu <= 0 {
		vcpu = 1
	}
	if memMB <= 0 {
		memMB = 512
	}
	return vcpu, memMB
}

// parseExitCode parses the exit_code.txt contents, defaulting to 0.
func parseExitCode(s string) int {
	code := 0
	neg := false
	started := false
	for _, r := range s {
		if r == '-' && !started {
			neg = true
			started = true
			continue
		}
		if r >= '0' && r <= '9' {
			code = code*10 + int(r-'0')
			started = true
			continue
		}
		if started {
			break
		}
	}
	if neg {
		return -code
	}
	return code
}
