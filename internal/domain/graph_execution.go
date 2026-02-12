package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphExecutionStatus represents the status of a graph execution
type GraphExecutionStatus string

const (
	GraphExecPending   GraphExecutionStatus = "pending"
	GraphExecRunning   GraphExecutionStatus = "running"
	GraphExecCompleted GraphExecutionStatus = "completed"
	GraphExecFailed    GraphExecutionStatus = "failed"
	GraphExecCancelled GraphExecutionStatus = "cancelled"
	GraphExecTimeout   GraphExecutionStatus = "timeout"
)

// IsValid checks if the graph execution status is valid
func (s GraphExecutionStatus) IsValid() bool {
	switch s {
	case GraphExecPending, GraphExecRunning, GraphExecCompleted,
		GraphExecFailed, GraphExecCancelled, GraphExecTimeout:
		return true
	}
	return false
}

// IsTerminal returns true if the status is a terminal state
func (s GraphExecutionStatus) IsTerminal() bool {
	switch s {
	case GraphExecCompleted, GraphExecFailed, GraphExecCancelled, GraphExecTimeout:
		return true
	}
	return false
}

// GraphExecution represents a single execution of a graph definition
type GraphExecution struct {
	ID             uuid.UUID            `json:"id" gorm:"type:uuid;primary_key"`
	GraphID        uuid.UUID            `json:"graph_id" gorm:"type:uuid;not null;index"`
	OrganizationID uuid.UUID            `json:"organization_id" gorm:"type:uuid;not null;index"`
	RequestedBy    uuid.UUID            `json:"requested_by" gorm:"type:uuid;not null"`
	Status         GraphExecutionStatus `json:"status" gorm:"type:varchar(20);not null;default:'pending';index"`
	Input          JSONMap              `json:"input,omitempty" gorm:"type:jsonb"`
	Output         JSONMap              `json:"output,omitempty" gorm:"type:jsonb"`
	Error          string               `json:"error,omitempty" gorm:"type:text"`
	TotalNodes     int                  `json:"total_nodes" gorm:"not null;default:0"`
	CompletedNodes int                  `json:"completed_nodes" gorm:"not null;default:0"`
	DebugMode      bool                 `json:"debug_mode" gorm:"not null;default:false"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	CompletedAt    *time.Time           `json:"completed_at,omitempty"`
	DurationMS     *int64               `json:"duration_ms,omitempty"`
	CreatedAt      time.Time            `json:"created_at" gorm:"not null"`
	UpdatedAt      time.Time            `json:"updated_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (GraphExecution) TableName() string { return "graph_executions" }

// MarkRunning transitions the graph execution to running state
func (e *GraphExecution) MarkRunning() {
	now := time.Now().UTC()
	e.Status = GraphExecRunning
	e.StartedAt = &now
	e.UpdatedAt = now
}

// MarkCompleted transitions the graph execution to completed state
func (e *GraphExecution) MarkCompleted(output JSONMap, completedNodes int) {
	now := time.Now().UTC()
	e.Status = GraphExecCompleted
	e.Output = output
	e.CompletedNodes = completedNodes
	e.CompletedAt = &now
	e.UpdatedAt = now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// MarkFailed transitions the graph execution to failed state
func (e *GraphExecution) MarkFailed(errMsg string, completedNodes int) {
	now := time.Now().UTC()
	e.Status = GraphExecFailed
	e.Error = errMsg
	e.CompletedNodes = completedNodes
	e.CompletedAt = &now
	e.UpdatedAt = now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// MarkCancelled transitions the graph execution to cancelled state
func (e *GraphExecution) MarkCancelled(completedNodes int) {
	now := time.Now().UTC()
	e.Status = GraphExecCancelled
	e.CompletedNodes = completedNodes
	e.CompletedAt = &now
	e.UpdatedAt = now
	if e.StartedAt != nil {
		dur := now.Sub(*e.StartedAt).Milliseconds()
		e.DurationMS = &dur
	}
}

// IsTerminal returns true if the execution is in a terminal state
func (e *GraphExecution) IsTerminal() bool {
	return e.Status.IsTerminal()
}
