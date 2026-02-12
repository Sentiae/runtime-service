package domain

import (
	"time"

	"github.com/google/uuid"
)

// DebugMode represents the debugging mode for a graph execution
type DebugMode string

const (
	DebugModeStepThrough DebugMode = "step_through"
	DebugModeBreakpoint  DebugMode = "breakpoint"
)

// IsValid checks if the debug mode is valid
func (m DebugMode) IsValid() bool {
	switch m {
	case DebugModeStepThrough, DebugModeBreakpoint:
		return true
	}
	return false
}

// DebugSessionStatus represents the status of a debug session
type DebugSessionStatus string

const (
	DebugStatusCreated   DebugSessionStatus = "created"
	DebugStatusRunning   DebugSessionStatus = "running"
	DebugStatusPaused    DebugSessionStatus = "paused"
	DebugStatusCompleted DebugSessionStatus = "completed"
	DebugStatusFailed    DebugSessionStatus = "failed"
	DebugStatusCancelled DebugSessionStatus = "cancelled"
)

// IsValid checks if the debug session status is valid
func (s DebugSessionStatus) IsValid() bool {
	switch s {
	case DebugStatusCreated, DebugStatusRunning, DebugStatusPaused,
		DebugStatusCompleted, DebugStatusFailed, DebugStatusCancelled:
		return true
	}
	return false
}

// IsTerminal returns true if the debug session is in a terminal state
func (s DebugSessionStatus) IsTerminal() bool {
	switch s {
	case DebugStatusCompleted, DebugStatusFailed, DebugStatusCancelled:
		return true
	}
	return false
}

// GraphDebugSession represents a debug session for a graph execution
type GraphDebugSession struct {
	ID               uuid.UUID          `json:"id" gorm:"type:uuid;primary_key"`
	GraphExecutionID uuid.UUID          `json:"graph_execution_id" gorm:"type:uuid;not null;index"`
	GraphID          uuid.UUID          `json:"graph_id" gorm:"type:uuid;not null;index"`
	OrganizationID   uuid.UUID          `json:"organization_id" gorm:"type:uuid;not null;index"`
	UserID           uuid.UUID          `json:"user_id" gorm:"type:uuid;not null"`
	Mode             DebugMode          `json:"mode" gorm:"type:varchar(50);not null;default:'step_through'"`
	Status           DebugSessionStatus `json:"status" gorm:"type:varchar(50);not null;default:'created'"`
	CurrentNodeID    *uuid.UUID         `json:"current_node_id,omitempty" gorm:"type:uuid"`
	Breakpoints      JSONMap            `json:"breakpoints" gorm:"type:jsonb"`
	CreatedAt        time.Time          `json:"created_at" gorm:"not null"`
	UpdatedAt        time.Time          `json:"updated_at" gorm:"not null"`
	CompletedAt      *time.Time         `json:"completed_at,omitempty"`
}

// TableName specifies the table name for GORM
func (GraphDebugSession) TableName() string { return "graph_debug_sessions" }
