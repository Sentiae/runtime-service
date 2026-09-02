package usecase

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// GraphDebugUseCase defines the interface for graph debug session management
type GraphDebugUseCase interface {
	CreateSession(ctx context.Context, input CreateDebugSessionInput) (*domain.GraphDebugSession, error)
	GetSession(ctx context.Context, sessionID uuid.UUID) (*domain.GraphDebugSession, error)
	StartSession(ctx context.Context, sessionID uuid.UUID) error
	StepOver(ctx context.Context, sessionID uuid.UUID) (*domain.GraphDebugSession, error)
	Continue(ctx context.Context, sessionID uuid.UUID) error
	PauseSession(ctx context.Context, sessionID uuid.UUID) error
	CancelSession(ctx context.Context, sessionID uuid.UUID) error
	SetBreakpoints(ctx context.Context, sessionID uuid.UUID, breakpoints domain.JSONMap) error
}

// CreateDebugSessionInput represents the input for creating a debug session
type CreateDebugSessionInput struct {
	GraphID        uuid.UUID
	OrganizationID uuid.UUID
	UserID         uuid.UUID
	Mode           domain.DebugMode
	Input          domain.JSONMap
	Breakpoints    domain.JSONMap
}

// graphDebugState holds the in-memory state of a created debug session. The
// STEPPER is retired (D-10), so this no longer carries a cursor or the step /
// continue / cancel channels that drove it — only what a created session is.
type graphDebugState struct {
	session    *domain.GraphDebugSession
	nodes      []domain.GraphNode // topologically sorted
	edges      []domain.GraphEdge
	graphInput domain.JSONMap
	graphExec  *domain.GraphExecution
}

// graphDebugService implements GraphDebugUseCase
type graphDebugService struct {
	sessionRepo    repository.GraphDebugSessionRepository
	graphRepo      repository.GraphDefinitionRepository
	nodeRepo       repository.GraphNodeRepository
	edgeRepo       repository.GraphEdgeRepository
	graphExecRepo  repository.GraphExecutionRepository
	nodeExecRepo   repository.NodeExecutionRepository
	eventPublisher EventPublisher
	traceRecorder  *GraphTraceRecorder

	mu       sync.Mutex
	sessions map[uuid.UUID]*graphDebugState
}

// NewGraphDebugService creates a new graph debug service
func NewGraphDebugService(
	sessionRepo repository.GraphDebugSessionRepository,
	graphRepo repository.GraphDefinitionRepository,
	nodeRepo repository.GraphNodeRepository,
	edgeRepo repository.GraphEdgeRepository,
	graphExecRepo repository.GraphExecutionRepository,
	nodeExecRepo repository.NodeExecutionRepository,
	eventPublisher EventPublisher,
	traceRecorder *GraphTraceRecorder,
) GraphDebugUseCase {
	return &graphDebugService{
		sessionRepo:    sessionRepo,
		graphRepo:      graphRepo,
		nodeRepo:       nodeRepo,
		edgeRepo:       edgeRepo,
		graphExecRepo:  graphExecRepo,
		nodeExecRepo:   nodeExecRepo,
		eventPublisher: eventPublisher,
		traceRecorder:  traceRecorder,
		sessions:       make(map[uuid.UUID]*graphDebugState),
	}
}

// CreateSession creates a new debug session for a graph
func (s *graphDebugService) CreateSession(ctx context.Context, input CreateDebugSessionInput) (*domain.GraphDebugSession, error) {
	// Verify graph exists and is active
	graph, err := s.graphRepo.FindByID(ctx, input.GraphID)
	if err != nil {
		return nil, err
	}
	if graph.Status != domain.GraphStatusActive {
		return nil, domain.ErrGraphNotActive
	}

	// Load nodes and edges
	nodes, err := s.nodeRepo.FindByGraph(ctx, input.GraphID)
	if err != nil {
		return nil, fmt.Errorf("failed to load graph nodes: %w", err)
	}
	edges, err := s.edgeRepo.FindByGraph(ctx, input.GraphID)
	if err != nil {
		return nil, fmt.Errorf("failed to load graph edges: %w", err)
	}

	// Create graph execution record for the debug session
	now := time.Now().UTC()
	graphExec := &domain.GraphExecution{
		ID:             uuid.New(),
		GraphID:        input.GraphID,
		OrganizationID: input.OrganizationID,
		RequestedBy:    input.UserID,
		Status:         domain.GraphExecPending,
		Input:          input.Input,
		TotalNodes:     len(nodes),
		DebugMode:      true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.graphExecRepo.Create(ctx, graphExec); err != nil {
		return nil, fmt.Errorf("failed to create graph execution: %w", err)
	}

	// Create debug session
	session := &domain.GraphDebugSession{
		ID:               uuid.New(),
		GraphExecutionID: graphExec.ID,
		GraphID:          input.GraphID,
		OrganizationID:   input.OrganizationID,
		UserID:           input.UserID,
		Mode:             input.Mode,
		Status:           domain.DebugStatusCreated,
		Breakpoints:      input.Breakpoints,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.sessionRepo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create debug session: %w", err)
	}

	// Topologically sort nodes for deterministic step order
	sorted := topologicalSort(nodes, edges)

	// Create in-memory state
	state := &graphDebugState{
		session:    session,
		nodes:      sorted,
		edges:      edges,
		graphInput: input.Input,
		graphExec:  graphExec,
	}

	s.mu.Lock()
	s.sessions[session.ID] = state
	s.mu.Unlock()

	_ = s.eventPublisher.Publish(ctx, EventGraphDebugCreated, session.ID.String(), session)
	return session, nil
}

// GetSession returns a debug session by ID
func (s *graphDebugService) GetSession(ctx context.Context, sessionID uuid.UUID) (*domain.GraphDebugSession, error) {
	session, err := s.sessionRepo.FindByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// StartSession is RETIRED (D-10). The stepper ran nodes through the
// interpreter's per-node executor, which no longer exists: a Phase 4 node is a
// built bundle in a sandbox, and stepping one is a different feature, not the
// same one with a pause. It refuses rather than silently doing nothing —
// T-RUN-DEBUG-SESSIONS-REBUILD owns the replacement.
func (s *graphDebugService) StartSession(context.Context, uuid.UUID) error {
	return domain.ErrGraphDebugRetired
}

// StepOver is RETIRED (D-10). See StartSession.
func (s *graphDebugService) StepOver(context.Context, uuid.UUID) (*domain.GraphDebugSession, error) {
	return nil, domain.ErrGraphDebugRetired
}

// Continue is RETIRED (D-10). See StartSession.
func (s *graphDebugService) Continue(context.Context, uuid.UUID) error {
	return domain.ErrGraphDebugRetired
}

// PauseSession re-enters pause mode during a continue run
func (s *graphDebugService) PauseSession(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}

	if state.session.Status != domain.DebugStatusRunning {
		return fmt.Errorf("session is not running")
	}
	return nil
}

// CancelSession cancels a created debug session. With the stepper retired
// there is no goroutine to join: the session is marked cancelled and dropped,
// which is what "cancel" means for something that never ran.
func (s *graphDebugService) CancelSession(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	if ok {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}
	if state.session.Status.IsTerminal() {
		return nil
	}

	now := time.Now().UTC()
	state.session.Status = domain.DebugStatusCancelled
	state.session.CompletedAt = &now
	state.session.UpdatedAt = now
	if err := s.sessionRepo.Update(ctx, state.session); err != nil {
		return fmt.Errorf("failed to cancel debug session: %w", err)
	}

	state.graphExec.MarkCancelled(state.graphExec.CompletedNodes)
	_ = s.graphExecRepo.Update(ctx, state.graphExec)
	_ = s.eventPublisher.Publish(ctx, EventGraphDebugCancelled, sessionID.String(), state.session)
	return nil
}

// SetBreakpoints updates the breakpoints for a debug session
func (s *graphDebugService) SetBreakpoints(ctx context.Context, sessionID uuid.UUID, breakpoints domain.JSONMap) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}

	state.session.Breakpoints = breakpoints
	state.session.UpdatedAt = time.Now().UTC()
	return s.sessionRepo.Update(ctx, state.session)
}

// topologicalSort returns nodes in topological order using Kahn's algorithm.
// This serializes the parallel waves into a deterministic linear order for
// step-through debugging.
func topologicalSort(nodes []domain.GraphNode, edges []domain.GraphEdge) []domain.GraphNode {
	nodeMap := make(map[uuid.UUID]*domain.GraphNode, len(nodes))
	inDegree := make(map[uuid.UUID]int, len(nodes))
	adj := make(map[uuid.UUID][]uuid.UUID)

	for i := range nodes {
		nodeMap[nodes[i].ID] = &nodes[i]
		inDegree[nodes[i].ID] = 0
	}

	for _, e := range edges {
		adj[e.SourceNodeID] = append(adj[e.SourceNodeID], e.TargetNodeID)
		inDegree[e.TargetNodeID]++
	}

	// Start with zero in-degree nodes, sorted by sort_order for determinism
	var queue []uuid.UUID
	for _, n := range nodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		return nodeMap[queue[i]].SortOrder < nodeMap[queue[j]].SortOrder
	})

	var sorted []domain.GraphNode
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, *nodeMap[id])

		// Collect and sort neighbors for determinism
		neighbors := adj[id]
		sort.Slice(neighbors, func(i, j int) bool {
			return nodeMap[neighbors[i]].SortOrder < nodeMap[neighbors[j]].SortOrder
		})

		for _, nid := range neighbors {
			inDegree[nid]--
			if inDegree[nid] == 0 {
				queue = append(queue, nid)
				// Re-sort queue to maintain order
				sort.Slice(queue, func(i, j int) bool {
					return nodeMap[queue[i]].SortOrder < nodeMap[queue[j]].SortOrder
				})
			}
		}
	}

	return sorted
}
