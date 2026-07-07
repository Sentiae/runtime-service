package domain

import (
	"time"

	"github.com/google/uuid"
)

// Volume is a persistent volume attached to a FleetApp, optionally pinned to a
// host. It doubles as the GORM model (see ImageWorkload). DDL is owned by
// golang-migrate (migrations/), not AutoMigrate.
type Volume struct {
	ID           uuid.UUID  `json:"id" gorm:"type:uuid;primary_key"`
	AppID        uuid.UUID  `json:"app_id" gorm:"type:uuid;not null;index"`
	SizeMB       int64      `json:"size_mb" gorm:"not null"`
	HostAffinity *uuid.UUID `json:"host_affinity,omitempty" gorm:"type:uuid"`
	SnapshotRef  string     `json:"snapshot_ref" gorm:"type:varchar(255);not null;default:''"`
	MountPath    string     `json:"mount_path" gorm:"type:varchar(255);not null;default:'/data'"`
	CreatedAt    time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt    time.Time  `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Volume) TableName() string {
	return "fleet_volumes"
}
