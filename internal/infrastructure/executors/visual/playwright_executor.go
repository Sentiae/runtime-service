// Package visual — Playwright visual-regression executor. Runs
// Playwright with the visual-comparison plugin inside a VM that has a
// browser image, then parses its JSON report into
// TestRun.ResultJSON["visual"]. CS-2 G2.5.
package visual

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

// DefaultPlaywrightImage is the rootfs used when callers don't supply
// one. The production image ships chromium + the compare plugin.
const DefaultPlaywrightImage = "mcr.microsoft.com/playwright:v1.45.0-jammy"

// DefaultReportPath is where we expect the visual-compare plugin to
// emit its JSON report inside the VM.
const DefaultReportPath = "/sentiae/visual-report.json"

// PlaywrightExecutor runs Playwright tests and parses the visual diff.
type PlaywrightExecutor struct {
	runner  executors.VMExecRunner
	updater executors.TestRunUpdater
}

// NewPlaywrightExecutor wires the executor.
func NewPlaywrightExecutor(runner executors.VMExecRunner, updater executors.TestRunUpdater) *PlaywrightExecutor {
	return &PlaywrightExecutor{runner: runner, updater: updater}
}

// visualReport mirrors the plugin's JSON output.
type visualReport struct {
	Comparisons []struct {
		Name         string  `json:"name"`
		BaselineHash string  `json:"baseline_hash"`
		ActualHash   string  `json:"actual_hash"`
		DiffPct      float64 `json:"diff_pct"`
		DiffImageRef string  `json:"diff_image_ref"`
	} `json:"comparisons"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type runConfig struct {
	Script       string
	ArtifactKey  string
	Image        string
	TestCommand  string
	MaxDiffPct   float64
}

func extractConfig(run *domain.TestRun) runConfig {
	cfg := runConfig{
		Image:       DefaultPlaywrightImage,
		ArtifactKey: "playwright/spec.js",
		TestCommand: "npx playwright test --reporter=list",
		MaxDiffPct:  1.0, // default: fail if any comparison diffs > 1 %
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
	if v, ok := run.ResultJSON["playwright_image"].(string); ok && v != "" {
		cfg.Image = v
	}
	if v, ok := run.ResultJSON["test_command"].(string); ok && v != "" {
		cfg.TestCommand = v
	}
	if v, ok := run.ResultJSON["max_diff_pct"].(float64); ok {
		cfg.MaxDiffPct = v
	}
	return cfg
}

func buildCommand(cfg runConfig) string {
	var b strings.Builder
	if cfg.Script != "" {
		b.WriteString("cat > /sentiae/spec.test.js <<'SENTIAE_EOF'\n")
		b.WriteString(cfg.Script)
		b.WriteString("\nSENTIAE_EOF\n")
	} else {
		fmt.Fprintf(&b, "sentiae-artifact fetch %q /sentiae/spec.test.js\n", cfg.ArtifactKey)
	}
	fmt.Fprintf(&b, "PLAYWRIGHT_VISUAL_REPORT=%s %s /sentiae/spec.test.js\n", DefaultReportPath, cfg.TestCommand)
	fmt.Fprintf(&b, "cat %s\n", DefaultReportPath)
	return b.String()
}

// DispatchInVM runs Playwright and records the parsed report.
func (e *PlaywrightExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun) error {
	if e == nil || e.runner == nil {
		return fmt.Errorf("playwright executor not configured")
	}
	cfg := extractConfig(run)
	command := buildCommand(cfg)

	timeoutSec := 300
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

	report, maxDiff := parseVisualReport(result.Stdout)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["visual"] = report

	status := domain.TestRunStatusPassed
	if result.ExitCode != 0 || maxDiff > cfg.MaxDiffPct {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, result.Stdout, result.Stderr, durationMS)
	}
	return nil
}

// parseVisualReport returns (reportJSON, maxDiffPct). The report is
// flattened into the documented `visual = { baseline_hash, actual_hash,
// diff_pct, diff_image_ref }` shape — when multiple comparisons exist
// we report the worst-offender hashes + aggregate the max diff.
func parseVisualReport(stdout string) (domain.JSONMap, float64) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return domain.JSONMap{}, 0
	}
	var doc visualReport
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return domain.JSONMap{"raw": stdout}, 0
	}
	var (
		maxDiff       float64
		baseline      string
		actual        string
		diffImage     string
		worstName     string
		comparisons   = make([]map[string]any, 0, len(doc.Comparisons))
	)
	for _, c := range doc.Comparisons {
		if c.DiffPct > maxDiff {
			maxDiff = c.DiffPct
			baseline = c.BaselineHash
			actual = c.ActualHash
			diffImage = c.DiffImageRef
			worstName = c.Name
		}
		comparisons = append(comparisons, map[string]any{
			"name":          c.Name,
			"baseline_hash": c.BaselineHash,
			"actual_hash":   c.ActualHash,
			"diff_pct":      c.DiffPct,
			"diff_image_ref": c.DiffImageRef,
		})
	}
	return domain.JSONMap{
		"baseline_hash":  baseline,
		"actual_hash":    actual,
		"diff_pct":       maxDiff,
		"diff_image_ref": diffImage,
		"worst_case":     worstName,
		"comparisons":    comparisons,
		"passed":         doc.Passed,
		"failed":         doc.Failed,
	}, maxDiff
}
