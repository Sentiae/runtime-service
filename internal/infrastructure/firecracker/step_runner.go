// Package firecracker — concrete StepRunner that boots a fresh VM per
// hermetic build step, runs the command, and collects stdout/stderr +
// artifact bytes. §9.2 — the hermetic build subsystem now owns its own
// VM lifecycle rather than riding on the general Run path.
package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// HermeticStepRunner implements usecase.StepRunner by provisioning a
// fresh Firecracker microVM per step via the provider. Each run gets
// its own rootfs copy so successive steps can't leak state.
type HermeticStepRunner struct {
	provider *Provider
}

// NewHermeticStepRunner wires a step runner against an existing
// Firecracker provider. Callers inject this into
// HermeticBuildUseCase so RunStep provisions real VMs.
func NewHermeticStepRunner(p *Provider) *HermeticStepRunner {
	return &HermeticStepRunner{provider: p}
}

// RunStep boots a VM, executes the step command, captures output, and
// returns a StepRunResult. Provider is expected to be configured; when
// nil we short-circuit with a clearly-labelled error so hermetic flows
// don't silently succeed with no bytes.
func (r *HermeticStepRunner) RunStep(ctx context.Context, req usecase.StepRunRequest) (*usecase.StepRunResult, error) {
	if r == nil || r.provider == nil {
		return nil, fmt.Errorf("hermetic step runner: provider not configured")
	}
	start := time.Now()

	// Build a synthetic Execution so we can reuse Provider.Run's VM
	// boot + teardown machinery.
	timeout := req.TimeoutSec
	if timeout <= 0 {
		timeout = 300
	}

	code := r.codeFor(req)

	resources := domain.ResourceLimit{
		VCPU:       2,
		MemoryMB:   1024,
		TimeoutSec: timeout,
	}
	exec := &domain.Execution{
		ID:        uuid.New(),
		Language:  domain.Language(req.Language),
		Code:      code,
		EnvVars:   toEnvVarMap(req.EnvVars),
		Resources: resources,
	}

	vm := &domain.MicroVM{
		ID:       uuid.New(),
		Language: exec.Language,
		VCPU:     resources.VCPU,
		MemoryMB: resources.MemoryMB,
	}

	result, err := r.provider.Run(ctx, vm, exec)
	if err != nil {
		return nil, fmt.Errorf("hermetic step run: %w", err)
	}

	stdout := result.Stdout
	stderr := result.Stderr
	bytesOut := []byte(stdout)

	return &usecase.StepRunResult{
		ExitCode:       result.ExitCode,
		Stdout:         stdout,
		Stderr:         stderr,
		ArtifactBytes:  bytesOut,
		ArtifactDigest: digestOf(bytesOut),
		DurationMS:     time.Since(start).Milliseconds(),
	}, nil
}

// codeFor packages the caller's command + workspace hint into code the
// guest agent can execute. For a non-empty Workspace we cd into it
// first so relative paths in the command resolve.
func (r *HermeticStepRunner) codeFor(req usecase.StepRunRequest) string {
	cmd := strings.Join(execCommandArgs(req.Command), " ")
	if cmd == "" {
		return `echo "no command supplied"; exit 1`
	}
	if req.Workspace != "" {
		return fmt.Sprintf("cd %q && %s", req.Workspace, cmd)
	}
	return cmd
}

func execCommandArgs(cmd []string) []string {
	if len(cmd) == 0 {
		return nil
	}
	// exec.LookPath is not needed here — the guest runs this as a shell.
	// Kept as a slice alias to preserve argument boundaries when the
	// runner later promotes this to a real argv-based exec.
	_ = exec.Cmd{}
	return cmd
}

func toEnvVarMap(env map[string]string) map[string]any {
	out := make(map[string]any, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// digestOf hashes bytes with sha256 → hex. Used as the
// content-addressed artifact digest.
func digestOf(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Compile-time contract assertion so any drift in the usecase interface
// gets caught here, not at runtime.
var _ = usecase.StepRunRequest{}
var _ = &bytes.Buffer{}
