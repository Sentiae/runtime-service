package a11y

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

type fakeRunner struct {
	last   executors.VMExecRequest
	stdout string
	exit   int
	err    error
}

func (f *fakeRunner) ExecuteInVM(_ context.Context, r executors.VMExecRequest) (*executors.VMExecResult, error) {
	f.last = r
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

func TestAxeExecutor_RequiresURL(t *testing.T) {
	runner := &fakeRunner{}
	updater := &captureUpdater{}
	exec := NewAxeExecutor(runner, updater)

	run := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New()}
	if err := exec.DispatchInVM(context.Background(), run); err == nil {
		t.Fatal("expected error without URL")
	}
	if updater.status != domain.TestRunStatusError {
		t.Errorf("expected error status, got %s", updater.status)
	}
}

func TestAxeExecutor_CriticalViolationsFailRun(t *testing.T) {
	report := `{
		"violations": [
			{"id": "color-contrast", "impact": "serious", "description": "a", "help": "", "helpUrl": "", "nodes": [{"target":["h1"],"html":"<h1>"}]},
			{"id": "img-alt", "impact": "critical", "description": "b", "help": "", "helpUrl": "", "nodes": []},
			{"id": "nested-list", "impact": "minor", "description": "c", "help": "", "helpUrl": "", "nodes": []}
		],
		"passes": [{"id":"landmark-one-main"}, {"id":"region"}],
		"incomplete": [],
		"inapplicable": [{"id":"skip-link"}]
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewAxeExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		TestType:   domain.TestTypeAccessibility,
		ResultJSON: domain.JSONMap{"url": "https://example.com"},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusFailed {
		t.Errorf("critical+serious violations should fail; got %s", updater.status)
	}
	a11y, _ := run.ResultJSON["a11y"].(domain.JSONMap)
	if a11y == nil {
		t.Fatal("a11y summary not written")
	}
	if a11y["blocking_count"] != 2 {
		t.Errorf("blocking_count should be 2 (critical+serious); got %v", a11y["blocking_count"])
	}
	if a11y["pass_count"] != 2 {
		t.Errorf("pass_count should be 2; got %v", a11y["pass_count"])
	}
	if a11y["fail_count"] != 3 {
		t.Errorf("fail_count should be 3; got %v", a11y["fail_count"])
	}
	if !strings.Contains(runner.last.Command, "https://example.com") {
		t.Errorf("URL should be in command: %s", runner.last.Command)
	}
}

func TestAxeExecutor_MinorOnlyPasses(t *testing.T) {
	report := `{
		"violations": [{"id": "nested-list", "impact": "minor", "description": "c", "help": "", "helpUrl": "", "nodes": []}],
		"passes": [],
		"incomplete": [],
		"inapplicable": []
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewAxeExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		ResultJSON: domain.JSONMap{"url": "https://example.com"},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusPassed {
		t.Errorf("minor-only should pass; got %s", updater.status)
	}
}

func TestAxeExecutor_CustomFailOnImpacts(t *testing.T) {
	report := `{
		"violations": [{"id": "nested-list", "impact": "minor", "description": "c", "help": "", "helpUrl": "", "nodes": []}],
		"passes": [],
		"incomplete": [],
		"inapplicable": []
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewAxeExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		ResultJSON: domain.JSONMap{
			"url":             "https://example.com",
			"fail_on_impacts": []any{"minor"},
		},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusFailed {
		t.Errorf("custom fail_on_impacts should include minor; got %s", updater.status)
	}
}
