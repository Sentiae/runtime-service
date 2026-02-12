package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphExecutionTrace records the full execution trace of a graph execution
// for replay and time-travel debugging.
type GraphExecutionTrace struct {
	ID               uuid.UUID  `json:"id" gorm:"type:uuid;primary_key"`
	GraphExecutionID uuid.UUID  `json:"graph_execution_id" gorm:"type:uuid;not null;uniqueIndex"`
	GraphID          uuid.UUID  `json:"graph_id" gorm:"type:uuid;not null;index"`
	OrganizationID   uuid.UUID  `json:"organization_id" gorm:"type:uuid;not null;index"`
	Status           string     `json:"status" gorm:"type:varchar(50);not null;default:'recording'"`
	TotalNodes       int        `json:"total_nodes" gorm:"not null;default:0"`
	TotalDurationMS  *float64   `json:"total_duration_ms,omitempty"`
	TriggerData      JSONMap    `json:"trigger_data" gorm:"type:jsonb"`
	CreatedAt        time.Time  `json:"created_at" gorm:"not null"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
}

// TableName specifies the table name for GORM
func (GraphExecutionTrace) TableName() string { return "graph_execution_traces" }

// GraphTraceNodeSnapshot captures the state of a single node execution
// within a trace for replay.
type GraphTraceNodeSnapshot struct {
	ID              uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	TraceID         uuid.UUID `json:"trace_id" gorm:"type:uuid;not null;index"`
	GraphNodeID     uuid.UUID `json:"graph_node_id" gorm:"type:uuid;not null;index"`
	NodeName        string    `json:"node_name" gorm:"type:varchar(255)"`
	NodeType        string    `json:"node_type" gorm:"type:varchar(100)"`
	SequenceNumber  int       `json:"sequence_number" gorm:"not null"`
	Input           JSONMap   `json:"input" gorm:"type:jsonb"`
	Output          JSONMap   `json:"output" gorm:"type:jsonb"`
	Config          JSONMap   `json:"config" gorm:"type:jsonb"`
	Status          string    `json:"status" gorm:"type:varchar(50);not null"`
	Error           string    `json:"error,omitempty" gorm:"type:text"`
	DurationMS      *float64  `json:"duration_ms,omitempty"`
	InputSizeBytes  *int64    `json:"input_size_bytes,omitempty"`
	OutputSizeBytes *int64    `json:"output_size_bytes,omitempty"`
	StartedAt       time.Time `json:"started_at" gorm:"not null"`
	CompletedAt     time.Time `json:"completed_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (GraphTraceNodeSnapshot) TableName() string { return "graph_trace_node_snapshots" }

// GraphReplayState holds the current state of a replay session
type GraphReplayState struct {
	SessionID    uuid.UUID               `json:"session_id,omitempty"`
	TraceID      uuid.UUID               `json:"trace_id"`
	CurrentIndex int                     `json:"current_index"`
	TotalNodes   int                     `json:"total_nodes"`
	HasNext      bool                    `json:"has_next"`
	HasPrevious  bool                    `json:"has_previous"`
	IsPlaying    bool                    `json:"is_playing"`
	Snapshot     *GraphTraceNodeSnapshot `json:"snapshot,omitempty"`
}
