// Package executors hosts the per-TestType executor adapters used by
// runtime-service to run perf, visual-regression, accessibility, and
// contract tests inside a Firecracker VM (CS-2 G2.5). Each adapter
// speaks the same TestRunDispatcher contract as the fallback
// FirecrackerTestRunDispatcher (invoke-shell-command-in-VM), but
// parses tool-specific output into TestRun.ResultJSON sub-objects.
//
// Keeping the adapters in a separate package lets us dep-inject a
// narrow VMExecRunner mock in unit tests without pulling in the full
// ExecutionUseCase + Pool wiring. The DI container wraps an
// ExecutionUseCase with an adapter that satisfies VMExecRunner.
package executors

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// VMExecRunner is the narrow surface each executor needs. The real
// implementation wraps runtime-service's ExecutionUseCase.ExecuteSync.
// Tests stub this with a canned-response fake so we can assert on the
// command and inject mock stdout/stderr.
type VMExecRunner interface {
	ExecuteInVM(ctx context.Context, req VMExecRequest) (*VMExecResult, error)
}

// VMExecRequest carries the command + resource shape into the VM.
type VMExecRequest struct {
	OrganizationID uuid.UUID
	Language       domain.Language
	Command        string
	TimeoutSec     int
	EnvVars        map[string]string
}

// VMExecResult is the outcome of running Command inside the VM.
type VMExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// TestRunUpdater is the post-run persistence surface. Keeps executors
// decoupled from the GORM repo concrete type.
type TestRunUpdater interface {
	UpdateAfterRun(ctx context.Context, id any, status domain.TestRunStatus, stdout, stderr string, durationMS int64) error
}
