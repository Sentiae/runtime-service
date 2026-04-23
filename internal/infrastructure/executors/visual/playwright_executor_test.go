package visual

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

type fakeRunner struct {
	lastReq executors.VMExecRequest
	stdout  string
	exit    int
	err     error
}

func (f *fakeRunner) ExecuteInVM(_ context.Context, req executors.VMExecRequest) (*executors.VMExecResult, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &executors.VMExecResult{Stdout: f.stdout, ExitCode: f.exit}, nil
}

type captureUpdater struct {
	status domain.TestRunStatus
	called bool
}

func (c *captureUpdater) UpdateAfterRun(_ context.Context, _ any, s domain.TestRunStatus, _, _ string, _ int64) error {
	c.called = true
	c.status = s
	return nil
}

func TestPlaywrightExecutor_PassesWhenDiffBelowThreshold(t *testing.T) {
	report := `{
		"comparisons": [
			{"name": "homepage", "baseline_hash": "abc", "actual_hash": "abc", "diff_pct": 0.1, "diff_image_ref": ""},
			{"name": "settings", "baseline_hash": "def", "actual_hash": "def", "diff_pct": 0.3, "diff_image_ref": ""}
		],
		"passed": 2,
		"failed": 0
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewPlaywrightExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		TestType:   domain.TestTypeVisualRegression,
		ResultJSON: domain.JSONMap{"max_diff_pct": 1.0, "script": "//test"},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusPassed {
		t.Errorf("expected passed, got %s", updater.status)
	}
	visual, _ := run.ResultJSON["visual"].(domain.JSONMap)
	if visual == nil || visual["diff_pct"] != 0.3 {
		t.Errorf("diff_pct should equal worst-case (0.3); got %+v", visual)
	}
}

func TestPlaywrightExecutor_FailsWhenDiffExceedsThreshold(t *testing.T) {
	report := `{
		"comparisons": [{"name": "homepage", "baseline_hash": "a", "actual_hash": "b", "diff_pct": 5.0, "diff_image_ref": "s3://diff.png"}],
		"passed": 0,
		"failed": 1
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewPlaywrightExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		TestType:   domain.TestTypeVisualRegression,
		ResultJSON: domain.JSONMap{"max_diff_pct": 1.0, "script": "//test"},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusFailed {
		t.Errorf("diff above threshold should fail; got %s", updater.status)
	}
}

func TestPlaywrightExecutor_InlineVsArtifact(t *testing.T) {
	runner := &fakeRunner{stdout: `{"comparisons":[],"passed":0,"failed":0}`, exit: 0}
	exec := NewPlaywrightExecutor(runner, nil)

	// Inline script → heredoc.
	run1 := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New(), TestType: domain.TestTypeVisualRegression,
		ResultJSON: domain.JSONMap{"script": "test('a', async ()=>{})"}}
	_ = exec.DispatchInVM(context.Background(), run1)
	if !strings.Contains(runner.lastReq.Command, "SENTIAE_EOF") {
		t.Errorf("inline path missing heredoc: %s", runner.lastReq.Command)
	}

	// Artifact key → fetch.
	run2 := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New(), TestType: domain.TestTypeVisualRegression,
		ResultJSON: domain.JSONMap{"artifact_key": "playwright/special.js"}}
	_ = exec.DispatchInVM(context.Background(), run2)
	if !strings.Contains(runner.lastReq.Command, "sentiae-artifact fetch") {
		t.Errorf("artifact path missing fetch: %s", runner.lastReq.Command)
	}
}
