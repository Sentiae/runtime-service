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
	ListBySystemEnv(ctx context.Context, systemID, env string) ([]domain.FleetApp, error)
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

// FleetResourceRepository persists P19 resource control-plane claims and their
// recovery points (CP4.5 §9 #3, D-164/D-183).
type FleetResourceRepository interface {
	// SaveResource upserts a resource claim by primary key.
	SaveResource(ctx context.Context, resource *domain.FleetResource) error
	// GetResourceByHandle resolves a resource by its id (handle). Returns
	// domain.ErrResourceNotFound when none matches.
	GetResourceByHandle(ctx context.Context, id uuid.UUID) (*domain.FleetResource, error)
	// FindResource is the idempotency lookup: it resolves the existing claim for
	// an (ownerOrg, claimKey, env) triple — the pair the unique index enforces.
	// Returns domain.ErrResourceNotFound when no claim matches.
	FindResource(ctx context.Context, ownerOrg uuid.UUID, claimKey, env string) (*domain.FleetResource, error)
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
