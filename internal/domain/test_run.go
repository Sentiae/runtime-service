package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxTestRetries caps automatic retries on transient failures so
// a broken dependency cannot infinitely re-run a failing test.
const DefaultMaxTestRetries = 2

// TestRun records the result of executing a test node. It links an execution
// to the test and code nodes on the canvas, enabling test history, trends,
// and quality gate evaluations.
type TestRun struct {
	ID             uuid.UUID     `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID     `json:"organization_id" gorm:"type:uuid;not null;index"`
	ExecutionID    uuid.UUID     `json:"execution_id" gorm:"type:uuid;not null;index"`
	TestNodeID     uuid.UUID     `json:"test_node_id" gorm:"type:uuid;not null;index:idx_test_runs_node"`
	CodeNodeID     *uuid.UUID    `json:"code_node_id,omitempty" gorm:"type:uuid;index"`
	CanvasID       *uuid.UUID    `json:"canvas_id,omitempty" gorm:"type:uuid;index"`
	Language       Language      `json:"language" gorm:"type:varchar(20);not null"`
	TestType       TestType      `json:"test_type" gorm:"type:varchar(20);not null;default:'unit';index"`
	Status         TestRunStatus `json:"status" gorm:"type:varchar(20);not null;default:'running';index"`
	Passed         int           `json:"passed" gorm:"default:0"`
	Failed         int           `json:"failed" gorm:"default:0"`
	Skipped        int           `json:"skipped" gorm:"default:0"`
	Total          int           `json:"total" gorm:"default:0"`
	CoveragePC     *float64      `json:"coverage_pc,omitempty"`
	DurationMS     *int64        `json:"duration_ms,omitempty"`
	ErrorMessage   string        `json:"error_message,omitempty" gorm:"type:text"`
	// Transient-failure retry state. A test is retried up to MaxRetries
	// times when the runner reports a classified-transient error (network
	// timeout, VM provisioning failure, etc). Non-transient errors land
	// in TestRunStatusError immediately.
	RetryCount int       `json:"retry_count" gorm:"default:0"`
	MaxRetries int       `json:"max_retries" gorm:"default:2"`
	WasRetried bool      `json:"was_retried" gorm:"default:false"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TestRunStatus represents the lifecycle of a test run.
type TestRunStatus string

const (
	TestRunStatusRunning   TestRunStatus = "running"
	TestRunStatusPassed    TestRunStatus = "passed"
	TestRunStatusFailed    TestRunStatus = "failed"
	TestRunStatusError     TestRunStatus = "error"
	TestRunStatusCancelled TestRunStatus = "cancelled"
)

func (s TestRunStatus) IsValid() bool {
	switch s {
	case TestRunStatusRunning, TestRunStatusPassed, TestRunStatusFailed,
		TestRunStatusError, TestRunStatusCancelled:
		return true
	}
	return false
}

// MarkCompleted sets the test run result based on pass/fail counts.
func (t *TestRun) MarkCompleted(passed, failed, skipped int, coveragePC *float64, durationMS int64) {
	t.Passed = passed
	t.Failed = failed
	t.Skipped = skipped
	t.Total = passed + failed + skipped
	t.CoveragePC = coveragePC
	t.DurationMS = &durationMS
	now := time.Now()
	t.UpdatedAt = now
	if failed > 0 {
		t.Status = TestRunStatusFailed
	} else {
		t.Status = TestRunStatusPassed
	}
}

// MarkError marks the test run as errored (e.g., compilation failure).
func (t *TestRun) MarkError(errMsg string) {
	t.Status = TestRunStatusError
	t.ErrorMessage = errMsg
	t.UpdatedAt = time.Now()
}

// IsTransientError classifies a runner error message as something worth
// retrying. The set of substrings is intentionally conservative: a real
// test-assertion failure should never be retried, only infrastructure
// flakes (network, DNS, VM provisioning, docker pull, rate limiting).
func IsTransientError(errMsg string) bool {
	if errMsg == "" {
		return false
	}
	lower := strings.ToLower(errMsg)
	for _, marker := range []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"i/o timeout",
		"deadline exceeded",
		"temporary failure in name resolution",
		"no such host",
		"tls handshake timeout",
		"eof",
		"broken pipe",
		"rate limit",
		"too many requests",
		"503 service unavailable",
		"502 bad gateway",
		"504 gateway timeout",
		"vm provisioning failed",
		"firecracker start timeout",
		"docker: error response from daemon",
		"image pull backoff",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// TryRetry consumes one retry slot and returns true if the caller should
// re-enqueue the run. Uses DefaultMaxTestRetries when MaxRetries was left
// at zero (e.g. pre-existing rows written before the retry fields
// landed). Resets the status to running so the next attempt restarts the
// lifecycle cleanly.
func (t *TestRun) TryRetry(errMsg string) bool {
	max := t.MaxRetries
	if max <= 0 {
		max = DefaultMaxTestRetries
	}
	if !IsTransientError(errMsg) {
		return false
	}
	if t.RetryCount >= max {
		return false
	}
	t.RetryCount++
	t.WasRetried = true
	t.Status = TestRunStatusRunning
	t.ErrorMessage = ""
	t.UpdatedAt = time.Now()
	return true
}

// TestSummary holds aggregated test statistics for a canvas.
type TestSummary struct {
	TotalRuns   int     `json:"total_runs"`
	PassedRuns  int     `json:"passed_runs"`
	FailedRuns  int     `json:"failed_runs"`
	TestNodes   int     `json:"test_nodes"`
	AvgCoverage float64 `json:"avg_coverage"`
}
