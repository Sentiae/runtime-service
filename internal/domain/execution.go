package domain

import (
	"time"

	"github.com/google/uuid"
)

// Execution represents a single code execution request and its lifecycle
type Execution struct {
	ID             uuid.UUID       `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID       `json:"organization_id" gorm:"type:uuid;not null;index"`
	RequestedBy    uuid.UUID       `json:"requested_by" gorm:"type:uuid;not null;index"`
	NodeID         *uuid.UUID      `json:"node_id,omitempty" gorm:"type:uuid;index"`
	WorkflowID     *uuid.UUID      `json:"workflow_id,omitempty" gorm:"type:uuid;index"`
	Language       Language        `json:"language" gorm:"type:varchar(20);not null"`
	Code           string          `json:"code" gorm:"type:text;not null"`
	Stdin          string          `json:"stdin,omitempty" gorm:"type:text"`
	Args           JSONMap         `json:"args,omitempty" gorm:"type:jsonb"`
	EnvVars        JSONMap         `json:"env_vars,omitempty" gorm:"type:jsonb"`
	Status         ExecutionStatus `json:"status" gorm:"type:varchar(20);not null;default:'pending';index"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Stdout         string          `json:"stdout,omitempty" gorm:"type:text"`
	Stderr         string          `json:"stderr,omitempty" gorm:"type:text"`
	Error          string          `json:"error,omitempty" gorm:"type:text"`
	VMID           *uuid.UUID      `json:"vm_id,omitempty" gorm:"type:uuid;index"`
	Resources      ResourceLimit   `json:"resources" gorm:"embedded;embeddedPrefix:resource_"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	DurationMS     *int64          `json:"duration_ms,omitempty"`
	CreatedAt      time.Time       `json:"created_at" gorm:"not null"`
	UpdatedAt      time.Time       `json:"updated_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (Execution) TableName() string {
	return "executions"
}

// Validate performs validation on the execution
func (e *Execution) Validate() error {
	if e.ID == uuid.Nil {
		return ErrInvalidID
	}
	if e.OrganizationID == uuid.Nil {
		return ErrInvalidID
	}
	if e.RequestedBy == uuid.Nil {
		return ErrInvalidID
	}
	if !e.Language.IsValid() {
		return ErrInvalidLanguage
	}
	if e.Code == "" {
		return ErrEmptyCode
	}
	if !e.Status.IsValid() {
		return ErrInvalidData
	}
	if err := e.Resources.Validate(); err != nil {
		return err
	}
	return nil
}

// IsTerminal returns true if the execution is in a terminal state
func (e *Execution) IsTerminal() bool {
	return e.Status.IsTerminal()
}

// MarkRunning transitions the execution to running state
func (e *Execution) MarkRunning(vmID uuid.UUID) {
	now := time.Now().UTC()
	e.Status = ExecutionStatusRunning
	e.VMID = &vmID
	e.StartedAt = &now
}

// MarkCompleted transitions the execution to completed state
func (e *Execution) MarkCompleted(exitCode int, stdout, stderr string) {
	now := time.Now().UTC()
	e.Status = ExecutionStatusCompleted
	e.ExitCode = &exitCode
	e.Stdout = stdout
	e.Stderr = stderr
	e.CompletedAt = &now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// MarkFailed transitions the execution to failed state
func (e *Execution) MarkFailed(errMsg string, exitCode int, stdout, stderr string) {
	now := time.Now().UTC()
	e.Status = ExecutionStatusFailed
	e.Error = errMsg
	e.ExitCode = &exitCode
	e.Stdout = stdout
	e.Stderr = stderr
	e.CompletedAt = &now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// MarkTimeout transitions the execution to timeout state
func (e *Execution) MarkTimeout(stdout, stderr string) {
	now := time.Now().UTC()
	e.Status = ExecutionStatusTimeout
	e.Error = "execution timeout exceeded"
	e.Stdout = stdout
	e.Stderr = stderr
	e.CompletedAt = &now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// ResourceLimit defines resource constraints for an execution
type ResourceLimit struct {
	VCPU       int           `json:"vcpu" gorm:"column:vcpu;not null;default:1"`
	MemoryMB   int           `json:"memory_mb" gorm:"column:memory_mb;not null;default:128"`
	TimeoutSec int           `json:"timeout_sec" gorm:"column:timeout_sec;not null;default:30"`
	Network    NetworkMode   `json:"network" gorm:"column:network;type:varchar(20);not null;default:'isolated'"`
	DiskMB     int           `json:"disk_mb" gorm:"column:disk_mb;not null;default:256"`
}

// Validate checks if resource limits are within acceptable bounds
func (r *ResourceLimit) Validate() error {
	if r.VCPU < 1 || r.VCPU > 8 {
		return ErrInvalidVCPU
	}
	if r.MemoryMB < 32 || r.MemoryMB > 4096 {
		return ErrInvalidMemory
	}
	if r.TimeoutSec < 1 || r.TimeoutSec > 600 {
		return ErrInvalidTimeout
	}
	if r.Network != "" && !r.Network.IsValid() {
		return ErrInvalidNetworkMode
	}
	return nil
}

// ApplyDefaults sets default values for unset fields
func (r *ResourceLimit) ApplyDefaults() {
	if r.VCPU == 0 {
		r.VCPU = 1
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = 128
	}
	if r.TimeoutSec == 0 {
		r.TimeoutSec = 30
	}
	if r.Network == "" {
		r.Network = NetworkModeIsolated
	}
	if r.DiskMB == 0 {
		r.DiskMB = 256
	}
}
