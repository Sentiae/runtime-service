package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphNodeType represents the type of a node within a graph
type GraphNodeType string

const (
	// GraphNodeTypeBundle is the ONLY type a Phase 4 node may carry: the node is
	// a built, digest-pinned bundle the sandbox runs, never a snippet the
	// runtime interprets (D-9).
	GraphNodeTypeBundle GraphNodeType = "bundle"

	// The interpreter's types are RETIRED. They remain declared only so the
	// graph execution engine and the debug service still compile until the S2
	// rewrite deletes them together with their last consumers; IsValid already
	// refuses every one of them, so no new row can carry one.
	GraphNodeTypeCode      GraphNodeType = "code"
	GraphNodeTypeTransform GraphNodeType = "transform"
	GraphNodeTypeCondition GraphNodeType = "condition"
	GraphNodeTypeHTTP      GraphNodeType = "http"
	GraphNodeTypeInput     GraphNodeType = "input"
	GraphNodeTypeOutput    GraphNodeType = "output"
	GraphNodeTypeDatabase  GraphNodeType = "database"
)

// IsValid checks if the graph node type is valid. Bundle is the whole
// vocabulary: an interpreter type reaching a row is a legacy graph, and it is
// refused rather than executed approximately.
func (t GraphNodeType) IsValid() bool {
	return t == GraphNodeTypeBundle
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
	// NodeRef is the built bundle this node runs, pinned by digest. Nil only on
	// pre-Phase-4 rows, which ExecuteGraph refuses (ErrLegacyGraph).
	NodeRef *NodeRef `json:"node_ref,omitempty" gorm:"type:jsonb;serializer:json"`
	// Ports is the manifest surface, carried on the row so the execution plan is
	// rebuildable from the database alone — the runtime never re-reads a manifest.
	Ports PortSpecs `json:"ports" gorm:"type:jsonb;serializer:json"`
	Role  NodeRole  `json:"role" gorm:"type:text;not null;default:''"`
	// Secrets is what the node may ASK for. No value is ever stored here.
	Secrets []SecretSpec `json:"secrets,omitempty" gorm:"type:jsonb;serializer:json"`
	// Egress is the declared allowlist. Empty ⇒ the sandbox runs --network none.
	Egress    []string  `json:"egress,omitempty" gorm:"type:jsonb;serializer:json"`
	CreatedAt time.Time `json:"created_at" gorm:"not null"`
}

// NeedsSidecar reports whether this node's invocation must be accompanied by a
// sidecar. Secrets OR egress: a secret is answered by the sidecar's broker, and
// egress is proxied by it, so either one alone is enough.
func (n *GraphNode) NeedsSidecar() bool { return len(n.Secrets) > 0 || len(n.Egress) > 0 }

// NeedsBridge reports whether the invocation also needs its own --internal
// network. Only egress does: a secret-only node still runs --network none and
// reaches its broker over a mounted unix socket, never over IP.
func (n *GraphNode) NeedsBridge() bool { return len(n.Egress) > 0 }

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

// Validate performs validation on the graph node. A Phase 4 node is a bundle
// with a resolved pin — a row that cannot name the bundle it runs is refused
// here, one layer before anything tries to run it.
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
	if n.NodeRef == nil {
		return ErrNodeRefRequired
	}
	return nil
}
