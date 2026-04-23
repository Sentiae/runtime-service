package perf

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

// --- fakes ----------------------------------------------------------

type fakeRunner struct {
	lastReq executors.VMExecRequest
	stdout  string
	stderr  string
	exit    int
	err     error
}

func (f *fakeRunner) ExecuteInVM(_ context.Context, req executors.VMExecRequest) (*executors.VMExecResult, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &executors.VMExecResult{
		Stdout:   f.stdout,
		Stderr:   f.stderr,
		ExitCode: f.exit,
	}, nil
}

type captureUpdater struct {
	gotStatus domain.TestRunStatus
	called    bool
}

func (c *captureUpdater) UpdateAfterRun(_ context.Context, _ any, status domain.TestRunStatus, _, _ string, _ int64) error {
	c.called = true
	c.gotStatus = status
	return nil
}

// --- tests ----------------------------------------------------------

func TestK6Executor_ParsesSummaryAndMarksPassed(t *testing.T) {
	// A minimal k6-style summary envelope.
	summary := `{
  "metrics": {
    "http_req_duration": {"values": {"p(50)": 120.5, "p(95)": 400.0, "p(99)": 900.0}},
    "http_reqs": {"values": {"rate": 42.0}},
    "http_req_failed": {"values": {"rate": 0.01}},
    "vus_max": {"values": {"value": 25}},
    "iterations": {"values": {"count": 1000}}
  },
  "state": {"testRunDurationMs": 30000}
}`
	runner := &fakeRunner{stdout: summary, exit: 0}
	updater := &captureUpdater{}
	exec := NewK6Executor(runner, updater)

	run := &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: uuid.New(),
		TestType:       domain.TestTypePerf,
		Status:         domain.TestRunStatusRunning,
		ResultJSON:     domain.JSONMap{"script": "export default function() {}", "duration_sec": float64(15), "vus": float64(5)},
	}

	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}

	// Command should carry the inline script heredoc + k6 invocation.
	if !strings.Contains(runner.lastReq.Command, "SENTIAE_EOF") {
		t.Errorf("command should inline heredoc; got: %s", runner.lastReq.Command)
	}
	if !strings.Contains(runner.lastReq.Command, "--vus=5") {
		t.Errorf("command should carry --vus flag; got: %s", runner.lastReq.Command)
	}
	if !strings.Contains(runner.lastReq.Command, "--duration=15s") {
		t.Errorf("command should carry --duration; got: %s", runner.lastReq.Command)
	}

	perf, ok := run.ResultJSON["perf"].(domain.JSONMap)
	if !ok {
		t.Fatalf("perf summary not written: %+v", run.ResultJSON)
	}
	if p99, _ := perf["p99_ms"].(float64); p99 != 900.0 {
		t.Errorf("p99_ms parsed wrong: %v", perf["p99_ms"])
	}
	if rps, _ := perf["rps"].(float64); rps != 42.0 {
		t.Errorf("rps parsed wrong: %v", perf["rps"])
	}
	errPct, _ := perf["error_rate_pct"].(float64)
	if errPct < 0.99 || errPct > 1.01 {
		t.Errorf("error_rate_pct parsed wrong: %v", errPct)
	}

	if !updater.called || updater.gotStatus != domain.TestRunStatusPassed {
		t.Errorf("updater should have been called with passed; got called=%v status=%s", updater.called, updater.gotStatus)
	}
}

func TestK6Executor_MarksFailedOnExitCode(t *testing.T) {
	runner := &fakeRunner{stdout: `{"metrics":{}}`, exit: 99}
	updater := &captureUpdater{}
	exec := NewK6Executor(runner, updater)

	run := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New(), TestType: domain.TestTypePerf}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.gotStatus != domain.TestRunStatusFailed {
		t.Errorf("non-zero exit should mark failed, got %s", updater.gotStatus)
	}
}

func TestK6Executor_FallsBackToArtifactFetch(t *testing.T) {
	runner := &fakeRunner{stdout: `{"metrics":{}}`, exit: 0}
	exec := NewK6Executor(runner, nil)

	run := &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: uuid.New(),
		TestType:       domain.TestTypePerf,
		ResultJSON:     domain.JSONMap{"artifact_key": "k6/my-script.js"},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if !strings.Contains(runner.lastReq.Command, "sentiae-artifact fetch") {
		t.Errorf("should fetch artifact; got: %s", runner.lastReq.Command)
	}
}

func TestK6Executor_RawStdoutOnParseFailure(t *testing.T) {
	runner := &fakeRunner{stdout: "not-json-output-at-all", exit: 0}
	exec := NewK6Executor(runner, nil)

	run := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New(), TestType: domain.TestTypePerf}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	perf, _ := run.ResultJSON["perf"].(domain.JSONMap)
	if perf == nil || perf["raw"] != "not-json-output-at-all" {
		t.Errorf("raw stdout should fall through on parse failure; got: %+v", perf)
	}
}
