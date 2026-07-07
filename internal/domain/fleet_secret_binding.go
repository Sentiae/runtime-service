package domain

import (
	"time"

	"github.com/google/uuid"
)

// SecretInjectMode is how a resolved secret is injected into a replica.
type SecretInjectMode string

const (
	SecretInjectModeEnv   SecretInjectMode = "env"
	SecretInjectModeMount SecretInjectMode = "mount"
)

// IsValid reports whether the inject mode is one the fleet recognizes.
func (m SecretInjectMode) IsValid() bool {
	switch m {
	case SecretInjectModeEnv, SecretInjectModeMount:
		return true
	}
	return false
}

// SecretBinding binds an external secret_ref (P14) to a FleetApp, describing how
// it is injected at boot. It doubles as the GORM model (see ImageWorkload). DDL
// is owned by golang-migrate (migrations/), not AutoMigrate.
type SecretBinding struct {
	ID        uuid.UUID        `json:"id" gorm:"type:uuid;primary_key"`
	AppID     uuid.UUID        `json:"app_id" gorm:"type:uuid;not null;index"`
	SecretRef string           `json:"secret_ref" gorm:"type:varchar(255);not null"`
	InjectAs  SecretInjectMode `json:"inject_as" gorm:"type:varchar(10);not null;default:'env'"`
	Target    string           `json:"target" gorm:"type:varchar(255);not null"`
	CreatedAt time.Time        `json:"created_at" gorm:"not null"`
	UpdatedAt time.Time        `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (SecretBinding) TableName() string {
	return "fleet_secret_bindings"
}
