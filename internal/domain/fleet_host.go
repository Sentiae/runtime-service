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
	ID uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	// Region is the placement region. It is HALF of the standard-ha placement
	// invariant (different failure domain AND same region) and it must never be
	// empty: two empty regions compare equal, which would satisfy the same-region
	// half vacuously. RegisterHost refuses an empty one, and migration 0022 fences
	// it in the DDL.
	Region string `json:"region" gorm:"type:varchar(64);not null"`
	// FailureDomain is this host's structured, human-supplied statement of what it
	// shares a fate with — site/power/network (see FailureDomain). It is the single
	// fact separating HA from theatre, so it has NO DEFAULT: RegisterHost refuses a
	// host that supplies none.
	//
	// ⚠ The GORM tag must match migration 0022 EXACTLY (column `failure_domain`,
	// TEXT, NOT NULL, no default, CHECK <> ''): a divergence here would be a schema
	// change the migration never authored, which is how a security index silently
	// reopened once (D-187). Rows created before 0022 carry
	// HostFailureDomainUnattested, which does not parse and therefore never counts
	// as a domain.
	FailureDomain     string            `json:"failure_domain" gorm:"column:failure_domain;type:text;not null"`
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
	// NetOrdinal is this host's term in the microVM addressing plane — the /1024
	// block of net indices it may allocate /30s, uids and jail ids from (see
	// fleet_net_lease.go). It is a POINTER because it is genuinely unknown until
	// assigned: NULL must not read as ordinal 0, which is a real block another
	// host owns, so a host with no ordinal allocates nothing rather than
	// defaulting into someone else's addresses. UNIQUE in the DDL.
	NetOrdinal    *int       `json:"net_ordinal,omitempty" gorm:"column:net_ordinal"`
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	CreatedAt     time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt     time.Time  `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Host) TableName() string {
	return "fleet_hosts"
}
