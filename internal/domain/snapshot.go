package domain

import (
	"time"

	"github.com/google/uuid"
)

// SnapshotKind discriminates between snapshots created by the user
// ("manual") and snapshots taken automatically by the checkpoint
// scheduler ("checkpoint"). Base-image snapshots stay independent on the
// existing IsBaseImage flag.
type SnapshotKind string

const (
	SnapshotKindManual     SnapshotKind = "manual"
	SnapshotKindCheckpoint SnapshotKind = "checkpoint"
)

// Snapshot represents a Firecracker microVM snapshot for fast restore
type Snapshot struct {
	ID             uuid.UUID  `json:"id" gorm:"type:uuid;primary_key"`
	VMID           uuid.UUID  `json:"vm_id" gorm:"type:uuid;not null;index"`
	ExecutionID    *uuid.UUID `json:"execution_id,omitempty" gorm:"type:uuid;index"`
	Language       Language   `json:"language" gorm:"type:varchar(20);not null;index"`
	MemoryFilePath string     `json:"memory_file_path" gorm:"type:varchar(500);not null"`
	StateFilePath  string     `json:"state_file_path" gorm:"type:varchar(500);not null"`
	SizeBytes      int64      `json:"size_bytes" gorm:"not null"`
	VCPU           int        `json:"vcpu" gorm:"not null"`
	MemoryMB       int        `json:"memory_mb" gorm:"not null"`
	Description    string     `json:"description,omitempty" gorm:"type:text"`
	IsBaseImage    bool       `json:"is_base_image" gorm:"not null;default:false;index"`
	// Kind distinguishes user-triggered snapshots from automatic
	// checkpoints. Indexed so the scheduler can quickly find "the latest
	// checkpoint for VM X" without scanning manual snapshots.
	Kind          SnapshotKind `json:"kind" gorm:"type:varchar(20);not null;default:'manual';index:idx_snap_vm_kind,priority:2"`
	RestoreTimeMS *int64       `json:"restore_time_ms,omitempty"`
	CreatedAt     time.Time    `json:"created_at" gorm:"not null;index:idx_snap_vm_kind,priority:3"`
}

// TableName specifies the table name for GORM
func (Snapshot) TableName() string {
	return "snapshots"
}

// Validate performs validation on the snapshot
func (s *Snapshot) Validate() error {
	if s.ID == uuid.Nil {
		return ErrInvalidID
	}
	if s.VMID == uuid.Nil {
		return ErrInvalidID
	}
	if !s.Language.IsValid() {
		return ErrInvalidLanguage
	}
	if s.MemoryFilePath == "" || s.StateFilePath == "" {
		return ErrInvalidData
	}
	return nil
}
