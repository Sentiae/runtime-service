package firecracker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// warm.go productizes the EXPERIMENTALLY-PROVEN warm-VM + snapshot-clone recipe
// (verified live on the KVM host: warm VM ~8ms/run; snapshot restore ~131ms; CoW
// clone ~42MB/clone, netns-isolated). It is a faithful Go translation of the
// host shell steps — shelling out to ip/iptables/sysctl/firecracker via os/exec
// and driving the Firecracker HTTP API over its unix socket. These are NEW
// capabilities; they are NOT wired into the single-shot execution path.

// Warm-VM network constants. A warm template VM uses a standalone /30 TAP (not
// the fcbr0 bridge) so the host reaches the resident agent point-to-point.
const (
	warmHostIP   = "172.30.0.1"
	warmGuestIP  = "172.30.0.2"
	warmNetmask  = "255.255.255.252"
	warmTapInNS  = "tap-warm0"
	agentPort    = 8000
	warmReadyTTL = 10 * time.Second
)

// WarmVM is a resident template VM running the guest-agent. The host POSTs code
// to it (no per-execution boot) and snapshots it to seed the CoW-clone template.
type WarmVM struct {
	ID         uuid.UUID
	SocketPath string
	PID        int
	HostIP     string
	GuestIP    string
	TapName    string
	Language   domain.Language
}

// Endpoint returns the "host:port" the guest-agent listens on.
func (w *WarmVM) Endpoint() string {
	return fmt.Sprintf("%s:%d", w.GuestIP, agentPort)
}

// TemplateSnapshot is a Full Firecracker snapshot of a paused warm VM. Cloning
// loads it CoW (mem_backend File = mmap) into a fresh, netns-isolated FC process.
type TemplateSnapshot struct {
	StatePath string
	MemPath   string
	Language  domain.Language
}

// Clone is one CoW restore of a TemplateSnapshot, isolated in its own network
// namespace and reachable from the host at 10.200.N.2:8000 (DNAT'd to the in-ns
// guest). N is unique per concurrent clone and allocated by the caller/pool.
type Clone struct {
	ID         int
	Namespace  string
	SocketPath string
	PID        int
	Endpoint   string
	VethHost   string
}

// WarmManager owns the warm-VM / snapshot / clone lifecycle. It reuses the
// Provider for FC config (BinaryPath, KernelPath, RootfsBasePath, SocketDir),
// the FC-API helpers (unixHTTPClient/apiPut), and waitForSocket. The AgentClient
// polls the in-guest agent for readiness.
type WarmManager struct {
	p     *Provider
	agent *AgentClient
}

// NewWarmManager builds a WarmManager over an existing Provider.
func NewWarmManager(p *Provider) *WarmManager {
	return &WarmManager{p: p, agent: NewAgentClient(10 * time.Second)}
}

// --- A. Warm boot (the resident template VM) ---

// BootWarm boots a Firecracker VM from the warm rootfs (<lang>-warm.ext4) with
// the guest-agent as init, configured via the kernel `ip=` boot arg, then polls
// the agent until it is serving. The returned WarmVM is the snapshot template.
func (m *WarmManager) BootWarm(ctx context.Context, language domain.Language) (*WarmVM, error) {
	vmID := uuid.New()
	socketPath := m.p.socketPath(vmID)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// The template's TAP MUST share the clone's in-netns tap name: a Full
	// snapshot bakes the net device's host_dev_name into the state, so a clone
	// can only re-attach the restored eth0 if its netns TAP has the identical
	// name. (A per-VM tap name here would restore with a dangling NIC and the
	// agent would be unreachable.) One template boots at a time on the host's
	// default netns, so the fixed name doesn't collide.
	tapName := warmTapInNS

	// Standalone /30 TAP: ip tuntap add / ip addr add <host>/30 / ip link set up.
	_ = exec.Command("ip", "link", "del", tapName).Run() // clear a stale tap from a prior template
	if err := runWarmTapSetup(tapName, warmHostIP); err != nil {
		return nil, err
	}

	rootfsPath := m.warmRootfsForLanguage(language)
	if _, err := os.Stat(rootfsPath); err != nil {
		_ = exec.Command("ip", "link", "del", tapName).Run()
		return nil, fmt.Errorf("warm rootfs not found: %s: %w", rootfsPath, err)
	}

	// Start firecracker: exec.CommandContext(BinaryPath, "--api-sock", socketPath).
	cmd := exec.CommandContext(ctx, m.p.cfg.BinaryPath, "--api-sock", socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = exec.Command("ip", "link", "del", tapName).Run()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	pid := cmd.Process.Pid

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
		_ = exec.Command("ip", "link", "del", tapName).Run()
	}

	if err := m.p.waitForSocket(ctx, socketPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	// Configure machine / boot-source (warm boot args) / rootfs / net via the API.
	if err := m.configureWarmVM(ctx, socketPath, rootfsPath, tapName); err != nil {
		cleanup()
		return nil, fmt.Errorf("configure warm VM: %w", err)
	}
	if err := m.p.startInstance(ctx, socketPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("start warm VM instance: %w", err)
	}

	endpoint := fmt.Sprintf("%s:%d", warmGuestIP, agentPort)
	if err := m.agent.WaitReady(ctx, endpoint, warmReadyTTL); err != nil {
		cleanup()
		return nil, fmt.Errorf("warm agent never became ready: %w", err)
	}

	log.Printf("Warm VM booted: lang=%s pid=%d socket=%s tap=%s guest=%s endpoint=%s",
		language, pid, socketPath, tapName, warmGuestIP, endpoint)

	return &WarmVM{
		ID:         vmID,
		SocketPath: socketPath,
		PID:        pid,
		HostIP:     warmHostIP,
		GuestIP:    warmGuestIP,
		TapName:    tapName,
		Language:   language,
	}, nil
}

// configureWarmVM drives the FC API to set machine config, the warm boot source
// (init=/sbin/warm-init + kernel ip= arg), the warm rootfs drive (rw, root), and
// the eth0 interface bound to the standalone TAP.
func (m *WarmManager) configureWarmVM(ctx context.Context, socketPath, rootfsPath, tapName string) error {
	client := m.p.unixHTTPClient(socketPath)

	if err := m.p.apiPut(ctx, client, "/machine-config", warmMachineConfigBody()); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}
	if err := m.p.apiPut(ctx, client, "/boot-source", warmBootSourceBody(m.p.cfg.KernelPath)); err != nil {
		return fmt.Errorf("configure boot source: %w", err)
	}
	if err := m.p.apiPut(ctx, client, "/drives/rootfs", warmRootfsDriveBody(rootfsPath)); err != nil {
		return fmt.Errorf("configure rootfs drive: %w", err)
	}
	if err := m.p.apiPut(ctx, client, "/network-interfaces/eth0", warmNetIfaceBody(tapName)); err != nil {
		return fmt.Errorf("configure network: %w", err)
	}
	// virtio-rng (entropy) device: feeds fresh host entropy into the guest so
	// every CoW clone reseeds its RNG uniquely + deterministically (the snapshot
	// bakes the device in, so clones inherit it with no clone-side change).
	// Best-effort: on a kernel/FC build without virtio-rng the PUT 4xx's — log
	// and continue so BootWarm still succeeds (the device is hardening, not a
	// hard requirement).
	if err := m.p.apiPut(ctx, client, "/entropy", warmEntropyBody()); err != nil {
		log.Printf("Warning: virtio-rng entropy device not configured (continuing without it): %v", err)
	}
	return nil
}

// DestroyWarm tears down a warm template VM: kill its FC process (by PID),
// delete its standalone TAP, and remove its API socket. A warm VM is only
// booted to be snapshotted; the snapshot files outlive it, so after a snapshot
// the template process can be released. Each step is idempotent and logs-but-
// continues so a partial teardown still frees as much as possible.
func (m *WarmManager) DestroyWarm(warm *WarmVM) error {
	if warm == nil {
		return nil
	}
	if warm.PID > 0 {
		if proc, err := os.FindProcess(warm.PID); err == nil {
			if err := proc.Kill(); err != nil {
				log.Printf("Warning: kill warm VM %s pid %d: %v", warm.ID, warm.PID, err)
			}
		}
	}
	if warm.TapName != "" {
		if out, err := exec.Command("ip", "link", "del", warm.TapName).CombinedOutput(); err != nil {
			log.Printf("Warning: delete warm tap %s: %s: %v", warm.TapName, string(out), err)
		}
	}
	if warm.SocketPath != "" {
		_ = os.Remove(warm.SocketPath)
	}
	log.Printf("Warm VM %s destroyed (tap=%s)", warm.ID, warm.TapName)
	return nil
}

// --- B. Template snapshot (from a warm VM) ---

// CreateTemplateSnapshot pauses the warm VM and writes a Full snapshot
// (state + mem) under the Provider's SnapshotPath. The warm VM is left paused;
// the caller may Resume, snapshot again, or kill it.
func (m *WarmManager) CreateTemplateSnapshot(ctx context.Context, warm *WarmVM) (*TemplateSnapshot, error) {
	snapshotDir := m.p.cfg.SnapshotPath
	if err := os.MkdirAll(snapshotDir, 0750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	statePath := filepath.Join(snapshotDir, warm.ID.String()+".state")
	memPath := filepath.Join(snapshotDir, warm.ID.String()+".mem")

	client := m.p.unixHTTPClient(warm.SocketPath)

	// PATCH /vm {"state":"Paused"}.
	if err := m.p.apiPatch(ctx, client, "/vm", vmPausedBody()); err != nil {
		return nil, fmt.Errorf("pause warm VM: %w", err)
	}
	// PUT /snapshot/create {Full, state, mem}.
	if err := m.p.apiPut(ctx, client, "/snapshot/create", snapshotCreateBody(statePath, memPath)); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	log.Printf("Template snapshot created: lang=%s state=%s mem=%s", warm.Language, statePath, memPath)
	return &TemplateSnapshot{StatePath: statePath, MemPath: memPath, Language: warm.Language}, nil
}

// --- C. Clone from snapshot (isolated, CoW, host-reachable) ---

// CloneFromSnapshot restores the template snapshot into a fresh Firecracker
// process inside its own network namespace, CoW (mem_backend File = mmap), with
// host-side DNAT so the clone's agent is reachable at 10.200.N.2:8000. N must be
// unique per concurrent clone (1..254) and is allocated by the caller/pool.
func (m *WarmManager) CloneFromSnapshot(ctx context.Context, snap *TemplateSnapshot, n int) (*Clone, error) {
	if n < 1 || n > 254 {
		return nil, fmt.Errorf("clone index out of range (want 1..254): %d", n)
	}
	d := cloneNaming(n)
	cloneSock := filepath.Join(m.p.cfg.SocketDir, d.namespace+".sock")
	if err := os.MkdirAll(filepath.Dir(cloneSock), 0750); err != nil {
		return nil, fmt.Errorf("create clone socket dir: %w", err)
	}

	// Namespace + veth + in-ns tap + in-ns NAT, in the proven order.
	if err := runCloneNetworkSetup(d); err != nil {
		_ = teardownCloneNetwork(d)
		return nil, err
	}

	// Boot FC INSIDE the netns: ip netns exec <ns> firecracker --api-sock <sock>.
	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", d.namespace,
		m.p.cfg.BinaryPath, "--api-sock", cloneSock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = teardownCloneNetwork(d)
		return nil, fmt.Errorf("start firecracker in netns %s: %w", d.namespace, err)
	}
	pid := cmd.Process.Pid

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(cloneSock)
		_ = teardownCloneNetwork(d)
	}

	if err := m.p.waitForSocket(ctx, cloneSock); err != nil {
		cleanup()
		return nil, fmt.Errorf("clone firecracker socket not ready: %w", err)
	}

	// PUT /snapshot/load — mem_backend File (mmap = CoW; NOT mem_file_path), resume.
	client := m.p.unixHTTPClient(cloneSock)
	if err := m.p.apiPut(ctx, client, "/snapshot/load", snapshotLoadBody(snap.StatePath, snap.MemPath)); err != nil {
		cleanup()
		return nil, fmt.Errorf("load snapshot into clone: %w", err)
	}

	endpoint := fmt.Sprintf("%s:%d", d.hostReachIP, agentPort)
	if err := m.agent.WaitReady(ctx, endpoint, warmReadyTTL); err != nil {
		cleanup()
		return nil, fmt.Errorf("clone agent never became ready: %w", err)
	}

	log.Printf("Clone %d restored: ns=%s pid=%d socket=%s endpoint=%s veth=%s",
		n, d.namespace, pid, cloneSock, endpoint, d.vethHost)

	return &Clone{
		ID:         n,
		Namespace:  d.namespace,
		SocketPath: cloneSock,
		PID:        pid,
		Endpoint:   endpoint,
		VethHost:   d.vethHost,
	}, nil
}

// DestroyClone tears down a clone: kill the FC pid, delete its netns, delete its
// host veth. Each step is idempotent and logs-but-continues on failure so a
// partial teardown still releases as much as possible.
func (m *WarmManager) DestroyClone(clone *Clone) error {
	if clone == nil {
		return nil
	}
	if clone.PID > 0 {
		if proc, err := os.FindProcess(clone.PID); err == nil {
			if err := proc.Kill(); err != nil {
				log.Printf("Warning: kill clone %d pid %d: %v", clone.ID, clone.PID, err)
			}
		}
	}
	if clone.SocketPath != "" {
		_ = os.Remove(clone.SocketPath)
	}
	if clone.Namespace != "" {
		if out, err := exec.Command("ip", "netns", "del", clone.Namespace).CombinedOutput(); err != nil {
			log.Printf("Warning: delete netns %s: %s: %v", clone.Namespace, string(out), err)
		}
	}
	if clone.VethHost != "" {
		if out, err := exec.Command("ip", "link", "del", clone.VethHost).CombinedOutput(); err != nil {
			log.Printf("Warning: delete veth %s: %s: %v", clone.VethHost, string(out), err)
		}
	}
	log.Printf("Clone %d destroyed (ns=%s veth=%s)", clone.ID, clone.Namespace, clone.VethHost)
	return nil
}

// --- pure helpers (unit-tested) ---

// warmRootfsForLanguage returns the warm rootfs path: <RootfsBasePath>/<lang>-warm.ext4.
func (m *WarmManager) warmRootfsForLanguage(lang domain.Language) string {
	return filepath.Join(m.p.cfg.RootfsBasePath, string(lang)+"-warm.ext4")
}

// warmBootArgs builds the kernel command line for a warm VM. init=/sbin/warm-init
// runs the agent; the ip= arg statically configures eth0 from the kernel so no
// in-guest DHCP is needed. Shape: ip=<guest>::<host>:<mask>::eth0:off.
func warmBootArgs(guestIP, hostIP, netmask string) string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off init=/sbin/warm-init ip=%s::%s:%s::eth0:off",
		guestIP, hostIP, netmask,
	)
}

// warmMachineConfigBody is the /machine-config body: 1 vcpu, 256 MiB.
func warmMachineConfigBody() map[string]any {
	return map[string]any{
		"vcpu_count":   1,
		"mem_size_mib": 256,
	}
}

// warmBootSourceBody is the /boot-source body for a warm VM.
func warmBootSourceBody(kernelPath string) map[string]any {
	return map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         warmBootArgs(warmGuestIP, warmHostIP, warmNetmask),
	}
}

// warmRootfsDriveBody is the /drives/rootfs body: rw root device on the warm image.
func warmRootfsDriveBody(rootfsPath string) map[string]any {
	return map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
}

// warmNetIfaceBody is the /network-interfaces/eth0 body binding eth0 to the host TAP.
func warmNetIfaceBody(tapName string) map[string]any {
	return map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": tapName,
	}
}

// warmEntropyBody is the PUT /entropy body for the virtio-rng device. The
// device has no required fields — its only config is an optional rate_limiter
// — so the minimal valid body is empty (no rate limit; the guest pulls host
// entropy on demand).
func warmEntropyBody() map[string]any {
	return map[string]any{}
}

// vmPausedBody is the PATCH /vm body that pauses a running VM before snapshot.
func vmPausedBody() map[string]any {
	return map[string]any{"state": "Paused"}
}

// snapshotCreateBody is the PUT /snapshot/create body for a Full snapshot.
func snapshotCreateBody(statePath, memPath string) map[string]any {
	return map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	}
}

// snapshotLoadBody is the PUT /snapshot/load body. mem_backend with backend_type
// "File" mmaps the mem file copy-on-write (NOT mem_file_path, which would be a
// private dirty copy); resume_vm true brings the clone straight back to running.
func snapshotLoadBody(statePath, memPath string) map[string]any {
	return map[string]any{
		"snapshot_path": statePath,
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"resume_vm": true,
	}
}

// cloneDerived holds the deterministic names + IPs derived from a clone index N.
type cloneDerived struct {
	n           int
	namespace   string // fc-clone<N>
	vethHost    string // vh<N>
	vethGuest   string // vg<N>
	hostVethIP  string // 10.200.N.1
	nsVethIP    string // 10.200.N.2  (the host-reachable address; DNAT target)
	hostReachIP string // 10.200.N.2  (alias for clarity at call sites)
}

// cloneNaming derives the netns, veth pair, and IPs for clone N. The 10.200.N.x
// /24 makes each clone's host-side address unique and routable from the host;
// the in-ns guest is DNAT'd from 10.200.N.2:8000 to the in-ns tap 172.30.0.2:8000.
func cloneNaming(n int) cloneDerived {
	return cloneDerived{
		n:           n,
		namespace:   fmt.Sprintf("fc-clone%d", n),
		vethHost:    fmt.Sprintf("vh%d", n),
		vethGuest:   fmt.Sprintf("vg%d", n),
		hostVethIP:  fmt.Sprintf("10.200.%d.1", n),
		nsVethIP:    fmt.Sprintf("10.200.%d.2", n),
		hostReachIP: fmt.Sprintf("10.200.%d.2", n),
	}
}

// --- exec orchestration (host-only; not unit-tested) ---

// runCmd runs a host command, capturing combined output and wrapping failures.
func runCmd(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s: %w", args, string(out), err)
	}
	return nil
}

// runWarmTapSetup creates the standalone /30 TAP for a warm VM.
func runWarmTapSetup(tapName, hostIP string) error {
	cmds := [][]string{
		{"ip", "tuntap", "add", tapName, "mode", "tap"},
		{"ip", "addr", "add", hostIP + "/30", "dev", tapName},
		{"ip", "link", "set", tapName, "up"},
	}
	for _, args := range cmds {
		if err := runCmd(args...); err != nil {
			_ = exec.Command("ip", "link", "del", tapName).Run()
			return fmt.Errorf("warm tap setup: %w", err)
		}
	}
	return nil
}

// runCloneNetworkSetup builds the clone's isolated network in the proven order:
// netns, veth pair (guest end moved into ns), addresses, in-ns tap, in-ns NAT.
func runCloneNetworkSetup(d cloneDerived) error {
	ns := d.namespace
	cmds := [][]string{
		// Namespace + loopback.
		{"ip", "netns", "add", ns},
		{"ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"},
		// veth pair; move guest end into the namespace.
		{"ip", "link", "add", d.vethHost, "type", "veth", "peer", "name", d.vethGuest},
		{"ip", "link", "set", d.vethGuest, "netns", ns},
		// Host side address + up.
		{"ip", "addr", "add", d.hostVethIP + "/24", "dev", d.vethHost},
		{"ip", "link", "set", d.vethHost, "up"},
		// In-ns side address + up.
		{"ip", "netns", "exec", ns, "ip", "addr", "add", d.nsVethIP + "/24", "dev", d.vethGuest},
		{"ip", "netns", "exec", ns, "ip", "link", "set", d.vethGuest, "up"},
		// In-ns tap for the VM's eth0.
		{"ip", "netns", "exec", ns, "ip", "tuntap", "add", warmTapInNS, "mode", "tap"},
		{"ip", "netns", "exec", ns, "ip", "addr", "add", warmHostIP + "/30", "dev", warmTapInNS},
		{"ip", "netns", "exec", ns, "ip", "link", "set", warmTapInNS, "up"},
		// In-ns NAT: forward + DNAT 10.200.N.2:8000 -> 172.30.0.2:8000 + MASQUERADE.
		{"ip", "netns", "exec", ns, "sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", "PREROUTING",
			"-d", d.nsVethIP, "-p", "tcp", "--dport", fmt.Sprintf("%d", agentPort),
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", warmGuestIP, agentPort)},
		{"ip", "netns", "exec", ns, "iptables", "-t", "nat", "-A", "POSTROUTING",
			"-o", warmTapInNS, "-j", "MASQUERADE"},
	}
	for _, args := range cmds {
		if err := runCmd(args...); err != nil {
			return fmt.Errorf("clone network setup: %w", err)
		}
	}
	return nil
}

// teardownCloneNetwork removes the netns and host veth. Idempotent best-effort
// (deleting the netns also removes the in-ns veth/tap/iptables automatically).
func teardownCloneNetwork(d cloneDerived) error {
	_ = exec.Command("ip", "netns", "del", d.namespace).Run()
	_ = exec.Command("ip", "link", "del", d.vethHost).Run()
	return nil
}
