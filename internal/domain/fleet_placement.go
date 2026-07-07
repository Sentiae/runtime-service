package domain

import (
	"time"

	"github.com/google/uuid"
)

// PlacementConstraint is the scheduling constraint that produced a placement.
type PlacementConstraint string

const (
	PlacementConstraintBinPack  PlacementConstraint = "bin_pack"
	PlacementConstraintSpread   PlacementConstraint = "spread"
	PlacementConstraintAffinity PlacementConstraint = "affinity"
)

// IsValid reports whether the constraint is one the scheduler recognizes.
func (c PlacementConstraint) IsValid() bool {
	switch c {
	case PlacementConstraintBinPack, PlacementConstraintSpread, PlacementConstraintAffinity:
		return true
	}
	return false
}

// Placement records the host a replica was scheduled onto and the constraint
// that decided it. It doubles as the GORM model (see ImageWorkload). DDL is
// owned by golang-migrate (migrations/), not AutoMigrate.
type Placement struct {
	ReplicaID      uuid.UUID           `json:"replica_id" gorm:"type:uuid;primary_key"`
	HostID         uuid.UUID           `json:"host_id" gorm:"type:uuid;not null"`
	ConstraintType PlacementConstraint `json:"constraint_type" gorm:"type:varchar(20);not null;default:'bin_pack'"`
	CreatedAt      time.Time           `json:"created_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Placement) TableName() string {
	return "fleet_placements"
}
