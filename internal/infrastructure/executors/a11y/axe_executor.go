// Package a11y — axe-core accessibility executor. Runs axe-core
// against a URL target inside a VM that has a headless browser,
// parses the JSON report into TestRun.ResultJSON["a11y"], and fails
// the run when critical violations are present. CS-2 G2.5.
package a11y

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
)

// DefaultAxeCommand is the shell template for invoking axe-core via
// axe-cli inside the VM. The `{{URL}}` placeholder is substituted with
// the configured target.
const DefaultAxeCommand = "axe {{URL}} --save /sentiae/a11y-report.json --exit"

// AxeExecutor runs axe-core against URL targets.
type AxeExecutor struct {
	runner  executors.VMExecRunner
	updater executors.TestRunUpdater
}

// NewAxeExecutor wires the executor.
func NewAxeExecutor(runner executors.VMExecRunner, updater executors.TestRunUpdater) *AxeExecutor {
	return &AxeExecutor{runner: runner, updater: updater}
}

// axeReport mirrors the axe-core JSON output shape.
type axeReport struct {
	Violations []struct {
		ID          string `json:"id"`
		Impact      string `json:"impact"`
		Description string `json:"description"`
		Help        string `json:"help"`
		HelpURL     string `json:"helpUrl"`
		Nodes       []struct {
			Target []string `json:"target"`
			HTML   string   `json:"html"`
		} `json:"nodes"`
	} `json:"violations"`
	Passes      []struct{ ID string `json:"id"` } `json:"passes"`
	Incomplete  []struct{ ID string `json:"id"` } `json:"incomplete"`
	Inapplicable []struct{ ID string `json:"id"` } `json:"inapplicable"`
}

// DispatchInVM runs axe-core and records the parsed report.
func (e *AxeExecutor) DispatchInVM(ctx context.Context, run *domain.TestRun) error {
	if e == nil || e.runner == nil {
		return fmt.Errorf("axe executor not configured")
	}
	targetURL := ""
	failOnImpacts := []string{"critical", "serious"}
	if run.ResultJSON != nil {
		if v, ok := run.ResultJSON["url"].(string); ok && v != "" {
			targetURL = v
		}
		if impacts, ok := run.ResultJSON["fail_on_impacts"].([]any); ok && len(impacts) > 0 {
			failOnImpacts = failOnImpacts[:0]
			for _, v := range impacts {
				if s, ok := v.(string); ok {
					failOnImpacts = append(failOnImpacts, s)
				}
			}
		}
	}
	if targetURL == "" {
		err := fmt.Errorf("a11y run requires result_json.url")
		if e.updater != nil {
			_ = e.updater.UpdateAfterRun(ctx, run.ID, domain.TestRunStatusError, "", err.Error(), 0)
		}
		return err
	}

	command := strings.ReplaceAll(DefaultAxeCommand, "{{URL}}", targetURL) + "\ncat /sentiae/a11y-report.json\n"

	timeoutSec := 180
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

	summary, blocking := parseAxeReport(result.Stdout, failOnImpacts)
	if run.ResultJSON == nil {
		run.ResultJSON = make(domain.JSONMap)
	}
	run.ResultJSON["a11y"] = summary

	status := domain.TestRunStatusPassed
	if blocking > 0 {
		status = domain.TestRunStatusFailed
	}
	if e.updater != nil {
		_ = e.updater.UpdateAfterRun(ctx, run.ID, status, result.Stdout, result.Stderr, durationMS)
	}
	return nil
}

// parseAxeReport returns the summary object and the count of
// violations that match the failOnImpacts list. Violations with an
// impact outside that list are still reported but don't fail the run.
func parseAxeReport(stdout string, failOnImpacts []string) (domain.JSONMap, int) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return domain.JSONMap{}, 0
	}
	var doc axeReport
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return domain.JSONMap{"raw": stdout}, 0
	}
	failSet := make(map[string]struct{}, len(failOnImpacts))
	for _, imp := range failOnImpacts {
		failSet[strings.ToLower(imp)] = struct{}{}
	}
	violations := make([]map[string]any, 0, len(doc.Violations))
	impactCounts := map[string]int{}
	blocking := 0
	for _, v := range doc.Violations {
		impact := strings.ToLower(v.Impact)
		impactCounts[impact]++
		if _, ok := failSet[impact]; ok {
			blocking++
		}
		violations = append(violations, map[string]any{
			"id":          v.ID,
			"impact":      v.Impact,
			"description": v.Description,
			"help":        v.Help,
			"help_url":    v.HelpURL,
			"node_count":  len(v.Nodes),
		})
	}
	return domain.JSONMap{
		"violations":       violations,
		"pass_count":       len(doc.Passes),
		"fail_count":       len(doc.Violations),
		"incomplete_count": len(doc.Incomplete),
		"inapplicable":    len(doc.Inapplicable),
		"impact_counts":    impactCounts,
		"blocking_count":   blocking,
	}, blocking
}
