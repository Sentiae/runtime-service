package contract

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

func TestPactExecutor_RequiresConfig(t *testing.T) {
	runner := &fakeRunner{}
	updater := &captureUpdater{}
	exec := NewPactExecutor(runner, updater)

	run := &domain.TestRun{ID: uuid.New(), OrganizationID: uuid.New(),
		ResultJSON: domain.JSONMap{"pact_broker_url": "http://b"}}
	if err := exec.DispatchInVM(context.Background(), run); err == nil {
		t.Fatal("should require provider + version")
	}
	if updater.status != domain.TestRunStatusError {
		t.Errorf("expected error status, got %s", updater.status)
	}
}

func TestPactExecutor_DeployableTrue(t *testing.T) {
	deployable := true
	_ = deployable
	report := `{
		"summary": {"deployable": true, "reason": "all good"},
		"matrix": [{
			"consumer": {"name":"portal","version":{"number":"1.0"}},
			"provider": {"name":"api","version":{"number":"2.5"}},
			"verificationResult": {"success": true}
		}]
	}`
	runner := &fakeRunner{stdout: report, exit: 0}
	updater := &captureUpdater{}
	exec := NewPactExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		TestType: domain.TestTypeContract,
		ResultJSON: domain.JSONMap{
			"pact_broker_url":  "https://broker.example.com",
			"provider_name":    "api",
			"provider_version": "2.5",
		},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusPassed {
		t.Errorf("deployable=true should pass; got %s", updater.status)
	}
	contract, _ := run.ResultJSON["contract"].(domain.JSONMap)
	if contract == nil || contract["compatible"] != true {
		t.Errorf("compatible should be true; got %+v", contract)
	}
	if !strings.Contains(runner.last.Command, "--pacticipant=api") {
		t.Errorf("provider name missing from command: %s", runner.last.Command)
	}
	if !strings.Contains(runner.last.Command, "--version=2.5") {
		t.Errorf("version missing from command: %s", runner.last.Command)
	}
}

func TestPactExecutor_DeployableFalseFails(t *testing.T) {
	report := `{
		"summary": {"deployable": false, "reason": "consumer portal@1.0 breaks"},
		"matrix": [{
			"consumer": {"name":"portal","version":{"number":"1.0"}},
			"provider": {"name":"api","version":{"number":"2.5"}},
			"verificationResult": {"success": false}
		}]
	}`
	runner := &fakeRunner{stdout: report, exit: 1}
	updater := &captureUpdater{}
	exec := NewPactExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		TestType: domain.TestTypeContract,
		ResultJSON: domain.JSONMap{
			"pact_broker_url":  "https://broker.example.com",
			"provider_name":    "api",
			"provider_version": "2.5",
		},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusFailed {
		t.Errorf("deployable=false should fail; got %s", updater.status)
	}
	contract, _ := run.ResultJSON["contract"].(domain.JSONMap)
	pairs, _ := contract["incompatible_pairs"].([]map[string]any)
	if len(pairs) != 1 {
		t.Errorf("expected 1 incompatible pair, got %d", len(pairs))
	}
}

func TestPactExecutor_FailsClosedOnBadJSON(t *testing.T) {
	runner := &fakeRunner{stdout: "broker-unreachable", exit: 2}
	updater := &captureUpdater{}
	exec := NewPactExecutor(runner, updater)

	run := &domain.TestRun{
		ID: uuid.New(), OrganizationID: uuid.New(),
		ResultJSON: domain.JSONMap{
			"pact_broker_url":  "https://broker.example.com",
			"provider_name":    "api",
			"provider_version": "2.5",
		},
	}
	if err := exec.DispatchInVM(context.Background(), run); err != nil {
		t.Fatalf("DispatchInVM: %v", err)
	}
	if updater.status != domain.TestRunStatusFailed {
		t.Errorf("bad JSON should fail; got %s", updater.status)
	}
}
