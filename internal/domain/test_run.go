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
	// Cross-domain ownership links. A test run targets a specific
	// service, may verify a Spec's acceptance criteria, and is
	// produced inside a Session (the branch that ran tests). All are
	// nullable for legacy rows and ad-hoc canvas runs that aren't
	// associated with a spec/session pipeline.
	ServiceID *uuid.UUID  `json:"service_id,omitempty" gorm:"type:uuid;index"`
	SpecID    *uuid.UUID  `json:"spec_id,omitempty" gorm:"type:uuid;index"`
	SessionID *uuid.UUID  `json:"session_id,omitempty" gorm:"type:uuid;index"`
	// FeatureIDs is the M:N capability mapping derived from
	// spec.features at run-time. Stored as a JSONB array of UUIDs so
	// the repo layer can stay dialect-free; the consumer is the Pulse
	// rollup which needs feature-level test signal without a join.
	FeatureIDs UUIDArray `json:"feature_ids,omitempty" gorm:"type:jsonb;serializer:json"`
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
	RetryCount int  `json:"retry_count" gorm:"default:0"`
	MaxRetries int  `json:"max_retries" gorm:"default:2"`
	WasRetried bool `json:"was_retried" gorm:"default:false"`

	// §8.1 gap-closure:
	//
	// Framework names the underlying test runner (jest, vitest, pytest,
	// go-test, cargo-test, etc). Stored as a string rather than an enum
	// so the field keeps up with new runners without a migration.
	Framework string `json:"framework,omitempty" gorm:"type:varchar(40)"`
	// TimeoutMS is the wall-clock limit the scheduler enforces on this
	// run. 0 means "use the service default" (typically 5min). Set per
	// run so long-running integration suites don't share a budget with
	// fast unit tests.
	TimeoutMS int64 `json:"timeout_ms,omitempty" gorm:"default:0"`
	// FlakinessScore is a 0-1 rolling estimate of how often this test
	// fails non-deterministically. Populated by a periodic job that
	// looks at the last N runs and computes pass-rate variance; a
	// flaky score triggers auto-quarantine in the test-gate policy.
	FlakinessScore float64 `json:"flakiness_score" gorm:"type:float;default:0"`

	// Critical marks tests that MUST pass before a deploy can promote
	// (§8.5 "all critical tests pass" quality gate). Gate queries can
	// select `WHERE critical = true AND status IN (passed)` to form
	// the authoritative pass subset rather than all-or-nothing.
	Critical bool `json:"critical" gorm:"default:false;index"`

	// §8.3 auto-quarantine. Quarantined=true signals the delivery gate
	// to skip this test when computing "critical tests pass" and lets
	// the UI render a muted badge. QuarantinedAt records when the
	// transition happened so the scheduler can re-evaluate and release
	// tests that have stabilised.
	Quarantined   bool       `json:"quarantined" gorm:"default:false;index"`
	QuarantinedAt *time.Time `json:"quarantined_at,omitempty"`

	// §8.4 — per-executor result payload (perf metrics, security
	// findings array, contract provider report). Populated by the
	// type-specific executor in multi_type_dispatcher.
	ResultJSON JSONMap `json:"result_json,omitempty" gorm:"type:jsonb"`

	// §8.3 db-mode — ephemeral Postgres provisioning for integration
	// tests that need an isolated DB. DBMode is read by the provisioning
	// middleware which resolves a connection string and injects it into
	// the runner profile env.
	DBMode TestDBMode `json:"db_mode,omitempty" gorm:"type:varchar(30);default:'none'"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TestDBMode classifies whether the test needs a provisioned database.
// The default `none` leaves DATABASE_URL untouched; `ephemeral_pg`
// provisions a throwaway Postgres DB for the duration of the run.
type TestDBMode string

const (
	TestDBModeNone        TestDBMode = "none"
	TestDBModeEphemeralPg TestDBMode = "ephemeral_pg"
)

// IsValid reports whether the mode is a recognized enum.
func (m TestDBMode) IsValid() bool {
	switch m {
	case TestDBModeNone, TestDBModeEphemeralPg, "":
		return true
	}
	return false
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

// Quarantine flips the quarantine flag and stamps the transition time.
// Idempotent: calling twice on an already-quarantined test is a no-op.
func (t *TestRun) Quarantine(now time.Time) {
	if t.Quarantined {
		return
	}
	t.Quarantined = true
	stamp := now
	t.QuarantinedAt = &stamp
	t.UpdatedAt = now
}

// Unquarantine clears the quarantine flag. Used both when a test has
// stabilised and when an operator manually overrides the scheduler.
func (t *TestRun) Unquarantine(now time.Time) {
	if !t.Quarantined {
		return
	}
	t.Quarantined = false
	t.QuarantinedAt = nil
	t.UpdatedAt = now
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
