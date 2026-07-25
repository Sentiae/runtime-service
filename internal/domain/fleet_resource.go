package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// FleetResourcePhase is the lifecycle state of a provisioned fleet resource
// (P19 control plane, CP4.5 §9 #3). Stored as text.
type FleetResourcePhase string

const (
	// FleetResourcePhasePending — the claim is recorded but backend work has not
	// started.
	FleetResourcePhasePending FleetResourcePhase = "pending"
	// FleetResourcePhaseProvisioning — the backend is being materialized.
	FleetResourcePhaseProvisioning FleetResourcePhase = "provisioning"
	// FleetResourcePhaseReady — the resource is provisioned and reachable.
	FleetResourcePhaseReady FleetResourcePhase = "ready"
	// FleetResourcePhaseDegraded — the resource exists but is impaired.
	FleetResourcePhaseDegraded FleetResourcePhase = "degraded"
	// FleetResourcePhaseRestoring — an in-place restore from a recovery point is
	// in flight (D-184). It is a STAND-OFF phase: while it holds, the resource's
	// volume refuses every boot, so a reconciler tick or an ingress wake cannot
	// open the backing file the restore is about to swap underneath it.
	FleetResourcePhaseRestoring FleetResourcePhase = "restoring"
	// FleetResourcePhaseFailed — provisioning failed terminally.
	FleetResourcePhaseFailed FleetResourcePhase = "failed"
	// FleetResourcePhaseDecommissioned — the resource has been torn down
	// (tombstoned); recovery points may still survive it.
	FleetResourcePhaseDecommissioned FleetResourcePhase = "decommissioned"
)

// IsValid reports whether the phase is one the fleet recognizes.
func (p FleetResourcePhase) IsValid() bool {
	switch p {
	case FleetResourcePhasePending, FleetResourcePhaseProvisioning,
		FleetResourcePhaseReady, FleetResourcePhaseDegraded,
		FleetResourcePhaseRestoring, FleetResourcePhaseFailed,
		FleetResourcePhaseDecommissioned:
		return true
	}
	return false
}

// FleetResource is a durable P19 resource claim (a database, cache, or queue
// backing a resident system). It doubles as the GORM model (see Volume). DDL is
// owned by golang-migrate (migrations/), not AutoMigrate. Idempotency is
// anchored on (OwnerOrg, ClaimKey, Env).
type FleetResource struct {
	ID uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	// OwnerOrg is the attested tenant (org uuid) that owns this claim (D-069/I28).
	OwnerOrg uuid.UUID `json:"owner_org" gorm:"type:uuid;not null;uniqueIndex:uq_fleet_resources_claim"`
	// ClaimKey is the caller's stable idempotency key for this resource.
	ClaimKey string `json:"claim_key" gorm:"type:text;not null;uniqueIndex:uq_fleet_resources_claim"`
	Env      string `json:"env" gorm:"type:text;not null;uniqueIndex:uq_fleet_resources_claim"`
	Revision int    `json:"revision" gorm:"not null;default:1"`
	Class    string `json:"class" gorm:"type:text;not null"`
	Tier     string `json:"tier" gorm:"type:text;not null"`
	// Phase is the resource lifecycle state.
	Phase FleetResourcePhase `json:"phase" gorm:"type:text;not null"`
	// AppID is the dedicated-variant handle: the FleetApp this resource runs as.
	// Nil for shared-variant resources (which use DBName/RoleName on a shared
	// engine instead).
	AppID *uuid.UUID `json:"app_id,omitempty" gorm:"type:uuid;index"`
	// DBName is the shared-variant logical database name (empty for dedicated).
	DBName string `json:"db_name" gorm:"column:db_name;type:text;not null;default:''"`
	// RoleName is the shared-variant role/user name (empty for dedicated).
	RoleName string `json:"role_name" gorm:"column:role_name;type:text;not null;default:''"`
	Endpoint string `json:"endpoint" gorm:"type:text;not null;default:''"`
	// SecretRefs are the resolver-resolvable engine credential refs — references
	// ONLY, never a credential value.
	SecretRefs pq.StringArray `json:"secret_refs,omitempty" gorm:"type:text[];not null;default:'{}'"`
	// SystemID binds the resource to a P21 fleet network (CP4.5 §9 #5). Opaque
	// scope key; the fleet stores and compares it, never dereferences it.
	SystemID string `json:"system_id" gorm:"column:system_id;type:text;not null;default:''"`
	// Params carries the extra claim parameters (map<string,string>) as JSONB.
	Params []byte `json:"params,omitempty" gorm:"type:jsonb"`
	// LastError is the terminal reason of the last failed lifecycle operation
	// (today: restore). It is NOT cleared by a phase change, so a resource that
	// ends in phase 'ready' after a ROLLED-BACK restore is distinguishable from
	// one whose restore succeeded — both are 'ready', only last_error differs.
	LastError string `json:"last_error" gorm:"column:last_error;type:text;not null;default:''"`
	// ExpiresAt is the reclamation deadline for a TTL'd resource; nil never
	// expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at" gorm:"not null;default:now()"`
	UpdatedAt time.Time  `json:"updated_at" gorm:"not null;default:now()"`
	// DecommissionedAt tombstones a torn-down resource; nil while live.
	DecommissionedAt *time.Time `json:"decommissioned_at,omitempty"`
}

// TableName specifies the GORM table name.
func (FleetResource) TableName() string {
	return "fleet_resources"
}

// FleetResourceRecoveryPoint is one snapshot/backup entry in a resource's
// recovery catalog. It intentionally SURVIVES a resource tombstone (no cascade
// delete) so a decommissioned resource can still be restored from.
type FleetResourceRecoveryPoint struct {
	ID         uuid.UUID  `json:"id" gorm:"type:uuid;primary_key"`
	ResourceID uuid.UUID  `json:"resource_id" gorm:"type:uuid;not null;index:idx_fleet_resource_recovery_points_resource,priority:1"`
	VolumeID   *uuid.UUID `json:"volume_id,omitempty" gorm:"type:uuid"`
	ObjectKey  string     `json:"object_key" gorm:"column:object_key;type:text;not null"`
	Kind       string     `json:"kind" gorm:"type:text;not null"`
	SizeBytes  int64      `json:"size_bytes" gorm:"column:size_bytes"`
	// Checksum is the lowercase hex sha256 of the uploaded blob, computed while
	// streaming the upload. Empty on legacy rows written before D-184: those can
	// only be size-verified on restore, and the restorer says so explicitly.
	Checksum string `json:"checksum" gorm:"column:checksum;type:text;not null;default:''"`
	// RestoredInPlaceOK records the ONE fact this system can currently prove about
	// a recovery point: it was restored IN PLACE over the resource's own volume,
	// the engine booted on those bytes, and it admitted a client. It says NOTHING
	// about the CONTENT of the restored database.
	//
	// It is deliberately NOT called `verified`. The P19 port doc reserves
	// "verified" for a G1 restore-verification DRILL (restore into a throwaway
	// target and assert the data), which does not exist yet — so `Verified` stays
	// unused here and the drill has a true place to land later. Anything shown to
	// a customer as a "verified backup" must come from the drill, never from this.
	//
	// The COLUMN is still `verified` (created by migration 0012): renaming it would
	// be a migration whose down is lossy, and the column name is not what anyone
	// reads meaning off. The name that lies is the one in the code and the API, and
	// that is the one this fixes.
	RestoredInPlaceOK bool      `json:"restored_in_place_ok" gorm:"column:verified;not null;default:false"`
	CreatedAt         time.Time `json:"created_at" gorm:"not null;default:now();index:idx_fleet_resource_recovery_points_resource,priority:2"`
}

// TableName specifies the GORM table name.
func (FleetResourceRecoveryPoint) TableName() string {
	return "fleet_resource_recovery_points"
}
