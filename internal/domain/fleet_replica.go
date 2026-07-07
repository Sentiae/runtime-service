package domain

import (
	"time"

	"github.com/google/uuid"
)

// ReplicaState is the lifecycle state of a resident replica.
type ReplicaState string

const (
	ReplicaStateScheduled ReplicaState = "scheduled"
	ReplicaStateBooting   ReplicaState = "booting"
	ReplicaStateResident  ReplicaState = "resident"
	ReplicaStatePaused    ReplicaState = "paused"
	ReplicaStateDraining  ReplicaState = "draining"
	ReplicaStateDead      ReplicaState = "dead"
)

// IsValid reports whether the replica state is one the fleet recognizes.
func (s ReplicaState) IsValid() bool {
	switch s {
	case ReplicaStateScheduled, ReplicaStateBooting, ReplicaStateResident,
		ReplicaStatePaused, ReplicaStateDraining, ReplicaStateDead:
		return true
	}
	return false
}

// Replica is the actual state of one resident replica of a FleetApp, placed on
// a host. It doubles as the GORM model (see ImageWorkload). DDL is owned by
// golang-migrate (migrations/), not AutoMigrate.
type Replica struct {
	ID              uuid.UUID     `json:"id" gorm:"type:uuid;primary_key"`
	AppID           uuid.UUID     `json:"app_id" gorm:"type:uuid;not null;index"`
	HostID          *uuid.UUID    `json:"host_id,omitempty" gorm:"type:uuid;index"`
	ImageRepository string        `json:"image_repository" gorm:"type:varchar(512);not null;default:''"`
	ImageDigest     string        `json:"image_digest" gorm:"type:varchar(255);not null;default:''"`
	State           ReplicaState  `json:"state" gorm:"type:varchar(20);not null;default:'scheduled';index"`
	Endpoint        string        `json:"endpoint" gorm:"type:varchar(512);not null;default:''"`
	GuestIP         string        `json:"guest_ip" gorm:"type:varchar(45);not null;default:''"`
	HostPort        int           `json:"host_port" gorm:"not null;default:0"`
	NetIndex        int           `json:"net_index" gorm:"not null;default:0"`
	PID             *int          `json:"pid,omitempty"`
	ExitCode        *int          `json:"exit_code,omitempty"`
	RestartPolicy   RestartPolicy `json:"restart_policy" gorm:"type:varchar(20);not null;default:'always'"`
	Message         string        `json:"message" gorm:"type:text;not null;default:''"`
	CreatedAt       time.Time     `json:"created_at" gorm:"not null"`
	UpdatedAt       time.Time     `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Replica) TableName() string {
	return "fleet_replicas"
}
