package domain

import (
	"time"

	"github.com/google/uuid"
)

// NodeExecution tracks the execution of a single node within a graph execution.
// For code-type nodes, ExecutionID links to the underlying Execution record.
type NodeExecution struct {
	ID               uuid.UUID            `json:"id" gorm:"type:uuid;primary_key"`
	GraphExecutionID uuid.UUID            `json:"graph_execution_id" gorm:"type:uuid;not null;index"`
	GraphNodeID      uuid.UUID            `json:"graph_node_id" gorm:"type:uuid;not null;index"`
	NodeType         GraphNodeType        `json:"node_type" gorm:"type:varchar(50);not null"`
	NodeName         string               `json:"node_name" gorm:"type:varchar(255)"`
	SequenceNumber   int                  `json:"sequence_number" gorm:"not null"`
	Status           GraphExecutionStatus `json:"status" gorm:"type:varchar(20);not null;default:'pending'"`
	Input            JSONMap              `json:"input,omitempty" gorm:"type:jsonb"`
	Output           JSONMap              `json:"output,omitempty" gorm:"type:jsonb"`
	Error            string               `json:"error,omitempty" gorm:"type:text"`
	ExecutionID      *uuid.UUID           `json:"execution_id,omitempty" gorm:"type:uuid;index"`
	StartedAt        *time.Time           `json:"started_at,omitempty"`
	CompletedAt      *time.Time           `json:"completed_at,omitempty"`
	DurationMS       *int64               `json:"duration_ms,omitempty"`
	CreatedAt        time.Time            `json:"created_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (NodeExecution) TableName() string { return "node_executions" }

// MarkRunning transitions the node execution to running state
func (n *NodeExecution) MarkRunning() {
	now := time.Now().UTC()
	n.Status = GraphExecRunning
	n.StartedAt = &now
}

// MarkCompleted transitions the node execution to completed state
func (n *NodeExecution) MarkCompleted(output JSONMap) {
	now := time.Now().UTC()
	n.Status = GraphExecCompleted
	n.Output = output
	n.CompletedAt = &now
	if n.StartedAt != nil {
		dur := now.Sub(*n.StartedAt).Milliseconds()
		n.DurationMS = &dur
	}
}

// MarkFailed transitions the node execution to failed state
func (n *NodeExecution) MarkFailed(errMsg string) {
	now := time.Now().UTC()
	n.Status = GraphExecFailed
	n.Error = errMsg
	n.CompletedAt = &now
	if n.StartedAt != nil {
		dur := now.Sub(*n.StartedAt).Milliseconds()
		n.DurationMS = &dur
	}
}
