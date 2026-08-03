package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/netfabric"
	"github.com/sentiae/runtime-service/internal/port/gateway"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// imgSubnet16 is the /16 the image-boot path carves per-workload /30s out of.
//
// ⚠ The ADDRESSING ITSELF IS NOT COMPUTED HERE ANY MORE. Every coordinate — the
// /30, the TAP name, the jail id and the per-VM uid — comes from a durable lease
// acquired through usecase.NetLeaseAllocator and derived in exactly one place
// (domain.DeriveNetLease). What used to live here was a process-local map: it
// forgot everything on restart, was seeded from a query whose errors were logged
// and ignored, never counted `dead` replicas whose VMM was still running, and had
// no host term at all. Any one of those hands a second tenant a live VM's address,
// uid AND chroot.
const (
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

	// inspect reads the /proc + pidfile facts the termination proof is built
	// from. A field for the same reason as findProcess; production is
	// procInspector.
	inspect processInspector

	natOnce sync.Once
	natErr  error

	// alloc is the durable addressing plane. It is the ONLY way this adapter
	// obtains a /30, a TAP name, a jail id or a uid — there is deliberately no
	// local fallback allocator to degrade into.
	alloc usecase.NetLeaseAllocator
}

var (
	_ usecase.ImageBooter       = (*ImageBooter)(nil)
	_ usecase.NetLeaseReclaimer = (*ImageBooter)(nil)
)

// NewImageBooter constructs an ImageBooter over an existing Provider. rt#8
// retired per-VM host-port DNAT (the fleet owns ingress via Caddy), so no host
// port range is tracked — guests are reached directly on their routed /30.
//
// alloc is the durable addressing plane and is required for any BOOT; a nil
// allocator refuses every boot (see setupNet) rather than falling back to
// computing an address locally.
func NewImageBooter(p *Provider, advertiseHost string, controlTokens *GuestControlTokens, alloc usecase.NetLeaseAllocator) *ImageBooter {
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
		inspect:      procInspector{},
		alloc:        alloc,
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
	// sigkillGrace is the wait AFTER SIGKILL before the teardown gives up and
	// declares the termination unproven. It exists because SIGKILL is not
	// instantaneous — an uninterruptible-sleep VMM does not die until it leaves
	// D-state — and "we sent SIGKILL" was never evidence that anything exited.
	// That assumption is precisely what left seven live VMMs behind deleted jails.
	sigkillGrace time.Duration
}

func defaultStopTimings() stopTimings {
	return stopTimings{
		guestShutdown: controlShutdownTimeout,
		powerOff:      15 * time.Second,
		exitPoll:      100 * time.Millisecond,
		sigtermGrace:  5 * time.Second,
		sigkillGrace:  5 * time.Second,
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

// imgNet is one boot's addressing, read straight off its lease. Nothing here is
// recomputed: the lease's RECORDED values are what the VM is configured with, so
// that a future change to the index arithmetic cannot move a running VM.
type imgNet struct {
	index   int
	slot    int
	uid     int
	tapName string
	hostIP  string
	guestIP string
}

// imgNetFromLease projects a lease onto the adapter's boot-local view.
func imgNetFromLease(lease domain.NetLease) imgNet {
	return imgNet{
		index:   lease.NetIndex,
		slot:    lease.LocalSlot,
		uid:     lease.VMUID,
		tapName: lease.TapName,
		hostIP:  lease.HostIP,
		guestIP: lease.GuestIP,
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
// The MASQUERADE is qualified with `! -d 10.201.0.0/16`: it must translate
// EGRESS to the outside world only, never fleet-internal traffic. Unqualified it
// also matched VM→VM packets, and once those hosts stop sharing one machine (or
// once inter-VM traffic is routed rather than link-local) SNAT'ing them would
// rewrite the source address the network policy program matches on — i.e. it
// would silently defeat the cross-tenant DROP. It is a no-op on a single host
// today and a prerequisite for the day it is not, which is exactly when it must
// already be right.
//
// The old unqualified form is DELETED first (check-then-delete): leaving both in
// place would leave the broader rule reachable, and a host that has run an older
// build carries it.
func (b *ImageBooter) ensureNAT() error {
	b.natOnce.Do(func() {
		_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)

		nat := func(op string, spec ...string) *exec.Cmd {
			return exec.Command("iptables", append([]string{"-t", "nat", op, "POSTROUTING"}, spec...)...)
		}
		// The rule that stays, and the unqualified one that used to be here.
		qualified := []string{"-s", imgSubnet16, "!", "-d", imgSubnet16, "-j", "MASQUERADE"}
		unqualified := []string{"-s", imgSubnet16, "-j", "MASQUERADE"}

		// ADD before DELETE, deliberately: a host upgrading from the unqualified rule
		// has live guests behind it, and doing it the other way round would leave a
		// window with no MASQUERADE at all (their egress would black-hole). During the
		// overlap the broader rule is still present, which is exactly today's state, so
		// the ordering costs nothing in strictness.
		if nat("-C", qualified...).Run() != nil {
			if out, err := nat("-A", qualified...).CombinedOutput(); err != nil {
				b.natErr = fmt.Errorf("install image-boot MASQUERADE: %s: %w", string(out), err)
				return
			}
		}
		if nat("-C", unqualified...).Run() == nil {
			if out, err := nat("-D", unqualified...).CombinedOutput(); err != nil {
				b.natErr = fmt.Errorf("remove the unqualified image-boot MASQUERADE (both rules are now installed, so fleet-internal traffic is still being SNAT'd): %s: %w", string(out), err)
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
	// The uid and the jail id come from the LEASE (the host-local slot), and both
	// are re-checked here against the configured span. This is the last gate before
	// a VMM is executed under an identity: a uid outside the span, or a lease that
	// somehow carried no slot, means two VMs could share one (uid,gid) and one
	// chroot — the cross-tenant hole the jail exists to close — so it refuses the
	// boot instead of substituting a value.
	if nw.slot <= 0 || nw.slot > domain.NetMaxSlot {
		return nil, "", fmt.Errorf("image-boot lease has no usable host-local slot (%d): refusing to boot without a fenced jail id", nw.slot)
	}
	uid := nw.uid
	if uid < b.p.cfg.VMUIDBase || uid >= b.p.cfg.VMUIDBase+b.p.cfg.VMUIDSpan {
		return nil, "", fmt.Errorf("image-boot leased vm uid %d outside the per-VM uid span [%d,%d)",
			uid, b.p.cfg.VMUIDBase, b.p.cfg.VMUIDBase+b.p.cfg.VMUIDSpan)
	}

	// The jail id is the host-local slot, not the VM uuid: the uuid already owns the
	// socket basename, and a second one would push the host socket path past the
	// AF_UNIX 107-byte limit. The slot is fenced unique per host by
	// fleet_net_leases_host_slot_key, and prepare() clears a stale dir left by
	// whoever held it before.
	jailID := strconv.Itoa(nw.slot)
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
	if err := b.p.apiPut(ctx, client, "/drives/rootfs",
		driveConfigBody("rootfs", rootfsChrootPath, true, false)); err != nil {
		b.killVM(cmd, socketPath)
		j.remove()
		return nil, "", fmt.Errorf("configure rootfs: %w", err)
	}
	// rt#9 — attach the persistent data volume as a 2nd virtio-blk device. The
	// guest sees it as /dev/vdb and mounts it at the runtime.json data_mount_path.
	if dataDiskPath != "" {
		// SentiaeDB Phase-0 P0 (D-184, #p19-firecracker-cache-type-fsync): the
		// persistent data volume MUST honor the guest's fsync down to the host.
		// driveConfigBody settles that for every drive, including this one.
		if err := b.p.apiPut(ctx, client, "/drives/data",
			driveConfigBody("data", dataChrootPath, false, false)); err != nil {
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
	nw, cleanupNet, err := b.setupNet(ctx, in.OwnerKind, in.WorkloadID)
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
	nw, cleanupNet, err := b.setupNet(ctx, in.OwnerKind, in.WorkloadID)
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

// Decommission tears down a resident workload in ONE mandatory order:
//
//  1. PROVE the VMM is gone (or make it gone and prove that);
//  2. destroy the TAP;
//  3. release this host's lease, and require that to succeed;
//  4. delete the control token, sockets, jail and rootfs;
//  5. return nil.
//
// ⚠ THE ORDER IS THE FIX, not a preference. The previous version was
// "teardown is never blockable": it stopped the VM best-effort and then removed
// the TAP, the lease, the jail and the rootfs UNCONDITIONALLY — including when
// the guest refused to die. The reasoning was that refusing to tear down strands
// a customer resource. What it actually produced is worse than a strand: a
// RUNNING microVM whose jail, executable link and addressing record have all
// been deleted out from under it, invisible to every ledger and impossible to
// clean up correctly. Seven such processes are on the live fleet host right now,
// with /proc/<pid>/root and /proc/<pid>/exe marked deleted — direct evidence of
// this exact ordering defect.
//
// Releasing a lease, deleting a TAP or clearing a jail slot while its VM still
// runs is not merely untidy: those are the resources the NEXT boot allocates
// from, so it hands a second tenant a live VM's address, uid and chroot. The
// fail-closed direction is therefore to retain everything and report
// ErrVMTerminationUnproven, which is retryable and which the replica row and
// volume attachment are deliberately preserved for.
func (b *ImageBooter) Decommission(ctx context.Context, in usecase.ImageDecommissionInput) error {
	// 1. Nothing below this line may run without positive proof.
	if err := b.proveTerminated(ctx, in); err != nil {
		logger.FromContext(ctx).Error("image-boot: teardown REFUSED — the VMM could not be proven to have exited, so its tap, lease, socket, jail and rootfs are ALL retained (retry once it can be proven gone)",
			"owner_kind", in.OwnerKind, "owner_id", in.OwnerID, "pid", in.PID,
			"tap_name", in.TapName, "net_index", in.NetIndex, "err", err)
		return err
	}

	// 2. The VM is gone: its device may go.
	b.destroyTap(ctx, in.TapName)

	// 3. The lease is released by OWNER, not by index: the index is only an
	//    address, whereas the lease row is the allocation. It is now REQUIRED to
	//    succeed — a swallowed failure leaves the slot held while every artifact
	//    that identifies it is deleted below, which is unrecoverable except by
	//    hand. Returning here keeps the jail and rootfs in place so a retry can
	//    finish the job.
	if err := b.releaseLease(ctx, in.OwnerKind, in.OwnerID); err != nil {
		return err
	}

	// 4. Only now the host-local files.
	if in.SocketPath != "" {
		// The VM is gone; stop bearing its control token (D-185a). This MUST stay
		// after the stop: that token is what authenticates the guest SHUTDOWN, so
		// dropping it first would make every graceful stop fail and fall back to
		// killing the VMM — the exact defect this path fixes.
		b.controlTokens.Delete(in.SocketPath)
		// Best-effort removals of files whose VM is provably gone; a leftover socket
		// inode fences nothing and the next boot on this slot clears the jail dir.
		_ = os.Remove(in.SocketPath)
		_ = os.Remove(in.SocketPath + ".vsock")
		// "" for a pre-jail VM, whose socket lives outside any chroot.
		if dir := jailDirFromSocketPath(b.p.cfg.ChrootBase, in.SocketPath); dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
	if in.RootfsPath != "" {
		// Best-effort for the same reason: the inode is unreferenced once the jail's
		// hard link above is gone.
		_ = os.Remove(in.RootfsPath)
	}
	return nil
}

// proveTerminated returns nil only when this microVM's VMM process is PROVEN not
// to be running — either it was already absent, or this call stopped it and
// observed the absence.
//
// Absence is observed with signal 0 and nothing else. Wait() is never used and
// no goroutine is started to call it: the boot path may already own the wait on
// the exec.Cmd, and after a service restart the VMM is not this process's child
// at all — in both cases Wait() returns an error immediately, which reads as
// "exited" for a process that is still running. That misreading is how a live VM
// gets its jail deleted.
func (b *ImageBooter) proveTerminated(ctx context.Context, in usecase.ImageDecommissionInput) error {
	if in.PID <= 0 {
		// No usable pid. Whether that is fine depends entirely on whether anything
		// was ever booted for this owner.
		if in.SocketPath == "" && in.TapName == "" && in.NetIndex <= 0 {
			// A scheduled row that never booted: no process, no artifacts, nothing to
			// stop. Proceeding is correct — there is nothing a live VM could be holding.
			return nil
		}
		// Artifacts exist but no pid does. NEVER guess a process from a socket name
		// and never kill by name: the VM may be running under a pid this row simply
		// failed to record, and picking a victim by pattern is how an unrelated
		// process gets killed.
		return fmt.Errorf("%w: %s %s records boot artifacts (socket=%q tap=%q net_index=%d) but no pid, so nothing can prove its VMM is gone",
			domain.ErrVMTerminationUnproven, in.OwnerKind, in.OwnerID, in.SocketPath, in.TapName, in.NetIndex)
	}

	find := b.findProcess
	if find == nil {
		find = osFindProcess
	}
	proc, err := find(in.PID)
	if err != nil {
		return fmt.Errorf("%w: no process handle for pid %d: %v", domain.ErrVMTerminationUnproven, in.PID, err)
	}

	// Absence FIRST, and it needs no identity check: a pid that does not exist
	// cannot be anyone's live VM, so ESRCH is complete proof on its own.
	gone, serr := processGone(proc)
	if gone {
		return nil
	}
	if serr != nil {
		// A permission error (or anything else) is NOT absence. It means we cannot
		// see the process, not that there is none.
		return fmt.Errorf("%w: pid %d could not be probed with signal 0: %v", domain.ErrVMTerminationUnproven, in.PID, serr)
	}

	// Something is alive at that pid. Prove it is OUR VM before signalling it.
	if err := proveVMIdentity(b.inspect, b.p.cfg.ChrootBase, in); err != nil {
		return err
	}
	return b.stopAndProve(ctx, in, proc)
}

// stopAndProve executes the bounded stop ladder and returns nil only on observed
// absence.
//
//  1. ask the guest to stop itself (the clean path: the workload gets its SIGINT,
//     Postgres runs its fast shutdown, image-init syncs and powers off);
//  2. poll signal 0 for the power-off budget;
//  3. SIGTERM, poll signal 0 for sigtermGrace;
//  4. SIGKILL, poll signal 0 for sigkillGrace;
//  5. still there ⇒ unproven.
//
// Steps 3–4 deliberately ignore a cancelled caller context: this is the last
// chance to stop the process, and abandoning it because an RPC deadline passed
// is what leaves an orphan. They remain bounded by their own timers, so nothing
// blocks indefinitely.
func (b *ImageBooter) stopAndProve(ctx context.Context, in usecase.ImageDecommissionInput, proc vmProcess) error {
	log := logger.FromContext(ctx)

	if b.guestControl == nil || in.SocketPath == "" {
		log.Warn("image-boot: resident stop has no guest control channel, signalling the vmm instead",
			"socket_path", in.SocketPath)
	} else {
		// The caller's context is threaded, not replaced: a cancelled caller cuts the
		// graceful attempt short and drops straight to the bounded fallback.
		shutdownCtx, cancel := context.WithTimeout(ctx, b.stop.guestShutdown)
		err := b.guestControl.Shutdown(shutdownCtx, in.SocketPath)
		cancel()
		if err != nil {
			log.Warn("image-boot: guest shutdown returned an error; still waiting out the power-off window before signalling",
				"socket_path", in.SocketPath, "err", err)
		}
		// ⚠ THE POWER-OFF WINDOW IS WAITED OUT ON BOTH BRANCHES, INCLUDING THE ERROR
		// ONE. A failed Shutdown call does not mean the guest did not receive it: a
		// timeout, a lost ack, or a reply that arrived late all look identical from
		// here, and in every one of them the guest may be part-way through the exact
		// clean stop this path exists to allow — Postgres running its fast shutdown
		// checkpoint, image-init syncing and issuing its power-off. Sending SIGTERM at
		// that moment crash-stops a customer's database mid-flush, which is the
		// failure mode the control channel was added to remove. Waiting costs at most
		// the power-off budget on a guest that really is wedged, and that guest still
		// gets TERM and KILL below.
		gone, werr := waitProcessGone(ctx, proc, b.stop.powerOff, b.stop.exitPoll)
		if gone {
			return nil
		}
		log.Warn("image-boot: vmm still alive after the guest shutdown window, falling back to signalling",
			"socket_path", in.SocketPath, "pid", in.PID, "shutdown_err", err, "err", werr)
	}

	// The fallback. Its own timers bound it; the caller's cancellation does not.
	fallbackCtx := context.WithoutCancel(ctx)
	for _, step := range []struct {
		sig   syscall.Signal
		grace time.Duration
	}{
		{syscall.SIGTERM, b.stop.sigtermGrace},
		{syscall.SIGKILL, b.stop.sigkillGrace},
	} {
		if err := proc.Signal(step.sig); err != nil {
			// A delivery failure that IS absence is proof; anything else is not.
			if signalReportsGone(err) {
				return nil
			}
			return fmt.Errorf("%w: pid %d could not be sent %v and its absence was not observed: %v",
				domain.ErrVMTerminationUnproven, in.PID, step.sig, err)
		}
		gone, werr := waitProcessGone(fallbackCtx, proc, step.grace, b.stop.exitPoll)
		if gone {
			return nil
		}
		if werr != nil {
			return fmt.Errorf("%w: pid %d could not be probed after %v: %v",
				domain.ErrVMTerminationUnproven, in.PID, step.sig, werr)
		}
	}
	return fmt.Errorf("%w: pid %d is still running %s after SIGKILL",
		domain.ErrVMTerminationUnproven, in.PID, b.stop.sigkillGrace)
}

// processGone reports whether a process is provably absent. The bool is the
// proof; the error is a probe that could not answer (permission, anything else),
// which is explicitly NOT absence.
func processGone(proc vmProcess) (bool, error) {
	err := proc.Signal(syscall.Signal(0))
	if err == nil {
		return false, nil
	}
	if signalReportsGone(err) {
		return true, nil
	}
	return false, err
}

// signalReportsGone reports whether a signal error means "no such process".
// ESRCH and Go's os.ErrProcessDone are the ONLY two reports of absence that do
// not depend on being the process's parent — which matters because after a
// service restart the VMM is not our child.
func signalReportsGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// waitProcessGone polls signal 0 until the process is provably absent, the
// timeout elapses, or the context ends. The bool is the proof; a non-nil error
// is a probe that could not answer.
func waitProcessGone(ctx context.Context, proc vmProcess, timeout, poll time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		gone, err := processGone(proc)
		if gone {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(poll):
		}
	}
}

// setupNet acquires this owner's addressing lease and creates the routed /30 TAP.
// It returns the leased addressing plus a cleanup func that reverses exactly what
// it did. rt#8 retired the per-VM DNAT, so ingress is served by Caddy, not
// iptables rules.
//
// The lease is taken BEFORE the TAP exists and released only after it is gone,
// which is the invariant that makes the plane safe across a crash at any point:
// a lease with no device is a slot temporarily unused, while a device with no
// lease is a slot that could be handed to a second tenant.
func (b *ImageBooter) setupNet(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (imgNet, func(), error) {
	if b.alloc == nil {
		// No fallback: the whole point of the lease plane is that there is no second
		// way to obtain an address.
		return imgNet{}, func() {}, fmt.Errorf("%w: this booter has no addressing allocator wired",
			domain.ErrNetPlaneUnreconciled)
	}
	lease, err := b.alloc.Acquire(ctx, kind, ownerID)
	if err != nil {
		return imgNet{}, func() {}, fmt.Errorf("acquire microVM addressing lease: %w", err)
	}
	nw := imgNetFromLease(lease)
	if err := b.createTap(nw, nw.uid, nw.uid); err != nil {
		// Boot-failure path: no VM was started, so the release outcome cannot change
		// what is reported — the createTap error IS the cause. releaseLease logs its
		// own failure.
		_ = b.releaseLease(ctx, kind, ownerID)
		return imgNet{}, func() {}, err
	}
	cleanup := func() {
		b.destroyTap(ctx, nw.tapName)
		// Same reasoning: this cleanup runs on a boot failure or after a single-shot
		// VM has already exited, and it has no error channel to report through.
		_ = b.releaseLease(ctx, kind, ownerID)
	}
	return nw, cleanup, nil
}

// releaseLease frees an owner's addressing lease and REPORTS whether it did.
//
// It used to log-and-swallow, on the reasoning that every caller is already on a
// teardown path. That made Decommission claim success while the slot stayed
// held — and Decommission then went on to delete the jail, the socket and the
// rootfs, i.e. every artifact that could later identify what the held slot
// belongs to. The allocator's own host check (a foreign lease is refused) also
// reaches the caller through this return, which is what stops one host quietly
// freeing another's addressing.
//
// The operational detail is still logged at ERROR here so the condition is
// visible even on the paths that legitimately cannot propagate it.
func (b *ImageBooter) releaseLease(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error {
	if b.alloc == nil {
		return nil
	}
	if err := b.alloc.Release(ctx, kind, ownerID); err != nil {
		logger.FromContext(ctx).Error("image-boot: release microVM addressing lease failed; its slot stays held until the next reconcile",
			"owner_kind", kind, "owner_id", ownerID, "err", err)
		return fmt.Errorf("release microVM addressing lease for %s %s: %w", kind, ownerID, err)
	}
	return nil
}

// ReclaimLeaseArtifacts removes the host-side artifacts a lease names: its TAP
// device and the jailer chroot keyed by its host-local slot. It is the
// boot-time reconcile's hook for a lease whose owner row is gone — there is no VM
// handle to decommission, only the lease's own recorded coordinates.
//
// It reports failures rather than refusing to continue (the caller is a cleanup
// path), but it reports them: a TAP that could not be deleted is a device the next
// boot on that slot will have to delete itself, and a chroot left behind holds
// mknod'd device nodes owned by that slot's uid.
func (b *ImageBooter) ReclaimLeaseArtifacts(ctx context.Context, lease domain.NetLease) error {
	var problems []string
	if lease.TapName != "" {
		if out, err := exec.Command("ip", "link", "del", lease.TapName).CombinedOutput(); err != nil {
			// A device that was already gone is the desired state, not a failure, but it
			// is indistinguishable from a real error here without parsing output — so it
			// is reported and the caller logs it at WARN.
			problems = append(problems, fmt.Sprintf("delete tap %s: %s: %v", lease.TapName, strings.TrimSpace(string(out)), err))
		}
	}
	// A nil provider (or an unconfigured chroot base) means there is no jail layout
	// to reason about; removing a path derived from an empty base could delete an
	// unrelated directory, so it is skipped rather than guessed.
	if b.p != nil && b.p.cfg.ChrootBase != "" && lease.LocalSlot > 0 {
		jail := newVMJail(b.p.cfg.ChrootBase, strconv.Itoa(lease.LocalSlot), lease.VMUID)
		if err := os.RemoveAll(jail.jailDir()); err != nil {
			problems = append(problems, fmt.Sprintf("remove jail dir %s: %v", jail.jailDir(), err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("reclaim lease %s artifacts: %s", lease.ID, strings.Join(problems, "; "))
	}
	return nil
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
