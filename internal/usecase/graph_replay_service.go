package usecase

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// GraphReplayUseCase defines the interface for graph execution replay and trace management
type GraphReplayUseCase interface {
	// Trace management
	GetTrace(ctx context.Context, traceID uuid.UUID) (*domain.GraphExecutionTrace, error)
	ListTraces(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecutionTrace, int64, error)
	DeleteTrace(ctx context.Context, traceID uuid.UUID) error
	GetTraceSnapshots(ctx context.Context, traceID uuid.UUID) ([]domain.GraphTraceNodeSnapshot, error)

	// Replay session management
	StartReplay(ctx context.Context, traceID uuid.UUID) (*domain.GraphReplayState, error)
	GetReplayState(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error)
	Forward(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error)
	Backward(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error)
	JumpTo(ctx context.Context, sessionID uuid.UUID, index int) (*domain.GraphReplayState, error)
	StopReplay(ctx context.Context, sessionID uuid.UUID) error

	// Trace comparison
	CompareTraces(ctx context.Context, traceID1, traceID2 uuid.UUID) ([]TraceComparisonStep, error)
}

// TraceComparisonStep shows the differences between two traces at a specific step
type TraceComparisonStep struct {
	SequenceNumber int                           `json:"sequence_number"`
	NodeName       string                        `json:"node_name"`
	NodeType       string                        `json:"node_type"`
	Trace1         *domain.GraphTraceNodeSnapshot `json:"trace1,omitempty"`
	Trace2         *domain.GraphTraceNodeSnapshot `json:"trace2,omitempty"`
	InputChanged   bool                          `json:"input_changed"`
	OutputChanged  bool                          `json:"output_changed"`
	StatusChanged  bool                          `json:"status_changed"`
}

// graphReplaySessionState holds in-memory state for an active replay session
type graphReplaySessionState struct {
	traceID    uuid.UUID
	snapshots  []domain.GraphTraceNodeSnapshot
	currentIdx int
}

// graphReplayService implements GraphReplayUseCase
type graphReplayService struct {
	traceRepo    repository.GraphTraceRepository
	snapshotRepo repository.GraphTraceSnapshotRepository

	mu       sync.Mutex
	sessions map[uuid.UUID]*graphReplaySessionState
}

// NewGraphReplayService creates a new graph replay service
func NewGraphReplayService(
	traceRepo repository.GraphTraceRepository,
	snapshotRepo repository.GraphTraceSnapshotRepository,
) GraphReplayUseCase {
	return &graphReplayService{
		traceRepo:    traceRepo,
		snapshotRepo: snapshotRepo,
		sessions:     make(map[uuid.UUID]*graphReplaySessionState),
	}
}

// GetTrace returns a trace by ID
func (s *graphReplayService) GetTrace(ctx context.Context, traceID uuid.UUID) (*domain.GraphExecutionTrace, error) {
	trace, err := s.traceRepo.FindByID(ctx, traceID)
	if err != nil {
		return nil, err
	}
	return trace, nil
}

// ListTraces returns traces for a graph with pagination
func (s *graphReplayService) ListTraces(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecutionTrace, int64, error) {
	return s.traceRepo.FindByGraph(ctx, graphID, limit, offset)
}

// DeleteTrace deletes a trace and its snapshots
func (s *graphReplayService) DeleteTrace(ctx context.Context, traceID uuid.UUID) error {
	if err := s.snapshotRepo.DeleteByTrace(ctx, traceID); err != nil {
		return fmt.Errorf("failed to delete trace snapshots: %w", err)
	}
	return s.traceRepo.Delete(ctx, traceID)
}

// GetTraceSnapshots returns all snapshots for a trace
func (s *graphReplayService) GetTraceSnapshots(ctx context.Context, traceID uuid.UUID) ([]domain.GraphTraceNodeSnapshot, error) {
	return s.snapshotRepo.FindByTrace(ctx, traceID)
}

// StartReplay creates a new replay session for a trace
func (s *graphReplayService) StartReplay(ctx context.Context, traceID uuid.UUID) (*domain.GraphReplayState, error) {
	// Verify trace exists
	if _, err := s.traceRepo.FindByID(ctx, traceID); err != nil {
		return nil, err
	}

	// Load snapshots
	snapshots, err := s.snapshotRepo.FindByTrace(ctx, traceID)
	if err != nil {
		return nil, fmt.Errorf("failed to load trace snapshots: %w", err)
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("trace has no snapshots")
	}

	sessionID := uuid.New()
	state := &graphReplaySessionState{
		traceID:    traceID,
		snapshots:  snapshots,
		currentIdx: 0,
	}

	s.mu.Lock()
	s.sessions[sessionID] = state
	s.mu.Unlock()

	return s.buildReplayState(sessionID, state), nil
}

// GetReplayState returns the current state of a replay session
func (s *graphReplayService) GetReplayState(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error) {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.ErrReplaySessionNotFound
	}

	return s.buildReplayState(sessionID, state), nil
}

// Forward advances the replay by one step
func (s *graphReplayService) Forward(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error) {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.ErrReplaySessionNotFound
	}

	if state.currentIdx >= len(state.snapshots)-1 {
		return nil, domain.ErrReplayIndexOutOfBounds
	}

	state.currentIdx++
	return s.buildReplayState(sessionID, state), nil
}

// Backward moves the replay back one step
func (s *graphReplayService) Backward(ctx context.Context, sessionID uuid.UUID) (*domain.GraphReplayState, error) {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.ErrReplaySessionNotFound
	}

	if state.currentIdx <= 0 {
		return nil, domain.ErrReplayIndexOutOfBounds
	}

	state.currentIdx--
	return s.buildReplayState(sessionID, state), nil
}

// JumpTo moves the replay to a specific step
func (s *graphReplayService) JumpTo(ctx context.Context, sessionID uuid.UUID, index int) (*domain.GraphReplayState, error) {
	s.mu.Lock()
	state, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.ErrReplaySessionNotFound
	}

	if index < 0 || index >= len(state.snapshots) {
		return nil, domain.ErrReplayIndexOutOfBounds
	}

	state.currentIdx = index
	return s.buildReplayState(sessionID, state), nil
}

// StopReplay removes a replay session
func (s *graphReplayService) StopReplay(ctx context.Context, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionID]; !ok {
		return domain.ErrReplaySessionNotFound
	}

	delete(s.sessions, sessionID)
	return nil
}

// CompareTraces compares two traces step-by-step
func (s *graphReplayService) CompareTraces(ctx context.Context, traceID1, traceID2 uuid.UUID) ([]TraceComparisonStep, error) {
	// Load both traces
	trace1, err := s.traceRepo.FindByID(ctx, traceID1)
	if err != nil {
		return nil, err
	}
	trace2, err := s.traceRepo.FindByID(ctx, traceID2)
	if err != nil {
		return nil, err
	}

	// Must be same graph
	if trace1.GraphID != trace2.GraphID {
		return nil, domain.ErrTracesNotComparable
	}

	// Load snapshots
	snaps1, err := s.snapshotRepo.FindByTrace(ctx, traceID1)
	if err != nil {
		return nil, fmt.Errorf("failed to load trace 1 snapshots: %w", err)
	}
	snaps2, err := s.snapshotRepo.FindByTrace(ctx, traceID2)
	if err != nil {
		return nil, fmt.Errorf("failed to load trace 2 snapshots: %w", err)
	}

	// Index by sequence number
	snapMap1 := make(map[int]*domain.GraphTraceNodeSnapshot, len(snaps1))
	for i := range snaps1 {
		snapMap1[snaps1[i].SequenceNumber] = &snaps1[i]
	}
	snapMap2 := make(map[int]*domain.GraphTraceNodeSnapshot, len(snaps2))
	for i := range snaps2 {
		snapMap2[snaps2[i].SequenceNumber] = &snaps2[i]
	}

	// Find the max sequence number
	maxSeq := 0
	for seq := range snapMap1 {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	for seq := range snapMap2 {
		if seq > maxSeq {
			maxSeq = seq
		}
	}

	var comparison []TraceComparisonStep
	for seq := 1; seq <= maxSeq; seq++ {
		s1 := snapMap1[seq]
		s2 := snapMap2[seq]

		step := TraceComparisonStep{
			SequenceNumber: seq,
			Trace1:         s1,
			Trace2:         s2,
		}

		if s1 != nil {
			step.NodeName = s1.NodeName
			step.NodeType = s1.NodeType
		} else if s2 != nil {
			step.NodeName = s2.NodeName
			step.NodeType = s2.NodeType
		}

		if s1 != nil && s2 != nil {
			step.InputChanged = !jsonMapEqual(s1.Input, s2.Input)
			step.OutputChanged = !jsonMapEqual(s1.Output, s2.Output)
			step.StatusChanged = s1.Status != s2.Status
		} else {
			// One side missing means everything changed
			step.InputChanged = true
			step.OutputChanged = true
			step.StatusChanged = true
		}

		comparison = append(comparison, step)
	}

	return comparison, nil
}

func (s *graphReplayService) buildReplayState(sessionID uuid.UUID, state *graphReplaySessionState) *domain.GraphReplayState {
	snapshot := state.snapshots[state.currentIdx]
	return &domain.GraphReplayState{
		SessionID:    sessionID,
		TraceID:      state.traceID,
		CurrentIndex: state.currentIdx,
		TotalNodes:   len(state.snapshots),
		HasNext:      state.currentIdx < len(state.snapshots)-1,
		HasPrevious:  state.currentIdx > 0,
		IsPlaying:    true,
		Snapshot:     &snapshot,
	}
}

// jsonMapEqual does a shallow comparison of two JSONMap values
func jsonMapEqual(a, b domain.JSONMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
			return false
		}
	}
	return true
}
