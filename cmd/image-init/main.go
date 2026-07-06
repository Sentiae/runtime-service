// Command image-init is PID 1 inside a Firecracker microVM booted from a
// compiled OCI image (runtime-fleet CP3, the I1 "boot the image" model). It is
// copied into the materialized rootfs as /sentiae/init and named via the kernel
// boot arg `init=/sentiae/init`.
//
// It reads /sentiae/runtime.json (written by the OCI materializer), mounts the
// standard pseudo-filesystems, and then either:
//   - mode "test": runs the workload once, captures stdout/stderr/exit_code to
//     /sentiae/out/*, syncs, and powers the VM off; or
//   - mode "resident": starts the workload as a child, reaps zombies as PID 1,
//     and powers off when the child exits.
//
// Networking is already configured by the kernel `ip=` boot arg — this init does
// NOT touch eth0. It brings up loopback best-effort so resident apps binding to
// 127.0.0.1 work; failures are ignored (arbitrary images may lack the tooling).
//
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/image-init
//
//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	runtimeSpecPath = "/sentiae/runtime.json"
	outDir          = "/sentiae/out"
)

// runtimeSpec mirrors the JSON the OCI materializer writes.
type runtimeSpec struct {
	Entrypoint  []string `json:"entrypoint"`
	Env         []string `json:"env"`
	WorkDir     string   `json:"workdir"`
	Mode        string   `json:"mode"`
	TestCommand string   `json:"test_command"`
	Port        int      `json:"port"`
}

func main() {
	mountPseudoFS()
	bringUpLoopback() // best-effort; eth0 comes from the kernel ip= arg

	spec, err := loadSpec()
	if err != nil {
		// Cannot proceed without a spec — record what we can and power off so
		// the host's timeout doesn't have to fire.
		_ = os.MkdirAll(outDir, 0o755)
		_ = os.WriteFile(filepath.Join(outDir, "stderr.txt"), []byte("image-init: "+err.Error()), 0o644)
		_ = os.WriteFile(filepath.Join(outDir, "exit_code.txt"), []byte("127"), 0o644)
		syncAndPowerOff()
		return
	}

	switch spec.Mode {
	case "resident":
		runResident(spec)
	default: // "test" and anything else default to single-shot
		runTest(spec)
	}
}

// mountPseudoFS mounts /proc, /sys, /dev, /tmp. EBUSY/EEXIST are ignored so a
// base image that already mounts some of these does not abort boot.
func mountPseudoFS() {
	type m struct{ source, target, fstype string }
	for _, mp := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
	} {
		_ = os.MkdirAll(mp.target, 0o755)
		if err := syscall.Mount(mp.source, mp.target, mp.fstype, 0, ""); err != nil {
			if err == syscall.EBUSY || err == syscall.EEXIST {
				continue
			}
			// Non-fatal: log to console and carry on.
			fmt.Fprintf(os.Stderr, "image-init: mount %s: %v\n", mp.target, err)
		}
	}
}

// bringUpLoopback sets IFF_UP on lo via ioctl (netlink-free). Best-effort.
func bringUpLoopback() {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)

	// struct ifreq: name[16] then a union whose first field (flags) is a short.
	var ifr [40]byte
	copy(ifr[:15], "lo")

	const siocGIFFLAGS = 0x8913
	const siocSIFFLAGS = 0x8914
	const iffUp = 0x1

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocGIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		return
	}
	flags := uint16(ifr[16]) | uint16(ifr[17])<<8
	flags |= iffUp
	ifr[16] = byte(flags)
	ifr[17] = byte(flags >> 8)
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocSIFFLAGS, uintptr(unsafe.Pointer(&ifr[0])))
}

func loadSpec() (*runtimeSpec, error) {
	body, err := os.ReadFile(runtimeSpecPath)
	if err != nil {
		return nil, fmt.Errorf("read runtime spec: %w", err)
	}
	var spec runtimeSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil, fmt.Errorf("decode runtime spec: %w", err)
	}
	return &spec, nil
}

// runTest runs the workload once and writes stdout/stderr/exit_code to /sentiae/out.
func runTest(spec *runtimeSpec) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "image-init: mkdir out: %v\n", err)
	}

	writeErr := func(msg string, code int) {
		_ = os.WriteFile(filepath.Join(outDir, "stderr.txt"), []byte(msg), 0o644)
		_ = os.WriteFile(filepath.Join(outDir, "exit_code.txt"), []byte(fmt.Sprintf("%d", code)), 0o644)
		syncAndPowerOff()
	}

	var cmd *exec.Cmd
	if spec.TestCommand != "" {
		if _, err := os.Stat("/bin/sh"); err != nil {
			writeErr("image-init: /bin/sh not found in image; cannot run test_command", 127)
			return
		}
		cmd = exec.Command("/bin/sh", "-c", spec.TestCommand)
	} else {
		if len(spec.Entrypoint) == 0 {
			writeErr("image-init: image has no entrypoint and no test_command", 127)
			return
		}
		cmd = exec.Command(spec.Entrypoint[0], spec.Entrypoint[1:]...)
	}

	applyEnv(cmd, spec)

	stdoutF, _ := os.OpenFile(filepath.Join(outDir, "stdout.txt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	stderrF, _ := os.OpenFile(filepath.Join(outDir, "stderr.txt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	cmd.Stdout = stdoutF
	cmd.Stderr = stderrF

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			if stderrF != nil {
				fmt.Fprintf(stderrF, "\nimage-init: exec: %v\n", err)
			}
			exitCode = 126
		}
	}
	if stdoutF != nil {
		_ = stdoutF.Close()
	}
	if stderrF != nil {
		_ = stderrF.Close()
	}
	_ = os.WriteFile(filepath.Join(outDir, "exit_code.txt"), []byte(fmt.Sprintf("%d", exitCode)), 0o644)

	syncAndPowerOff()
}

// runResident starts the workload as a child and stays PID 1, reaping zombies
// until the child exits, then powers off.
func runResident(spec *runtimeSpec) {
	if len(spec.Entrypoint) == 0 {
		fmt.Fprintln(os.Stderr, "image-init: resident image has no entrypoint")
		syncAndPowerOff()
		return
	}

	console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		console = os.Stdout
	}

	cmd := exec.Command(spec.Entrypoint[0], spec.Entrypoint[1:]...)
	applyEnv(cmd, spec)
	cmd.Stdout = console
	cmd.Stderr = console

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(console, "image-init: start workload: %v\n", err)
		syncAndPowerOff()
		return
	}
	childPID := cmd.Process.Pid

	// PID 1 reaper loop: wait for ANY child so grandchildren zombies are reaped;
	// power off once our direct child exits.
	for {
		var ws syscall.WaitStatus
		wpid, werr := syscall.Wait4(-1, &ws, 0, nil)
		if werr == syscall.EINTR {
			continue
		}
		if werr != nil {
			fmt.Fprintf(console, "image-init: wait4: %v — powering off\n", werr)
			break
		}
		if wpid == childPID {
			fmt.Fprintf(console, "image-init: workload pid %d exited (status=%d signaled=%v signal=%v) — powering off\n",
				wpid, ws.ExitStatus(), ws.Signaled(), ws.Signal())
			break
		}
		fmt.Fprintf(console, "image-init: reaped orphan pid %d (status=%d)\n", wpid, ws.ExitStatus())
	}
	syncAndPowerOff()
}

// applyEnv sets the child's environment and working directory from the spec.
func applyEnv(cmd *exec.Cmd, spec *runtimeSpec) {
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
}

// syncAndPowerOff flushes disk buffers and halts the VM. RESTART maps to a
// guest reset which, with `reboot=k` on the kernel command line, makes the
// Firecracker process exit — the same power-off mechanism the exec path uses.
func syncAndPowerOff() {
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	// If Reboot returns (unprivileged), block forever so PID 1 never exits
	// (an exiting PID 1 panics the kernel).
	select {}
}
