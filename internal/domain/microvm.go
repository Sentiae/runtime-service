package domain

import (
	"time"

	"github.com/google/uuid"
)

// MicroVM represents a Firecracker microVM instance
type MicroVM struct {
	ID           uuid.UUID   `json:"id" gorm:"type:uuid;primary_key"`
	Status       VMStatus    `json:"status" gorm:"type:varchar(20);not null;default:'creating';index"`
	VCPU         int         `json:"vcpu" gorm:"not null;default:1"`
	MemoryMB     int         `json:"memory_mb" gorm:"not null;default:128"`
	KernelPath   string      `json:"kernel_path" gorm:"type:varchar(500);not null"`
	RootfsPath   string      `json:"rootfs_path" gorm:"type:varchar(500);not null"`
	SocketPath   string      `json:"socket_path" gorm:"type:varchar(500)"`
	PID          *int        `json:"pid,omitempty"`
	IPAddress    string      `json:"ip_address,omitempty" gorm:"type:varchar(45)"`
	NetworkMode  NetworkMode `json:"network_mode" gorm:"type:varchar(20);not null;default:'isolated'"`
	Language     Language    `json:"language" gorm:"type:varchar(20);not null"`
	BootTimeMS   *int64      `json:"boot_time_ms,omitempty"`
	ExecutionID  *uuid.UUID  `json:"execution_id,omitempty" gorm:"type:uuid;index"`
	CreatedAt    time.Time   `json:"created_at" gorm:"not null"`
	TerminatedAt *time.Time  `json:"terminated_at,omitempty"`
}

// TableName specifies the table name for GORM
func (MicroVM) TableName() string {
	return "micro_vms"
}

// Validate performs validation on the microVM
func (vm *MicroVM) Validate() error {
	if vm.ID == uuid.Nil {
		return ErrInvalidID
	}
	if !vm.Status.IsValid() {
		return ErrInvalidData
	}
	if vm.VCPU < 1 {
		return ErrInvalidVCPU
	}
	if vm.MemoryMB < 32 {
		return ErrInvalidMemory
	}
	if !vm.Language.IsValid() {
		return ErrInvalidLanguage
	}
	return nil
}

// IsAvailable returns true if the VM is ready to execute code
func (vm *MicroVM) IsAvailable() bool {
	return vm.Status == VMStatusReady && vm.ExecutionID == nil
}

// AssignExecution assigns an execution to this VM
func (vm *MicroVM) AssignExecution(executionID uuid.UUID) {
	vm.Status = VMStatusRunning
	vm.ExecutionID = &executionID
}

// Release returns the VM to the ready pool
func (vm *MicroVM) Release() {
	vm.Status = VMStatusReady
	vm.ExecutionID = nil
}

// Terminate marks the VM as terminated
func (vm *MicroVM) Terminate() {
	now := time.Now().UTC()
	vm.Status = VMStatusTerminated
	vm.TerminatedAt = &now
}
