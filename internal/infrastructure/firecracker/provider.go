package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

const (
	bridgeName   = "fcbr0"
	bridgeSubnet = "172.16.0.0/24"
	bridgeIP     = "172.16.0.1/24"
)

// Provider implements the VMProvider and ExecutionRunner interfaces
// using Firecracker microVMs for secure code execution
type Provider struct {
	cfg        config.FirecrackerConfig
	bridgeOnce sync.Once
	bridgeErr  error
	tapCounter atomic.Uint32 // monotonic counter for unique /30 subnets

	// pool is the optional pre-warmed VM pool (§9.1). When set, Boot
	// hands a pooled VM to callers when available and falls back to a
	// fresh cold boot when the pool is empty. Nil pool preserves the
	// legacy "always cold boot" behaviour.
	pool *VMPool

	// checkpointScheduler, when set, drives per-VM automatic
	// snapshotting (CS-2 G2.8). Boot() registers the new VM; Terminate()
	// deregisters it. Nil is safe — Boot/Terminate stay on the legacy
	// no-snapshot path.
	checkpointScheduler *CheckpointScheduler
}

// SetCheckpointScheduler attaches a running CheckpointScheduler so
// Boot/Terminate register + deregister VMs automatically. Must be
// called before Boot for the wiring to apply to the first VM.
func (p *Provider) SetCheckpointScheduler(s *CheckpointScheduler) {
	p.checkpointScheduler = s
}

// NewProvider creates a new Firecracker provider
func NewProvider(cfg config.FirecrackerConfig) *Provider {
	return &Provider{cfg: cfg}
}

// --- Network Setup ---

// setupBridge creates the fcbr0 bridge if it does not already exist.
// It is safe to call multiple times; the actual work runs only once.
func (p *Provider) setupBridge() error {
	p.bridgeOnce.Do(func() {
		p.bridgeErr = p.doSetupBridge()
	})
	return p.bridgeErr
}

func (p *Provider) doSetupBridge() error {
	// Check whether the bridge already exists
	if err := exec.Command("ip", "link", "show", bridgeName).Run(); err == nil {
		log.Printf("Bridge %s already exists, skipping creation", bridgeName)
		return nil
	}

	cmds := [][]string{
		{"ip", "link", "add", bridgeName, "type", "bridge"},
		{"ip", "addr", "add", bridgeIP, "dev", bridgeName},
		{"ip", "link", "set", bridgeName, "up"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("bridge setup %v: %s: %w", args, string(out), err)
		}
	}

	// Enable IP forwarding (best-effort; may already be enabled)
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// Add NAT masquerade rule (idempotent with -C check)
	if exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", bridgeSubnet, "-j", "MASQUERADE").Run() != nil {
		if out, err := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", bridgeSubnet, "-j", "MASQUERADE").CombinedOutput(); err != nil {
			log.Printf("Warning: iptables masquerade failed: %s: %v", string(out), err)
		}
	}

	log.Printf("Bridge %s created with %s", bridgeName, bridgeIP)
	return nil
}

// createTapDevice creates a TAP device for a VM and attaches it to the bridge.
// It returns the tap device name and the guest IP address to configure inside the VM.
func (p *Provider) createTapDevice(vmID uuid.UUID) (tapName string, hostIP string, guestIP string, err error) {
	if err = p.setupBridge(); err != nil {
		return "", "", "", fmt.Errorf("setup bridge: %w", err)
	}

	shortID := vmID.String()[:8]
	tapName = "tap-" + shortID

	// Allocate a unique /30 subnet index. Each VM gets a 4-address block:
	//   base = 172.16.0.(n*4)  (network)
	//   host = 172.16.0.(n*4+1)
	//   guest = 172.16.0.(n*4+2)
	//   bcast = 172.16.0.(n*4+3)
	// We skip index 0 because 172.16.0.1 is the bridge IP.
	idx := p.tapCounter.Add(1) // starts at 1
	base := idx * 4
	if base+3 > 254 {
		return "", "", "", fmt.Errorf("TAP IP space exhausted (index %d)", idx)
	}
	hostIP = fmt.Sprintf("172.16.0.%d", base+1)
	guestIP = fmt.Sprintf("172.16.0.%d", base+2)
	hostCIDR := fmt.Sprintf("%s/30", hostIP)

	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tapName, "mode", "tap"},
		{"ip", "addr", "add", hostCIDR, "dev", tapName},
		{"ip", "link", "set", tapName, "up"},
		{"ip", "link", "set", tapName, "master", bridgeName},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			// Best-effort cleanup on failure
			_ = exec.Command("ip", "link", "del", tapName).Run()
			return "", "", "", fmt.Errorf("create tap %v: %s: %w", args, string(out), err)
		}
	}

	log.Printf("Created TAP %s: host=%s guest=%s", tapName, hostIP, guestIP)
	return tapName, hostIP, guestIP, nil
}

// destroyTapDevice removes a TAP device.
func (p *Provider) destroyTapDevice(tapName string) error {
	if out, err := exec.Command("ip", "link", "del", tapName).CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %s: %w", tapName, string(out), err)
	}
	log.Printf("Destroyed TAP %s", tapName)
	return nil
}

// cleanupTap is a no-op when tapName is empty (isolated mode). Otherwise
// it tears down both the device and any iptables rules installed for it.
func (p *Provider) cleanupTap(tapName string, policy domain.NetworkPolicy) {
	if tapName == "" {
		return
	}
	if policy.Mode == domain.NetworkPolicyEgressList {
		p.flushEgressList(tapName)
	}
	if err := p.destroyTapDevice(tapName); err != nil {
		log.Printf("Warning: failed to destroy TAP %s: %v", tapName, err)
	}
}

// applyEgressList installs an iptables FORWARD chain that drops every
// packet leaving the VM's TAP except those destined to one of the
// allowed hosts/CIDRs. It runs `iptables` as a child process — the same
// pattern used by setupBridge — so we don't need cgo or netlink bindings.
//
// Hostnames are resolved once at boot. Failed lookups are logged but do
// not abort boot: the caller still gets a working (but more locked-down)
// network rather than a dead VM.
func (p *Provider) applyEgressList(tapName string, allowed []string) error {
	chain := egressChainName(tapName)
	// Create a dedicated chain so we never collide with another VM.
	cmds := [][]string{
		{"iptables", "-N", chain},
		{"iptables", "-I", "FORWARD", "-i", tapName, "-j", chain},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("egress chain setup %v: %s: %w", args, string(out), err)
		}
	}

	// ACCEPT each allowed destination.
	for _, host := range allowed {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		dests := []string{host}
		// If the entry is neither IP nor CIDR, resolve it.
		if _, _, err := net.ParseCIDR(host); err != nil {
			if ip := net.ParseIP(host); ip == nil {
				ips, dnsErr := net.LookupIP(host)
				if dnsErr != nil {
					log.Printf("Warning: failed to resolve %s for egress allowlist: %v", host, dnsErr)
					continue
				}
				dests = nil
				for _, ip := range ips {
					if ip.To4() != nil {
						dests = append(dests, ip.String())
					}
				}
			}
		}
		for _, d := range dests {
			args := []string{"iptables", "-A", chain, "-d", d, "-j", "ACCEPT"}
			if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
				log.Printf("Warning: failed to add ACCEPT rule for %s: %s: %v", d, string(out), err)
			}
		}
	}

	// Trailing DROP for everything else leaving the TAP.
	if out, err := exec.Command("iptables", "-A", chain, "-j", "DROP").CombinedOutput(); err != nil {
		return fmt.Errorf("egress trailing DROP: %s: %w", string(out), err)
	}
	log.Printf("Egress allowlist installed on %s with %d host(s)", tapName, len(allowed))
	return nil
}

// flushEgressList removes the per-VM iptables chain installed by
// applyEgressList. Called from cleanupTap on shutdown / boot failure.
func (p *Provider) flushEgressList(tapName string) {
	chain := egressChainName(tapName)
	// Best-effort: each command is idempotent on the rule-not-found path.
	cmds := [][]string{
		{"iptables", "-D", "FORWARD", "-i", tapName, "-j", chain},
		{"iptables", "-F", chain},
		{"iptables", "-X", chain},
	}
	for _, args := range cmds {
		_ = exec.Command(args[0], args[1:]...).Run()
	}
}

// egressChainName returns a deterministic, length-bounded iptables chain
// name. iptables enforces a 28-character limit on chain names, so we use
// a short prefix plus the tap suffix (which is itself bounded by Linux
// IFNAMSIZ at 15 chars).
func egressChainName(tapName string) string {
	suffix := tapName
	if len(suffix) > 20 {
		suffix = suffix[:20]
	}
	return "fc-eg-" + suffix
}

// generateMAC returns a deterministic MAC address derived from a VM UUID.
// Format: AA:FC:xx:xx:xx:xx using the first 4 bytes of the UUID.
func generateMAC(vmID uuid.UUID) string {
	b := vmID[:]
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3])
}

// vmIDFromSocketPath extracts the VM UUID from the socket path (basename minus .sock).
func vmIDFromSocketPath(socketPath string) (uuid.UUID, error) {
	base := filepath.Base(socketPath)
	name := strings.TrimSuffix(base, ".sock")
	return uuid.Parse(name)
}

// SetPool wires the pre-warmed pool. The pool must be started separately
// by the caller (typically from DI). Passing nil clears the pool and
// reverts Boot to the cold-boot-only path.
func (p *Provider) SetPool(pool *VMPool) {
	p.pool = pool
}

// Boot starts a Firecracker microVM. When a pool is wired via SetPool
// and has a VM available, the pooled VM is returned directly — this
// is the §9.1 fast path. Otherwise Boot falls back to bootCold, the
// original "start a fresh Firecracker process" logic.
func (p *Provider) Boot(ctx context.Context, bootCfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	if p.pool != nil {
		// Non-blocking pool check: we don't want Boot's latency to
		// regress in the empty-pool case. The pool's internal bootFresh
		// uses bootCold, not Boot, so there's no recursion.
		select {
		case vm := <-p.pool.available:
			if vm != nil {
				return &usecase.VMBootResult{
					PID:        vm.PID,
					IPAddress:  vm.IPAddress,
					SocketPath: vm.SocketPath,
					BootTimeMS: vm.BootTimeMS,
				}, nil
			}
		default:
		}
	}
	return p.bootCold(ctx, bootCfg)
}

// bootCold starts a fresh Firecracker microVM, bypassing any pool.
// This is the original Boot implementation.
func (p *Provider) bootCold(ctx context.Context, bootCfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	start := time.Now()

	socketPath := p.socketPath(bootCfg.VMID)
	kernelPath := p.cfg.KernelPath
	rootfsPath := p.rootfsForLanguage(bootCfg.Language)

	// Ensure socket directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return nil, fmt.Errorf("failed to create socket dir: %w", err)
	}

	// Apply network policy. In "isolated" mode we skip TAP creation
	// entirely so the guest boots with no network device. In "egress
	// list" mode we create the TAP and then install iptables DROP/ACCEPT
	// rules. "allow_host" and the empty default behave like before.
	policy := bootCfg.NetworkPolicy
	if policy.Mode == "" {
		policy.Mode = domain.NetworkPolicyAllowHost
	}

	var (
		tapName string
		guestIP string
	)
	attachNetwork := policy.Mode != domain.NetworkPolicyIsolated
	if attachNetwork {
		var err error
		tapName, _, guestIP, err = p.createTapDevice(bootCfg.VMID)
		if err != nil {
			return nil, fmt.Errorf("failed to create TAP device: %w", err)
		}
		// Apply per-VM iptables rules for egress restriction. We do it
		// before starting Firecracker so the kernel never sees an
		// unfiltered packet leaving the new TAP.
		if policy.Mode == domain.NetworkPolicyEgressList {
			if err := p.applyEgressList(tapName, policy.AllowedHosts); err != nil {
				_ = p.destroyTapDevice(tapName)
				return nil, fmt.Errorf("failed to apply egress list: %w", err)
			}
		}
	} else {
		log.Printf("VM %s booting in isolated mode (no network)", bootCfg.VMID)
	}

	// Build Firecracker command
	args := []string{
		"--api-sock", socketPath,
	}

	var cmd *exec.Cmd
	if p.cfg.UseJailer {
		// Use jailer for production: chroot + seccomp + cgroup isolation
		cmd = exec.CommandContext(ctx, p.cfg.JailerPath,
			"--id", bootCfg.VMID.String(),
			"--exec-file", p.cfg.BinaryPath,
			"--uid", "0",
			"--gid", "0",
			"--",
		)
		cmd.Args = append(cmd.Args, args...)
	} else {
		// Direct Firecracker for development
		cmd = exec.CommandContext(ctx, p.cfg.BinaryPath, args...)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		p.cleanupTap(tapName, policy)
		return nil, fmt.Errorf("failed to start firecracker: %w", err)
	}

	pid := cmd.Process.Pid

	// Wait for the API socket to be available
	if err := p.waitForSocket(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		p.cleanupTap(tapName, policy)
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	// Configure the VM via Firecracker API
	if err := p.configureVMWithPolicy(ctx, socketPath, kernelPath, rootfsPath, bootCfg, attachNetwork, tapName); err != nil {
		_ = cmd.Process.Kill()
		p.cleanupTap(tapName, policy)
		return nil, fmt.Errorf("failed to configure VM: %w", err)
	}

	// Start the VM instance
	if err := p.startInstance(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		p.cleanupTap(tapName, policy)
		return nil, fmt.Errorf("failed to start VM instance: %w", err)
	}

	bootTimeMS := time.Since(start).Milliseconds()
	log.Printf("Firecracker VM booted: pid=%d, socket=%s, tap=%s, guest=%s, boot=%dms",
		pid, socketPath, tapName, guestIP, bootTimeMS)

	// CS-2 G2.8 — register with the per-VM checkpoint scheduler. Nil
	// scheduler leaves the legacy (no auto-snapshot) behaviour intact.
	if p.checkpointScheduler != nil {
		p.checkpointScheduler.Register(context.Background(), VMRegistration{
			VMID:       bootCfg.VMID,
			SocketPath: socketPath,
		})
	}

	return &usecase.VMBootResult{
		PID:        pid,
		IPAddress:  guestIP,
		SocketPath: socketPath,
		BootTimeMS: bootTimeMS,
	}, nil
}

// Terminate kills a running microVM and cleans up its TAP device.
func (p *Provider) Terminate(ctx context.Context, socketPath string, pid int) error {
	// CS-2 G2.8 — deregister from the checkpoint scheduler BEFORE we
	// send the shutdown signal so the goroutine can't race with the
	// guest tearing down.
	if p.checkpointScheduler != nil && socketPath != "" {
		if vmID, err := vmIDFromSocketPath(socketPath); err == nil {
			p.checkpointScheduler.Deregister(vmID)
		}
	}

	// Send shutdown request via API first for graceful shutdown
	if socketPath != "" {
		_ = p.sendAction(ctx, socketPath, "SendCtrlAltDel")
	}

	// Kill the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Already dead — fall through to cleanup
	} else {
		// Wait briefly for graceful shutdown, then force kill
		done := make(chan error, 1)
		go func() {
			_, err := process.Wait()
			done <- err
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = process.Kill()
		}
	}

	// Clean up the TAP device associated with this VM
	if socketPath != "" {
		if vmID, err := vmIDFromSocketPath(socketPath); err == nil {
			tapName := "tap-" + vmID.String()[:8]
			if err := p.destroyTapDevice(tapName); err != nil {
				log.Printf("Warning: failed to destroy TAP %s: %v", tapName, err)
			}
		}
	}

	// Clean up the socket file
	if socketPath != "" {
		_ = os.Remove(socketPath)
	}

	return nil
}

// Pause pauses a running microVM (for snapshot creation)
func (p *Provider) Pause(ctx context.Context, socketPath string) error {
	return p.sendAction(ctx, socketPath, "Pause")
}

// Resume resumes a paused microVM
func (p *Provider) Resume(ctx context.Context, socketPath string) error {
	return p.sendAction(ctx, socketPath, "Resume")
}

// Run executes code inside a dedicated Firecracker microVM using rootfs injection.
//
// Instead of SSH, it:
//  1. Copies the language rootfs to a temporary file for this execution.
//  2. Loop-mounts the copy and injects the user's code plus a run script.
//  3. Overwrites /init so the VM executes the code on boot and powers off.
//  4. Boots a fresh Firecracker instance with the modified rootfs.
//  5. Waits for the Firecracker process to exit (the VM powers off after execution).
//  6. Remounts the rootfs and reads stdout/stderr/exit_code from well-known files.
//
// This approach is simpler and more secure than SSH: no network stack required
// inside the guest, no SSH keys to manage, and no race conditions on SSH readiness.
func (p *Provider) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	start := time.Now()

	// 1. Create a copy of the rootfs for this execution
	srcRootfs := p.rootfsForLanguage(execution.Language)
	execRootfs := filepath.Join(p.cfg.RootfsBasePath, fmt.Sprintf("exec-%s.ext4", execution.ID))
	if err := copyFile(srcRootfs, execRootfs); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}
	defer os.Remove(execRootfs)

	// 2. Mount the rootfs and inject code + run script
	mountDir := filepath.Join(os.TempDir(), "fc-mount-"+execution.ID.String())
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return nil, fmt.Errorf("create mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if err := p.injectCode(execRootfs, mountDir, execution); err != nil {
		return nil, fmt.Errorf("inject code: %w", err)
	}

	// 3. Boot a temporary VM with the modified rootfs
	execVMID := uuid.New()
	socketPath := p.socketPath(execVMID)

	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Create TAP device (networking is set up even though we don't use SSH;
	// Firecracker requires a network config or explicit opt-out per API)
	tapName, _, guestIP, err := p.createTapDevice(execVMID)
	if err != nil {
		return nil, fmt.Errorf("create TAP for exec VM: %w", err)
	}
	defer func() { _ = p.destroyTapDevice(tapName) }()

	cmd := exec.CommandContext(ctx, p.cfg.BinaryPath, "--api-sock", socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	// Ensure the process is cleaned up no matter what
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
	}()

	if err := p.waitForSocket(ctx, socketPath); err != nil {
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	// Configure VM: use execution resources from the parent VM. Rate
	// limits come from the execution's ResourceLimit so tests inherit
	// the same disk+net caps as any other sandbox. (§8.3 / §9.1.5)
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/init"
	execBootCfg := usecase.VMBootConfig{
		VMID:                 execVMID,
		Language:             execution.Language,
		VCPU:                 vm.VCPU,
		MemoryMB:             vm.MemoryMB,
		DiskBandwidthMBps:    execution.Resources.DiskBandwidthMBps,
		DiskIOPS:             execution.Resources.DiskIOPS,
		NetworkBandwidthMBps: execution.Resources.NetworkBandwidthMBps,
		NetworkPPS:           execution.Resources.NetworkPPS,
	}

	client := p.unixHTTPClient(socketPath)

	// Machine config
	machineConfig := map[string]any{
		"vcpu_count":   execBootCfg.VCPU,
		"mem_size_mib": execBootCfg.MemoryMB,
	}
	if err := p.apiPut(ctx, client, "/machine-config", machineConfig); err != nil {
		return nil, fmt.Errorf("configure machine: %w", err)
	}

	// Boot source with init=/init
	bootSource := map[string]any{
		"kernel_image_path": p.cfg.KernelPath,
		"boot_args":         bootArgs,
	}
	if err := p.apiPut(ctx, client, "/boot-source", bootSource); err != nil {
		return nil, fmt.Errorf("configure boot source: %w", err)
	}

	// Rootfs drive (the modified copy). Apply per-drive rate limiter
	// when caller specified DiskBandwidthMBps / DiskIOPS (§9.1.5).
	rootfsDrive := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   execRootfs,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if rl := driveRateLimiter(execBootCfg.DiskBandwidthMBps, execBootCfg.DiskIOPS); rl != nil {
		rootfsDrive["rate_limiter"] = rl
	}
	if err := p.apiPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return nil, fmt.Errorf("configure rootfs: %w", err)
	}

	// Network interface with per-iface rate limiters for egress shaping.
	mac := generateMAC(execVMID)
	netIface := map[string]any{
		"iface_id":      "eth0",
		"guest_mac":     mac,
		"host_dev_name": tapName,
	}
	if rl := netRateLimiter(execBootCfg.NetworkBandwidthMBps, execBootCfg.NetworkPPS); rl != nil {
		netIface["tx_rate_limiter"] = rl
		netIface["rx_rate_limiter"] = rl
	}
	if err := p.apiPut(ctx, client, "/network-interfaces/eth0", netIface); err != nil {
		return nil, fmt.Errorf("configure network: %w", err)
	}

	// Vsock device (for guest agent communication)
	// Guest CID must be >= 3 (0=hypervisor, 1=reserved, 2=host)
	guestCID := 3 + (p.tapCounter.Load() % 250) // unique CID per VM
	vsockCfg := map[string]any{
		"guest_cid": guestCID,
		"uds_path":  socketPath + ".vsock",
	}
	if err := p.apiPut(ctx, client, "/vsock", vsockCfg); err != nil {
		log.Printf("Warning: failed to configure vsock (agent communication disabled): %v", err)
		// Don't fail — rootfs-injection still works without vsock
	}

	// Start the instance
	if err := p.startInstance(ctx, socketPath); err != nil {
		return nil, fmt.Errorf("start instance: %w", err)
	}

	// 4. Wait for the VM to exit (it powers off after running the script)
	timeout := time.Duration(execution.Resources.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	timedOut := false

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-waitDone:
		// VM exited normally
	case <-time.After(timeout):
		timedOut = true
		_ = cmd.Process.Kill()
		<-waitDone
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waitDone
		return nil, ctx.Err()
	}

	_ = guestIP // used by TAP but not needed for rootfs-injection reads

	// 5. Remount rootfs and read results
	stdout, stderr, exitCode, readErr := p.readResults(execRootfs, mountDir)

	execTimeMS := time.Since(start).Milliseconds()

	if timedOut {
		return &usecase.RunResult{
			ExitCode:   124,
			Stdout:     stdout,
			Stderr:     "execution timed out",
			ExecTimeMS: execTimeMS,
		}, nil
	}

	if readErr != nil {
		return nil, fmt.Errorf("read execution results: %w", readErr)
	}

	return &usecase.RunResult{
		ExitCode:   exitCode,
		Stdout:     stdout,
		Stderr:     stderr,
		ExecTimeMS: execTimeMS,
	}, nil
}

// injectCode mounts the rootfs image, writes the user's code and a run script,
// and overwrites /init so the VM executes the code on boot and then powers off.
func (p *Provider) injectCode(rootfsPath, mountDir string, execution *domain.Execution) error {
	// Mount
	if out, err := exec.Command("mount", "-o", "loop", rootfsPath, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs: %s: %w", string(out), err)
	}

	// We must unmount before returning so Firecracker can use the image
	defer func() {
		_ = exec.Command("umount", mountDir).Run()
	}()

	// Ensure /tmp exists inside the rootfs
	tmpDir := filepath.Join(mountDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("mkdir /tmp: %w", err)
	}

	// Write code file
	ext, compileCmd, runCmd := p.languageCommands(execution.Language)
	codeFile := "code" + ext
	if err := os.WriteFile(filepath.Join(tmpDir, codeFile), []byte(execution.Code), 0644); err != nil {
		return fmt.Errorf("write code file: %w", err)
	}

	// Write stdin if present
	if execution.Stdin != "" {
		if err := os.WriteFile(filepath.Join(tmpDir, "stdin.txt"), []byte(execution.Stdin), 0644); err != nil {
			return fmt.Errorf("write stdin: %w", err)
		}
	}

	// Build the run script
	stdinRedirect := ""
	if execution.Stdin != "" {
		stdinRedirect = " < /tmp/stdin.txt"
	}

	var scriptBody string
	if compileCmd != "" {
		// Compiled language: compile first, then run the binary
		scriptBody = fmt.Sprintf(`# Compile
%s > /tmp/compile_stdout.txt 2> /tmp/compile_stderr.txt
COMPILE_RC=$?
if [ $COMPILE_RC -ne 0 ]; then
    cat /tmp/compile_stdout.txt > /tmp/stdout.txt
    cat /tmp/compile_stderr.txt > /tmp/stderr.txt
    echo $COMPILE_RC > /tmp/exit_code.txt
    sync
    reboot -f
fi
# Run
%s%s > /tmp/stdout.txt 2> /tmp/stderr.txt
echo $? > /tmp/exit_code.txt
`, compileCmd, runCmd, stdinRedirect)
	} else {
		scriptBody = fmt.Sprintf(`%s%s > /tmp/stdout.txt 2> /tmp/stderr.txt
echo $? > /tmp/exit_code.txt
`, runCmd, stdinRedirect)
	}

	runScript := fmt.Sprintf(`#!/bin/sh
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
cd /tmp
%s
sync
reboot -f
`, scriptBody)

	if err := os.WriteFile(filepath.Join(tmpDir, "run.sh"), []byte(runScript), 0755); err != nil {
		return fmt.Errorf("write run script: %w", err)
	}

	// Overwrite /init (or /sbin/init) to call our run script.
	// The kernel boot_args specify init=/init so we write to /init.
	initScript := `#!/bin/sh
exec /tmp/run.sh
`
	if err := os.WriteFile(filepath.Join(mountDir, "init"), []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write /init: %w", err)
	}

	return nil
}

// readResults mounts the rootfs image and reads stdout, stderr, and exit code
// from well-known files written by the run script.
func (p *Provider) readResults(rootfsPath, mountDir string) (stdout, stderr string, exitCode int, err error) {
	if out, mountErr := exec.Command("mount", "-o", "loop", rootfsPath, mountDir).CombinedOutput(); mountErr != nil {
		return "", "", 1, fmt.Errorf("remount rootfs: %s: %w", string(out), mountErr)
	}
	defer func() {
		_ = exec.Command("umount", mountDir).Run()
	}()

	stdoutBytes, _ := os.ReadFile(filepath.Join(mountDir, "tmp", "stdout.txt"))
	stderrBytes, _ := os.ReadFile(filepath.Join(mountDir, "tmp", "stderr.txt"))
	exitCodeBytes, _ := os.ReadFile(filepath.Join(mountDir, "tmp", "exit_code.txt"))

	stdout = string(stdoutBytes)
	stderr = string(stderrBytes)
	exitCode, _ = strconv.Atoi(strings.TrimSpace(string(exitCodeBytes)))

	return stdout, stderr, exitCode, nil
}

// copyFile copies src to dst using io.Copy for efficiency.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// sshResult holds the output of an SSH command execution.
type sshResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// sshWriteFile writes content to a file on the VM via SSH.
func (p *Provider) sshWriteFile(ctx context.Context, ip, remotePath, content string) error {
	// Use ssh with stdin to write the file
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", ip),
		fmt.Sprintf("cat > %s", remotePath),
	)
	cmd.Stdin = bytes.NewBufferString(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh write: %s: %w", stderr.String(), err)
	}
	return nil
}

// sshExec runs a command on the VM via SSH and returns the result.
func (p *Provider) sshExec(ctx context.Context, ip, command, stdin string, timeoutSec int) (*sshResult, error) {
	timeout := time.Duration(timeoutSec) * time.Second
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", ip),
		command,
	)

	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if execCtx.Err() == context.DeadlineExceeded {
			return &sshResult{ExitCode: 124, Stderr: "execution timed out"}, nil
		} else {
			return nil, err
		}
	}

	return &sshResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// languageCommands returns the file extension, compile command, and run command
// for a given language. Compiled languages have a non-empty compileCmd.
func (p *Provider) languageCommands(lang domain.Language) (ext, compileCmd, runCmd string) {
	switch lang {
	case domain.LanguageGo:
		return ".go", "cd /tmp && go build -o code_bin code.go", "/tmp/code_bin"
	case domain.LanguagePython:
		return ".py", "", "python3 /tmp/code.py"
	case domain.LanguageJavaScript:
		return ".js", "", "node /tmp/code.js"
	case domain.LanguageTypeScript:
		return ".ts", "", "npx tsx /tmp/code.ts"
	case domain.LanguageRust:
		return ".rs", "rustc /tmp/code.rs -o /tmp/code_bin", "/tmp/code_bin"
	case domain.LanguageC:
		return ".c", "gcc /tmp/code.c -o /tmp/code_bin", "/tmp/code_bin"
	case domain.LanguageCPP:
		return ".cpp", "g++ /tmp/code.cpp -o /tmp/code_bin", "/tmp/code_bin"
	case domain.LanguageBash:
		return ".sh", "", "bash /tmp/code.sh"
	default:
		return ".txt", "", "cat /tmp/code.txt"
	}
}

// socketPath returns the Unix socket path for a VM
func (p *Provider) socketPath(vmID uuid.UUID) string {
	return filepath.Join(p.cfg.SocketDir, vmID.String()+".sock")
}

// rootfsForLanguage returns the rootfs image path for a given language
func (p *Provider) rootfsForLanguage(lang domain.Language) string {
	return filepath.Join(p.cfg.RootfsBasePath, string(lang)+".ext4")
}

// waitForSocket waits for the Firecracker API socket to become available
func (p *Provider) waitForSocket(ctx context.Context, socketPath string) error {
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for socket %s", socketPath)
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err == nil {
				return nil
			}
		}
	}
}

// configureVMWithPolicy is the network-policy-aware variant. When
// attachNetwork is false (isolated mode) the network-interfaces step is
// skipped entirely; otherwise it falls through to the standard
// configureVM. Splitting the helper keeps configureVM's signature stable
// for the existing call sites and tests.
func (p *Provider) configureVMWithPolicy(ctx context.Context, socketPath, kernelPath, rootfsPath string, cfg usecase.VMBootConfig, attachNetwork bool, tapName string) error {
	client := p.unixHTTPClient(socketPath)

	machineConfig := map[string]any{
		"vcpu_count":   cfg.VCPU,
		"mem_size_mib": cfg.MemoryMB,
	}
	if err := p.apiPut(ctx, client, "/machine-config", machineConfig); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}

	bootSource := map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init",
	}
	if err := p.apiPut(ctx, client, "/boot-source", bootSource); err != nil {
		return fmt.Errorf("configure boot source: %w", err)
	}

	rootfsDrive := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if rl := driveRateLimiter(cfg.DiskBandwidthMBps, cfg.DiskIOPS); rl != nil {
		rootfsDrive["rate_limiter"] = rl
	}
	if err := p.apiPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return fmt.Errorf("configure rootfs drive: %w", err)
	}

	if attachNetwork {
		if tapName == "" {
			tapName = "tap-" + cfg.VMID.String()[:8]
		}
		netIface := map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     generateMAC(cfg.VMID),
			"host_dev_name": tapName,
		}
		if rl := netRateLimiter(cfg.NetworkBandwidthMBps, cfg.NetworkPPS); rl != nil {
			netIface["tx_rate_limiter"] = rl
			netIface["rx_rate_limiter"] = rl
		}
		if err := p.apiPut(ctx, client, "/network-interfaces/eth0", netIface); err != nil {
			return fmt.Errorf("configure network: %w", err)
		}
	}

	guestCID := 3 + (p.tapCounter.Load() % 250)
	vsockCfg := map[string]any{
		"guest_cid": guestCID,
		"uds_path":  socketPath + ".vsock",
	}
	if err := p.apiPut(ctx, client, "/vsock", vsockCfg); err != nil {
		log.Printf("Warning: vsock config failed (agent communication disabled): %v", err)
	}

	log.Printf("Configured VM via socket %s: kernel=%s, rootfs=%s, vcpu=%d, mem=%dMB, network=%v",
		socketPath, kernelPath, rootfsPath, cfg.VCPU, cfg.MemoryMB, attachNetwork)
	return nil
}

// configureVM sends configuration to the Firecracker API via Unix socket HTTP.
func (p *Provider) configureVM(ctx context.Context, socketPath, kernelPath, rootfsPath string, cfg usecase.VMBootConfig) error {
	client := p.unixHTTPClient(socketPath)

	// 1. Set machine configuration (vCPU + memory)
	machineConfig := map[string]any{
		"vcpu_count":   cfg.VCPU,
		"mem_size_mib": cfg.MemoryMB,
	}
	if err := p.apiPut(ctx, client, "/machine-config", machineConfig); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}

	// 2. Set boot source (kernel + boot args)
	bootSource := map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init",
	}
	if err := p.apiPut(ctx, client, "/boot-source", bootSource); err != nil {
		return fmt.Errorf("configure boot source: %w", err)
	}

	// 3. Attach rootfs drive
	rootfsDrive := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if rl := driveRateLimiter(cfg.DiskBandwidthMBps, cfg.DiskIOPS); rl != nil {
		rootfsDrive["rate_limiter"] = rl
	}
	if err := p.apiPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return fmt.Errorf("configure rootfs drive: %w", err)
	}

	// 4. Always configure networking (TAP device was created before Firecracker started)
	tapName := "tap-" + cfg.VMID.String()[:8]
	mac := generateMAC(cfg.VMID)
	netIface := map[string]any{
		"iface_id":      "eth0",
		"guest_mac":     mac,
		"host_dev_name": tapName,
	}
	if rl := netRateLimiter(cfg.NetworkBandwidthMBps, cfg.NetworkPPS); rl != nil {
		netIface["tx_rate_limiter"] = rl
		netIface["rx_rate_limiter"] = rl
	}
	if err := p.apiPut(ctx, client, "/network-interfaces/eth0", netIface); err != nil {
		return fmt.Errorf("configure network: %w", err)
	}

	// 5. Configure vsock for guest agent communication
	guestCID := 3 + (p.tapCounter.Load() % 250)
	vsockCfg := map[string]any{
		"guest_cid": guestCID,
		"uds_path":  socketPath + ".vsock",
	}
	if err := p.apiPut(ctx, client, "/vsock", vsockCfg); err != nil {
		log.Printf("Warning: vsock config failed (agent communication disabled): %v", err)
	} else {
		log.Printf("Vsock configured: guest_cid=%d", guestCID)
	}

	log.Printf("Configured VM via socket %s: kernel=%s, rootfs=%s, vcpu=%d, mem=%dMB",
		socketPath, kernelPath, rootfsPath, cfg.VCPU, cfg.MemoryMB)
	return nil
}

// startInstance sends the InstanceStart action to Firecracker.
func (p *Provider) startInstance(ctx context.Context, socketPath string) error {
	return p.sendAction(ctx, socketPath, "InstanceStart")
}

// sendAction sends an action to the Firecracker API via Unix socket HTTP.
func (p *Provider) sendAction(ctx context.Context, socketPath, action string) error {
	client := p.unixHTTPClient(socketPath)
	payload := map[string]string{"action_type": action}
	if err := p.apiPut(ctx, client, "/actions", payload); err != nil {
		return fmt.Errorf("action %s: %w", action, err)
	}
	log.Printf("Firecracker action: %s (socket=%s)", action, socketPath)
	return nil
}

// unixHTTPClient creates an HTTP client that communicates over a Unix socket.
func (p *Provider) unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// apiPut sends a PUT request to the Firecracker API.
func (p *Provider) apiPut(ctx context.Context, client *http.Client, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// apiGet sends a GET request to the Firecracker API and decodes the JSON response.
func (p *Provider) apiGet(ctx context.Context, client *http.Client, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}

// --- Snapshot/Restore ---

// CreateSnapshot creates a snapshot of a paused VM via the Firecracker API.
// The VM must be paused before calling this method.
func (p *Provider) CreateSnapshot(ctx context.Context, socketPath string, snapshotID uuid.UUID) (*usecase.SnapshotResult, error) {
	snapshotDir := p.cfg.SnapshotPath
	if err := os.MkdirAll(snapshotDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	memPath := filepath.Join(snapshotDir, snapshotID.String()+".mem")
	statePath := filepath.Join(snapshotDir, snapshotID.String()+".state")

	client := p.unixHTTPClient(socketPath)

	// Call Firecracker PUT /snapshot/create
	snapshotReq := map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	}
	if err := p.apiPut(ctx, client, "/snapshot/create", snapshotReq); err != nil {
		return nil, fmt.Errorf("firecracker snapshot create: %w", err)
	}

	// Determine snapshot file sizes
	var totalSize int64
	if info, err := os.Stat(memPath); err == nil {
		totalSize += info.Size()
	}
	if info, err := os.Stat(statePath); err == nil {
		totalSize += info.Size()
	}

	log.Printf("Snapshot created: %s (mem=%s, state=%s, size=%d bytes)", snapshotID, memPath, statePath, totalSize)

	return &usecase.SnapshotResult{
		MemoryFilePath: memPath,
		StateFilePath:  statePath,
		SizeBytes:      totalSize,
	}, nil
}

// RestoreSnapshot restores a VM from a snapshot via the Firecracker API.
// This should be called on a fresh (unconfigured) Firecracker instance.
func (p *Provider) RestoreSnapshot(ctx context.Context, socketPath, memPath, statePath string) error {
	client := p.unixHTTPClient(socketPath)

	// Validate that snapshot files exist
	if _, err := os.Stat(memPath); err != nil {
		return fmt.Errorf("memory file not found: %w", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		return fmt.Errorf("state file not found: %w", err)
	}

	// Call Firecracker PUT /snapshot/load
	loadReq := map[string]any{
		"snapshot_path": statePath,
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": memPath,
		},
		"enable_diff_snapshots": false,
		"resume_vm":             true,
	}
	if err := p.apiPut(ctx, client, "/snapshot/load", loadReq); err != nil {
		return fmt.Errorf("firecracker snapshot load: %w", err)
	}

	log.Printf("Snapshot restored from %s", statePath)
	return nil
}

// DeleteSnapshotFiles removes snapshot files from disk.
func (p *Provider) DeleteSnapshotFiles(memPath, statePath string) error {
	var errs []string
	if memPath != "" {
		if err := os.Remove(memPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove mem file: %v", err))
		}
	}
	if statePath != "" {
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove state file: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete snapshot files: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- IP Address Discovery ---

// discoverIPAddress discovers the IP address of a Firecracker VM after boot.
// It first tries parsing the network configuration, then falls back to SSH.
func (p *Provider) discoverIPAddress(ctx context.Context, socketPath string, vmID uuid.UUID) (string, error) {
	// Strategy 1: Query the Firecracker API for network interface info.
	// Firecracker does not expose the guest IP via its API, so we look at the
	// tap device configuration on the host side and infer the guest IP.
	// Convention: host tap IP is x.x.x.1, guest IP is x.x.x.2 on a /30 subnet.
	ip, err := p.discoverIPFromTap(ctx, vmID)
	if err == nil && ip != "" {
		log.Printf("Discovered VM %s IP from tap: %s", vmID, ip)
		return ip, nil
	}

	// Strategy 2: Wait briefly for SSH to come up, then query via SSH.
	// This works when the guest has a DHCP-assigned address or a known static IP.
	sshIP, err := p.discoverIPViaSSH(ctx, vmID)
	if err == nil && sshIP != "" {
		log.Printf("Discovered VM %s IP via SSH: %s", vmID, sshIP)
		return sshIP, nil
	}

	return "", fmt.Errorf("unable to discover IP for VM %s", vmID)
}

// discoverIPFromTap reads the IP configuration from the host tap interface.
// Firecracker VMs typically use a tap device named tap-<short-vmid>.
func (p *Provider) discoverIPFromTap(_ context.Context, vmID uuid.UUID) (string, error) {
	// Derive tap device name from VM ID (use first 8 chars to fit interface name limits)
	shortID := vmID.String()[:8]
	tapName := "tap-" + shortID

	// Read the tap device IP from the system
	cmd := exec.Command("ip", "addr", "show", tapName)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to query tap %s: %w", tapName, err)
	}

	// Parse the output to find the inet address
	output := stdout.String()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "inet ") {
			// Format: inet 172.16.0.1/30 ...
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				hostIP := strings.Split(parts[1], "/")[0]
				// Infer guest IP: if host is x.x.x.1, guest is x.x.x.2
				guestIP, err := inferGuestIP(hostIP)
				if err == nil {
					return guestIP, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no inet address found on tap %s", tapName)
}

// inferGuestIP derives the guest IP from the host tap IP.
// Convention: host ends in .1, guest ends in .2 on a /30 point-to-point link.
func inferGuestIP(hostIP string) (string, error) {
	parts := strings.Split(hostIP, ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("invalid IPv4 address: %s", hostIP)
	}
	lastOctet, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", fmt.Errorf("invalid last octet: %w", err)
	}
	// Guest is host + 1 in a /30 subnet
	parts[3] = strconv.Itoa(lastOctet + 1)
	return strings.Join(parts, "."), nil
}

// discoverIPViaSSH tries known candidate IPs and runs hostname -I to confirm reachability.
func (p *Provider) discoverIPViaSSH(ctx context.Context, vmID uuid.UUID) (string, error) {
	// Try well-known default guest IPs used in Firecracker setups
	candidateIPs := []string{
		"172.16.0.2",  // Default /30 point-to-point
		"192.168.0.2", // Alternative
	}

	for _, ip := range candidateIPs {
		result, err := p.sshExec(ctx, ip, "hostname -I", "", 3)
		if err != nil {
			continue
		}
		if result.ExitCode == 0 {
			discoveredIP := strings.TrimSpace(strings.Fields(result.Stdout)[0])
			if discoveredIP != "" {
				return discoveredIP, nil
			}
		}
	}

	return "", fmt.Errorf("SSH discovery failed for VM %s", vmID)
}

// --- Resource Metrics Collection ---

// CollectMetrics gathers CPU, memory, and I/O metrics from a running VM via SSH.
// It reads /proc pseudo-files inside the guest to collect resource usage.
func (p *Provider) CollectMetrics(ctx context.Context, ip string) (*usecase.VMMetrics, error) {
	if ip == "" {
		return nil, fmt.Errorf("cannot collect metrics: VM has no IP address")
	}

	metrics := &usecase.VMMetrics{}

	// Collect CPU metrics from /proc/stat
	cpuResult, err := p.sshExec(ctx, ip, "cat /proc/stat | head -1", "", 5)
	if err == nil && cpuResult.ExitCode == 0 {
		metrics.CPUTimeMS = parseCPUTime(cpuResult.Stdout)
	}

	// Collect memory metrics from /proc/meminfo
	memResult, err := p.sshExec(ctx, ip, "cat /proc/meminfo", "", 5)
	if err == nil && memResult.ExitCode == 0 {
		peak, avg := parseMemInfo(memResult.Stdout)
		metrics.MemoryPeakMB = peak
		metrics.MemoryAvgMB = avg
	}

	// Collect I/O metrics from /proc/diskstats
	ioResult, err := p.sshExec(ctx, ip, "cat /proc/diskstats", "", 5)
	if err == nil && ioResult.ExitCode == 0 {
		readBytes, writeBytes := parseDiskStats(ioResult.Stdout)
		metrics.IOReadBytes = readBytes
		metrics.IOWriteBytes = writeBytes
	}

	// Collect network metrics from /proc/net/dev
	netResult, err := p.sshExec(ctx, ip, "cat /proc/net/dev", "", 5)
	if err == nil && netResult.ExitCode == 0 {
		rxBytes, txBytes := parseNetDev(netResult.Stdout)
		metrics.NetBytesIn = rxBytes
		metrics.NetBytesOut = txBytes
	}

	return metrics, nil
}

// parseCPUTime parses the first line of /proc/stat (cpu line) and returns total
// CPU time in milliseconds. Format: cpu user nice system idle iowait irq softirq ...
func parseCPUTime(statLine string) int64 {
	line := strings.TrimSpace(statLine)
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var totalJiffies int64
	// Sum user, nice, system (fields 1-3), skip idle (field 4)
	for i := 1; i <= 3 && i < len(fields); i++ {
		val, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil {
			continue
		}
		totalJiffies += val
	}

	// Convert jiffies to milliseconds (assuming 100 Hz = 10ms per jiffy)
	return totalJiffies * 10
}

// parseMemInfo parses /proc/meminfo and returns peak and average memory usage in MB.
// Since we capture a single point in time, peak and average are the same (current usage).
func parseMemInfo(meminfo string) (peakMB, avgMB float64) {
	var totalKB, freeKB, buffersKB, cachedKB int64

	for _, line := range strings.Split(meminfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB = val
		case "MemFree:":
			freeKB = val
		case "Buffers:":
			buffersKB = val
		case "Cached:":
			cachedKB = val
		}
	}

	// Used = Total - Free - Buffers - Cached
	usedKB := totalKB - freeKB - buffersKB - cachedKB
	if usedKB < 0 {
		usedKB = totalKB - freeKB
	}

	usedMB := float64(usedKB) / 1024.0
	return usedMB, usedMB
}

// parseDiskStats parses /proc/diskstats and returns total read and write bytes.
// Format per line: major minor name ... rd_sectors ... wr_sectors ...
// Sectors are typically 512 bytes.
func parseDiskStats(diskstats string) (readBytes, writeBytes int64) {
	for _, line := range strings.Split(diskstats, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		// Field indices (0-based): 5 = sectors_read, 9 = sectors_written
		rdSectors, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			continue
		}
		wrSectors, err := strconv.ParseInt(fields[9], 10, 64)
		if err != nil {
			continue
		}
		readBytes += rdSectors * 512
		writeBytes += wrSectors * 512
	}
	return readBytes, writeBytes
}

// parseNetDev parses /proc/net/dev and returns total receive and transmit bytes
// across all non-loopback interfaces.
func parseNetDev(netdev string) (rxBytes, txBytes int64) {
	for _, line := range strings.Split(netdev, "\n") {
		line = strings.TrimSpace(line)
		// Skip header lines
		if strings.Contains(line, "|") || line == "" {
			continue
		}
		// Format: iface: rx_bytes rx_packets ... tx_bytes tx_packets ...
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		rxBytes += rx
		txBytes += tx
	}
	return rxBytes, txBytes
}

// driveRateLimiter builds a Firecracker rate_limiter block for a drive.
// Values default to nil (unlimited) when both bandwidth and IOPS are 0.
// Bandwidth caps are per-second bytes; IOPS caps are operations/sec.
// See https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/patch-block.md
func driveRateLimiter(bandwidthMBps, iops int64) map[string]any {
	if bandwidthMBps <= 0 && iops <= 0 {
		return nil
	}
	rl := map[string]any{}
	if bandwidthMBps > 0 {
		rl["bandwidth"] = map[string]any{
			"size":        bandwidthMBps * 1024 * 1024, // MBps → bytes
			"refill_time": 1000,                        // ms
		}
	}
	if iops > 0 {
		rl["ops"] = map[string]any{
			"size":        iops,
			"refill_time": 1000,
		}
	}
	return rl
}

// netRateLimiter builds a Firecracker rate_limiter block for a network
// interface. bandwidthMBps caps bytes/sec; pps caps packets/sec.
func netRateLimiter(bandwidthMBps, pps int64) map[string]any {
	if bandwidthMBps <= 0 && pps <= 0 {
		return nil
	}
	rl := map[string]any{}
	if bandwidthMBps > 0 {
		rl["bandwidth"] = map[string]any{
			"size":        bandwidthMBps * 1024 * 1024,
			"refill_time": 1000,
		}
	}
	if pps > 0 {
		rl["ops"] = map[string]any{
			"size":        pps,
			"refill_time": 1000,
		}
	}
	return rl
}
