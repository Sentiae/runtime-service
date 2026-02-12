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
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// Provider implements the VMProvider and ExecutionRunner interfaces
// using Firecracker microVMs for secure code execution
type Provider struct {
	cfg config.FirecrackerConfig
}

// NewProvider creates a new Firecracker provider
func NewProvider(cfg config.FirecrackerConfig) *Provider {
	return &Provider{cfg: cfg}
}

// Boot starts a new Firecracker microVM
func (p *Provider) Boot(ctx context.Context, bootCfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	start := time.Now()

	socketPath := p.socketPath(bootCfg.VMID)
	kernelPath := p.cfg.KernelPath
	rootfsPath := p.rootfsForLanguage(bootCfg.Language)

	// Ensure socket directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return nil, fmt.Errorf("failed to create socket dir: %w", err)
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
		return nil, fmt.Errorf("failed to start firecracker: %w", err)
	}

	pid := cmd.Process.Pid

	// Wait for the API socket to be available
	if err := p.waitForSocket(ctx, socketPath); err != nil {
		// Kill the process if socket doesn't come up
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	// Configure the VM via Firecracker API
	if err := p.configureVM(ctx, socketPath, kernelPath, rootfsPath, bootCfg); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("failed to configure VM: %w", err)
	}

	// Start the VM instance
	if err := p.startInstance(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("failed to start VM instance: %w", err)
	}

	bootTimeMS := time.Since(start).Milliseconds()
	log.Printf("Firecracker VM booted: pid=%d, socket=%s, boot=%dms", pid, socketPath, bootTimeMS)

	// Discover the VM's IP address
	ipAddress := ""
	if bootCfg.NetworkMode == domain.NetworkModeBridged {
		ip, err := p.discoverIPAddress(ctx, socketPath, bootCfg.VMID)
		if err != nil {
			log.Printf("Warning: failed to discover IP for VM %s: %v", bootCfg.VMID, err)
		} else {
			ipAddress = ip
		}
	}

	return &usecase.VMBootResult{
		PID:        pid,
		IPAddress:  ipAddress,
		BootTimeMS: bootTimeMS,
	}, nil
}

// Terminate kills a running microVM
func (p *Provider) Terminate(ctx context.Context, socketPath string, pid int) error {
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
		// Already dead
		return nil
	}

	// Wait briefly for graceful shutdown, then force kill
	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return process.Kill()
	}
}

// Pause pauses a running microVM (for snapshot creation)
func (p *Provider) Pause(ctx context.Context, socketPath string) error {
	return p.sendAction(ctx, socketPath, "Pause")
}

// Resume resumes a paused microVM
func (p *Provider) Resume(ctx context.Context, socketPath string) error {
	return p.sendAction(ctx, socketPath, "Resume")
}

// Run executes code inside a microVM by writing the source file and running it
// via the guest agent (SSH or vsock). The guest rootfs image must include the
// language runtime and an SSH server listening on port 22.
func (p *Provider) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	start := time.Now()

	if vm.IPAddress == "" {
		return nil, fmt.Errorf("VM %s has no IP address for SSH access", vm.ID)
	}

	// Determine file extension and run command based on language
	ext, compileCmd, runCmd := p.languageCommands(execution.Language)
	filename := "code" + ext
	remotePath := "/tmp/" + filename

	// 1. Copy code into the VM via SSH
	if err := p.sshWriteFile(ctx, vm.IPAddress, remotePath, execution.Code); err != nil {
		return nil, fmt.Errorf("copy code to VM: %w", err)
	}

	// 2. Compile if needed
	var compileTimeMS *int64
	if compileCmd != "" {
		compileStart := time.Now()
		compileResult, err := p.sshExec(ctx, vm.IPAddress, compileCmd, "", execution.Resources.TimeoutSec)
		if err != nil {
			return nil, fmt.Errorf("compile in VM: %w", err)
		}
		ct := time.Since(compileStart).Milliseconds()
		compileTimeMS = &ct

		if compileResult.ExitCode != 0 {
			return &usecase.RunResult{
				ExitCode:      compileResult.ExitCode,
				Stdout:        "",
				Stderr:        compileResult.Stderr,
				CompileTimeMS: compileTimeMS,
				ExecTimeMS:    time.Since(start).Milliseconds(),
			}, nil
		}
	}

	// 3. Run the code
	result, err := p.sshExec(ctx, vm.IPAddress, runCmd, execution.Stdin, execution.Resources.TimeoutSec)
	if err != nil {
		return nil, fmt.Errorf("execute in VM: %w", err)
	}

	execTimeMS := time.Since(start).Milliseconds()

	return &usecase.RunResult{
		ExitCode:      result.ExitCode,
		Stdout:        result.Stdout,
		Stderr:        result.Stderr,
		CompileTimeMS: compileTimeMS,
		ExecTimeMS:    execTimeMS,
	}, nil
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

// configureVM sends configuration to the Firecracker API via Unix socket HTTP.
func (p *Provider) configureVM(ctx context.Context, socketPath, kernelPath, rootfsPath string, cfg usecase.VMBootConfig) error {
	client := p.unixHTTPClient(socketPath)

	// 1. Set machine configuration (vCPU + memory)
	machineConfig := map[string]interface{}{
		"vcpu_count":  cfg.VCPU,
		"mem_size_mib": cfg.MemoryMB,
	}
	if err := p.apiPut(ctx, client, "/machine-config", machineConfig); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}

	// 2. Set boot source (kernel + boot args)
	bootSource := map[string]interface{}{
		"kernel_image_path": kernelPath,
		"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off",
	}
	if err := p.apiPut(ctx, client, "/boot-source", bootSource); err != nil {
		return fmt.Errorf("configure boot source: %w", err)
	}

	// 3. Attach rootfs drive
	rootfsDrive := map[string]interface{}{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if err := p.apiPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return fmt.Errorf("configure rootfs drive: %w", err)
	}

	// 4. Configure network if bridged mode
	if cfg.NetworkMode == domain.NetworkModeBridged {
		netIface := map[string]interface{}{
			"iface_id":    "eth0",
			"guest_mac":   "AA:FC:00:00:00:01",
			"host_dev_name": "tap0",
		}
		if err := p.apiPut(ctx, client, "/network-interfaces/eth0", netIface); err != nil {
			return fmt.Errorf("configure network: %w", err)
		}
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
func (p *Provider) apiPut(ctx context.Context, client *http.Client, path string, payload interface{}) error {
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
func (p *Provider) apiGet(ctx context.Context, client *http.Client, path string, result interface{}) error {
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
	snapshotReq := map[string]interface{}{
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
	loadReq := map[string]interface{}{
		"snapshot_path":      statePath,
		"mem_backend": map[string]interface{}{
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
