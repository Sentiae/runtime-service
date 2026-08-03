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
	// A `pending` phase used to be declared here and accepted by IsValid, but no
	// code ever wrote it: a resource is born straight into `provisioning`, inside the
	// same call that records the claim, so there is no moment at which the claim
	// exists and backend work has not started. A phase that exists in the type but
	// never in the data is a reader trap — it invites a caller to branch on a state
	// the fleet cannot produce — so it is gone rather than kept as documentation of
	// an intent nothing implements (D-046).
	//
	// FleetResourcePhaseProvisioning — the backend is being materialized. It is the
	// BIRTH phase.
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
	case FleetResourcePhaseProvisioning,
		FleetResourcePhaseReady, FleetResourcePhaseDegraded,
		FleetResourcePhaseRestoring, FleetResourcePhaseFailed,
		FleetResourcePhaseDecommissioned:
		return true
	}
	return false
}

// FleetResourceInitialGeneration is the generation a resource is born with. See
// FleetResource.Generation — it must be set explicitly on every insert.
const FleetResourceInitialGeneration = 1

// Durability is the retention promise a resource was ACCEPTED under (D-202).
// Stored and enforced, never inferred from tier after acceptance: tier is
// ISOLATION and availability_class is REPLICATION, and a promise a customer is
// sold must be recorded rather than read off a column that means something else.
//
// Migration 0025 fences the two combinations the platform cannot hold —
// dedicated is durable (not disableable), shared is TTL-reaped and therefore
// ephemeral.
type Durability string

const (
	// DurabilityDurable — the resource's data is protected: it is enrolled in a
	// snapshot cadence and its artifacts reach the durability store of record.
	DurabilityDurable Durability = "durable"
	// DurabilityEphemeral — the resource is reclaimable and nothing promises its
	// data survives it (the shared logical-database tier).
	DurabilityEphemeral Durability = "ephemeral"
)

// IsValid reports whether the durability is one the fleet recognizes.
func (d Durability) IsValid() bool {
	return d == DurabilityDurable || d == DurabilityEphemeral
}

// Protection component names (migration 0025's CHECK vocabulary, D-202/J1).
const (
	// ProtectionComponentOffsite is a PLATFORM-wide fact (scope ""): every
	// artifact a resource produces reaches the off-provider durability store of
	// record. D-212 is its sole writer; nothing in this service beats it.
	ProtectionComponentOffsite = "offsite"
	// ProtectionComponentCadence is a PER-HOST fact (scope = the fleet host's
	// UUID): the snapshot-cadence worker on that host is provably passing.
	ProtectionComponentCadence = "cadence"
)

// ProtectionScopePlatform is the scope of a platform-wide component. It is the
// EMPTY string by construction: 0025's scope-shape CHECK admits an empty scope
// only for `offsite`, which is what forbids a per-host beat from impersonating a
// platform-wide capability.
const ProtectionScopePlatform = ""

// ProtectionHeartbeat is a protection worker's liveness FACT (D-202): the accept
// gate reads it, and configuration is never consulted. A worker that died, was
// never started, or beats into a different database all present identically —
// the fact is absent and the accept refuses.
//
// It doubles as the GORM model, matching FleetResource above. ⚠ The tags must
// match migration 0025 exactly (the D-187 lesson).
type ProtectionHeartbeat struct {
	Component string    `json:"component" gorm:"column:component;type:text;primaryKey"`
	Scope     string    `json:"scope" gorm:"column:scope;type:text;primaryKey;default:''"`
	BeatenAt  time.Time `json:"beaten_at" gorm:"column:beaten_at;not null"`
	Detail    string    `json:"detail" gorm:"column:detail;type:text;not null;default:''"`
}

// TableName specifies the GORM table name.
func (ProtectionHeartbeat) TableName() string { return "fleet_protection_heartbeats" }

// IsFreshAt reports whether the beat is no older than staleness at `now`. A beat
// from the FUTURE counts as fresh: clock skew between the writer and the reader
// must not make a live worker read as dead, and the failure it would cause
// (refusing an accept) is not the safe direction of that particular unknown —
// absence already covers the dangerous one.
func (h ProtectionHeartbeat) IsFreshAt(now time.Time, staleness time.Duration) bool {
	return now.Sub(h.BeatenAt) <= staleness
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
	// Endpoint is the INTERNAL placement address ("guest-ip:5432") of the backing
	// engine — where the fleet currently reaches it. It is NOT what a customer
	// connects to and it moves whenever the workload moves; EndpointID below is
	// the permanent public identity.
	Endpoint string `json:"endpoint" gorm:"type:text;not null;default:''"`
	// EndpointID is the minted, PERMANENT customer-facing name of this resource
	// (D-190): `quiet-forest-4821`, which db-gate will serve as
	// <endpoint_id>.<region>.<zone>. Minted from crypto/rand at birth and
	// IMMUTABLE for life — a claim rename, a host move or an internal key rotation
	// must never change it, because it lives in a connection string a customer has
	// already pasted into an application.
	//
	// A POINTER on purpose: absence must be SQL NULL, never ''. Postgres does not
	// collide NULLs in a unique index, so every endpoint-less row (a shared-tier
	// claim, or a row created before D-190) coexists — whereas two '' rows would
	// be a spurious uniqueness conflict.
	//
	// ⚠ The GORM tag must match migration 0021 EXACTLY (column `endpoint_id`,
	// TEXT, nullable, unique index `fleet_resources_endpoint_id_key`): a
	// divergence here is a schema change the migration never authored, which is
	// how a security index silently reopened once (D-187).
	EndpointID *string `json:"endpoint_id,omitempty" gorm:"column:endpoint_id;type:text;uniqueIndex:fleet_resources_endpoint_id_key"`
	// Region is the region label encoded in the customer-facing name, stamped at
	// birth from config and never inferred per request (D-190). Empty on a
	// resource that has no endpoint identity.
	Region string `json:"region" gorm:"column:region;type:text;not null;default:''"`
	// Generation counts the incarnations of this resource's data. It is the fence
	// the durability sequencing needs: recovery-point / archive object prefixes are
	// GENERATION-SCOPED, so a restore or a rebuild starts a new prefix instead of
	// interleaving artifacts with the incarnation it replaced. It rides along with
	// the endpoint identity deliberately — adding it after archiving has started is
	// a repository-layout migration under live tenant data, whereas today no
	// customer holds anything.
	//
	// ⚠ Set EXPLICITLY at creation (FleetResourceInitialGeneration): GORM writes
	// every field of a struct it saves, so a zero value would be written as 0 —
	// which the migration's CHECK (generation >= 1) refuses, loudly, rather than
	// letting a generation-0 prefix exist.
	Generation int `json:"generation" gorm:"column:generation;type:int;not null;default:1"`
	// AvailabilityClass is the third axis (migration 0022): whether this resource
	// has a second, synchronously-replicating member. Independent of Tier
	// (isolation) and of durability (retention) on purpose — a promise a customer
	// is sold must be RECORDED, never inferred from a column that means something
	// else.
	//
	// ⚠ It records what was CLAIMED, not what is HELD. The evidence that the
	// promise is held is a streaming standby member row, and that machinery is
	// unbuilt; nothing may read this field as protection.
	//
	// ⚠ Set EXPLICITLY at creation (GORM writes every field it saves, so a zero
	// value would be written as '' and refused by the 0022 CHECK — loudly, which is
	// the point).
	AvailabilityClass AvailabilityClass `json:"availability_class" gorm:"column:availability_class;type:text;not null;default:'single'"`
	// SyncDegradePolicy is what an `ha` resource does when its synchronous standby
	// is gone. Same explicit-stamping rule as AvailabilityClass.
	SyncDegradePolicy SyncDegradePolicy `json:"sync_degrade_policy" gorm:"column:sync_degrade_policy;type:text;not null;default:'fail_closed'"`
	// Durability is the retention promise this resource was ACCEPTED under
	// (D-202, migration 0025). Stored, never inferred from Tier afterwards.
	//
	// ⚠ Set EXPLICITLY at creation, for the same reason as Generation and
	// AvailabilityClass: GORM writes every field of a struct it saves, so a zero
	// value would be written as '' and refused by the 0025 CHECKs — loudly, which
	// is the point.
	Durability Durability `json:"durability" gorm:"column:durability;type:text;not null"`
	// ProtectionCadenceSeconds is this resource's snapshot-cadence ENROLMENT: the
	// period the cadence worker on its host snapshots it on. NULL means cadence is
	// not attached at all (a pre-D-202 row, an ephemeral row, or a waived row where
	// it could not attach) — never 0, which the 0025 CHECK refuses because a zero
	// cadence is "no cadence" wearing a number.
	//
	// Stamped at accept from configuration, so a later config change does NOT
	// retro-apply to existing rows: per-resource enrolment is the D-202 point, and
	// a converge verb for it is a later slice.
	ProtectionCadenceSeconds *int `json:"protection_cadence_seconds,omitempty" gorm:"column:protection_cadence_seconds"`
	// ProtectionAttachedAt is when the FULL protection component set attached, in
	// the same INSERT that created the claim. NULL on waived rows and on rows that
	// predate D-202; the status path turns NULL-on-durable into a condition rather
	// than into silence.
	ProtectionAttachedAt *time.Time `json:"protection_attached_at,omitempty" gorm:"column:protection_attached_at"`
	// ProtectionWaivedBy / ProtectionWaiverReason / ProtectionWaivedAt are the
	// D-202 per-resource audited override: the ONLY way a durable resource is
	// accepted while a protection component cannot attach. All three or none (0025
	// CHECK) — a bare name is not an audit record and a bare reason is not
	// attributable. There is no configuration path to a waiver, and placement is
	// never waivable.
	ProtectionWaivedBy     string     `json:"protection_waived_by" gorm:"column:protection_waived_by;type:text;not null;default:''"`
	ProtectionWaiverReason string     `json:"protection_waiver_reason" gorm:"column:protection_waiver_reason;type:text;not null;default:''"`
	ProtectionWaivedAt     *time.Time `json:"protection_waived_at,omitempty" gorm:"column:protection_waived_at"`
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

// RecoveryPointLocations names WHERE a recovery point's blob exists — how many
// FAILURE DOMAINS hold it. It is not a status and not a progress indicator: it is
// the only answer to "would this recovery point survive the loss of the fleet
// chassis", and everything that publishes a durability number reads it.
//
// It must be stamped AT CAPTURE and can never be backfilled: the second domain's
// credential grants object read/write but NOT bucket listing (D-199 — LIST returns
// 403, verified live), so nothing can go and look. A row written without it is
// permanently ambiguous, which is why migration 0023 lands with the mirroring
// rather than after it.
type RecoveryPointLocations string

const (
	// RecoveryPointLocationsUnknown — the row predates migration 0023. It must be
	// treated as the WEAKEST class: NOT provably in a second failure domain, and
	// never counted as if it were.
	RecoveryPointLocationsUnknown RecoveryPointLocations = "unknown"
	// RecoveryPointLocationsPrimaryOnly — the blob is in the primary store and
	// NOWHERE else. Stamped at capture, BEFORE the second copy is attempted, so a
	// crash between the two can never leave a row claiming a copy that does not
	// exist. It is also the honest value on a host with no second domain wired at
	// all.
	RecoveryPointLocationsPrimaryOnly RecoveryPointLocations = "primary_only"
	// RecoveryPointLocationsSecondDomain — the second copy was written AND read
	// back and hashed to the recorded checksum. Only a CONFIRMED, verified copy
	// earns this: an optimistic stamp would be a durability claim made before the
	// durability exists.
	RecoveryPointLocationsSecondDomain RecoveryPointLocations = "primary_and_second_domain"
)

// IsValid reports whether the class is one the fleet recognizes (the vocabulary
// migration 0023's CHECK constraint fences).
func (l RecoveryPointLocations) IsValid() bool {
	switch l {
	case RecoveryPointLocationsUnknown, RecoveryPointLocationsPrimaryOnly,
		RecoveryPointLocationsSecondDomain:
		return true
	}
	return false
}

// InTwoFailureDomains reports whether this class PROVES the blob exists in two
// failure domains. Only the confirmed class does — `unknown` deliberately answers
// false, because "we did not record it" is not evidence of a second copy.
func (l RecoveryPointLocations) InTwoFailureDomains() bool {
	return l == RecoveryPointLocationsSecondDomain
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
	RestoredInPlaceOK bool `json:"restored_in_place_ok" gorm:"column:verified;not null;default:false"`
	// Locations records WHERE this blob exists — see RecoveryPointLocations. It is
	// the fact behind every "failure_domains = 2" claim, and it is a fact rather
	// than an inference from configuration precisely because a mirror can fail
	// while the snapshot succeeds.
	//
	// ⚠ The GORM tags on all four second-domain fields must match migration 0023
	// EXACTLY (columns `locations` TEXT NOT NULL DEFAULT 'unknown',
	// `second_domain_store` TEXT NOT NULL DEFAULT '', `second_domain_at`
	// TIMESTAMPTZ NULL, `second_domain_error` TEXT NOT NULL DEFAULT ''): the fleet
	// host runs AutoMigrate, so any divergence here is a schema change the
	// migration never authored (the D-187 lesson).
	Locations RecoveryPointLocations `json:"locations" gorm:"column:locations;type:text;not null;default:'unknown'"`
	// SecondDomainStore names WHICH second domain holds the copy (e.g.
	// "cloudflare-r2:sentiae-recovery-points"). Empty while none is confirmed.
	SecondDomainStore string `json:"second_domain_store,omitempty" gorm:"column:second_domain_store;type:text;not null;default:''"`
	// SecondDomainAt is when the second copy was CONFIRMED — verified, not merely
	// requested. Nil while there is none.
	SecondDomainAt *time.Time `json:"second_domain_at,omitempty" gorm:"column:second_domain_at"`
	// SecondDomainError is the last failed mirror attempt's cause. A failed mirror
	// is NOT a failed snapshot (a recovery point exists), so it lives here and not
	// on the resource's snapshot-health columns — see migration 0023.
	SecondDomainError string    `json:"second_domain_error,omitempty" gorm:"column:second_domain_error;type:text;not null;default:''"`
	CreatedAt         time.Time `json:"created_at" gorm:"not null;default:now();index:idx_fleet_resource_recovery_points_resource,priority:2"`
}

// TableName specifies the GORM table name.
func (FleetResourceRecoveryPoint) TableName() string {
	return "fleet_resource_recovery_points"
}
