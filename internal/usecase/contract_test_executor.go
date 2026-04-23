package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// ContractTestExecutor implements TestRunDispatcher for
// TestTypeContract. Reads pact_broker_url, provider_name, consumer_name,
// consumer_version from TestRun.ResultJSON and runs `pact-verifier`
// against the broker, storing the raw output on the run.
type ContractTestExecutor struct {
	execUC  ExecutionUseCase
	updater TestRunUpdater
}

// NewContractTestExecutor wires the executor.
func NewContractTestExecutor(execUC ExecutionUseCase, updater TestRunUpdater) *ContractTestExecutor {
	return &ContractTestExecutor{execUC: execUC, updater: updater}
}

// DispatchInVM runs pact-verifier against the broker and persists
// the raw output + status onto the TestRun.
func (e *ContractTestExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	if e == nil || e.execUC == nil {
		return fmt.Errorf("contract test executor not configured")
	}
	cfg := readContractConfig(run)
	if cfg.BrokerURL == "" || cfg.ProviderName == "" {
		err := fmt.Errorf("contract test: pact_broker_url + provider_name required")
		if e.updater != nil {
			_ = e.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), 0)
		}
		return err
	}

	cmd := fmt.Sprintf("pact-verifier --pact-broker-base-url=%s --provider=%s", cfg.BrokerURL, cfg.ProviderName)
	if cfg.ConsumerName != "" {
		cmd += fmt.Sprintf(" --consumer=%s", cfg.ConsumerName)
	}
	if cfg.ConsumerVersion != "" {
		cmd += fmt.Sprintf(" --consumer-version=%s", cfg.ConsumerVersion)
	}

	input := CreateExecutionInput{
		OrganizationID: run.OrganizationID,
		Language:       run.Language,
		Code:           cmd,
	}
	if run.TimeoutMS > 0 {
		input.Resources = &domain.ResourceLimit{TimeoutSec: int(run.TimeoutMS / 1000)}
	}

	start := time.Now()
	exec, err := e.execUC.ExecuteSync(ctx, input)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if e.updater != nil {
			_ = e.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), durationMS)
		}
		return err
	}
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["pact_broker_url"] = cfg.BrokerURL
	run.ResultJSON["provider_name"] = cfg.ProviderName
	run.ResultJSON["consumer_name"] = cfg.ConsumerName
	run.ResultJSON["consumer_version"] = cfg.ConsumerVersion
	run.ResultJSON["raw_stdout"] = exec.Stdout
	run.ResultJSON["raw_stderr"] = exec.Stderr

	status := domain.TestRunStatusPassed
	if exec.Status == domain.ExecutionStatusFailed || (exec.ExitCode != nil && *exec.ExitCode != 0) {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, exec.Stdout, exec.Stderr, durationMS)
	}
	return nil
}

type contractConfig struct {
	BrokerURL       string
	ProviderName    string
	ConsumerName    string
	ConsumerVersion string
}

func readContractConfig(run *domain.TestRun) contractConfig {
	var cfg contractConfig
	if run.ResultJSON == nil {
		return cfg
	}
	if v, ok := run.ResultJSON["pact_broker_url"].(string); ok {
		cfg.BrokerURL = v
	}
	if v, ok := run.ResultJSON["provider_name"].(string); ok {
		cfg.ProviderName = v
	}
	if v, ok := run.ResultJSON["consumer_name"].(string); ok {
		cfg.ConsumerName = v
	}
	if v, ok := run.ResultJSON["consumer_version"].(string); ok {
		cfg.ConsumerVersion = v
	}
	return cfg
}
