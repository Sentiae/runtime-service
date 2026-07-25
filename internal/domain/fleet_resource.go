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
	// ConsecutiveSnapshotFailures counts snapshot attempts that have failed IN A
	// ROW since the last one that actually captured a recovery point. The count —
	// not a boolean — is the whole point: a single failed snapshot is a blip worth
	// a log line, while a resource whose count has been climbing for a week has no
	// recent recovery point at all, and the two must not look alike. Reset to 0 by
	// a snapshot that captures something.
	ConsecutiveSnapshotFailures int `json:"consecutive_snapshot_failures" gorm:"column:consecutive_snapshot_failures;not null;default:0"`
	// LastSnapshotFailureAt / LastSnapshotError describe the most recent failed
	// snapshot. LastSnapshotError is operator-facing detail (it holds the
	// underlying cause), so it is NOT what the tenant-visible status reports — the
	// status carries a stable condition token instead.
	LastSnapshotFailureAt *time.Time `json:"last_snapshot_failure_at,omitempty" gorm:"column:last_snapshot_failure_at"`
	LastSnapshotError     string     `json:"last_snapshot_error" gorm:"column:last_snapshot_error;type:text;not null;default:''"`
	// LastSnapshotSuccessAt is when this resource last got a recovery point. Nil
	// means it has never had one recorded here. Together with the count above it is
	// what makes "protection stopped N days ago" answerable — an alert keys on the
	// AGE of this timestamp, not on the failure that happens to be freshest.
	LastSnapshotSuccessAt *time.Time `json:"last_snapshot_success_at,omitempty" gorm:"column:last_snapshot_success_at"`
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

// RecoveryPointConsistency names HOW a recovery point's artifact was made
// consistent. It is NOT a quality score and NOT a status — it is the physics of
// the capture, and it is the only thing that answers whether a given artifact is
// a valid anchor for point-in-time recovery.
//
// It must be stamped AT CAPTURE and can never be backfilled: nothing about a
// blob sitting in the artifact store reveals whether the filesystem it was read
// from was frozen at the time. A recovery point written without this is
// permanently ambiguous, which is why the column exists before any real customer
// data does.
type RecoveryPointConsistency string

const (
	// RecoveryPointGuestFrozen — the GUEST filesystem was flushed and frozen for
	// the ENTIRE capture (and the freeze's dead-man was held open throughout, with
	// a lapsed renew aborting the capture rather than producing an artifact). The
	// bytes are therefore a crash-free filesystem image: a valid PITR base.
	RecoveryPointGuestFrozen RecoveryPointConsistency = "guest_frozen"
	// RecoveryPointDetachedClean — no VM was attached and the engine that wrote
	// the volume had been STOPPED CLEANLY before the capture. Equivalent in
	// practice to a clean shutdown copy.
	//
	// ⚠ Nothing stamps this today, deliberately: a resident stop is a VMM kill
	// (#resident-stop-is-vmm-kill), so this platform cannot currently prove a
	// clean stop. The value exists so the fix has a true class to stamp, not so a
	// detached copy can be optimistically labelled.
	RecoveryPointDetachedClean RecoveryPointConsistency = "detached_clean"
	// RecoveryPointDetachedUnclean — captured with no freeze from a volume whose
	// writer was not proven to have stopped cleanly. Restoring it is restoring a
	// crashed filesystem: recoverable by engine crash recovery in the common case,
	// NOT a guarantee, and not a PITR anchor.
	RecoveryPointDetachedUnclean RecoveryPointConsistency = "detached_unclean"
	// RecoveryPointUnknown — the row predates this column (migration 0019 default).
	// It must be treated as the WEAKEST class, never as "probably fine".
	RecoveryPointUnknown RecoveryPointConsistency = "unknown"
)

// IsValid reports whether the class is one the fleet recognizes.
func (c RecoveryPointConsistency) IsValid() bool {
	switch c {
	case RecoveryPointGuestFrozen, RecoveryPointDetachedClean,
		RecoveryPointDetachedUnclean, RecoveryPointUnknown:
		return true
	}
	return false
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
	// Consistency records HOW this artifact was made consistent — see
	// RecoveryPointConsistency. `kind` says what the artifact IS; this says what it
	// GUARANTEES, and PITR needs the second one to know which anchors are valid.
	//
	// ⚠ The GORM tag must match migration 0019 EXACTLY (column `consistency`, TEXT,
	// NOT NULL, DEFAULT 'unknown'): the fleet host runs AutoMigrate, so any
	// divergence here is a schema change the migration never authored (the D-187
	// lesson).
	Consistency RecoveryPointConsistency `json:"consistency" gorm:"column:consistency;type:text;not null;default:'unknown'"`
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
