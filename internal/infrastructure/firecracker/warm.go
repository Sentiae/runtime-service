package firecracker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Jail identities for the warm path. They are deliberately NON-numeric: the
// numeric jail-id space belongs to the image-boot network index and the
// ephemeral uid slot, and no two live VMs may share a chroot.
const (
	warmJailID = "warm"
	// warmRootfsRel is where the warm rootfs sits inside EVERY warm chroot. It
	// must be identical for the template and its clones: a Full snapshot records
	// the block device's path, and /snapshot/load re-opens it at exactly that
	// path — inside the restoring VMM's chroot.
	warmRootfsRel = "rootfs.ext4"
)

// cloneJailID names one clone's chroot. n is already unique across live clones
// (the warm pool's index allocator).
func cloneJailID(n int) string { return fmt.Sprintf("clone%d", n) }

// linkWarmRootfs hard-links the shared warm rootfs into a warm chroot (the
// template's or a clone's) and returns its chroot view.
//
// It NEVER chowns. One <lang>-warm.ext4 inode is linked into the template's
// chroot and into every concurrent clone's chroot, and the drive is served
// READ-ONLY (warmRootfsDriveBody), so no VM needs to own it — exactly like the
// template .state/.mem pair. A chown here would be a per-clone write to an inode
// every other live clone is using. Both call sites go through this one function
// so the invariant cannot drift between them.
//
// ⚠ Host precondition: <lang>-warm.ext4 must be root-owned mode 0644, or the
// jailed VMM (running as the unprivileged warm uid) cannot open its root device.
func linkWarmRootfs(j *vmJail, rootfsPath string) (string, error) {
	chrootPath, err := j.link(rootfsPath, warmRootfsRel, false)
	if err != nil {
		return "", fmt.Errorf("link warm rootfs: %w", err)
	}
	return chrootPath, nil
}

// warmUID is the uid AND gid the warm template and every warm clone run as —
// ONE shared identity for the whole warm path (D-185d, owner-ruled), drawn from
// the slot reserved at the bottom of the ephemeral range.
//
// Why shared rather than per-VM: the clones' rootfs is a single shared ext4
// file (the CoW in this path is on memory only — mem_backend File is an mmap;
// the drive is not copied). It is served READ-ONLY to every VM (see
// warmRootfsDriveBody), which is what makes one shared inode safe. A per-clone
// uid would have to chown that one shared inode per clone, which is a fight no
// clone wins. Handing clones distinct uids is a separate change — it needs its
// own non-colliding uid sub-range — and is deliberately NOT done here.
// What the shared uid DOES buy is the point of the ruling: an escaping warm VMM
// lands as an unprivileged user, not as host root next to every tenant's data
// volume.
func (m *WarmManager) warmUID() int {
	return vmUID(m.p.cfg.VMUIDBase, ephUIDOffset)
}

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

	// jail is the template's chroot. Teardown removes it, and the snapshot is
	// written inside it (the jailed VMM resolves the snapshot paths after its
	// chroot), so it has to travel with the VM.
	jail *vmJail
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

	// jail is the clone's chroot, removed on teardown. A leaked one also pins the
	// hard-linked snapshot inodes.
	jail *vmJail
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
	uid := m.warmUID()

	rootfsPath := m.warmRootfsForLanguage(language)
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("warm rootfs not found: %s: %w", rootfsPath, err)
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
	if err := runWarmTapSetup(tapName, warmHostIP, uid, uid); err != nil {
		return nil, err
	}
	dropTap := func() { _ = exec.Command("ip", "link", "del", tapName).Run() }

	j := newVMJail(m.p.cfg.ChrootBase, warmJailID, uid)
	if err := j.prepare(); err != nil {
		dropTap()
		return nil, fmt.Errorf("prepare warm jail: %w", err)
	}
	for _, dir := range []string{"run", "kernel"} {
		if err := j.mkdir(dir); err != nil {
			j.remove()
			dropTap()
			return nil, fmt.Errorf("prepare warm jail: %w", err)
		}
	}
	socketRel := "run/" + vmID.String() + ".sock"
	socketPath := j.hostPath(socketRel)
	if err := checkSocketPathFits(socketPath); err != nil {
		j.remove()
		dropTap()
		return nil, err
	}
	kernelChroot, err := j.link(m.p.cfg.KernelPath, "kernel/vmlinux", false)
	if err != nil {
		j.remove()
		dropTap()
		return nil, fmt.Errorf("place kernel in warm jail: %w", err)
	}
	// Root-owned, NOT chowned — see linkWarmRootfs.
	rootfsChroot, err := linkWarmRootfs(j, rootfsPath)
	if err != nil {
		j.remove()
		dropTap()
		return nil, fmt.Errorf("place warm rootfs in jail: %w", err)
	}

	// Always through the jailer: an unjailed VMM escape lands as host root, and
	// this path runs customer code beside every tenant's data volume.
	cmd := m.p.jailerCmd(ctx, j, j.chrootPath(socketRel), "")
	if err := cmd.Start(); err != nil {
		j.remove()
		dropTap()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	pid := cmd.Process.Pid

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
		j.remove()
		dropTap()
	}

	if err := m.p.waitForSocket(ctx, socketPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	// Configure machine / boot-source (warm boot args) / rootfs / net via the API.
	// Every path handed to the API is the chroot view — the VMM opens it after
	// the jailer has chroot'ed.
	if err := m.configureWarmVM(ctx, socketPath, kernelChroot, rootfsChroot, tapName); err != nil {
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
		jail:       j,
	}, nil
}

// configureWarmVM drives the FC API to set machine config, the warm boot source
// (init=/sbin/warm-init + kernel ip= arg), the warm rootfs drive (read-only, root), and
// the eth0 interface bound to the standalone TAP. kernelPath and rootfsPath are
// chroot views.
func (m *WarmManager) configureWarmVM(ctx context.Context, socketPath, kernelPath, rootfsPath, tapName string) error {
	client := m.p.unixHTTPClient(socketPath)

	if err := m.p.apiPut(ctx, client, "/machine-config", warmMachineConfigBody()); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}
	if err := m.p.apiPut(ctx, client, "/boot-source", warmBootSourceBody(kernelPath)); err != nil {
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
	// The chroot outlives the process unless it is removed here; a leaked one
	// also pins the hard-linked warm rootfs inode.
	if warm.jail != nil {
		warm.jail.remove()
	}
	log.Printf("Warm VM %s destroyed (tap=%s)", warm.ID, warm.TapName)
	return nil
}

// --- B. Template snapshot (from a warm VM) ---

// CreateTemplateSnapshot pauses the warm VM and writes a Full snapshot
// (state + mem) under the Provider's SnapshotPath. The warm VM is left paused;
// the caller may Resume, snapshot again, or kill it.
func (m *WarmManager) CreateTemplateSnapshot(ctx context.Context, warm *WarmVM) (*TemplateSnapshot, error) {
	if warm == nil || warm.jail == nil {
		return nil, fmt.Errorf("create template snapshot: warm VM has no jail")
	}
	snapshotDir := m.p.cfg.SnapshotPath
	if err := os.MkdirAll(snapshotDir, 0750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	statePath := filepath.Join(snapshotDir, warm.ID.String()+".state")
	memPath := filepath.Join(snapshotDir, warm.ID.String()+".mem")

	// The VMM writes the pair itself, resolving the paths AFTER its chroot and
	// with no rights outside it — so it is asked for jail-local paths it owns and
	// the results are moved out to SnapshotPath afterwards, where the template
	// cache and the durable object-store persistence expect host paths.
	j := warm.jail
	if err := j.mkdir("snap"); err != nil {
		return nil, fmt.Errorf("prepare jail snapshot dir: %w", err)
	}
	stateRel := "snap/" + warm.ID.String() + ".state"
	memRel := "snap/" + warm.ID.String() + ".mem"

	client := m.p.unixHTTPClient(warm.SocketPath)

	// PATCH /vm {"state":"Paused"}.
	if err := m.p.apiPatch(ctx, client, "/vm", vmPausedBody()); err != nil {
		return nil, fmt.Errorf("pause warm VM: %w", err)
	}
	// PUT /snapshot/create {Full, state, mem}.
	if err := m.p.apiPut(ctx, client, "/snapshot/create", snapshotCreateBody(j.chrootPath(stateRel), j.chrootPath(memRel))); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	if err := adoptJailedFile(j.hostPath(stateRel), statePath); err != nil {
		return nil, fmt.Errorf("collect template state file: %w", err)
	}
	if err := adoptJailedFile(j.hostPath(memRel), memPath); err != nil {
		return nil, fmt.Errorf("collect template memory file: %w", err)
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
	uid := m.warmUID()

	// Namespace + veth + in-ns tap + in-ns NAT, in the proven order.
	if err := runCloneNetworkSetup(d, uid, uid); err != nil {
		_ = teardownCloneNetwork(d)
		return nil, err
	}

	j := newVMJail(m.p.cfg.ChrootBase, cloneJailID(n), uid)
	failClone := func(err error) (*Clone, error) {
		j.remove()
		_ = teardownCloneNetwork(d)
		return nil, err
	}
	if err := j.prepare(); err != nil {
		return failClone(fmt.Errorf("prepare clone jail: %w", err))
	}
	for _, dir := range []string{"run", "snap"} {
		if err := j.mkdir(dir); err != nil {
			return failClone(fmt.Errorf("prepare clone jail: %w", err))
		}
	}
	socketRel := "run/" + d.namespace + ".sock"
	cloneSock := j.hostPath(socketRel)
	if err := checkSocketPathFits(cloneSock); err != nil {
		return failClone(err)
	}

	// The restored VM re-opens its block device at the path the snapshot recorded
	// — the template's chroot view — so the same shared inode has to be reachable
	// at that exact path inside THIS chroot.
	//
	// Root-owned and NOT chowned (see linkWarmRootfs), exactly like the template
	// .state/.mem pair below: the drive is READ-ONLY — the template snapshot bakes
	// is_read_only=true in, so every restored clone inherits it — and a read-only
	// device needs no per-VM ownership. This was the last per-clone write to the
	// one shared rootfs inode; without it, "N clones, one inode" is structurally
	// safe rather than accidentally safe.
	if _, err := linkWarmRootfs(j, m.warmRootfsForLanguage(snap.Language)); err != nil {
		return failClone(fmt.Errorf("place warm rootfs in clone jail: %w", err))
	}
	// The template snapshot pair is read-only to the VMM (mem_backend File is an
	// mmap) and shared by every clone, so it is linked in root-owned and NOT
	// chowned — a per-clone chown would rewrite the owner of the one canonical
	// template out from under every other live clone.
	stateChroot, err := j.link(snap.StatePath, "snap/state", false)
	if err != nil {
		return failClone(fmt.Errorf("place template state in clone jail: %w", err))
	}
	memChroot, err := j.link(snap.MemPath, "snap/mem", false)
	if err != nil {
		return failClone(fmt.Errorf("place template memory in clone jail: %w", err))
	}

	// Boot FC inside the clone's netns via the JAILER's own --netns, not
	// `ip netns exec` (which would leave a process between us and the pid).
	cmd := m.p.jailerCmd(ctx, j, j.chrootPath(socketRel), netnsPath(d.namespace))
	if err := cmd.Start(); err != nil {
		return failClone(fmt.Errorf("start firecracker in netns %s: %w", d.namespace, err))
	}
	pid := cmd.Process.Pid

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(cloneSock)
		j.remove()
		_ = teardownCloneNetwork(d)
	}

	if err := m.p.waitForSocket(ctx, cloneSock); err != nil {
		cleanup()
		return nil, fmt.Errorf("clone firecracker socket not ready: %w", err)
	}

	// PUT /snapshot/load — mem_backend File (mmap = CoW; NOT mem_file_path), resume.
	client := m.p.unixHTTPClient(cloneSock)
	if err := m.p.apiPut(ctx, client, "/snapshot/load", snapshotLoadBody(stateChroot, memChroot)); err != nil {
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
		jail:       j,
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
	// Removing the chroot is also what unlinks this clone's references to the
	// shared template snapshot and rootfs inodes.
	if clone.jail != nil {
		clone.jail.remove()
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
//
// `ro` is EXPLICIT and load-bearing: the rootfs is one file shared by the
// template and every concurrent clone, so / must never be writable. Omitting it
// happened to work only because the kernel's unstated default root_mountflags is
// MS_RDONLY — one token away from silent cross-tenant filesystem corruption.
// State it; do not inherit it. (The device is read-only too — see
// warmRootfsDriveBody — so this is the guest-side half of a belt-and-braces
// pair, not the only guard.)
func warmBootArgs(guestIP, hostIP, netmask string) string {
	return fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ro init=/sbin/warm-init ip=%s::%s:%s::eth0:off",
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

// warmRootfsDriveBody is the /drives/rootfs body: READ-ONLY root device on the
// warm image.
//
// ⚠ is_read_only MUST stay true — do not "optimize" it back to writable.
// The warm path hard-links ONE <lang>-warm.ext4 inode into the template's chroot
// and into every concurrent clone's chroot; the CoW here is on memory only
// (mem_backend File is an mmap), so the drive is genuinely shared, not copied.
// Read-only at the device is the ONLY thing that makes sharing one inode safe:
// Firecracker refuses guest writes at the virtio-blk layer, so one tenant's
// clone cannot corrupt the filesystem every other tenant's clone is reading.
// Because a Full snapshot bakes the device config in, setting it here also
// settles it for every restored clone. Guest writes belong on the tmpfs the
// guest-agent mounts at /tmp.
//
// ⚠ Operational trap: a read-only ext4 with a DIRTY journal cannot replay and
// the guest panics at root mount (the same failure image_boot.go dodges with
// `ro,noload`). The warm image must be cleanly unmounted / e2fsck-clean on the
// host before it is served this way.
func warmRootfsDriveBody(rootfsPath string) map[string]any {
	return map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   true,
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

// netnsPath is the filesystem handle `ip netns add <ns>` creates, and what the
// jailer's --netns flag expects.
func netnsPath(ns string) string { return "/var/run/netns/" + ns }

// runWarmTapSetup creates the standalone /30 TAP for a warm VM, owned by the
// warm uid/gid: a jailed VMM has no CAP_NET_ADMIN and can only TUNSETIFF-attach
// to a tap it owns.
func runWarmTapSetup(tapName, hostIP string, uid, gid int) error {
	cmds := [][]string{
		{"ip", "tuntap", "add", tapName, "mode", "tap", "user", strconv.Itoa(uid), "group", strconv.Itoa(gid)},
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
func runCloneNetworkSetup(d cloneDerived, uid, gid int) error {
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
		// In-ns tap for the VM's eth0, owned by the warm uid: the jailed VMM
		// attaches to it with no CAP_NET_ADMIN.
		{"ip", "netns", "exec", ns, "ip", "tuntap", "add", warmTapInNS, "mode", "tap",
			"user", strconv.Itoa(uid), "group", strconv.Itoa(gid)},
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
