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
	GraphNodeTypeDatabase  GraphNodeType = "database"
)

// IsValid checks if the graph node type is valid
func (t GraphNodeType) IsValid() bool {
	switch t {
	case GraphNodeTypeCode, GraphNodeTypeTransform, GraphNodeTypeCondition,
		GraphNodeTypeHTTP, GraphNodeTypeInput, GraphNodeTypeOutput, GraphNodeTypeDatabase:
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

// ResolvedCode returns the source the node should execute. A placed Code node
// carries the user's per-instance source under Config["code"] (the F1 seam:
// node Config = {language, code}); when present it WINS over the dedicated Code
// field (a shared NodeVersion stub). This makes the execution boundary honor
// the config seam regardless of how the graph node was populated.
func (n *GraphNode) ResolvedCode() string {
	if n.Config != nil {
		if c, ok := n.Config["code"].(string); ok && c != "" {
			return c
		}
	}
	return n.Code
}

// ResolvedLanguage returns the language the node should execute in, preferring
// the per-instance Config["language"] (the F1 seam) over the dedicated Language
// field. Returns nil when neither yields a value.
func (n *GraphNode) ResolvedLanguage() *Language {
	if n.Config != nil {
		if l, ok := n.Config["language"].(string); ok && l != "" {
			lang := Language(l)
			return &lang
		}
	}
	return n.Language
}

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
		// Honor the F1 config seam: a Code node may carry its language + source
		// in Config (config.language / config.code) instead of the dedicated
		// fields, so validate against the resolved values.
		lang := n.ResolvedLanguage()
		if lang == nil || !lang.IsValid() {
			return ErrInvalidLanguage
		}
		if n.ResolvedCode() == "" {
			return ErrEmptyCode
		}
	}
	return nil
}
