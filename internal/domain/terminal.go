package domain

import (
	"time"

	"github.com/google/uuid"
)

// TerminalSessionStatus represents the status of a terminal session
type TerminalSessionStatus string

const (
	TerminalSessionStatusCreating TerminalSessionStatus = "creating"
	TerminalSessionStatusActive   TerminalSessionStatus = "active"
	TerminalSessionStatusClosed   TerminalSessionStatus = "closed"
	TerminalSessionStatusError    TerminalSessionStatus = "error"
)

// IsValid checks if the terminal session status is valid
func (s TerminalSessionStatus) IsValid() bool {
	switch s {
	case TerminalSessionStatusCreating, TerminalSessionStatusActive,
		TerminalSessionStatusClosed, TerminalSessionStatusError:
		return true
	}
	return false
}

// TerminalSession represents an interactive terminal session connected to a VM
type TerminalSession struct {
	ID        uuid.UUID             `json:"id" gorm:"type:uuid;primary_key;default:gen_random_uuid()"`
	UserID    uuid.UUID             `json:"user_id" gorm:"type:uuid;not null;index"`
	VMID      uuid.UUID             `json:"vm_id" gorm:"type:uuid;not null"`
	Language  Language              `json:"language" gorm:"type:varchar(20);not null"`
	RepoID    *uuid.UUID            `json:"repo_id,omitempty" gorm:"type:uuid"`
	Status    TerminalSessionStatus `json:"status" gorm:"type:varchar(20);not null;default:'creating';index"`
	IPAddress string                `json:"ip_address,omitempty" gorm:"type:varchar(45)"`
	CreatedAt time.Time             `json:"created_at" gorm:"not null"`
	ClosedAt  *time.Time            `json:"closed_at,omitempty"`
}

// TableName specifies the table name for GORM
func (TerminalSession) TableName() string {
	return "terminal_sessions"
}

// Validate performs validation on the terminal session
func (ts *TerminalSession) Validate() error {
	if ts.UserID == uuid.Nil {
		return ErrInvalidID
	}
	if !ts.Language.IsValid() {
		return ErrInvalidLanguage
	}
	return nil
}

// Close marks the terminal session as closed
func (ts *TerminalSession) Close() {
	now := time.Now().UTC()
	ts.Status = TerminalSessionStatusClosed
	ts.ClosedAt = &now
}

// MarkActive marks the terminal session as active
func (ts *TerminalSession) MarkActive(vmID uuid.UUID, ipAddress string) {
	ts.VMID = vmID
	ts.IPAddress = ipAddress
	ts.Status = TerminalSessionStatusActive
}

// MarkError marks the terminal session as errored
func (ts *TerminalSession) MarkError() {
	ts.Status = TerminalSessionStatusError
}
