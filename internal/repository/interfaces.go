package repository

import (
	"context"

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
	FindByComponentEnv(ctx context.Context, componentID, env string) (*domain.FleetApp, error)
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

// VolumeRepository persists volumes for fleet apps.
type VolumeRepository interface {
	Create(ctx context.Context, volume *domain.Volume) error
	ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Volume, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Volume, error)
	Update(ctx context.Context, volume *domain.Volume) error
	Delete(ctx context.Context, id uuid.UUID) error
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error)
}
