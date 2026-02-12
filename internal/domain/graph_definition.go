package domain

import (
	"time"

	"github.com/google/uuid"
)

// GraphStatus represents the lifecycle status of a graph definition
type GraphStatus string

const (
	GraphStatusDraft    GraphStatus = "draft"
	GraphStatusActive   GraphStatus = "active"
	GraphStatusDisabled GraphStatus = "disabled"
	GraphStatusArchived GraphStatus = "archived"
)

// IsValid checks if the graph status is valid
func (s GraphStatus) IsValid() bool {
	switch s {
	case GraphStatusDraft, GraphStatusActive, GraphStatusDisabled, GraphStatusArchived:
		return true
	}
	return false
}

// GraphDefinition represents a deployable node graph that can be executed
// independently by the runtime-service.
type GraphDefinition struct {
	ID             uuid.UUID   `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID   `json:"organization_id" gorm:"type:uuid;not null;index"`
	CanvasID       *uuid.UUID  `json:"canvas_id,omitempty" gorm:"type:uuid;index"`
	WorkflowID     *uuid.UUID  `json:"workflow_id,omitempty" gorm:"type:uuid;index"`
	Name           string      `json:"name" gorm:"type:varchar(255);not null"`
	Description    string      `json:"description,omitempty" gorm:"type:text"`
	Version        int         `json:"version" gorm:"not null;default:1"`
	Status         GraphStatus `json:"status" gorm:"type:varchar(20);not null;default:'draft';index"`
	CreatedBy      uuid.UUID   `json:"created_by" gorm:"type:uuid;not null"`
	CreatedAt      time.Time   `json:"created_at" gorm:"not null"`
	UpdatedAt      time.Time   `json:"updated_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (GraphDefinition) TableName() string { return "graph_definitions" }

// Validate performs validation on the graph definition
func (g *GraphDefinition) Validate() error {
	if g.ID == uuid.Nil {
		return ErrInvalidID
	}
	if g.OrganizationID == uuid.Nil {
		return ErrInvalidID
	}
	if g.CreatedBy == uuid.Nil {
		return ErrInvalidID
	}
	if g.Name == "" {
		return ErrInvalidData
	}
	if !g.Status.IsValid() {
		return ErrInvalidData
	}
	return nil
}
