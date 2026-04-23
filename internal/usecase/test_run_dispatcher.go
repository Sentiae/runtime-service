// Package usecase — concrete TestRunDispatcher that bridges test runs
// to the general-purpose ExecutionUseCase. §8.3 — closes the "dispatch
// stops at profile resolution" gap by actually booting a VM and running
// the resolved command.
//
// Flow: handler resolves TestRunnerProfile → this dispatcher packages
// the profile.Command + TestRun.Language into a CreateExecutionInput →
// ExecuteSync boots a VM and returns results → dispatcher writes
// stdout/stderr/status back to the TestRun row.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// TestRunUpdater is what the dispatcher needs to mutate the TestRun
// row after execution. Implemented by the postgres repo.
type TestRunUpdater interface {
	UpdateAfterRun(ctx context.Context, id any, status domain.TestRunStatus, stdout, stderr string, durationMS int64) error
}

// FirecrackerTestRunDispatcher is the real TestRunDispatcher impl.
// Depends on ExecutionUseCase + a TestRunUpdater. Handler calls
// DispatchInVM in a goroutine; the dispatcher handles VM lifecycle
// through ExecutionUseCase.ExecuteSync.
type FirecrackerTestRunDispatcher struct {
	execUC  ExecutionUseCase
	updater TestRunUpdater
}

// NewFirecrackerTestRunDispatcher wires the dispatcher.
func NewFirecrackerTestRunDispatcher(execUC ExecutionUseCase, updater TestRunUpdater) *FirecrackerTestRunDispatcher {
	return &FirecrackerTestRunDispatcher{execUC: execUC, updater: updater}
}

// DispatchInVM implements the TestRunDispatcher contract (declared on
// the HTTP handler side as an interface to avoid import cycles).
func (d *FirecrackerTestRunDispatcher) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	if d == nil || d.execUC == nil {
		return fmt.Errorf("test dispatcher not configured")
	}

	// Translate the profile command into code the ExecutionRunner can
	// execute. profile.Command is a single shell string (e.g. "pytest -q").
	code := profile.Command
	if code == "" {
		code = fmt.Sprintf("echo \"test runner for %s/%s not configured\"; exit 1", run.Language, run.TestType)
	}

	input := CreateExecutionInput{
		OrganizationID: run.OrganizationID,
		Language:       run.Language,
		Code:           code,
	}
	if run.TimeoutMS > 0 {
		input.Resources = &domain.ResourceLimit{
			TimeoutSec: int(run.TimeoutMS / 1000),
		}
	}

	start := time.Now()
	exec, err := d.execUC.ExecuteSync(ctx, input)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if d.updater != nil {
			_ = d.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), durationMS)
		}
		return err
	}

	status := domain.TestRunStatusPassed
	if exec.Status == domain.ExecutionStatusFailed || (exec.ExitCode != nil && *exec.ExitCode != 0) {
		status = domain.TestRunStatusFailed
	}
	if d.updater != nil {
		_ = d.updater.UpdateAfterRun(ctx, run.ID, status, exec.Stdout, exec.Stderr, durationMS)
	}
	return nil
}
