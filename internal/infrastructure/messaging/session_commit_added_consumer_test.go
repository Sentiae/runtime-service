package messaging

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type fakePoolSubmitter struct {
	mu   sync.Mutex
	jobs []usecase.PoolJob
	err  error
}

func (f *fakePoolSubmitter) Submit(_ context.Context, job usecase.PoolJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, job)
	return f.err
}

type fakeCapturedEvent struct {
	EventType string
	Data      any
}

type fakeRuntimePublisher struct {
	mu     sync.Mutex
	events []fakeCapturedEvent
}

func (f *fakeRuntimePublisher) Publish(_ context.Context, eventType, _ string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeCapturedEvent{EventType: eventType, Data: data})
	return nil
}
func (f *fakeRuntimePublisher) Close() error { return nil }

// fakeTestRunRepo is a minimal in-memory implementation of the narrow
// SessionCommitTestRepo the consumer needs. Good enough for unit
// testing without pulling in a real Postgres container — integration
// coverage for the repository lives alongside the repository itself.
type fakeTestRunRepo struct {
	mu    sync.Mutex
	runs  []domain.TestRun
	byCvs map[uuid.UUID][]domain.TestRun
}

func newFakeTestRunRepo() *fakeTestRunRepo {
	return &fakeTestRunRepo{byCvs: make(map[uuid.UUID][]domain.TestRun)}
}

func (f *fakeTestRunRepo) Create(_ context.Context, run *domain.TestRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, *run)
	if run.CanvasID != nil {
		f.byCvs[*run.CanvasID] = append(f.byCvs[*run.CanvasID], *run)
	}
	return nil
}

func (f *fakeTestRunRepo) FindByCanvas(_ context.Context, canvasID uuid.UUID, _ int, _ int) ([]domain.TestRun, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.TestRun, len(f.byCvs[canvasID]))
	copy(out, f.byCvs[canvasID])
	return out, int64(len(out)), nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSessionCommitAddedConsumer_IgnoresOtherEvents(t *testing.T) {
	t.Parallel()

	c := NewSessionCommitAddedConsumer(nil, nil, nil)
	err := c.HandleGitEvent(context.Background(), kafka.CloudEvent{
		Type: "sentiae.git.push",
	})
	assert.NoError(t, err)
}

func TestSessionCommitAddedConsumer_NoCanvasSkipsDispatch(t *testing.T) {
	t.Parallel()

	repo := newFakeTestRunRepo()
	pool := &fakePoolSubmitter{}
	pub := &fakeRuntimePublisher{}

	c := NewSessionCommitAddedConsumer(repo, pool, pub)

	ev := mustCloudEvent(t, EventTypeSessionCommitAdded, map[string]any{
		"organization_id": uuid.NewString(),
		"metadata": map[string]any{
			"session_id":    "42",
			"repository_id": uuid.NewString(),
			"commit_sha":    "deadbeef",
		},
	})
	err := c.HandleGitEvent(context.Background(), ev)
	assert.NoError(t, err)
	assert.Empty(t, pool.jobs, "no canvas means no dispatch")
	assert.Empty(t, pub.events)
}

func TestSessionCommitAddedConsumer_DispatchesLinkedTests(t *testing.T) {
	t.Parallel()

	repo := newFakeTestRunRepo()
	orgID := uuid.New()
	canvasID := uuid.New()

	// Seed two historical test runs on the canvas so the consumer has a
	// set to re-run.
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     uuid.New(),
		Language:       domain.Language("python"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
		Framework:      "pytest",
	}))
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     uuid.New(),
		Language:       domain.Language("go"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
		Framework:      "go-test",
	}))

	pool := &fakePoolSubmitter{}
	pub := &fakeRuntimePublisher{}
	c := NewSessionCommitAddedConsumer(repo, pool, pub)

	ev := mustCloudEvent(t, EventTypeSessionCommitAdded, map[string]any{
		"organization_id": orgID.String(),
		"metadata": map[string]any{
			"session_id":    "77",
			"repository_id": uuid.NewString(),
			"canvas_id":     canvasID.String(),
			"commit_sha":    "cafebabe",
			"head_branch":   "feat/x",
		},
	})
	err := c.HandleGitEvent(ctx, ev)
	require.NoError(t, err)

	pool.mu.Lock()
	jobsCount := len(pool.jobs)
	jobStatus := domain.TestRunStatus("")
	if jobsCount > 0 {
		jobStatus = pool.jobs[0].Run.Status
	}
	pool.mu.Unlock()
	assert.Equal(t, 2, jobsCount)
	assert.Equal(t, domain.TestRunStatusRunning, jobStatus)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.events, 1)
	assert.Equal(t, usecase.EventTestQueued, pub.events[0].EventType)
	payload, ok := pub.events[0].Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "session_commit_added", payload["trigger"])
	assert.Equal(t, canvasID.String(), payload["canvas_id"])
	assert.Equal(t, "cafebabe", payload["commit_sha"])
	runIDs, _ := payload["test_run_ids"].([]string)
	assert.Len(t, runIDs, 2)
}

func TestSessionCommitAddedConsumer_DeduplicatesByTestCodeTuple(t *testing.T) {
	t.Parallel()

	repo := newFakeTestRunRepo()
	orgID := uuid.New()
	canvasID := uuid.New()
	testNodeID := uuid.New()
	codeNodeID := uuid.New()
	ctx := context.Background()

	// Seed 3 historical runs — two share the same (test, code) tuple so
	// the consumer should only enqueue 2 fresh rows.
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     testNodeID,
		CodeNodeID:     &codeNodeID,
		Language:       domain.Language("go"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
	}))
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     testNodeID, // duplicate tuple
		CodeNodeID:     &codeNodeID,
		Language:       domain.Language("go"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
	}))
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     uuid.New(),
		Language:       domain.Language("python"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
	}))

	pool := &fakePoolSubmitter{}
	pub := &fakeRuntimePublisher{}
	c := NewSessionCommitAddedConsumer(repo, pool, pub)

	err := c.HandleGitEvent(ctx, mustCloudEvent(t, EventTypeSessionCommitAdded, map[string]any{
		"organization_id": orgID.String(),
		"metadata": map[string]any{
			"canvas_id":  canvasID.String(),
			"commit_sha": "abc",
		},
	}))
	require.NoError(t, err)

	pool.mu.Lock()
	defer pool.mu.Unlock()
	assert.Equal(t, 2, len(pool.jobs), "duplicate (test, code) tuple should de-dup")
}

// mustCloudEvent constructs a CloudEvent with JSON-encoded Data.
func mustCloudEvent(t *testing.T, eventType string, payload map[string]any) kafka.CloudEvent {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return kafka.CloudEvent{
		Type: eventType,
		Data: b,
	}
}
