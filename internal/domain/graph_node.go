package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphNodeType represents the type of a node within a graph
type GraphNodeType string

const (
	GraphNodeTypeCode      GraphNodeType = "code"
	GraphNodeTypeTransform GraphNodeType = "transform"
	GraphNodeTypeCondition GraphNodeType = "condition"
	GraphNodeTypeHTTP      GraphNodeType = "http"
	GraphNodeTypeInput     GraphNodeType = "input"
	GraphNodeTypeOutput    GraphNodeType = "output"
)

// IsValid checks if the graph node type is valid
func (t GraphNodeType) IsValid() bool {
	switch t {
	case GraphNodeTypeCode, GraphNodeTypeTransform, GraphNodeTypeCondition,
		GraphNodeTypeHTTP, GraphNodeTypeInput, GraphNodeTypeOutput:
		return true
	}
	return false
}

// GraphNode represents a single node within a graph definition
type GraphNode struct {
	ID        uuid.UUID     `json:"id" gorm:"type:uuid;primary_key"`
	GraphID   uuid.UUID     `json:"graph_id" gorm:"type:uuid;not null;index"`
	NodeType  GraphNodeType `json:"node_type" gorm:"type:varchar(50);not null"`
	Name      string        `json:"name" gorm:"type:varchar(255);not null"`
	Config    JSONMap       `json:"config" gorm:"type:jsonb"`
	Language  *Language     `json:"language,omitempty" gorm:"type:varchar(20)"`
	Code      string        `json:"code,omitempty" gorm:"type:text"`
	Resources ResourceLimit `json:"resources" gorm:"embedded;embeddedPrefix:resource_"`
	Position  JSONMap       `json:"position" gorm:"type:jsonb"`
	SortOrder int           `json:"sort_order" gorm:"not null;default:0"`
	CreatedAt time.Time     `json:"created_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (GraphNode) TableName() string { return "graph_nodes" }

// Validate performs validation on the graph node
func (n *GraphNode) Validate() error {
	if n.ID == uuid.Nil {
		return ErrInvalidID
	}
	if n.GraphID == uuid.Nil {
		return ErrInvalidID
	}
	if !n.NodeType.IsValid() {
		return ErrInvalidData
	}
	if n.Name == "" {
		return ErrInvalidData
	}
	if n.NodeType == GraphNodeTypeCode {
		if n.Language == nil || !n.Language.IsValid() {
			return ErrInvalidLanguage
		}
		if n.Code == "" {
			return ErrEmptyCode
		}
	}
	return nil
}
