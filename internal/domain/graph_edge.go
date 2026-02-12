package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphEdge represents a directed connection between two nodes in a graph
type GraphEdge struct {
	ID            uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	GraphID       uuid.UUID `json:"graph_id" gorm:"type:uuid;not null;index"`
	SourceNodeID  uuid.UUID `json:"source_node_id" gorm:"type:uuid;not null;index"`
	TargetNodeID  uuid.UUID `json:"target_node_id" gorm:"type:uuid;not null;index"`
	SourcePort    string    `json:"source_port" gorm:"type:varchar(100);not null;default:'output'"`
	TargetPort    string    `json:"target_port" gorm:"type:varchar(100);not null;default:'input'"`
	ConditionExpr string    `json:"condition_expr,omitempty" gorm:"type:text"`
	CreatedAt     time.Time `json:"created_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (GraphEdge) TableName() string { return "graph_edges" }

// Validate performs validation on the graph edge
func (e *GraphEdge) Validate() error {
	if e.ID == uuid.Nil {
		return ErrInvalidID
	}
	if e.GraphID == uuid.Nil {
		return ErrInvalidID
	}
	if e.SourceNodeID == uuid.Nil || e.TargetNodeID == uuid.Nil {
		return ErrInvalidID
	}
	if e.SourceNodeID == e.TargetNodeID {
		return ErrGraphHasCycle
	}
	return nil
}
