// Package contract — pact-broker-cli `can-i-deploy` executor. Asks
// the pact broker whether the (provider, version) tuple can deploy
// without breaking an active consumer contract; parses the JSON
// response into TestRun.ResultJSON["contract"]. CS-2 G2.5.
package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

// DefaultPactCommand is the shell template for can-i-deploy. The
// executor substitutes {{BROKER}}, {{PROVIDER}}, {{VERSION}} before
// the VM runs it.
const DefaultPactCommand = "pact-broker can-i-deploy --broker-base-url={{BROKER}} --pacticipant={{PROVIDER}} --version={{VERSION}} --output=json"

// PactExecutor runs pact-broker-cli against a configured broker.
type PactExecutor struct {
	runner  executors.VMExecRunner
	updater executors.TestRunUpdater
}

// NewPactExecutor wires the executor.
func NewPactExecutor(runner executors.VMExecRunner, updater executors.TestRunUpdater) *PactExecutor {
	return &PactExecutor{runner: runner, updater: updater}
}

// pactReport mirrors the can-i-deploy JSON shape.
type pactReport struct {
	Summary struct {
		Deployable *bool `json:"deployable"`
		Reason     string `json:"reason"`
	} `json:"summary"`
	Matrix []struct {
		Consumer struct {
			Name    string `json:"name"`
			Version struct {
				Number string `json:"number"`
			} `json:"version"`
		} `json:"consumer"`
		Provider struct {
			Name    string `json:"name"`
			Version struct {
				Number string `json:"number"`
			} `json:"version"`
		} `json:"provider"`
		VerificationResult struct {
			Success *bool `json:"success"`
		} `json:"verificationResult"`
	} `json:"matrix"`
}

type pactConfig struct {
	BrokerURL string
	Provider  string
	Version   string
}

func extractConfig(run *domain.TestRun) (pactConfig, error) {
	var cfg pactConfig
	if run.ResultJSON == nil {
		return cfg, fmt.Errorf("contract test: result_json required")
	}
	if v, ok := run.ResultJSON["pact_broker_url"].(string); ok {
		cfg.BrokerURL = v
	}
	if v, ok := run.ResultJSON["provider_name"].(string); ok {
		cfg.Provider = v
	}
	if v, ok := run.ResultJSON["provider_version"].(string); ok {
		cfg.Version = v
	}
	if cfg.BrokerURL == "" || cfg.Provider == "" || cfg.Version == "" {
		return cfg, fmt.Errorf("contract test: pact_broker_url + provider_name + provider_version required")
	}
	return cfg, nil
}

func buildCommand(cfg pactConfig) string {
	cmd := DefaultPactCommand
	cmd = strings.ReplaceAll(cmd, "{{BROKER}}", cfg.BrokerURL)
	cmd = strings.ReplaceAll(cmd, "{{PROVIDER}}", cfg.Provider)
	cmd = strings.ReplaceAll(cmd, "{{VERSION}}", cfg.Version)
	return cmd
}

// DispatchInVM runs pact-broker can-i-deploy and records the result.
func (e *PactExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun) error {
	if e == nil || e.runner == nil {
		return fmt.Errorf("pact executor not configured")
	}
	cfg, err := extractConfig(run)
	if err != nil {
		if e.updater != nil {
			_ = e.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), 0)
		}
		return err
	}
	command := buildCommand(cfg)

	timeoutSec := 120
	if run.TimeoutMS > 0 {
		timeoutSec = int(run.TimeoutMS / 1000)
	}

	start := time.Now()
	result, err := e.runner.ExecuteInVM(ctx, executors.VMExecRequest{
		OrganizationID: run.OrganizationID,
		Language:       run.Language,
		Command:        command,
		TimeoutSec:     timeoutSec,
	})
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if e.updater != nil {
			_ = e.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), durationMS)
		}
		return err
	}

	compatible, summary := parsePactReport(result.Stdout)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["contract"] = summary

	status := domain.TestRunStatusPassed
	// can-i-deploy exits non-zero when the answer is "no", so either
	// signal means the gate fails. We trust the parsed flag first so a
	// parse error that erroneously leaves compatible=true doesn't
	// upgrade a genuine tool failure into a pass.
	if !compatible || result.ExitCode != 0 {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, result.Stdout, result.Stderr, durationMS)
	}
	return nil
}

// parsePactReport returns (compatible, summary). Missing deployable
// field is treated as incompatible — we fail closed when the broker
// response doesn't tell us either way.
func parsePactReport(stdout string) (bool, domain.JSONMap) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return false, domain.JSONMap{}
	}
	var doc pactReport
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return false, domain.JSONMap{"raw": stdout, "compatible": false}
	}
	compatible := doc.Summary.Deployable != nil && *doc.Summary.Deployable
	incompatible := []map[string]any{}
	for _, m := range doc.Matrix {
		if m.VerificationResult.Success != nil && !*m.VerificationResult.Success {
			incompatible = append(incompatible, map[string]any{
				"consumer":         m.Consumer.Name,
				"consumer_version": m.Consumer.Version.Number,
				"provider":         m.Provider.Name,
				"provider_version": m.Provider.Version.Number,
			})
		}
	}
	return compatible, domain.JSONMap{
		"compatible":         compatible,
		"reason":             doc.Summary.Reason,
		"incompatible_pairs": incompatible,
	}
}
