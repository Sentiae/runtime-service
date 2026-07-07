package domain

import (
	"time"

	"github.com/google/uuid"
)

// HostHealth is the observed health of a fleet host.
type HostHealth string

const (
	HostHealthUnknown   HostHealth = "unknown"
	HostHealthHealthy   HostHealth = "healthy"
	HostHealthDegraded  HostHealth = "degraded"
	HostHealthUnhealthy HostHealth = "unhealthy"
)

// IsValid reports whether the health value is one the fleet recognizes.
func (h HostHealth) IsValid() bool {
	switch h {
	case HostHealthUnknown, HostHealthHealthy, HostHealthDegraded, HostHealthUnhealthy:
		return true
	}
	return false
}

// HostStatus is the administrative status of a fleet host.
type HostStatus string

const (
	HostStatusActive         HostStatus = "active"
	HostStatusDraining       HostStatus = "draining"
	HostStatusCordoned       HostStatus = "cordoned"
	HostStatusDecommissioned HostStatus = "decommissioned"
)

// IsValid reports whether the status value is one the fleet recognizes.
func (s HostStatus) IsValid() bool {
	switch s {
	case HostStatusActive, HostStatusDraining, HostStatusCordoned, HostStatusDecommissioned:
		return true
	}
	return false
}

// Host is a machine in the Sentiae fleet that can run resident replicas. It
// doubles as the GORM model — this service persists domain structs directly
// (see ImageWorkload). DDL is owned by golang-migrate (migrations/), not
// AutoMigrate.
type Host struct {
	ID                uuid.UUID         `json:"id" gorm:"type:uuid;primary_key"`
	Region            string            `json:"region" gorm:"type:varchar(64);not null"`
	Labels            map[string]string `json:"labels,omitempty" gorm:"type:jsonb;serializer:json"`
	CapacityVCPU      int               `json:"capacity_vcpu" gorm:"column:capacity_vcpu;not null"`
	CapacityMemMB     int64             `json:"capacity_mem_mb" gorm:"not null"`
	CapacityDiskMB    int64             `json:"capacity_disk_mb" gorm:"not null"`
	AllocatableVCPU   int               `json:"allocatable_vcpu" gorm:"column:allocatable_vcpu;not null"`
	AllocatableMemMB  int64             `json:"allocatable_mem_mb" gorm:"not null"`
	AllocatableDiskMB int64             `json:"allocatable_disk_mb" gorm:"not null"`
	Health            HostHealth        `json:"health" gorm:"type:varchar(20);not null;default:'unknown';index"`
	Status            HostStatus        `json:"status" gorm:"type:varchar(20);not null;default:'active';index"`
	Endpoint          string            `json:"endpoint" gorm:"type:varchar(255);not null;default:''"`
	LastHeartbeat     *time.Time        `json:"last_heartbeat,omitempty"`
	CreatedAt         time.Time         `json:"created_at" gorm:"not null"`
	UpdatedAt         time.Time         `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Host) TableName() string {
	return "fleet_hosts"
}
