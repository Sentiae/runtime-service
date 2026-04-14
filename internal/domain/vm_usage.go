package domain

import (
	"time"

	"github.com/google/uuid"
)

// VMUsageRecord tracks per-execution resource usage for billing.
type VMUsageRecord struct {
	ID              uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID  uuid.UUID `json:"organization_id" gorm:"type:uuid;not null;index:idx_vm_usage_org"`
	ExecutionID     uuid.UUID `json:"execution_id" gorm:"type:uuid;not null;index"`
	Language        Language  `json:"language" gorm:"type:varchar(20);not null"`
	VCPU            int       `json:"vcpu" gorm:"not null;default:1"`
	MemoryMB        int       `json:"memory_mb" gorm:"not null;default:128"`
	ExecutionTimeMS int64     `json:"execution_time_ms" gorm:"not null;default:0"`
	BootTimeMS      int64     `json:"boot_time_ms" gorm:"not null;default:0"`
	Reused          bool      `json:"reused" gorm:"not null;default:false"` // warm pool reuse
	CreatedAt       time.Time `json:"created_at" gorm:"not null;index:idx_vm_usage_org"`
}
