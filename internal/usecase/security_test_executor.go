package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// SecurityScanner names one of the shipped security scan tools.
type SecurityScanner string

const (
	SecurityScannerTrivy    SecurityScanner = "trivy"    // container image scan
	SecurityScannerZAP      SecurityScanner = "zap"      // DAST
	SecurityScannerBandit   SecurityScanner = "bandit"   // SAST (python)
	SecurityScannerSemgrep  SecurityScanner = "semgrep"  // SAST
	SecurityScannerDefault  SecurityScanner = "semgrep"
)

// SecurityTestExecutor implements TestRunDispatcher for TestTypeSecurity.
// The caller chooses the scanner through TestRun.ResultJSON["scanner"];
// defaults to semgrep when unset. The executor issues the scanner via
// HermeticStepRunner, parses the JSON output into findings + severity
// counts, and emits `security.scan.completed` with org + deployment
// context from the TestRun row.
type SecurityTestExecutor struct {
	execUC         ExecutionUseCase
	updater        TestRunUpdater
	eventPublisher EventPublisher
}

// NewSecurityTestExecutor wires the executor.
func NewSecurityTestExecutor(execUC ExecutionUseCase, updater TestRunUpdater, pub EventPublisher) *SecurityTestExecutor {
	return &SecurityTestExecutor{execUC: execUC, updater: updater, eventPublisher: pub}
}

// scannerCommand returns the per-tool invocation. Callers should
// configure tool paths via the runner profile's base image; we only
// select the argv.
func scannerCommand(s SecurityScanner, target string) string {
	if target == "" {
		target = "."
	}
	switch s {
	case SecurityScannerTrivy:
		return fmt.Sprintf("trivy image --format json %s", target)
	case SecurityScannerZAP:
		return fmt.Sprintf("zap-cli --quick-scan --format=json %s", target)
	case SecurityScannerBandit:
		return fmt.Sprintf("bandit -r %s -f json", target)
	default:
		return fmt.Sprintf("semgrep --config=auto --json %s", target)
	}
}

// DispatchInVM runs the configured security scanner and records the
// findings in TestRun.ResultJSON.
func (e *SecurityTestExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	if e == nil || e.execUC == nil {
		return fmt.Errorf("security test executor not configured")
	}
	scanner := SecurityScannerDefault
	target := "."
	if run.ResultJSON != nil {
		if s, ok := run.ResultJSON["scanner"].(string); ok && s != "" {
			scanner = SecurityScanner(s)
		}
		if t, ok := run.ResultJSON["target"].(string); ok && t != "" {
			target = t
		}
	}
	cmd := scannerCommand(scanner, target)

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

	findings, severityCounts := parseSecurityOutput(scanner, exec.Stdout)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["scanner"] = string(scanner)
	run.ResultJSON["findings"] = findings
	run.ResultJSON["severity_counts"] = severityCounts

	status := domain.TestRunStatusPassed
	if severityCounts["critical"] > 0 || severityCounts["high"] > 0 {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, exec.Stdout, exec.Stderr, durationMS)
	}

	// Emit cross-service completion event so downstream consumers
	// (ops-service deployment gate, Pulse) can react.
	if e.eventPublisher != nil {
		canvasID := ""
		if run.CanvasID != nil {
			canvasID = run.CanvasID.String()
		}
		_ = e.eventPublisher.Publish(ctx, "security.scan.completed", run.ID.String(), map[string]any{
			"test_run_id":     run.ID.String(),
			"organization_id": run.OrganizationID.String(),
			"canvas_id":       canvasID,
			"scanner":         string(scanner),
			"severity_counts": severityCounts,
			"finding_count":   len(findings),
			"status":          string(status),
		})
	}
	return nil
}

// parseSecurityOutput unmarshals scanner JSON into a flat findings list
// plus a severity tally. Falls back to an empty list on parse failure
// so a bad tool run still writes a minimal ResultJSON.
func parseSecurityOutput(scanner SecurityScanner, stdout string) ([]map[string]any, map[string]int) {
	counts := map[string]int{}
	findings := []map[string]any{}
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return findings, counts
	}
	// Scanner outputs vary; walk the top-level shape we know.
	switch scanner {
	case SecurityScannerSemgrep:
		var doc struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err == nil {
			findings = append(findings, doc.Results...)
		}
	case SecurityScannerTrivy:
		var doc struct {
			Results []struct {
				Vulnerabilities []map[string]any `json:"Vulnerabilities"`
			} `json:"Results"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err == nil {
			for _, r := range doc.Results {
				findings = append(findings, r.Vulnerabilities...)
			}
		}
	case SecurityScannerBandit:
		var doc struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err == nil {
			findings = doc.Results
		}
	case SecurityScannerZAP:
		var doc struct {
			Alerts []map[string]any `json:"alerts"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err == nil {
			findings = doc.Alerts
		}
	default:
		var fallback []map[string]any
		if err := json.Unmarshal([]byte(stdout), &fallback); err == nil {
			findings = fallback
		}
	}
	for _, f := range findings {
		sev := ""
		for _, k := range []string{"severity", "Severity", "issue_severity"} {
			if v, ok := f[k].(string); ok && v != "" {
				sev = strings.ToLower(v)
				break
			}
		}
		if sev == "" {
			sev = "info"
		}
		counts[sev]++
	}
	return findings, counts
}
