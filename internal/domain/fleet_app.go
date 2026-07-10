package domain

import (
	"time"

	"github.com/google/uuid"
)

// RestartPolicy governs whether a replica is restarted after it exits.
type RestartPolicy string

const (
	RestartPolicyAlways    RestartPolicy = "always"
	RestartPolicyOnFailure RestartPolicy = "on_failure"
	RestartPolicyNever     RestartPolicy = "never"
)

// IsValid reports whether the restart policy is one the fleet recognizes.
func (p RestartPolicy) IsValid() bool {
	switch p {
	case RestartPolicyAlways, RestartPolicyOnFailure, RestartPolicyNever:
		return true
	}
	return false
}

// FleetApp is the desired state of a resident application (component+env) in
// the Sentiae fleet. It doubles as the GORM model (see ImageWorkload). DDL is
// owned by golang-migrate (migrations/), not AutoMigrate.
type FleetApp struct {
	ID              uuid.UUID     `json:"id" gorm:"type:uuid;primary_key"`
	ComponentID     string        `json:"component_id" gorm:"type:varchar(255);not null;index;uniqueIndex:uq_fleet_apps_component_env"`
	Env             string        `json:"env" gorm:"type:varchar(64);not null;uniqueIndex:uq_fleet_apps_component_env"`
	ImageRepository string        `json:"image_repository" gorm:"type:varchar(512);not null"`
	ImageDigest     string        `json:"image_digest" gorm:"type:varchar(255);not null"`
	DesiredReplicas int           `json:"desired_replicas" gorm:"not null;default:1"`
	MinReplicas     int           `json:"min_replicas" gorm:"not null;default:0"`
	MaxReplicas     int           `json:"max_replicas" gorm:"not null;default:1"`
	ScaleToZero     bool          `json:"scale_to_zero" gorm:"not null;default:false"`
	Port            int           `json:"port" gorm:"not null;default:0"`
	ResourcesVCPU   int           `json:"resources_vcpu" gorm:"column:resources_vcpu;not null;default:1"`
	ResourcesMemMB  int64         `json:"resources_mem_mb" gorm:"not null;default:512"`
	RestartPolicy   RestartPolicy `json:"restart_policy" gorm:"type:varchar(20);not null;default:'always'"`
	// SecretRefs is the app's desired secret intent — the resolver-resolvable refs
	// the reconciler re-supplies to every replica boot over the vsock channel
	// (invariant I32). Real refs are still gate-rejected at provision today
	// (ErrSecretsNotSupported); this carries the desired state the resolver (P3.4)
	// will resolve into pushed secrets. ExpectSecrets is derived from it at boot.
	SecretRefs      []string      `json:"secret_refs,omitempty" gorm:"type:jsonb;serializer:json"`
	CreatedAt       time.Time     `json:"created_at" gorm:"not null"`
	UpdatedAt       time.Time     `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (FleetApp) TableName() string {
	return "fleet_apps"
}
