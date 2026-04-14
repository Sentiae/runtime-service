package simulated

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Provider implements VMProvider and ExecutionRunner without any external
// dependencies (no Docker, no Firecracker). It simulates code execution by
// echoing the submitted code back and returning a zero exit code. This is
// useful for development, CI, and environments where container runtimes are
// unavailable.
type Provider struct{}

// NewProvider creates a new simulated provider.
func NewProvider() *Provider {
	return &Provider{}
}

// ---------------------------------------------------------------------------
// VMProvider implementation
// ---------------------------------------------------------------------------

// Boot simulates booting a microVM. No real process is created.
func (p *Provider) Boot(_ context.Context, cfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	bootTime := int64(5) // simulate a 5ms boot

	log.Printf("[SIMULATED] VM booted: id=%s, lang=%s, vcpu=%d, mem=%dMB",
		cfg.VMID, cfg.Language, cfg.VCPU, cfg.MemoryMB)

	return &usecase.VMBootResult{
		PID:        0,
		IPAddress:  "",
		SocketPath: "simulated-vm-" + cfg.VMID.String(),
		BootTimeMS: bootTime,
	}, nil
}

// Terminate is a no-op for simulated VMs.
func (p *Provider) Terminate(_ context.Context, socketPath string, _ int) error {
	log.Printf("[SIMULATED] VM terminated: %s", socketPath)
	return nil
}

// Pause is a no-op.
func (p *Provider) Pause(_ context.Context, _ string) error { return nil }

// Resume is a no-op.
func (p *Provider) Resume(_ context.Context, _ string) error { return nil }

// CreateSnapshot is a no-op.
func (p *Provider) CreateSnapshot(_ context.Context, _ string, _ uuid.UUID) (*usecase.SnapshotResult, error) {
	return &usecase.SnapshotResult{}, nil
}

// RestoreSnapshot is a no-op.
func (p *Provider) RestoreSnapshot(_ context.Context, _, _, _ string) error { return nil }

// CollectMetrics returns zero metrics.
func (p *Provider) CollectMetrics(_ context.Context, _ string) (*usecase.VMMetrics, error) {
	return &usecase.VMMetrics{}, nil
}

// DeleteSnapshotFiles is a no-op.
func (p *Provider) DeleteSnapshotFiles(_, _ string) error { return nil }

// ---------------------------------------------------------------------------
// ExecutionRunner implementation
// ---------------------------------------------------------------------------

// Run simulates executing code. It sleeps briefly to mimic real execution,
// then returns the code source as stdout with exit code 0.
func (p *Provider) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	start := time.Now()

	// Respect context cancellation / timeout
	select {
	case <-ctx.Done():
		return &usecase.RunResult{
			ExitCode:   124,
			Stderr:     "execution timed out (simulated)",
			ExecTimeMS: time.Since(start).Milliseconds(),
		}, nil
	case <-time.After(50 * time.Millisecond): // simulate 50ms execution
	}

	stdout := simulateOutput(execution.Language, execution.Code)
	execTimeMS := time.Since(start).Milliseconds()

	log.Printf("[SIMULATED] Execution complete: id=%s, lang=%s, exec_time=%dms",
		execution.ID, execution.Language, execTimeMS)

	return &usecase.RunResult{
		ExitCode:   0,
		Stdout:     stdout,
		Stderr:     "",
		ExecTimeMS: execTimeMS,
	}, nil
}

// simulateOutput generates a plausible stdout for the given language and code.
func simulateOutput(lang domain.Language, code string) string {
	// Try to extract a print/echo statement value for a more realistic output
	var lines []string
	for _, line := range strings.Split(code, "\n") {
		trimmed := strings.TrimSpace(line)
		switch lang {
		case domain.LanguagePython:
			if strings.HasPrefix(trimmed, "print(") {
				lines = append(lines, extractArg(trimmed, "print("))
			}
		case domain.LanguageJavaScript, domain.LanguageTypeScript:
			if strings.HasPrefix(trimmed, "console.log(") {
				lines = append(lines, extractArg(trimmed, "console.log("))
			}
		case domain.LanguageGo:
			if strings.HasPrefix(trimmed, "fmt.Println(") {
				lines = append(lines, extractArg(trimmed, "fmt.Println("))
			}
		case domain.LanguageBash:
			if strings.HasPrefix(trimmed, "echo ") {
				lines = append(lines, strings.TrimPrefix(trimmed, "echo "))
			}
		}
	}

	if len(lines) > 0 {
		return strings.Join(lines, "\n") + "\n"
	}

	return fmt.Sprintf("[simulated] %s code executed successfully\n", lang)
}

// extractArg pulls the argument from a function call like print("hello")
func extractArg(line, prefix string) string {
	arg := strings.TrimPrefix(line, prefix)
	arg = strings.TrimSuffix(arg, ")")
	arg = strings.TrimSuffix(arg, ";")
	arg = strings.TrimSpace(arg)
	// Remove surrounding quotes
	if len(arg) >= 2 {
		if (arg[0] == '"' && arg[len(arg)-1] == '"') || (arg[0] == '\'' && arg[len(arg)-1] == '\'') {
			arg = arg[1 : len(arg)-1]
		}
	}
	return arg
}
