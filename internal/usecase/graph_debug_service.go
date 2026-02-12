package usecase

import (
	"context"
	"fmt"
	"log"
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

// graphDebugState holds the in-memory runtime state for an active debug session
type graphDebugState struct {
	session    *domain.GraphDebugSession
	nodes      []domain.GraphNode // topologically sorted
	edges      []domain.GraphEdge
	graphInput domain.JSONMap
	outputs    map[uuid.UUID]domain.JSONMap
	currentIdx int
	graphExec  *domain.GraphExecution

	// Channels for step control
	stepCh     chan struct{} // signal to execute next node
	continueCh chan struct{} // signal to run remaining nodes
	cancelCh   chan struct{} // signal to cancel
	doneCh     chan struct{} // closed when goroutine finishes
	stepDoneCh chan struct{} // closed after each step completes
	pauseCh    chan struct{} // signal to re-enter pause mode
}

// graphDebugService implements GraphDebugUseCase
type graphDebugService struct {
	sessionRepo    repository.GraphDebugSessionRepository
	graphRepo      repository.GraphDefinitionRepository
	nodeRepo       repository.GraphNodeRepository
	edgeRepo       repository.GraphEdgeRepository
	graphExecRepo  repository.GraphExecutionRepository
	nodeExecRepo   repository.NodeExecutionRepository
	engine         *GraphExecutionEngine
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
	engine *GraphExecutionEngine,
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
		engine:         engine,
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
		outputs:    make(map[uuid.UUID]domain.JSONMap),
		currentIdx: 0,
		graphExec:  graphExec,
		stepCh:     make(chan struct{}, 1),
		continueCh: make(chan struct{}, 1),
		cancelCh:   make(chan struct{}),
		doneCh:     make(chan struct{}),
		stepDoneCh: make(chan struct{}, 1),
		pauseCh:    make(chan struct{}, 1),
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

// StartSession starts the debug goroutine for a session
func (s *graphDebugService) StartSession(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}

	if state.session.Status != domain.DebugStatusCreated {
		return fmt.Errorf("session is not in created state")
	}

	// Mark session and execution as running
	state.session.Status = domain.DebugStatusRunning
	state.session.UpdatedAt = time.Now().UTC()
	if err := s.sessionRepo.Update(ctx, state.session); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	state.graphExec.MarkRunning()
	if err := s.graphExecRepo.Update(ctx, state.graphExec); err != nil {
		return fmt.Errorf("failed to update graph execution: %w", err)
	}

	_ = s.eventPublisher.Publish(ctx, EventGraphDebugStarted, sessionID.String(), state.session)

	// Start debug goroutine
	go s.runDebug(context.Background(), state)

	return nil
}

// StepOver executes the next node and pauses again
func (s *graphDebugService) StepOver(ctx context.Context, sessionID uuid.UUID) (*domain.GraphDebugSession, error) {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.ErrDebugSessionNotFound
	}

	if state.session.Status != domain.DebugStatusPaused {
		return nil, domain.ErrDebugSessionNotPaused
	}

	// Signal step
	select {
	case state.stepCh <- struct{}{}:
	default:
	}

	// Wait for step to complete
	select {
	case <-state.stepDoneCh:
		// Recreate stepDoneCh for next step
		state.stepDoneCh = make(chan struct{}, 1)
	case <-state.doneCh:
		// Session finished
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return state.session, nil
}

// Continue runs all remaining nodes without pausing (unless breakpoints)
func (s *graphDebugService) Continue(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}

	if state.session.Status != domain.DebugStatusPaused {
		return domain.ErrDebugSessionNotPaused
	}

	select {
	case state.continueCh <- struct{}{}:
	default:
	}

	return nil
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

	select {
	case state.pauseCh <- struct{}{}:
	default:
	}

	return nil
}

// CancelSession cancels a debug session
func (s *graphDebugService) CancelSession(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return domain.ErrDebugSessionNotFound
	}

	if state.session.Status.IsTerminal() {
		return nil // already done
	}

	close(state.cancelCh)

	// Wait for goroutine to finish
	select {
	case <-state.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}

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

// runDebug is the debug goroutine that processes nodes one at a time
func (s *graphDebugService) runDebug(ctx context.Context, state *graphDebugState) {
	defer close(state.doneCh)
	defer func() {
		s.mu.Lock()
		delete(s.sessions, state.session.ID)
		s.mu.Unlock()
	}()

	var trace *domain.GraphExecutionTrace
	if s.traceRecorder != nil {
		t, err := s.traceRecorder.StartTrace(ctx, state.graphExec.ID, state.graphExec.GraphID, state.graphExec.OrganizationID, state.graphInput)
		if err != nil {
			log.Printf("Warning: failed to start debug trace: %v", err)
		} else {
			trace = t
		}
	}

	// If step_through mode, immediately pause before first node
	if state.session.Mode == domain.DebugModeStepThrough {
		s.pauseSession(ctx, state)
	}

	continueMode := false

	for state.currentIdx < len(state.nodes) {
		node := state.nodes[state.currentIdx]

		// Check for cancel
		select {
		case <-state.cancelCh:
			s.finishDebug(ctx, state, trace, domain.DebugStatusCancelled, domain.GraphExecCancelled)
			return
		default:
		}

		// In continue mode, check if we should pause at breakpoints
		if continueMode {
			// Check for pause signal
			select {
			case <-state.pauseCh:
				continueMode = false
				s.pauseSession(ctx, state)
			default:
			}

			// Check breakpoints
			if !continueMode && s.isBreakpoint(state, node.ID) {
				s.pauseSession(ctx, state)
			} else if s.isBreakpoint(state, node.ID) {
				continueMode = false
				s.pauseSession(ctx, state)
			}
		}

		// Wait for signal if paused
		if state.session.Status == domain.DebugStatusPaused {
			select {
			case <-state.stepCh:
				// Execute one node
			case <-state.continueCh:
				continueMode = true
				state.session.Status = domain.DebugStatusRunning
				state.session.UpdatedAt = time.Now().UTC()
				_ = s.sessionRepo.Update(ctx, state.session)
			case <-state.cancelCh:
				s.finishDebug(ctx, state, trace, domain.DebugStatusCancelled, domain.GraphExecCancelled)
				return
			}
		}

		// Update current node
		state.session.CurrentNodeID = &node.ID
		state.session.UpdatedAt = time.Now().UTC()
		_ = s.sessionRepo.Update(ctx, state.session)

		// Resolve input for this node
		nodeInput := s.engine.resolveNodeInput(node.ID, state.edges, state.outputs, state.graphInput)

		// Execute node
		startedAt := time.Now().UTC()
		output, _, err := s.engine.executeNode(ctx, &node, nodeInput, state.graphExec)
		completedAt := time.Now().UTC()

		status := domain.GraphExecCompleted
		var errMsg string
		if err != nil {
			status = domain.GraphExecFailed
			errMsg = err.Error()
		}

		// Store output
		if output != nil {
			state.outputs[node.ID] = output
		}

		// Record node execution
		now := time.Now().UTC()
		nodeExec := &domain.NodeExecution{
			ID:               uuid.New(),
			GraphExecutionID: state.graphExec.ID,
			GraphNodeID:      node.ID,
			NodeType:         node.NodeType,
			NodeName:         node.Name,
			SequenceNumber:   state.currentIdx + 1,
			Status:           status,
			Input:            nodeInput,
			Output:           output,
			Error:            errMsg,
			StartedAt:        &startedAt,
			CompletedAt:      &completedAt,
			CreatedAt:        now,
		}
		if err := s.nodeExecRepo.Create(ctx, nodeExec); err != nil {
			log.Printf("Warning: failed to record debug node execution: %v", err)
		}

		// Record trace snapshot
		if trace != nil && s.traceRecorder != nil {
			_ = s.traceRecorder.RecordNode(ctx, trace, node.ID, node.Name, string(node.NodeType),
				state.currentIdx+1, nodeInput, output, node.Config, string(status), errMsg, startedAt, completedAt)
		}

		state.graphExec.CompletedNodes++

		_ = s.eventPublisher.Publish(ctx, EventGraphDebugStep, state.session.ID.String(), map[string]any{
			"session_id": state.session.ID,
			"node_id":    node.ID,
			"node_name":  node.Name,
			"status":     string(status),
			"step":       state.currentIdx + 1,
			"total":      len(state.nodes),
		})

		if err != nil {
			// Node failed — fail the session
			s.finishDebug(ctx, state, trace, domain.DebugStatusFailed, domain.GraphExecFailed)
			return
		}

		state.currentIdx++

		// In step_through mode, pause after each node
		if !continueMode && state.currentIdx < len(state.nodes) {
			s.pauseSession(ctx, state)
		}

		// Signal step complete
		select {
		case state.stepDoneCh <- struct{}{}:
		default:
		}
	}

	// All nodes completed
	s.finishDebug(ctx, state, trace, domain.DebugStatusCompleted, domain.GraphExecCompleted)
}

func (s *graphDebugService) pauseSession(ctx context.Context, state *graphDebugState) {
	state.session.Status = domain.DebugStatusPaused
	state.session.UpdatedAt = time.Now().UTC()
	_ = s.sessionRepo.Update(ctx, state.session)
	_ = s.eventPublisher.Publish(ctx, EventGraphDebugPaused, state.session.ID.String(), state.session)
}

func (s *graphDebugService) finishDebug(ctx context.Context, state *graphDebugState, trace *domain.GraphExecutionTrace, sessionStatus domain.DebugSessionStatus, execStatus domain.GraphExecutionStatus) {
	now := time.Now().UTC()
	state.session.Status = sessionStatus
	state.session.CompletedAt = &now
	state.session.UpdatedAt = now
	_ = s.sessionRepo.Update(ctx, state.session)

	// Collect final output from output-type nodes or last node
	var finalOutput domain.JSONMap
	for i := len(state.nodes) - 1; i >= 0; i-- {
		if out, ok := state.outputs[state.nodes[i].ID]; ok {
			if state.nodes[i].NodeType == domain.GraphNodeTypeOutput {
				finalOutput = out
				break
			}
			if finalOutput == nil {
				finalOutput = out
			}
		}
	}

	completed := state.graphExec.CompletedNodes
	if execStatus == domain.GraphExecCompleted {
		state.graphExec.MarkCompleted(finalOutput, completed)
	} else if execStatus == domain.GraphExecFailed {
		state.graphExec.MarkFailed("debug session failed", completed)
	} else {
		state.graphExec.MarkCancelled(completed)
	}
	_ = s.graphExecRepo.Update(ctx, state.graphExec)

	if trace != nil && s.traceRecorder != nil {
		traceStatus := "completed"
		if sessionStatus == domain.DebugStatusFailed {
			traceStatus = "failed"
		} else if sessionStatus == domain.DebugStatusCancelled {
			traceStatus = "cancelled"
		}
		_ = s.traceRecorder.CompleteTrace(ctx, trace, traceStatus)
	}

	eventType := EventGraphDebugCompleted
	if sessionStatus == domain.DebugStatusCancelled {
		eventType = EventGraphDebugCancelled
	}
	_ = s.eventPublisher.Publish(ctx, eventType, state.session.ID.String(), state.session)
}

func (s *graphDebugService) isBreakpoint(state *graphDebugState, nodeID uuid.UUID) bool {
	if state.session.Breakpoints == nil {
		return false
	}
	nodeIDs, ok := state.session.Breakpoints["node_ids"]
	if !ok {
		return false
	}
	ids, ok := nodeIDs.([]any)
	if !ok {
		return false
	}
	for _, id := range ids {
		if idStr, ok := id.(string); ok && idStr == nodeID.String() {
			return true
		}
	}
	return false
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
