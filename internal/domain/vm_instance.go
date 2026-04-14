package domain

import (
	"time"

	"github.com/google/uuid"
)

// VMInstanceState represents the lifecycle state of a VM instance in the control plane
type VMInstanceState string

const (
	VMInstanceStatePending    VMInstanceState = "pending"
	VMInstanceStateBooting    VMInstanceState = "booting"
	VMInstanceStateRunning    VMInstanceState = "running"
	VMInstanceStatePaused     VMInstanceState = "paused"
	VMInstanceStateTerminated VMInstanceState = "terminated"
	VMInstanceStateError      VMInstanceState = "error"
)

// IsValid checks if the VM instance state is valid
func (s VMInstanceState) IsValid() bool {
	switch s {
	case VMInstanceStatePending, VMInstanceStateBooting, VMInstanceStateRunning,
		VMInstanceStatePaused, VMInstanceStateTerminated, VMInstanceStateError:
		return true
	}
	return false
}

// IsTerminal returns true if the state is a terminal state
func (s VMInstanceState) IsTerminal() bool {
	return s == VMInstanceStateTerminated || s == VMInstanceStateError
}

// VMInstance represents a VM in the desired state store. The control plane
// tracks both the current observed state and the desired state, and the
// reconciliation controller drives convergence between them.
type VMInstance struct {
	ID           uuid.UUID       `json:"id" gorm:"type:uuid;primary_key"`
	ExecutionID  *uuid.UUID      `json:"execution_id,omitempty" gorm:"type:uuid;index"`
	HostID       string          `json:"host_id" gorm:"type:varchar(255);not null;index"`
	State        VMInstanceState `json:"state" gorm:"type:varchar(20);not null;default:'pending';index"`
	DesiredState VMInstanceState `json:"desired_state" gorm:"type:varchar(20);not null;default:'running'"`
	Language     Language        `json:"language" gorm:"type:varchar(20);not null"`
	BaseImage    string          `json:"base_image,omitempty" gorm:"type:varchar(500)"`
	VCPU         int             `json:"vcpu" gorm:"not null;default:1"`
	MemoryMB     int             `json:"memory_mb" gorm:"not null;default:128"`
	DiskMB       int             `json:"disk_mb" gorm:"not null;default:256"`
	IPAddress    string          `json:"ip_address,omitempty" gorm:"type:varchar(45)"`
	SocketPath   string          `json:"socket_path,omitempty" gorm:"type:varchar(500)"`
	PID          *int            `json:"pid,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty" gorm:"type:text"`

	// CheckpointIntervalSeconds drives the automatic checkpoint scheduler.
	// 0 (the default) disables periodic snapshots; any positive value
	// causes the scheduler to pause-snapshot-resume the VM at that
	// cadence. Long-running test VMs typically set this to 300 (5 min).
	CheckpointIntervalSeconds int        `json:"checkpoint_interval_seconds" gorm:"not null;default:0"`
	LastCheckpointAt          *time.Time `json:"last_checkpoint_at,omitempty"`

	CreatedAt    time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt    time.Time  `json:"updated_at" gorm:"not null"`
	TerminatedAt *time.Time `json:"terminated_at,omitempty"`
}

// TableName specifies the table name for GORM
func (VMInstance) TableName() string {
	return "vm_instances"
}

// Validate performs validation on the VM instance
func (v *VMInstance) Validate() error {
	if v.ID == uuid.Nil {
		return ErrInvalidID
	}
	if !v.State.IsValid() {
		return ErrInvalidData
	}
	if !v.DesiredState.IsValid() {
		return ErrInvalidData
	}
	if !v.Language.IsValid() {
		return ErrInvalidLanguage
	}
	if v.VCPU < 1 {
		return ErrInvalidVCPU
	}
	if v.MemoryMB < 32 {
		return ErrInvalidMemory
	}
	return nil
}

// NeedsReconciliation returns true if the current state does not match the desired state
func (v *VMInstance) NeedsReconciliation() bool {
	if v.State.IsTerminal() && v.DesiredState == VMInstanceStateTerminated {
		return false
	}
	return v.State != v.DesiredState
}

// SetState sets the current state and updates the timestamp
func (v *VMInstance) SetState(state VMInstanceState) {
	v.State = state
	v.UpdatedAt = time.Now().UTC()
}

// SetError sets the VM instance to the error state with a message
func (v *VMInstance) SetError(message string) {
	v.State = VMInstanceStateError
	v.ErrorMessage = message
	v.UpdatedAt = time.Now().UTC()
}

// MarkTerminated sets the VM instance to the terminated state
func (v *VMInstance) MarkTerminated() {
	now := time.Now().UTC()
	v.State = VMInstanceStateTerminated
	v.TerminatedAt = &now
	v.UpdatedAt = now
}
