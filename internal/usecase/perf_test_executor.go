package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// PerfTestExecutor implements TestRunDispatcher for TestTypePerformance.
// It builds a k6 script from TestDefinition.Config (PerfGateConfig shape:
// MaxP99LatencyMs, MinThroughputRps, MaxMemoryMb, TargetURL, DurationSec,
// VUs) and runs it through the shared execution surface in a PerfVM
// profile (4 vCPU / 2048MB). k6's JSON output is unmarshaled into the
// test-run ResultJSON column so dashboards can show trends.
type PerfTestExecutor struct {
	execUC  ExecutionUseCase
	updater TestRunUpdater
}

// NewPerfTestExecutor wires the executor.
func NewPerfTestExecutor(execUC ExecutionUseCase, updater TestRunUpdater) *PerfTestExecutor {
	return &PerfTestExecutor{execUC: execUC, updater: updater}
}

// perfConfigFromProfile parses the profile/command's config blob if any.
// Keys are read case-insensitively from the config map.
func perfConfigFromRun(run *domain.TestRun, profile TestRunnerProfile) perfRunConfig {
	cfg := perfRunConfig{
		TargetURL:        "http://localhost:8080",
		DurationSec:      30,
		VUs:              10,
		MaxP99LatencyMs:  500,
		MinThroughputRps: 10,
		MaxMemoryMb:      2048,
	}
	// TestRun.ResultJSON may carry the caller-provided perf config on
	// creation so the executor can adapt without a schema change.
	if run.ResultJSON != nil {
		if v, ok := run.ResultJSON["target_url"].(string); ok && v != "" {
			cfg.TargetURL = v
		}
		if v, ok := run.ResultJSON["duration_sec"].(float64); ok {
			cfg.DurationSec = int(v)
		}
		if v, ok := run.ResultJSON["vus"].(float64); ok {
			cfg.VUs = int(v)
		}
		if v, ok := run.ResultJSON["max_p99_latency_ms"].(float64); ok {
			cfg.MaxP99LatencyMs = int(v)
		}
		if v, ok := run.ResultJSON["min_throughput_rps"].(float64); ok {
			cfg.MinThroughputRps = int(v)
		}
		if v, ok := run.ResultJSON["max_memory_mb"].(float64); ok {
			cfg.MaxMemoryMb = int(v)
		}
	}
	return cfg
}

type perfRunConfig struct {
	TargetURL        string
	DurationSec      int
	VUs              int
	MaxP99LatencyMs  int
	MinThroughputRps int
	MaxMemoryMb      int
}

// buildK6Script emits a minimal k6 scenario against TargetURL with the
// configured VUs + duration. The script writes its summary as JSON on
// stdout so the dispatcher can parse it without file plumbing.
func buildK6Script(cfg perfRunConfig) string {
	var b strings.Builder
	b.WriteString(`import http from 'k6/http';` + "\n")
	b.WriteString(`import { sleep } from 'k6';` + "\n")
	fmt.Fprintf(&b, "export const options = { vus: %d, duration: '%ds' };\n", cfg.VUs, cfg.DurationSec)
	b.WriteString("export default function () {\n")
	fmt.Fprintf(&b, "  http.get(%q);\n", cfg.TargetURL)
	b.WriteString("  sleep(1);\n}\n")
	b.WriteString(`export function handleSummary(data) {` + "\n")
	b.WriteString(`  return { stdout: JSON.stringify(data) };` + "\n")
	b.WriteString("}\n")
	return b.String()
}

// DispatchInVM implements the TestRunDispatcher contract for perf runs.
func (e *PerfTestExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	if e == nil || e.execUC == nil {
		return fmt.Errorf("perf test executor not configured")
	}
	cfg := perfConfigFromRun(run, profile)
	script := buildK6Script(cfg)

	input := CreateExecutionInput{
		OrganizationID: run.OrganizationID,
		Language:       domain.Language("javascript"),
		Code:           script,
	}
	// Perf VM profile: 4 vCPU / 2048MB.
	input.Resources = &domain.ResourceLimit{
		VCPU:       4,
		MemoryMB:   2048,
		TimeoutSec: cfg.DurationSec + 60,
	}
	if run.TimeoutMS > 0 {
		input.Resources.TimeoutSec = int(run.TimeoutMS / 1000)
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

	// Parse k6 summary JSON; keep the raw stdout as fallback.
	summary := parsePerfSummary(exec.Stdout)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["perf_summary"] = summary
	run.ResultJSON["gate_config"] = map[string]any{
		"max_p99_latency_ms": cfg.MaxP99LatencyMs,
		"min_throughput_rps": cfg.MinThroughputRps,
		"max_memory_mb":      cfg.MaxMemoryMb,
	}

	status := domain.TestRunStatusPassed
	if exec.Status == domain.ExecutionStatusFailed || (exec.ExitCode != nil && *exec.ExitCode != 0) {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, exec.Stdout, exec.Stderr, durationMS)
	}
	return nil
}

// parsePerfSummary unmarshals the k6 summary envelope. A parse failure
// returns {"raw": stdout} so downstream dashboards can still show the
// bytes.
func parsePerfSummary(stdout string) map[string]any {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err == nil {
		return m
	}
	return map[string]any{"raw": stdout}
}
