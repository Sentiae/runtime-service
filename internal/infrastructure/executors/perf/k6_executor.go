// Package perf — k6 executor adapter. Runs a k6 performance script
// inside a Firecracker VM, parses the k6 summary.json payload into
// TestRun.ResultJSON["perf"], and marks the run passed/failed based on
// the exit code. CS-2 G2.5.
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

// DefaultK6Image is the baseline container/rootfs image that carries
// the k6 binary. Overridable per-run via TestRun.ResultJSON["k6_image"].
const DefaultK6Image = "grafana/k6:latest"

// DefaultScriptArtifactKey names the artifact-store key the test script
// is uploaded under when callers don't inline it via ResultJSON["script"].
const DefaultScriptArtifactKey = "k6/script.js"

// PerfSummary is the subset of the k6 summary-json payload the gate
// evaluator consumes. Fields are filled opportunistically — a missing
// metric stays at zero.
type PerfSummary struct {
	P50LatencyMS float64 `json:"p50_ms"`
	P95LatencyMS float64 `json:"p95_ms"`
	P99LatencyMS float64 `json:"p99_ms"`
	RPS          float64 `json:"rps"`
	ErrorRatePct float64 `json:"error_rate_pct"`
	VUsMax       int     `json:"vus_max"`
	Iterations   int     `json:"iterations"`
}

// K6Executor runs k6 scripts inside a VM.
type K6Executor struct {
	runner  executors.VMExecRunner
	updater executors.TestRunUpdater
}

// NewK6Executor wires the executor.
func NewK6Executor(runner executors.VMExecRunner, updater executors.TestRunUpdater) *K6Executor {
	return &K6Executor{runner: runner, updater: updater}
}

// runConfig describes everything needed to launch k6.
type runConfig struct {
	Script      string
	ArtifactKey string
	Image       string
	DurationSec int
	VUs         int
}

func extractConfig(run *domain.TestRun) runConfig {
	cfg := runConfig{
		Image:       DefaultK6Image,
		ArtifactKey: DefaultScriptArtifactKey,
		DurationSec: 30,
		VUs:         10,
	}
	if run.ResultJSON == nil {
		return cfg
	}
	if v, ok := run.ResultJSON["script"].(string); ok && v != "" {
		cfg.Script = v
	}
	if v, ok := run.ResultJSON["artifact_key"].(string); ok && v != "" {
		cfg.ArtifactKey = v
	}
	if v, ok := run.ResultJSON["k6_image"].(string); ok && v != "" {
		cfg.Image = v
	}
	if v, ok := run.ResultJSON["duration_sec"].(float64); ok {
		cfg.DurationSec = int(v)
	}
	if v, ok := run.ResultJSON["vus"].(float64); ok {
		cfg.VUs = int(v)
	}
	return cfg
}

// buildCommand emits the shell string we ask the VM to execute.
// `/sentiae/script.js` is the agreed-upon mount point inside the VM
// where the runtime drops the fetched artifact; when an inline script
// is present on ResultJSON, we write it via a heredoc so the VM can
// run without an artifact round-trip.
func buildCommand(cfg runConfig) string {
	var b strings.Builder
	if cfg.Script != "" {
		b.WriteString("cat > /sentiae/script.js <<'SENTIAE_EOF'\n")
		b.WriteString(cfg.Script)
		b.WriteString("\nSENTIAE_EOF\n")
	} else {
		fmt.Fprintf(&b, "sentiae-artifact fetch %q /sentiae/script.js\n", cfg.ArtifactKey)
	}
	fmt.Fprintf(&b, "k6 run --summary-export=/sentiae/summary.json --vus=%d --duration=%ds /sentiae/script.js\n", cfg.VUs, cfg.DurationSec)
	b.WriteString("cat /sentiae/summary.json\n")
	return b.String()
}

// DispatchInVM executes the run and persists the parsed summary.
func (e *K6Executor) DispatchInVM(ctx context.Context, run *domain.TestRun) error {
	if e == nil || e.runner == nil {
		return fmt.Errorf("k6 executor not configured")
	}
	cfg := extractConfig(run)
	command := buildCommand(cfg)

	timeoutSec := cfg.DurationSec + 120 // hard cap — scenario + buffer
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

	summary := parseK6Summary(result.Stdout)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["perf"] = summary

	status := domain.TestRunStatusPassed
	if result.ExitCode != 0 {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, result.Stdout, result.Stderr, durationMS)
	}
	return nil
}

// parseK6Summary walks the k6 summary.json shape we care about and
// emits a flat JSONMap ready for storage. On any parse error it
// returns the raw stdout under the "raw" key so dashboards still
// surface something.
func parseK6Summary(stdout string) domain.JSONMap {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return domain.JSONMap{}
	}
	// k6 emits a summary envelope with top-level "metrics" map.
	var doc struct {
		Metrics map[string]struct {
			Values map[string]float64 `json:"values"`
		} `json:"metrics"`
		State struct {
			TestRunDurationMS float64 `json:"testRunDurationMs"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return domain.JSONMap{"raw": stdout}
	}
	summary := PerfSummary{}
	if m, ok := doc.Metrics["http_req_duration"]; ok {
		summary.P50LatencyMS = m.Values["p(50)"]
		summary.P95LatencyMS = m.Values["p(95)"]
		summary.P99LatencyMS = m.Values["p(99)"]
	}
	if m, ok := doc.Metrics["http_reqs"]; ok {
		summary.RPS = m.Values["rate"]
	}
	if m, ok := doc.Metrics["http_req_failed"]; ok {
		summary.ErrorRatePct = m.Values["rate"] * 100
	}
	if m, ok := doc.Metrics["vus_max"]; ok {
		summary.VUsMax = int(m.Values["value"])
	}
	if m, ok := doc.Metrics["iterations"]; ok {
		summary.Iterations = int(m.Values["count"])
	}
	return domain.JSONMap{
		"p50_ms":          summary.P50LatencyMS,
		"p95_ms":          summary.P95LatencyMS,
		"p99_ms":          summary.P99LatencyMS,
		"rps":             summary.RPS,
		"error_rate_pct":  summary.ErrorRatePct,
		"vus_max":         summary.VUsMax,
		"iterations":      summary.Iterations,
		"duration_ms":     doc.State.TestRunDurationMS,
	}
}
