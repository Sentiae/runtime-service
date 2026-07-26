package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ExecutionRepository defines the interface for execution persistence
type ExecutionRepository interface {
	Create(ctx context.Context, execution *domain.Execution) error
	Update(ctx context.Context, execution *domain.Execution) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Execution, error)
	FindByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error)
	FindPending(ctx context.Context, limit int) ([]domain.Execution, error)
	FindRunning(ctx context.Context) ([]domain.Execution, error)
}

// MicroVMRepository defines the interface for microVM persistence
type MicroVMRepository interface {
	Create(ctx context.Context, vm *domain.MicroVM) error
	Update(ctx context.Context, vm *domain.MicroVM) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.MicroVM, error)
	FindAvailable(ctx context.Context, language domain.Language) (*domain.MicroVM, error)
	FindByExecution(ctx context.Context, executionID uuid.UUID) (*domain.MicroVM, error)
	FindActive(ctx context.Context) ([]domain.MicroVM, error)
	CountByStatus(ctx context.Context, status domain.VMStatus) (int64, error)
}

// SnapshotRepository defines the interface for snapshot persistence
type SnapshotRepository interface {
	Create(ctx context.Context, snapshot *domain.Snapshot) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Snapshot, error)
	FindBaseByLanguage(ctx context.Context, language domain.Language) (*domain.Snapshot, error)
	FindByExecution(ctx context.Context, executionID uuid.UUID) ([]domain.Snapshot, error)
	// FindLatestCheckpointByVM returns the most recent automatic
	// checkpoint for a VM. Returns ErrSnapshotNotFound when none exists.
	FindLatestCheckpointByVM(ctx context.Context, vmID uuid.UUID) (*domain.Snapshot, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// ExecutionMetricsRepository defines the interface for metrics persistence
type ExecutionMetricsRepository interface {
	Create(ctx context.Context, metrics *domain.ExecutionMetrics) error
	FindByExecution(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error)
	FindByVM(ctx context.Context, vmID uuid.UUID, limit int) ([]domain.ExecutionMetrics, error)
}

// VMInstanceRepository defines the interface for VM instance persistence
type VMInstanceRepository interface {
	Create(ctx context.Context, instance *domain.VMInstance) error
	Update(ctx context.Context, instance *domain.VMInstance) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.VMInstance, error)
	FindAll(ctx context.Context, statusFilter *domain.VMInstanceState) ([]domain.VMInstance, error)
	FindNeedingReconciliation(ctx context.Context) ([]domain.VMInstance, error)
	FindByHost(ctx context.Context, hostID string) ([]domain.VMInstance, error)
	// FindCheckpointable returns running VMs whose checkpoint interval has
	// elapsed since the last automatic snapshot.
	FindCheckpointable(ctx context.Context) ([]domain.VMInstance, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// GraphDefinitionRepository defines the interface for graph definition persistence
type GraphDefinitionRepository interface {
	Create(ctx context.Context, graph *domain.GraphDefinition) error
	Update(ctx context.Context, graph *domain.GraphDefinition) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, error)
	FindByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.GraphDefinition, int64, error)
	FindActive(ctx context.Context, orgID uuid.UUID) ([]domain.GraphDefinition, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// GraphNodeRepository defines the interface for graph node persistence
type GraphNodeRepository interface {
	Create(ctx context.Context, node *domain.GraphNode) error
	CreateBatch(ctx context.Context, nodes []domain.GraphNode) error
	Update(ctx context.Context, node *domain.GraphNode) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphNode, error)
	FindByGraph(ctx context.Context, graphID uuid.UUID) ([]domain.GraphNode, error)
	DeleteByGraph(ctx context.Context, graphID uuid.UUID) error
}

// GraphEdgeRepository defines the interface for graph edge persistence
type GraphEdgeRepository interface {
	Create(ctx context.Context, edge *domain.GraphEdge) error
	CreateBatch(ctx context.Context, edges []domain.GraphEdge) error
	FindByGraph(ctx context.Context, graphID uuid.UUID) ([]domain.GraphEdge, error)
	DeleteByGraph(ctx context.Context, graphID uuid.UUID) error
}

// GraphExecutionRepository defines the interface for graph execution persistence
type GraphExecutionRepository interface {
	Create(ctx context.Context, exec *domain.GraphExecution) error
	Update(ctx context.Context, exec *domain.GraphExecution) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphExecution, error)
	FindByGraph(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecution, int64, error)
	FindPending(ctx context.Context, limit int) ([]domain.GraphExecution, error)
}

// NodeExecutionRepository defines the interface for per-node execution persistence
type NodeExecutionRepository interface {
	Create(ctx context.Context, exec *domain.NodeExecution) error
	Update(ctx context.Context, exec *domain.NodeExecution) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.NodeExecution, error)
	FindByGraphExecution(ctx context.Context, graphExecID uuid.UUID) ([]domain.NodeExecution, error)
}

// GraphDebugSessionRepository defines the interface for debug session persistence
type GraphDebugSessionRepository interface {
	Create(ctx context.Context, session *domain.GraphDebugSession) error
	Update(ctx context.Context, session *domain.GraphDebugSession) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphDebugSession, error)
	FindActiveByGraph(ctx context.Context, graphID uuid.UUID) (*domain.GraphDebugSession, error)
}

// GraphTraceRepository defines the interface for execution trace persistence
type GraphTraceRepository interface {
	Create(ctx context.Context, trace *domain.GraphExecutionTrace) error
	Update(ctx context.Context, trace *domain.GraphExecutionTrace) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphExecutionTrace, error)
	FindByExecution(ctx context.Context, execID uuid.UUID) (*domain.GraphExecutionTrace, error)
	FindByGraph(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecutionTrace, int64, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// GraphTraceSnapshotRepository defines the interface for trace snapshot persistence
type GraphTraceSnapshotRepository interface {
	Create(ctx context.Context, snapshot *domain.GraphTraceNodeSnapshot) error
	FindByTrace(ctx context.Context, traceID uuid.UUID) ([]domain.GraphTraceNodeSnapshot, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphTraceNodeSnapshot, error)
	DeleteByTrace(ctx context.Context, traceID uuid.UUID) error
}

// TerminalSessionRepository defines the interface for terminal session persistence
type TerminalSessionRepository interface {
	Create(ctx context.Context, session *domain.TerminalSession) error
	Update(ctx context.Context, session *domain.TerminalSession) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.TerminalSession, error)
	FindByUser(ctx context.Context, userID uuid.UUID) ([]domain.TerminalSession, error)
	FindActive(ctx context.Context) ([]domain.TerminalSession, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// ImageWorkloadRepository defines the interface for image-boot workload
// persistence (runtime-fleet CP3). Workloads booted from a compiled OCI image.
type ImageWorkloadRepository interface {
	Create(ctx context.Context, workload *domain.ImageWorkload) error
	Update(ctx context.Context, workload *domain.ImageWorkload) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.ImageWorkload, error)
	FindActive(ctx context.Context) ([]domain.ImageWorkload, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// FindByIdempotencyKey resolves an existing job by its (owner_org, key)
	// scope — the pair the unique index enforces, so a key never resolves across
	// tenants (I28). Returns ErrWorkloadNotFound when no run matches.
	FindByIdempotencyKey(ctx context.Context, ownerOrg, key string) (*domain.ImageWorkload, error)
	// IsDuplicateKey reports whether err is the unique-constraint violation
	// raised by a racing Create on the same (owner_org, idempotency_key). It
	// lives on the repository because the SQLSTATE is a persistence detail the
	// use case must not know.
	IsDuplicateKey(err error) bool
}

// HostRepository persists fleet hosts (runtime-fleet CP4 durable control plane).
type HostRepository interface {
	Create(ctx context.Context, host *domain.Host) error
	Update(ctx context.Context, host *domain.Host) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Host, error)
	List(ctx context.Context) ([]domain.Host, error)
	ListActive(ctx context.Context) ([]domain.Host, error)
	ListByStatus(ctx context.Context, status domain.HostStatus) ([]domain.Host, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// FleetAppRepository persists fleet apps (desired state per component+env).
type FleetAppRepository interface {
	Create(ctx context.Context, app *domain.FleetApp) error
	Update(ctx context.Context, app *domain.FleetApp) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.FleetApp, error)
	// FindByComponentEnv looks an app up by its FULL identity. ownerOrg is part of
	// that identity, not a filter: fleet_apps is unique on
	// (component_id, env, owner_org) and there is no RLS on this table, so an
	// org-blind lookup would hand one org's app row to another org's provision.
	FindByComponentEnv(ctx context.Context, componentID, env, ownerOrg string) (*domain.FleetApp, error)
	List(ctx context.Context) ([]domain.FleetApp, error)
	// ListBySystemEnv returns the apps that are members of one P21 fleet network
	// (CP4.5 §9 #5). An empty systemID matches NOTHING: '' means "no network
	// membership", and returning every unscoped app for it would make the resolver
	// compile rules between strangers.
	//
	// ownerOrg is part of the membership key, not a convenience filter: system_id is
	// an opaque VARCHAR the fleet never dereferences and no producer proves unique,
	// so an org-blind query makes two orgs that happen to carry the same system_id
	// network PEERS of each other. An empty ownerOrg likewise matches NOTHING — two
	// unscoped rows are not each other's tenant.
	ListBySystemEnv(ctx context.Context, systemID, env, ownerOrg string) ([]domain.FleetApp, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// FleetNetworkRepository persists P21 fleet networks (the per-system×env policy
// scope). CP4.5 §9 #5.
type FleetNetworkRepository interface {
	Create(ctx context.Context, n *domain.FleetNetwork) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.FleetNetwork, error)
	// FindBySystemEnv returns the network for a (system_id, env) regardless of
	// status; callers decide what a deprovisioned network means for them.
	FindBySystemEnv(ctx context.Context, systemID, env string) (*domain.FleetNetwork, error)
	ListActive(ctx context.Context) ([]domain.FleetNetwork, error)
	MarkDeprovisioned(ctx context.Context, id uuid.UUID) error
	// MarkActive revives a tombstoned scope by reusing its existing row: it flips
	// status back to 'active' so a re-EnsureNetwork after Deprovision never has to
	// violate uq_fleet_networks_system_env with a second row (D-179 §807,
	// #fleet-network-revive-after-teardown). This is runtime-INTERNAL persistence,
	// not a new P21 verb.
	MarkActive(ctx context.Context, id uuid.UUID) error
}

// FleetNetworkPolicyRepository persists the compiled arch edges of a network.
type FleetNetworkPolicyRepository interface {
	// ReplaceForNetwork atomically swaps a network's COMPLETE policy set in one
	// transaction. It replaces, never merges: the caller always supplies the whole
	// desired set, and an empty set means "revoke everything".
	ReplaceForNetwork(ctx context.Context, networkID uuid.UUID, ps []domain.FleetNetworkPolicy) error
	ListForNetwork(ctx context.Context, networkID uuid.UUID) ([]domain.FleetNetworkPolicy, error)
}

// ReplicaRepository persists resident replicas (actual state per app).
type ReplicaRepository interface {
	Create(ctx context.Context, replica *domain.Replica) error
	Update(ctx context.Context, replica *domain.Replica) error
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Replica, error)
	ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Replica, error)
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Replica, error)
	ListByState(ctx context.Context, state domain.ReplicaState) ([]domain.Replica, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// NetLeaseRepository persists microVM addressing leases — the durable
// allocations of the /30, uid, TAP and jail id every microVM runs under (see
// domain/fleet_net_lease.go and migrations/0020).
//
// It is the SERIALIZATION POINT of the whole plane: Acquire's INSERT either wins
// a unique index or is refused, so there is no in-process lock to get wrong and
// no restart that forgets what is held.
type NetLeaseRepository interface {
	// Acquire inserts the lease. It returns domain.ErrNetLeaseConflict when any
	// unique fence rejects it (net_index, (host,slot), (host,uid), (host,tap),
	// (owner_kind,owner_id)) — the caller retries with the next free slot rather
	// than assuming the insert succeeded.
	Acquire(ctx context.Context, lease *domain.NetLease) error
	// UsedSlots returns the local slots this host currently holds, so the
	// allocator can pick the lowest free one. It is a hint, never a guarantee:
	// the INSERT is what decides.
	UsedSlots(ctx context.Context, hostID uuid.UUID) ([]int, error)
	// FindByOwner returns the lease held by an owner row, or
	// domain.ErrNetLeaseNotFound. This is what makes allocation idempotent per
	// owner: a retried boot re-uses its own addresses instead of taking a second
	// set and leaking the first.
	FindByOwner(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (*domain.NetLease, error)
	// ListByHost returns every lease held on a host — the input to the boot-time
	// reconcile.
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.NetLease, error)
	// Release deletes an owner's lease. Idempotent: releasing a lease that is
	// already gone is a success, because teardown must never be blockable.
	Release(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error
	// EnsureHostOrdinal assigns the host the lowest free net_ordinal and returns
	// it, or returns the one it already has. Idempotent, and racing callers are
	// serialized by the UNIQUE index (never by a read-then-write). Returns
	// domain.ErrNetOrdinalExhausted when every ordinal is taken.
	EnsureHostOrdinal(ctx context.Context, hostID uuid.UUID) (int, error)
}

// PlacementRepository persists replica-to-host placements.
type PlacementRepository interface {
	Upsert(ctx context.Context, placement *domain.Placement) error
	FindByReplica(ctx context.Context, replicaID uuid.UUID) (*domain.Placement, error)
	Delete(ctx context.Context, replicaID uuid.UUID) error
}

// RouteRepository persists ingress routes for fleet apps.
type RouteRepository interface {
	Create(ctx context.Context, route *domain.Route) error
	ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Route, error)
	DeleteByApp(ctx context.Context, appID uuid.UUID) error
	// FindByHost resolves the route matching an ingress host (host_pattern or
	// custom_domain). Returns domain.ErrRouteNotFound when none matches. Used by
	// the scale-to-zero activator to map a woken request's host to its app (rt#11).
	FindByHost(ctx context.Context, host string) (*domain.Route, error)
}

// ResourceDurability is the per-resource protection projection the durability
// metric collector reads. It is a QUERY PROJECTION, not an entity: it exists so
// one indexed aggregate answers "when was each live resource last protected"
// instead of an N+1 walk over every resource's recovery catalog.
//
// ⚠ Both timestamps and the count are reported EXACTLY as the ledger holds them —
// nil means the fact does not exist and must never be substituted with a zero
// time. A resource that has never had a recovery point is the state this whole
// projection exists to make visible, so collapsing it into "age 0" would report
// the safest possible value for the least safe possible state.
type ResourceDurability struct {
	ResourceID uuid.UUID `gorm:"column:resource_id"`
	OwnerOrg   uuid.UUID `gorm:"column:owner_org"`
	Phase      string    `gorm:"column:phase"`
	Class      string    `gorm:"column:class"`
	Tier       string    `gorm:"column:tier"`
	// ConsecutiveSnapshotFailures / LastSnapshotSuccessAt mirror the resource's
	// snapshot-health columns (migration 0018).
	ConsecutiveSnapshotFailures int        `gorm:"column:consecutive_snapshot_failures"`
	LastSnapshotSuccessAt       *time.Time `gorm:"column:last_snapshot_success_at"`
	// LatestRecoveryPointAt is the creation time of this resource's NEWEST recovery
	// point, or nil when it has none at all. It is read from the recovery-point
	// catalog rather than from last_snapshot_success_at because the two can
	// disagree: the catalog is the artifact that would actually be restored.
	LatestRecoveryPointAt *time.Time `gorm:"column:latest_recovery_point_at"`
	// RecoveryPointCount is how many recovery points the catalog holds for this
	// resource. Zero is the alarming case and is reported as zero, not as absence.
	RecoveryPointCount int `gorm:"column:recovery_point_count"`
}

// RecoveryPointLocationFacts is one location class's slice of the recovery-point
// catalog: how many blobs are in it and how old the OLDEST of them is. A QUERY
// PROJECTION, not an entity.
//
// ⚠ It covers EVERY recovery point, including those of decommissioned resources.
// A tombstoned resource's recovery points deliberately survive it (they are what a
// restore-after-deletion uses), so a blob that exists in one place is a
// single-domain blob whether or not the claim above it is still live — filtering
// them out would hide real, restorable customer data that the loss of one machine
// would destroy.
type RecoveryPointLocationFacts struct {
	// Locations is the class (domain.RecoveryPointLocations).
	Locations string `gorm:"column:locations"`
	// Count is how many recovery points are in the class. Reported as zero, never
	// as an absent row — see ComputeRecoveryPointLocations, which fills in the
	// classes the query did not return.
	Count int `gorm:"column:count"`
	// OldestCreatedAt is the creation time of the oldest recovery point in the
	// class, or nil when the class is empty.
	OldestCreatedAt *time.Time `gorm:"column:oldest_created_at"`
}

// FleetResourceRepository persists P19 resource control-plane claims and their
// recovery points (CP4.5 §9 #3, D-164/D-183).
type FleetResourceRepository interface {
	// SaveResource upserts a resource claim by primary key. A collision on the
	// customer-facing endpoint identity (the unique index on endpoint_id, the
	// ONLY arbiter of it) is returned as domain.ErrEndpointTaken so the caller
	// re-mints and retries; every other constraint violation is returned raw.
	SaveResource(ctx context.Context, resource *domain.FleetResource) error
	// GetResourceByHandle resolves a resource by its id (handle). Returns
	// domain.ErrResourceNotFound when none matches.
	GetResourceByHandle(ctx context.Context, id uuid.UUID) (*domain.FleetResource, error)
	// FindResource is the idempotency lookup: it resolves the existing claim for
	// an (ownerOrg, claimKey, env) triple — the pair the unique index enforces.
	// Returns domain.ErrResourceNotFound when no claim matches.
	FindResource(ctx context.Context, ownerOrg uuid.UUID, claimKey, env string) (*domain.FleetResource, error)
	// FindLiveResourceByApp resolves the LIVE (non-tombstoned) resource claim
	// whose backing app is appID — the reverse of the app_id column, which
	// carries no FK. It is the app seam's guard: a decommission of an app a
	// durable resource still claims must be refused there and routed through the
	// resource's own snapshot-first teardown. Returns
	// domain.ErrResourceNotFound when no live claim points at the app, which is
	// the ordinary case (every component deploy) and not a fault.
	FindLiveResourceByApp(ctx context.Context, appID uuid.UUID) (*domain.FleetResource, error)
	// UpdateResourcePhase advances a resource's lifecycle phase. Returns
	// domain.ErrResourceNotFound when no resource matches the id.
	UpdateResourcePhase(ctx context.Context, id uuid.UUID, phase domain.FleetResourcePhase) error
	// CompareAndSwapPhase advances a resource's phase ONLY when its current phase
	// is one of `from`, and reports whether a row actually changed. This is the
	// admission gate for a destructive lifecycle operation (D-184 restore): an
	// unconditional update cannot tell a second caller that the first already
	// owns the resource.
	CompareAndSwapPhase(ctx context.Context, id uuid.UUID, from []domain.FleetResourcePhase, to domain.FleetResourcePhase) (bool, error)
	// SetResourceLastError records (or, with an empty string, clears) the terminal
	// reason of the last failed lifecycle operation.
	SetResourceLastError(ctx context.Context, id uuid.UUID, msg string) error
	// RecordSnapshotFailure records that a snapshot of this resource FAILED:
	// consecutive_snapshot_failures is incremented (atomically, in the UPDATE — two
	// concurrent failures must both count) and the failure time + cause are
	// stamped. It never touches phase or last_error: a failed recovery point is a
	// protection fact, not a lifecycle transition.
	RecordSnapshotFailure(ctx context.Context, id uuid.UUID, at time.Time, cause string) error
	// RecordSnapshotSuccess records that a snapshot CAPTURED a recovery point:
	// consecutive_snapshot_failures resets to 0 and last_snapshot_success_at is
	// stamped. Only a snapshot that produced a recovery point may call it — a
	// successful CALL over an app with no volumes captured nothing and resumed no
	// protection.
	RecordSnapshotSuccess(ctx context.Context, id uuid.UUID, at time.Time) error
	// ListResourcesByPhase returns every non-tombstoned resource currently in a
	// phase. The boot-time restore sweep uses it to find restores interrupted by
	// a restart.
	ListResourcesByPhase(ctx context.Context, phase domain.FleetResourcePhase) ([]domain.FleetResource, error)
	// ListExpiredShared returns the shared-variant resources (app_id IS NULL)
	// whose TTL has elapsed and that are not yet tombstoned — the reaper's work
	// list. Dedicated resources (app_id set) are reclaimed via their FleetApp.
	ListExpiredShared(ctx context.Context, now time.Time) ([]domain.FleetResource, error)
	// SaveRecoveryPoint records a snapshot/backup catalog entry.
	SaveRecoveryPoint(ctx context.Context, rp *domain.FleetResourceRecoveryPoint) error
	// ListRecoveryPoints returns a resource's recovery points, newest first.
	ListRecoveryPoints(ctx context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error)
	// GetRecoveryPointByRef resolves ONE recovery point by (resourceID, objectKey).
	// It filters on BOTH: a ref is never resolved globally, so a leaked or guessed
	// object key from another org's resource is not restorable. Returns
	// domain.ErrRecoveryPointNotFound when none matches.
	GetRecoveryPointByRef(ctx context.Context, resourceID uuid.UUID, objectKey string) (*domain.FleetResourceRecoveryPoint, error)
	// ListResourceDurability returns the protection projection (see
	// ResourceDurability) for every LIVE claim — one row per non-tombstoned
	// resource, including those with no recovery point at all, which are precisely
	// the rows the durability gauges have to be able to report on.
	ListResourceDurability(ctx context.Context) ([]ResourceDurability, error)
	// MarkRecoveryPointInSecondDomain records a CONFIRMED, checksum-verified copy of
	// a recovery point in a second failure domain (D-192/D-195): it sets locations
	// to primary_and_second_domain, names the store, stamps the time and clears any
	// previous failure. Returns domain.ErrRecoveryPointNotFound when no row matches.
	//
	// ⚠ One-way by contract. There is deliberately NO method that moves a row back
	// to primary_only: the second bucket cannot be enumerated (D-199 — LIST is 403)
	// and its objects cannot be deleted for 30 days (object lock), so nothing this
	// service can observe would justify retracting the claim.
	MarkRecoveryPointInSecondDomain(ctx context.Context, id uuid.UUID, store string, at time.Time) error
	// RecordRecoveryPointMirrorFailure records that the second-domain copy FAILED,
	// leaving locations untouched (primary_only). It writes only the cause and its
	// time — a failed mirror is not a failed snapshot, so it must not touch the
	// resource's snapshot-health columns.
	RecordRecoveryPointMirrorFailure(ctx context.Context, id uuid.UUID, at time.Time, cause string) error
	// ListRecoveryPointLocations aggregates the WHOLE recovery-point catalog by
	// location class (see RecoveryPointLocationFacts). It is the read behind the
	// two-domain gauges.
	ListRecoveryPointLocations(ctx context.Context) ([]RecoveryPointLocationFacts, error)
	// MarkRecoveryPointRestoredInPlace records that a restore FROM this recovery
	// point, over the resource's own volume, booted an engine that admitted a
	// client. It is not a verification drill and must never be reported as one
	// (see domain.FleetResourceRecoveryPoint.RestoredInPlaceOK).
	MarkRecoveryPointRestoredInPlace(ctx context.Context, id uuid.UUID) error
}

// VolumeRepository persists volumes for fleet apps.
type VolumeRepository interface {
	Create(ctx context.Context, volume *domain.Volume) error
	ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Volume, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Volume, error)
	Update(ctx context.Context, volume *domain.Volume) error
	Delete(ctx context.Context, id uuid.UUID) error
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error)
}
