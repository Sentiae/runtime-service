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
	ID              uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	ComponentID     string    `json:"component_id" gorm:"type:varchar(255);not null;index;uniqueIndex:fleet_apps_component_env_owner_key"`
	Env             string    `json:"env" gorm:"type:varchar(64);not null;uniqueIndex:fleet_apps_component_env_owner_key"`
	ImageRepository string    `json:"image_repository" gorm:"type:varchar(512);not null"`
	ImageDigest     string    `json:"image_digest" gorm:"type:varchar(255);not null"`
	// OwnerOrg is the attested tenant (org uuid) that owns this app's secrets
	// (D-069). The resident replica runtime scopes every secret_ref resolution to
	// it (I28): secrets resolve only under this org's per-tenant KEK. It is now
	// REQUIRED — ProvisionApp refuses an empty org outright rather than accepting
	// an unscoped row (see the rationale there).
	//
	// ⚠ OwnerOrg is the THIRD column of the app's uniqueness key, alongside
	// ComponentID and Env (migrations/0014). The index name here MUST stay equal to
	// the one 0014 creates: the fleet host runs with APP_DATABASE_AUTO_MIGRATE=true,
	// so a name or column-set mismatch makes AutoMigrate build a second, org-blind
	// index and silently reopen #two-orgs-same-claim-key-share-one-database.
	OwnerOrg string `json:"owner_org" gorm:"type:text;not null;default:'';uniqueIndex:fleet_apps_component_env_owner_key"`
	// SystemID binds this app to a P21 fleet network (CP4.5 §9 #5, D-164). It is
	// the opaque scope key delivery resolves from catalog — the fleet stores and
	// compares it, never dereferences it. Non-empty requires an ACTIVE
	// fleet_networks row for (SystemID, Env); a provision naming an unknown
	// network is rejected, never auto-created. Empty = no network membership =
	// this app reaches NO fleet peer (the SNT-XVM terminal DROP governs), which is
	// exactly the pre-#5 behavior.
	SystemID        string        `json:"system_id" gorm:"type:varchar(255);not null;default:'';index"`
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
	SecretRefs []string `json:"secret_refs,omitempty" gorm:"type:jsonb;serializer:json"`
	// IdleTTLSeconds is the inactivity window before a scale-to-zero app drains to
	// zero replicas (rt#11, D-082). 0 disables idle scale-down.
	IdleTTLSeconds int `json:"idle_ttl_seconds" gorm:"column:idle_ttl_seconds;not null;default:0"`
	// LastActiveAt is the app's last observed activity: stamped at provision and
	// refreshed by the activator on each wake. SweepIdle scales the app to zero
	// once now-LastActiveAt exceeds IdleTTLSeconds (rt#11, D-082).
	LastActiveAt time.Time `json:"last_active_at" gorm:"column:last_active_at;not null;default:now()"`
	CreatedAt    time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt    time.Time `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (FleetApp) TableName() string {
	return "fleet_apps"
}
