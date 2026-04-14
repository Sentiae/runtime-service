package container

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// Provider implements VMProvider and ExecutionRunner using Docker containers.
// It is intended for development on macOS where Firecracker (Linux KVM) is unavailable.
type Provider struct {
	cfg config.ContainerConfig
}

// NewProvider creates a new Docker container provider.
func NewProvider(cfg config.ContainerConfig) *Provider {
	return &Provider{cfg: cfg}
}

// imageForLanguage returns the Docker image to use for a given language.
func imageForLanguage(lang domain.Language) string {
	switch lang {
	case domain.LanguagePython:
		return "python:3.12-slim"
	case domain.LanguageJavaScript, domain.LanguageTypeScript:
		return "node:22-slim"
	case domain.LanguageGo:
		return "golang:1.22-alpine"
	case domain.LanguageRust:
		return "rust:1.78-slim"
	case domain.LanguageBash:
		return "alpine:3.19"
	case domain.LanguageC, domain.LanguageCPP:
		return "gcc:14"
	default:
		return "alpine:3.19"
	}
}

// Boot creates and starts a Docker container for the given language.
// The container ID is stored in VMBootResult and should be placed into MicroVM.SocketPath.
func (p *Provider) Boot(ctx context.Context, bootCfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	start := time.Now()

	image := imageForLanguage(bootCfg.Language)
	containerName := "sentiae-vm-" + bootCfg.VMID.String()

	args := []string{
		"run", "-d",
		"--name", containerName,
	}

	// Apply resource limits
	if bootCfg.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", bootCfg.MemoryMB))
	}
	if bootCfg.VCPU > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", bootCfg.VCPU))
	}

	// Network mode mapping
	switch bootCfg.NetworkMode {
	case domain.NetworkModeIsolated:
		args = append(args, "--network", "none")
	case domain.NetworkModeHost:
		args = append(args, "--network", "host")
	default:
		// bridged or unset: use default Docker networking
	}

	args = append(args, image, "sleep", "infinity")

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker run failed: %s: %w", stderr.String(), err)
	}

	containerID := strings.TrimSpace(stdout.String())
	bootTimeMS := time.Since(start).Milliseconds()

	log.Printf("Docker container started: name=%s, id=%s, image=%s, boot=%dms",
		containerName, containerID[:12], image, bootTimeMS)

	return &usecase.VMBootResult{
		PID:        0,
		IPAddress:  "",
		SocketPath: containerName,
		BootTimeMS: bootTimeMS,
	}, nil
}

// Terminate forcefully removes a Docker container.
// socketPath is expected to hold the container name (sentiae-vm-<vmID>).
func (p *Provider) Terminate(ctx context.Context, socketPath string, _ int) error {
	containerName := socketPath
	if containerName == "" {
		return nil
	}

	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If the container is already gone, that is fine.
		if strings.Contains(stderr.String(), "No such container") {
			return nil
		}
		return fmt.Errorf("docker rm failed: %s: %w", stderr.String(), err)
	}

	log.Printf("Docker container terminated: %s", containerName)
	return nil
}

// Pause is a no-op for Docker containers in dev mode.
func (p *Provider) Pause(_ context.Context, _ string) error {
	return nil
}

// Resume is a no-op for Docker containers in dev mode.
func (p *Provider) Resume(_ context.Context, _ string) error {
	return nil
}

// CreateSnapshot is a no-op for Docker containers in dev mode.
func (p *Provider) CreateSnapshot(_ context.Context, _ string, snapshotID uuid.UUID) (*usecase.SnapshotResult, error) {
	return &usecase.SnapshotResult{
		MemoryFilePath: "",
		StateFilePath:  "",
		SizeBytes:      0,
	}, nil
}

// RestoreSnapshot is a no-op for Docker containers in dev mode.
func (p *Provider) RestoreSnapshot(_ context.Context, _, _, _ string) error {
	return nil
}

// CollectMetrics is a no-op for Docker containers in dev mode.
func (p *Provider) CollectMetrics(_ context.Context, _ string) (*usecase.VMMetrics, error) {
	return &usecase.VMMetrics{}, nil
}

// DeleteSnapshotFiles is a no-op for Docker containers in dev mode.
func (p *Provider) DeleteSnapshotFiles(_, _ string) error {
	return nil
}

// Run executes code inside a Docker container.
// The container name is read from vm.SocketPath.
func (p *Provider) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	start := time.Now()

	containerName := vm.SocketPath
	if containerName == "" {
		return nil, fmt.Errorf("no container name in VM %s SocketPath", vm.ID)
	}

	// Determine timeout
	timeoutSec := execution.Resources.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = int(p.cfg.DefaultTimeout.Seconds())
		if timeoutSec <= 0 {
			timeoutSec = 30
		}
	}
	maxTimeoutSec := int(p.cfg.MaxTimeout.Seconds())
	if maxTimeoutSec > 0 && timeoutSec > maxTimeoutSec {
		timeoutSec = maxTimeoutSec
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// Determine file extension and commands
	ext, compileCmd, runCmd := languageCommands(execution.Language)
	filename := "code" + ext

	// Write code to a temp file on the host
	tmpDir, err := os.MkdirTemp("", "sentiae-exec-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	hostPath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(hostPath, []byte(execution.Code), 0644); err != nil {
		return nil, fmt.Errorf("write code file: %w", err)
	}

	// Copy the file into the container
	remotePath := "/tmp/" + filename
	cpCmd := exec.CommandContext(execCtx, "docker", "cp", hostPath, containerName+":"+remotePath)
	var cpStderr bytes.Buffer
	cpCmd.Stderr = &cpStderr
	if err := cpCmd.Run(); err != nil {
		return nil, fmt.Errorf("docker cp failed: %s: %w", cpStderr.String(), err)
	}

	// If stdin is provided, write it to a file too
	if execution.Stdin != "" {
		stdinHost := filepath.Join(tmpDir, "stdin.txt")
		if err := os.WriteFile(stdinHost, []byte(execution.Stdin), 0644); err != nil {
			return nil, fmt.Errorf("write stdin file: %w", err)
		}
		stdinCp := exec.CommandContext(execCtx, "docker", "cp", stdinHost, containerName+":/tmp/stdin.txt")
		var stdinCpErr bytes.Buffer
		stdinCp.Stderr = &stdinCpErr
		if err := stdinCp.Run(); err != nil {
			return nil, fmt.Errorf("docker cp stdin failed: %s: %w", stdinCpErr.String(), err)
		}
	}

	// Compile if needed
	var compileTimeMS *int64
	if compileCmd != "" {
		compileStart := time.Now()
		compResult := p.dockerExec(execCtx, containerName, compileCmd, "")
		ct := time.Since(compileStart).Milliseconds()
		compileTimeMS = &ct

		if compResult.err != nil {
			if execCtx.Err() == context.DeadlineExceeded {
				return &usecase.RunResult{
					ExitCode:      124,
					Stderr:        "compilation timed out",
					CompileTimeMS: compileTimeMS,
					ExecTimeMS:    time.Since(start).Milliseconds(),
				}, nil
			}
			return nil, fmt.Errorf("compile failed: %w", compResult.err)
		}

		if compResult.exitCode != 0 {
			return &usecase.RunResult{
				ExitCode:      compResult.exitCode,
				Stdout:        "",
				Stderr:        compResult.stderr,
				CompileTimeMS: compileTimeMS,
				ExecTimeMS:    time.Since(start).Milliseconds(),
			}, nil
		}
	}

	// Run the code
	stdinRedirect := ""
	if execution.Stdin != "" {
		stdinRedirect = " < /tmp/stdin.txt"
	}
	result := p.dockerExec(execCtx, containerName, runCmd+stdinRedirect, "")

	if result.err != nil && execCtx.Err() == context.DeadlineExceeded {
		return &usecase.RunResult{
			ExitCode:      124,
			Stdout:        result.stdout,
			Stderr:        "execution timed out",
			CompileTimeMS: compileTimeMS,
			ExecTimeMS:    time.Since(start).Milliseconds(),
		}, nil
	}

	execTimeMS := time.Since(start).Milliseconds()

	return &usecase.RunResult{
		ExitCode:      result.exitCode,
		Stdout:        result.stdout,
		Stderr:        result.stderr,
		CompileTimeMS: compileTimeMS,
		ExecTimeMS:    execTimeMS,
	}, nil
}

// execResult holds the output of a docker exec invocation.
type execResult struct {
	exitCode int
	stdout   string
	stderr   string
	err      error
}

// dockerExec runs a command inside a container via docker exec.
func (p *Provider) dockerExec(ctx context.Context, containerName, command, stdin string) execResult {
	args := []string{"exec"}
	if stdin != "" {
		args = append(args, "-i")
	}
	args = append(args, containerName, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "docker", args...)

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
			err = nil // non-zero exit is not an error in our context
		}
	}

	return execResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		err:      err,
	}
}

// languageCommands returns the file extension, compile command, and run command
// for a given language.
func languageCommands(lang domain.Language) (ext, compileCmd, runCmd string) {
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

// containerName derives the Docker container name from a VM ID.
func containerName(vmID uuid.UUID) string {
	return "sentiae-vm-" + vmID.String()
}
