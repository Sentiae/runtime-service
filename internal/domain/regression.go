package domain

import (
	"time"

	"github.com/google/uuid"
)

// RegressionTestTemplate links a captured production trace to a
// generated reproducer test. The actual test source code lives in
// GeneratedCode; the trace metadata lives on the row so the
// "what triggered this test" link is always one query away.
//
// Lifecycle: ops-service publishes ops.trace.captured_for_regression →
// runtime-service generates code via foundry → row is created →
// runtime publishes runtime.regression_test.created → portal renders
// the new row in the regression-test list. When the test runs (via the
// normal TestRun flow), TestRunID is back-filled.
type RegressionTestTemplate struct {
	ID             uuid.UUID  `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID  `json:"organization_id" gorm:"type:uuid;not null;index"`
	TraceID        string     `json:"trace_id" gorm:"type:varchar(128);not null;index"`
	ServiceID      string     `json:"service_id" gorm:"type:varchar(128);not null;index"`
	Language       Language   `json:"language" gorm:"type:varchar(20);not null"`
	Framework      string     `json:"framework" gorm:"type:varchar(40);not null"`
	GeneratedCode  string     `json:"generated_code" gorm:"type:text;not null"`
	TraceSummary   string     `json:"trace_summary,omitempty" gorm:"type:text"`
	Notes          string     `json:"notes,omitempty" gorm:"type:text"`
	TestRunID      *uuid.UUID `json:"test_run_id,omitempty" gorm:"type:uuid;index"`
	CreatedAt      time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt      time.Time  `json:"updated_at" gorm:"not null"`
}

// TableName pins the GORM table name.
func (RegressionTestTemplate) TableName() string {
	return "regression_test_templates"
}
