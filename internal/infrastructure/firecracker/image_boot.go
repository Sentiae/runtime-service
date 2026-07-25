package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/infrastructure/netfabric"
	"github.com/sentiae/runtime-service/internal/port/gateway"
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

	// controlTokens holds the per-VM post-boot control token this booter mints
	// and delivers inside the sealed secret push (D-185a). The
	// GuestControlClient reads from the SAME instance — that shared store is the
	// whole handoff between "the VM booted" and "the host can talk to it".
	controlTokens *GuestControlTokens

	// guestControl is the post-boot control channel into a resident guest — the
	// sibling adapter in this package over the SAME token store. The stop path
	// uses it to ask the guest to shut itself down before any signal reaches the
	// VMM.
	guestControl gateway.GuestControl

	// stop bounds each stage of the resident stop path. Fields rather than
	// constants only so a unit test can compress them; the defaults are what the
	// fleet runs.
	stop stopTimings

	// findProcess resolves a recorded VMM pid to a host process handle. A field
	// so the stop path's signal ORDER is assertable without a real microVM;
	// production always runs the os.FindProcess default.
	findProcess func(pid int) (vmProcess, error)

	natOnce sync.Once
	natErr  error

	mu        sync.Mutex
	usedIndex map[int]bool
}

var _ usecase.ImageBooter = (*ImageBooter)(nil)

// NewImageBooter constructs an ImageBooter over an existing Provider. rt#8
// retired per-VM host-port DNAT (the fleet owns ingress via Caddy), so no host
// port range is tracked — guests are reached directly on their routed /30.
func NewImageBooter(p *Provider, advertiseHost string, controlTokens *GuestControlTokens) *ImageBooter {
	if controlTokens == nil {
		controlTokens = NewGuestControlTokens()
	}
	return &ImageBooter{
		p:             p,
		advertiseHost: advertiseHost,
		controlTokens: controlTokens,
		// Same store as the booter's, which is what makes the token minted at boot
		// spendable at stop time.
		guestControl: NewGuestControlClient(controlTokens),
		stop:         defaultStopTimings(),
		findProcess:  osFindProcess,
		usedIndex:    make(map[int]bool),
	}
}

// stopTimings bounds each stage of the resident stop path.
type stopTimings struct {
	// guestShutdown bounds the in-guest graceful stop: the vsock round-trip, the
	// SIGINT forwarded to the workload, and the guest's own bounded wait for that
	// child to exit. Postgres fast shutdown has to finish a checkpoint — seconds
	// on a small database, but never instant — so this matches the control
	// client's own SHUTDOWN budget (the guest's wait plus transport slack). A
	// tighter value here would abandon a guest that is still shutting down
	// correctly, which is exactly the crash-stop this path exists to avoid.
	guestShutdown time.Duration
	// powerOff bounds the wait for the VMM to exit after the guest acked. By then
	// the guest only has to sync and issue its power-off reboot(2), so it is
	// short.
	powerOff time.Duration
	// exitPoll is the interval between VMM liveness polls in that wait.
	exitPoll time.Duration
	// sigtermGrace is the fallback path's wait between SIGTERM and SIGKILL —
	// unchanged from the pre-control-channel behaviour.
	sigtermGrace time.Duration
}

func defaultStopTimings() stopTimings {
	return stopTimings{
		guestShutdown: controlShutdownTimeout,
		powerOff:      15 * time.Second,
		exitPoll:      100 * time.Millisecond,
		sigtermGrace:  5 * time.Second,
	}
}

// vmProcess is the host handle to a running VMM process.
type vmProcess interface {
	Signal(sig os.Signal) error
	Kill() error
	Wait() (*os.ProcessState, error)
}

var _ vmProcess = (*os.Process)(nil)

func osFindProcess(pid int) (vmProcess, error) { return os.FindProcess(pid) }

// Seed marks network indices already in use by active workloads so the allocator
// (re)started from a persisted set never double-allocates a /30.
func (b *ImageBooter) Seed(indices []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range indices {
		if n > 0 {
			b.usedIndex[n] = true
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
//
// ⚠ It deliberately writes NOTHING to the FORWARD chain (CP4.5 §9#5, D-164).
// The netfabric enforcer is the single writer of the fleet's FORWARD program —
// including the cross-tenant DROP and the two blanket /16 ACCEPTs that used to
// live here. That is not a refactor for tidiness; it is the fix for a real bug:
//
//	ensureNAT is a sync.Once fired from the FIRST VM boot, i.e. AFTER DI init, and
//	it inserted each rule at FORWARD position 1. So on a fresh host it landed its
//	DROP and ACCEPTs ABOVE the enforcer's anchors, which (a) made every network
//	policy unreachable — the DROP swallowed inter-VM traffic before SNT-XVM could
//	ACCEPT it — and (b) silently disabled every job's egress allowlist, because
//	`-s 10.201.0.0/16 -j ACCEPT` terminated the packet before SNT-EGRESS was
//	reached. It was ordering-by-boot-sequence, so it worked after a service
//	restart and broke after a host reboot.
//
// Two uncoordinated writers to one ordered chain IS the bug. MASQUERADE stays
// here: it is the nat table, a different resource with no ordering contention.
func (b *ImageBooter) ensureNAT() error {
	b.natOnce.Do(func() {
		_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
		if exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", imgSubnet16, "-j", "MASQUERADE").Run() != nil {
			if out, err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", imgSubnet16, "-j", "MASQUERADE").CombinedOutput(); err != nil {
				b.natErr = fmt.Errorf("install image-boot MASQUERADE: %s: %w", string(out), err)
			}
		}
	})
	return b.natErr
}

// createTap creates the routed /30 TAP for a workload, owned by the VM's
// unprivileged uid/gid: the jailed VMM has no CAP_NET_ADMIN, so it can only
// TUNSETIFF-attach to a tap device it owns.
func (b *ImageBooter) createTap(nw imgNet, uid, gid int) error {
	// A stale device from a crashed workload blocks TUNSETIFF — delete first.
	_ = exec.Command("ip", "link", "del", nw.tapName).Run()
	if err := b.ensureNAT(); err != nil {
		return err
	}
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", nw.tapName, "mode", "tap", "user", strconv.Itoa(uid), "group", strconv.Itoa(gid)},
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
func (b *ImageBooter) destroyTap(ctx context.Context, tapName string) {
	if tapName == "" {
		return
	}
	if out, err := exec.Command("ip", "link", "del", tapName).CombinedOutput(); err != nil {
		logger.FromContext(ctx).Warn("image-boot: delete tap failed", "tap_name", tapName, "output", string(out), "err", err)
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
func (b *ImageBooter) startVM(ctx context.Context, vmID uuid.UUID, rootfsPath string, nw imgNet, vcpu, memMB int, expectSecrets bool, dataDiskPath string) (*exec.Cmd, string, error) {
	// The per-VM uid is derived from the network index, which is unique by
	// construction and reclaimed on restart — outside the span two VMs could
	// collide on one uid, which is the cross-tenant hole the jail closes.
	if nw.index <= 0 || nw.index >= b.p.cfg.VMUIDSpan {
		return nil, "", fmt.Errorf("image-boot network index %d outside the per-VM uid span [1,%d)", nw.index, b.p.cfg.VMUIDSpan)
	}
	uid := vmUID(b.p.cfg.VMUIDBase, nw.index)

	// The jail id is the network index, not the VM uuid: the uuid already owns the
	// socket basename, and a second one would push the host socket path past the
	// AF_UNIX 107-byte limit. The index is unique across live VMs (same allocator
	// as the uid), and prepare() clears a stale dir left by whoever held it before.
	jailID := strconv.Itoa(nw.index)
	j := newVMJail(b.p.cfg.ChrootBase, jailID, uid)
	if err := j.prepare(); err != nil {
		return nil, "", fmt.Errorf("prepare vm jail: %w", err)
	}
	if err := j.mkdir("run"); err != nil {
		j.remove()
		return nil, "", fmt.Errorf("prepare vm jail: %w", err)
	}
	if err := j.mkdir("kernel"); err != nil {
		j.remove()
		return nil, "", fmt.Errorf("prepare vm jail: %w", err)
	}

	// The socket keeps its host view and its <vm-id>.sock basename outside this
	// function: vmIDFromSocketPath parses that basename, and the vsock UDS is
	// derived as socketPath+".vsock". The VMM only ever sees the chroot view.
	socketRel := "run/" + vmID.String() + ".sock"
	socketPath := j.hostPath(socketRel)
	chrootSocketPath := j.chrootPath(socketRel)
	if err := checkSocketPathFits(socketPath); err != nil {
		j.remove()
		return nil, "", err
	}

	kernelChrootPath, err := j.link(b.p.cfg.KernelPath, "kernel/vmlinux", false)
	if err != nil {
		j.remove()
		return nil, "", fmt.Errorf("place kernel in jail: %w", err)
	}
	rootfsChrootPath, err := j.link(rootfsPath, "rootfs.ext4", true)
	if err != nil {
		j.remove()
		return nil, "", fmt.Errorf("place rootfs in jail: %w", err)
	}
	dataChrootPath := ""
	if dataDiskPath != "" {
		dataChrootPath, err = j.link(dataDiskPath, "data.ext4", true)
		if err != nil {
			j.remove()
			return nil, "", fmt.Errorf("place data volume in jail: %w", err)
		}
	}

	// Deliberately NOT CommandContext: the VM must outlive the Provision RPC's
	// request context (a resident VM killed on gRPC return was the CP3 bring-up
	// bug). Lifecycle is owned by killVM/Decommission and the test timeout.
	//
	// Always through the jailer (chroot + seccomp + cgroup at an unprivileged
	// per-VM uid) — no flag, no fallback: an unjailed VMM escape lands as host
	// root, cross-tenant. No --daemonize (it would send the guest console to
	// /dev/null) and no --new-pid-ns (jailer execve's firecracker in place, so
	// cmd.Process.Pid stays the pid Decommission signals).
	cmd := exec.Command(b.p.cfg.JailerPath,
		"--id", jailID,
		"--exec-file", b.p.cfg.BinaryPath,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(uid),
		"--chroot-base-dir", b.p.cfg.ChrootBase,
		"--cgroup-version", "2",
		"--",
		"--api-sock", chrootSocketPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The guest serial console (console=ttyS0) arrives on the firecracker
	// process's stdout — capture it next to the rootfs so a dead workload is
	// diagnosable (the only place the image's own crash output ever appears).
	// This is a host-side fd inherited across the jailer's execve, so it stays
	// outside the chroot on purpose.
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(rootfsPath), "console.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		j.remove()
		return nil, "", fmt.Errorf("start firecracker: %w", err)
	}

	if err := b.p.waitForSocket(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		j.remove()
		return nil, "", fmt.Errorf("firecracker socket not ready: %w", err)
	}

	client := b.p.unixHTTPClient(socketPath)
	if err := b.p.apiPut(ctx, client, "/machine-config", map[string]any{
		"vcpu_count":   vcpu,
		"mem_size_mib": memMB,
	}); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
		return nil, "", fmt.Errorf("configure machine: %w", err)
	}
	if err := b.p.apiPut(ctx, client, "/boot-source", map[string]any{
		"kernel_image_path": kernelChrootPath,
		"boot_args":         imageBootArgs(nw.guestIP, nw.hostIP),
	}); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
		return nil, "", fmt.Errorf("configure boot source: %w", err)
	}
	if err := b.p.apiPut(ctx, client, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsChrootPath,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
		return nil, "", fmt.Errorf("configure rootfs: %w", err)
	}
	// rt#9 — attach the persistent data volume as a 2nd virtio-blk device. The
	// guest sees it as /dev/vdb and mounts it at the runtime.json data_mount_path.
	if dataDiskPath != "" {
		if err := b.p.apiPut(ctx, client, "/drives/data", map[string]any{
			"drive_id":       "data",
			"path_on_host":   dataChrootPath,
			"is_root_device": false,
			"is_read_only":   false,
			// SentiaeDB Phase-0 P0 (D-184, #p19-firecracker-cache-type-fsync):
			// the persistent data volume MUST use Writeback so the guest's fsync
			// is honored down to the host. Firecracker's default (Unsafe) drops
			// flushes at the host boundary — every Postgres fsync becomes a no-op
			// and a host crash loses committed data. Writeback is the durable mode.
			"cache_type": "Writeback",
		}); err != nil {
			b.killVM(cmd, socketPath)
			j.remove()
			return nil, "", fmt.Errorf("configure data drive: %w", err)
		}
	}
	if err := b.p.apiPut(ctx, client, "/network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"guest_mac":     generateMAC(vmID),
		"host_dev_name": nw.tapName,
	}); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
		return nil, "", fmt.Errorf("configure network: %w", err)
	}
	// Secret channel: attach a vsock device so the host can push the secret
	// bundle to the guest after start (invariant I32). Unlike the warm path this
	// must NOT warn-and-continue — a secret workload must not boot without its
	// channel, so a /vsock config failure fails the boot.
	if expectSecrets {
		// Chroot view: the VMM creates the UDS at the host path
		// socketPath+".vsock", which is what pushSecrets dials.
		if err := b.p.apiPut(ctx, client, "/vsock", map[string]any{
			"guest_cid": 3,
			"uds_path":  chrootSocketPath + ".vsock",
		}); err != nil {
			b.killVM(cmd, socketPath)
			j.remove()
			return nil, "", fmt.Errorf("configure vsock secret channel: %w", err)
		}
	}
	if err := b.p.startInstance(ctx, socketPath); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
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
		// VMs booted before the image-boot path was jailed have their sockets
		// outside any chroot and yield "" — the removals above are their cleanup.
		if dir := jailDirFromSocketPath(b.p.cfg.ChrootBase, socketPath); dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
}

// BootTest boots a single-shot VM, waits for it to power off (bounded by the
// timeout), reads /sentiae/out/* from the rootfs, and tears everything down.
// It serves both the test class and the one-shot job class; the job class is the
// same path plus a secret push (already handled below via ExpectSecrets) and an
// egress allowlist (in.EgressAllow).
func (b *ImageBooter) BootTest(ctx context.Context, in usecase.ImageBootInput) (usecase.ImageTestResult, error) {
	nw, cleanupNet, err := b.setupNet(ctx)
	if err != nil {
		return usecase.ImageTestResult{}, err
	}
	defer cleanupNet()
	defer func() { _ = os.Remove(in.RootfsPath) }()

	// Job class: lock egress down to the allowlist BEFORE the VM starts, so the
	// workload never gets an unfiltered moment. The jump lands in SNT-EGRESS, which
	// the netfabric enforcer anchors BELOW the inter-VM decision (SNT-XVM) and
	// ABOVE the blanket subnet ACCEPT — so the allowlist still wins over the
	// ACCEPT, but can no longer win over the cross-tenant DENY. Before #5 this jump
	// went to FORWARD position 1, above everything, which is how an allowlist
	// naming 10.201.0.0/16 bought lateral reach to every other tenant's microVM.
	// Fail closed: if the chain cannot be installed the boot aborts rather than run
	// a secret-bearing job with unrestricted egress.
	if len(in.EgressAllow) > 0 {
		if err := b.p.applyEgressList(ctx, netfabric.EgressParentChain, nw.tapName, in.EgressAllow); err != nil {
			b.p.flushEgressList(netfabric.EgressParentChain, nw.tapName)
			return usecase.ImageTestResult{}, fmt.Errorf("apply egress allowlist: %w", err)
		}
		defer b.p.flushEgressList(netfabric.EgressParentChain, nw.tapName)
	}

	vcpu, memMB := normalizeResources(in.VCPU, in.MemoryMB)
	cmd, socketPath, err := b.startVM(ctx, in.WorkloadID, in.RootfsPath, nw, vcpu, memMB, in.ExpectSecrets, "")
	if err != nil {
		return usecase.ImageTestResult{}, err
	}
	defer func() {
		_ = os.Remove(socketPath)
		// The jail holds a hard link to the rootfs, so its removal is also what
		// frees the rootfs inode the deferred RootfsPath removal unlinks.
		if dir := jailDirFromSocketPath(b.p.cfg.ChrootBase, socketPath); dir != "" {
			_ = os.RemoveAll(dir)
		}
	}()

	// Push secrets over vsock before the guest execs its workload. Fail-closed:
	// a push failure kills the VM and aborts the test (invariant I32).
	// No control token: the test/job class is single-shot and its guest never arms
	// the control listener, so minting one would only hand out a credential for a
	// channel that does not exist.
	if in.ExpectSecrets {
		if err := pushSecrets(ctx, socketPath, in.Secrets, in.BootstrapNonce, "", secretPushTimeout); err != nil {
			b.killVM(cmd, socketPath)
			return usecase.ImageTestResult{}, fmt.Errorf("push secrets: %w", err)
		}
	}

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
	// rt#8 — no host-port allocation: the fleet reaches the guest directly on its
	// routed /30 (Caddy proxies the public host to guestIP:appPort).
	nw, cleanupNet, err := b.setupNet(ctx)
	if err != nil {
		return usecase.ImageResidentResult{}, err
	}

	vcpu, memMB := normalizeResources(in.VCPU, in.MemoryMB)
	cmd, socketPath, err := b.startVM(ctx, in.WorkloadID, in.RootfsPath, nw, vcpu, memMB, in.ExpectSecrets, in.DataDiskPath)
	if err != nil {
		cleanupNet()
		return usecase.ImageResidentResult{}, err
	}

	// Push secrets over vsock before the guest execs its workload. Fail-closed:
	// a push failure kills the VM and tears down the net plumbing (invariant I32).
	//
	// D-185a — the same push carries the post-boot control token. It is minted per
	// VM here and recorded ONLY after the guest has acked the bundle: a token
	// stored for a push that failed would leave the client believing in a channel
	// that was never armed.
	if in.ExpectSecrets {
		controlToken, terr := newControlToken()
		if terr != nil {
			b.killVM(cmd, socketPath)
			cleanupNet()
			return usecase.ImageResidentResult{}, terr
		}
		if err := pushSecrets(ctx, socketPath, in.Secrets, in.BootstrapNonce, controlToken, secretPushTimeout); err != nil {
			b.killVM(cmd, socketPath)
			cleanupNet()
			return usecase.ImageResidentResult{}, fmt.Errorf("push secrets: %w", err)
		}
		b.controlTokens.Put(socketPath, controlToken)
	}

	// Reap the process if it dies while we wait (defensive — a resident VM that
	// exits during boot must not become a zombie).
	go func() {
		defer func() { _ = recover() }()
		_ = cmd.Wait()
	}()

	if err := waitForTCP(ctx, nw.guestIP, in.Port, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		b.controlTokens.Delete(socketPath)
		_ = os.Remove(socketPath)
		if dir := jailDirFromSocketPath(b.p.cfg.ChrootBase, socketPath); dir != "" {
			_ = os.RemoveAll(dir)
		}
		cleanupNet()
		return usecase.ImageResidentResult{}, fmt.Errorf("resident workload did not serve %s:%d: %w", nw.guestIP, in.Port, err)
	}

	return usecase.ImageResidentResult{
		PID:        cmd.Process.Pid,
		GuestIP:    nw.guestIP,
		HostPort:   0, // rt#8 — DNAT retired; endpoint is the private guest addr.
		NetIndex:   nw.index,
		TapName:    nw.tapName,
		SocketPath: socketPath,
	}, nil
}

// Decommission tears down a resident workload: stop the guest, remove the TAP,
// free the index, delete the rootfs. rt#8 retired the per-VM DNAT so there is no
// host-port rule or port to reclaim.
//
// ⚠ Teardown is NEVER blockable. stopVM returns nothing to fail on and every
// step below runs unconditionally — including when the guest refuses to die and
// when the caller's context is already cancelled. That is the deliberate
// opposite of the snapshot path's fail-closed posture, and the asymmetry is the
// point: refusing to snapshot protects data, whereas refusing to tear down
// protects nothing and strands the customer's resource (a running VM, its /30,
// its rootfs) with no way to release it.
func (b *ImageBooter) Decommission(ctx context.Context, in usecase.ImageDecommissionInput) error {
	b.stopVM(ctx, in)
	b.destroyTap(ctx, in.TapName)
	b.freeIndex(in.NetIndex)
	if in.SocketPath != "" {
		// The VM is gone; stop bearing its control token (D-185a). This MUST stay
		// after stopVM: that token is what authenticates the guest SHUTDOWN, so
		// dropping it first would make every graceful stop fail and fall back to
		// killing the VMM — the exact defect this path fixes.
		b.controlTokens.Delete(in.SocketPath)
		_ = os.Remove(in.SocketPath)
		_ = os.Remove(in.SocketPath + ".vsock")
		// "" for a pre-jail VM, whose socket lives outside any chroot.
		if dir := jailDirFromSocketPath(b.p.cfg.ChrootBase, in.SocketPath); dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
	if in.RootfsPath != "" {
		_ = os.Remove(in.RootfsPath)
	}
	return nil
}

// stopVM stops the microVM, preferring a clean in-guest shutdown over killing
// the VMM.
//
// Signalling the Firecracker process stops the guest the way pulling the power
// cord does: the workload never gets its SIGINT, so Postgres never runs its fast
// shutdown, and image-init never runs its own sync-and-power-off. Doing that on
// every scale-to-zero and every decommission meant every customer database was
// crash-stopped, and left a detached volume's snapshot only as consistent as
// that crash. So the guest is asked to stop ITSELF first; the signals below are
// the fallback for a guest that cannot.
func (b *ImageBooter) stopVM(ctx context.Context, in usecase.ImageDecommissionInput) {
	if in.PID <= 0 {
		return
	}
	find := b.findProcess
	if find == nil {
		find = osFindProcess
	}
	proc, err := find(in.PID)
	if err != nil {
		// No handle means nothing to signal AND no way to observe the power-off,
		// so there is no graceful attempt to make either.
		logger.FromContext(ctx).Warn("image-boot: no handle for vmm process", "pid", in.PID, "err", err)
		return
	}
	if b.shutdownGuest(ctx, in.SocketPath, proc) {
		return
	}
	b.signalStop(proc)
}

// shutdownGuest asks the guest to stop itself over the post-boot control channel
// and waits for the VMM to exit on its own. It reports whether that fully
// succeeded.
//
// Every failure mode — a VM booted before the control channel existed, a guest
// too old to know the verb, an unreachable or wedged guest, a timeout, a
// cancelled caller — returns false so the caller falls back to signalling, and
// is logged at WARN: a fleet that has silently stopped shutting down cleanly has
// to be visible rather than invisible.
func (b *ImageBooter) shutdownGuest(ctx context.Context, socketPath string, proc vmProcess) bool {
	if b.guestControl == nil || socketPath == "" {
		logger.FromContext(ctx).Warn("image-boot: resident stop has no guest control channel, signalling the vmm instead",
			"socket_path", socketPath)
		return false
	}

	// The caller's context is threaded, not replaced: a cancelled caller cuts the
	// graceful attempt short and drops straight to the bounded fallback below,
	// which is what keeps teardown finishing rather than being abandoned.
	shutdownCtx, cancel := context.WithTimeout(ctx, b.stop.guestShutdown)
	defer cancel()
	if err := b.guestControl.Shutdown(shutdownCtx, socketPath); err != nil {
		logger.FromContext(ctx).Warn("image-boot: guest shutdown failed, falling back to signalling the vmm",
			"socket_path", socketPath, "err", err)
		return false
	}

	// The guest powers itself off once it has acked, so the VMM exits on its own.
	if err := waitForProcessExit(ctx, proc, b.stop.powerOff, b.stop.exitPoll); err != nil {
		logger.FromContext(ctx).Warn("image-boot: vmm still alive after guest shutdown, falling back to signalling",
			"socket_path", socketPath, "err", err)
		return false
	}
	return true
}

// signalStop is the pre-control-channel stop: SIGTERM the VMM, grace window,
// SIGKILL. It is the fallback, not the norm — it stops the guest without letting
// it flush anything.
//
// Deliberately NOT bounded by the caller's context: this is the last chance to
// stop the process, and a cancelled caller must never leave a live VM behind a
// destroyed TAP and a deleted rootfs.
func (b *ImageBooter) signalStop(proc vmProcess) {
	_ = proc.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	// No ctx by design (see above): the wait is bounded by the select's grace
	// window, and the goroutine only reaps a process we are already killing.
	go func() {
		defer func() { _ = recover() }()
		_, _ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(b.stop.sigtermGrace):
		_ = proc.Kill()
	}
}

// waitForProcessExit polls until the process is gone, bounded by both timeout
// and the caller's context.
//
// It polls signal 0 instead of calling Wait(): the boot path already started a
// reaper goroutine on the VMM's exec.Cmd, and after a service restart the VMM is
// not this process's child at all — in both cases a Wait() here returns an error
// immediately, which would read as "exited" for a process that is still running.
func waitForProcessExit(ctx context.Context, proc vmProcess, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// ESRCH (or Go's ErrProcessDone) is the only report of "gone" that does not
		// depend on being the parent.
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("vmm still running %s after guest shutdown", timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for vmm exit: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// setupNet allocates an index and creates the routed /30 TAP. It returns the
// derived addressing plus a cleanup func that reverses exactly what it did. rt#8
// retired the per-VM DNAT, so ingress is served by Caddy, not iptables rules.
func (b *ImageBooter) setupNet(ctx context.Context) (imgNet, func(), error) {
	idx, err := b.allocIndex()
	if err != nil {
		return imgNet{}, func() {}, err
	}
	nw := deriveNet(idx)
	uid := vmUID(b.p.cfg.VMUIDBase, idx)
	if err := b.createTap(nw, uid, uid); err != nil {
		b.freeIndex(idx)
		return imgNet{}, func() {}, err
	}
	cleanup := func() {
		b.destroyTap(ctx, nw.tapName)
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
