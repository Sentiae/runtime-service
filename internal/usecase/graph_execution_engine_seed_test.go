//go:build unit

package usecase

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// recordingExecutionUC records each ExecuteSync call so the seed test can assert
// which nodes actually executed (a seeded node must NOT reach the executor) and
// what stdin (resolved input) an executed node received. Only ExecuteSync is
// exercised by runGraph; the rest satisfy the ExecutionUseCase interface.
type recordingExecutionUC struct {
	mu        sync.Mutex
	stdinByID map[uuid.UUID]string // node id → stdin JSON passed to ExecuteSync
	called    []uuid.UUID          // node ids that reached the executor
}

func newRecordingExecutionUC() *recordingExecutionUC {
	return &recordingExecutionUC{stdinByID: make(map[uuid.UUID]string)}
}

func (r *recordingExecutionUC) ExecuteSync(_ context.Context, input CreateExecutionInput) (*domain.Execution, error) {
	r.mu.Lock()
	if input.NodeID != nil {
		r.stdinByID[*input.NodeID] = input.Stdin
		r.called = append(r.called, *input.NodeID)
	}
	r.mu.Unlock()
	zero := 0
	return &domain.Execution{ID: uuid.New(), Stdout: "{}", ExitCode: &zero}, nil
}

func (r *recordingExecutionUC) CreateExecution(context.Context, CreateExecutionInput) (*domain.Execution, error) {
	return nil, nil
}
func (r *recordingExecutionUC) GetExecution(context.Context, uuid.UUID) (*domain.Execution, error) {
	return nil, nil
}
func (r *recordingExecutionUC) ListExecutions(context.Context, uuid.UUID, int, int) ([]domain.Execution, int64, error) {
	return nil, 0, nil
}
func (r *recordingExecutionUC) CancelExecution(context.Context, uuid.UUID) error { return nil }
func (r *recordingExecutionUC) GetExecutionMetrics(context.Context, uuid.UUID) (*domain.ExecutionMetrics, error) {
	return nil, nil
}
func (r *recordingExecutionUC) ProcessPending(context.Context, int) (int, error) { return 0, nil }

func (r *recordingExecutionUC) wasCalled(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.called {
		if c == id {
			return true
		}
	}
	return false
}

// memNodeExecRepo is an in-memory NodeExecutionRepository capturing the records
// the engine writes (so the test can assert the seeded node was recorded cached).
type memNodeExecRepo struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.NodeExecution
}

func newMemNodeExecRepo() *memNodeExecRepo {
	return &memNodeExecRepo{rows: make(map[uuid.UUID]*domain.NodeExecution)}
}

func (m *memNodeExecRepo) Create(_ context.Context, e *domain.NodeExecution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	m.rows[e.ID] = &cp
	return nil
}
func (m *memNodeExecRepo) Update(_ context.Context, e *domain.NodeExecution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	m.rows[e.ID] = &cp
	return nil
}
func (m *memNodeExecRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.NodeExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		return r, nil
	}
	return nil, domain.ErrNodeExecutionNotFound
}
func (m *memNodeExecRepo) FindByGraphExecution(_ context.Context, _ uuid.UUID) ([]domain.NodeExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.NodeExecution, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (m *memNodeExecRepo) byNodeName(name string) (domain.NodeExecution, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.NodeName == name {
			return *r, true
		}
	}
	return domain.NodeExecution{}, false
}

// memGraphExecRepo is an in-memory GraphExecutionRepository (the engine only
// Updates the passed-in record during runGraph).
type memGraphExecRepo struct{}

func (memGraphExecRepo) Create(context.Context, *domain.GraphExecution) error { return nil }
func (memGraphExecRepo) Update(context.Context, *domain.GraphExecution) error { return nil }
func (memGraphExecRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphExecution, error) {
	return nil, nil
}
func (memGraphExecRepo) FindByGraph(context.Context, uuid.UUID, int, int) ([]domain.GraphExecution, int64, error) {
	return nil, 0, nil
}
func (memGraphExecRepo) FindPending(context.Context, int) ([]domain.GraphExecution, error) {
	return nil, nil
}

// TestRunGraph_SeededNodeSkipsExecutor verifies the notebook-style re-run skip:
// in a code(0)→code(1) graph with node[0] seeded, the executor is NOT called for
// node[0] (no microVM), node[0] is recorded cached, and the seed propagates to
// node[1] as its input.
func TestRunGraph_SeededNodeSkipsExecutor(t *testing.T) {
	lang := domain.Language("python")
	upstream := domain.GraphNode{
		ID:        uuid.New(),
		NodeType:  domain.GraphNodeTypeCode,
		Name:      "upstream",
		Language:  &lang,
		Code:      "print('upstream')",
		SortOrder: 0,
	}
	downstream := domain.GraphNode{
		ID:        uuid.New(),
		NodeType:  domain.GraphNodeTypeCode,
		Name:      "downstream",
		Language:  &lang,
		Code:      "print('downstream')",
		SortOrder: 1,
	}
	nodes := []domain.GraphNode{upstream, downstream}
	edges := []domain.GraphEdge{
		{
			SourceNodeID: upstream.ID,
			TargetNodeID: downstream.ID,
			SourcePort:   "seeded_value",
			TargetPort:   "seeded_value",
		},
	}

	execUC := newRecordingExecutionUC()
	nodeRepo := newMemNodeExecRepo()
	eng := &GraphExecutionEngine{
		graphExecRepo:  memGraphExecRepo{},
		nodeExecRepo:   nodeRepo,
		executionUC:    execUC,
		eventPublisher: noopEventPublisher{},
		maxParallelism: 1,
		cancellations:  make(map[uuid.UUID]context.CancelFunc),
		timings:        make(map[uuid.UUID][]nodeTimings),
	}

	graphExec := &domain.GraphExecution{
		ID:             uuid.New(),
		GraphID:        uuid.New(),
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Status:         domain.GraphExecPending,
		TotalNodes:     len(nodes),
	}
	graphDef := &domain.GraphDefinition{ID: graphExec.GraphID, Status: domain.GraphStatusActive}

	seed := domain.JSONMap{"seeded_value": "from-cache"}
	seeded := map[string]domain.JSONMap{"upstream": seed}

	eng.runGraph(context.Background(), graphExec, graphDef, nodes, edges, domain.JSONMap{}, seeded)

	// 1. The seeded upstream node must NOT have reached the executor.
	if execUC.wasCalled(upstream.ID) {
		t.Fatal("seeded node[0] reached the executor — it must be skipped (no microVM)")
	}

	// 2. The downstream node MUST have executed.
	if !execUC.wasCalled(downstream.ID) {
		t.Fatal("downstream node[1] did not execute")
	}

	// 3. node[0] must be recorded completed + cached.
	rec, ok := nodeRepo.byNodeName("upstream")
	if !ok {
		t.Fatal("no node-execution record written for the seeded node")
	}
	if !rec.Cached {
		t.Error("seeded node-execution record must have Cached=true")
	}
	if rec.Status != domain.GraphExecCompleted {
		t.Errorf("seeded node status: got %v, want completed", rec.Status)
	}
	if rec.DurationMS == nil || *rec.DurationMS != 0 {
		t.Errorf("seeded node duration: got %v, want 0", rec.DurationMS)
	}

	// 4. The seed must have propagated to node[1] as its input (the edge carried
	// seeded_value through to downstream's stdin).
	stdin := execUC.stdinByID[downstream.ID]
	var got map[string]any
	if err := json.Unmarshal([]byte(stdin), &got); err != nil {
		t.Fatalf("downstream stdin not JSON: %q (%v)", stdin, err)
	}
	if got["seeded_value"] != "from-cache" {
		t.Errorf("seed did not propagate downstream: stdin=%q", stdin)
	}
}
