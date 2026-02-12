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
