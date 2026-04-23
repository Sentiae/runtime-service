package event

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

func rawMessage(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return json.RawMessage(b)
}

// --- fakes ----------------------------------------------------------

type fakeResolver struct {
	out *usecase.AffectedTestsOutput
	err error
}

func (f *fakeResolver) Resolve(_ context.Context, _ usecase.AffectedTestsInput) (*usecase.AffectedTestsOutput, error) {
	return f.out, f.err
}

type fakePool struct {
	mu   sync.Mutex
	jobs []usecase.PoolJob
	err  error
}

func (f *fakePool) Submit(_ context.Context, j usecase.PoolJob) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.jobs = append(f.jobs, j)
	f.mu.Unlock()
	return nil
}

type fakeRepo struct {
	mu      sync.Mutex
	created []*domain.TestRun
}

func (f *fakeRepo) Create(_ context.Context, r *domain.TestRun) error {
	f.mu.Lock()
	f.created = append(f.created, r)
	f.mu.Unlock()
	return nil
}

type fakeSmokeLister struct {
	rows []domain.TestRun
	err  error
}

func (f *fakeSmokeLister) ListSmokeTests(_ context.Context, _ uuid.UUID, _ string) ([]domain.TestRun, error) {
	return f.rows, f.err
}

type fakePublisher struct {
	mu     sync.Mutex
	events []string
}

func (f *fakePublisher) Publish(_ context.Context, eventType, _ string, _ any) error {
	f.mu.Lock()
	f.events = append(f.events, eventType)
	f.mu.Unlock()
	return nil
}

// --- tests ----------------------------------------------------------

func TestContinuousTestTrigger_PushDispatchesAffected(t *testing.T) {
	nodeA := uuid.New()
	nodeB := uuid.New()
	resolver := &fakeResolver{out: &usecase.AffectedTestsOutput{
		TestNodeIDs: []string{nodeA.String(), nodeB.String()},
	}}
	pool := &fakePool{}
	repo := &fakeRepo{}
	pub := &fakePublisher{}
	trig := NewContinuousTestTrigger(resolver, pool, nil, repo, pub)

	orgID := uuid.New()
	repoID := uuid.New()
	event := kafka.CloudEvent{
		Type: "sentiae.git.push.received",
		Data: rawMessage(t, map[string]any{
			"organization_id": orgID.String(),
			"metadata": map[string]any{
				"repository_id": repoID.String(),
				"branch":        "main",
				"sha":           "abc123",
				"files_changed": []any{"pkg/x.go"},
			},
		}),
	}
	if err := trig.HandleGitEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleGitEvent: %v", err)
	}
	if len(pool.jobs) != 2 {
		t.Errorf("want 2 jobs, got %d", len(pool.jobs))
	}
	if len(repo.created) != 2 {
		t.Errorf("want 2 rows, got %d", len(repo.created))
	}
	// All created rows must carry the orgID from the envelope.
	for _, r := range repo.created {
		if r.OrganizationID != orgID {
			t.Errorf("org id mismatch: want %s got %s", orgID, r.OrganizationID)
		}
	}
	if len(pub.events) != 1 || pub.events[0] != usecase.EventTestQueued {
		t.Errorf("should publish test.queued, got %+v", pub.events)
	}
}

func TestContinuousTestTrigger_PushIdempotencyBlocksDuplicate(t *testing.T) {
	nodeA := uuid.New()
	resolver := &fakeResolver{out: &usecase.AffectedTestsOutput{TestNodeIDs: []string{nodeA.String()}}}
	pool := &fakePool{}
	repo := &fakeRepo{}
	trig := NewContinuousTestTrigger(resolver, pool, nil, repo, nil)

	orgID := uuid.New()
	repoID := uuid.New()
	event := kafka.CloudEvent{
		Type: "git.push.received",
		Data: rawMessage(t, map[string]any{
			"organization_id": orgID.String(),
			"metadata": map[string]any{
				"repository_id": repoID.String(),
				"sha":           "SAME-SHA",
				"files_changed": []any{"a.go"},
			},
		}),
	}
	if err := trig.HandleGitEvent(context.Background(), event); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := trig.HandleGitEvent(context.Background(), event); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(pool.jobs) != 1 {
		t.Errorf("idempotency should block duplicate, got %d jobs", len(pool.jobs))
	}
	if len(repo.created) != 1 {
		t.Errorf("idempotency should block duplicate row, got %d rows", len(repo.created))
	}
}

func TestContinuousTestTrigger_SessionCreatedDispatchesSmoke(t *testing.T) {
	crit1 := domain.TestRun{
		ID: uuid.New(), TestNodeID: uuid.New(), Critical: true,
		OrganizationID: uuid.New(), Language: "go", TestType: domain.TestTypeUnit,
	}
	crit2 := domain.TestRun{
		ID: uuid.New(), TestNodeID: uuid.New(), Critical: true,
		OrganizationID: uuid.New(), Language: "go", TestType: domain.TestTypeIntegration,
	}
	smoke := &fakeSmokeLister{rows: []domain.TestRun{crit1, crit2}}
	pool := &fakePool{}
	repo := &fakeRepo{}
	trig := NewContinuousTestTrigger(nil, pool, smoke, repo, nil)

	orgID := uuid.New()
	canvasID := uuid.New()
	event := kafka.CloudEvent{
		Type: "sentiae.git.session.created",
		Data: rawMessage(t, map[string]any{
			"organization_id": orgID.String(),
			"metadata": map[string]any{
				"session_id":  "sess-1",
				"canvas_id":   canvasID.String(),
				"base_branch": "main",
			},
		}),
	}
	if err := trig.HandleGitEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleGitEvent: %v", err)
	}
	if len(pool.jobs) != 2 {
		t.Errorf("want 2 smoke jobs, got %d", len(pool.jobs))
	}
	if !repo.created[0].Critical {
		t.Errorf("critical flag should propagate")
	}
	if repo.created[0].CanvasID == nil || *repo.created[0].CanvasID != canvasID {
		t.Errorf("canvas id should be set on the fresh run")
	}
}

func TestContinuousTestTrigger_SessionIdempotent(t *testing.T) {
	node := uuid.New()
	crit := domain.TestRun{
		ID: uuid.New(), TestNodeID: node, Critical: true,
		OrganizationID: uuid.New(),
	}
	smoke := &fakeSmokeLister{rows: []domain.TestRun{crit}}
	pool := &fakePool{}
	repo := &fakeRepo{}
	trig := NewContinuousTestTrigger(nil, pool, smoke, repo, nil)

	event := kafka.CloudEvent{
		Type: "git.session.created",
		Data: rawMessage(t, map[string]any{
			"metadata": map[string]any{
				"session_id": "dupe-session",
				"canvas_id":  uuid.New().String(),
			},
		}),
	}
	_ = trig.HandleGitEvent(context.Background(), event)
	_ = trig.HandleGitEvent(context.Background(), event)
	if len(pool.jobs) != 1 {
		t.Errorf("session idempotency failed: want 1 job, got %d", len(pool.jobs))
	}
}

func TestContinuousTestTrigger_IgnoresOtherEventTypes(t *testing.T) {
	trig := NewContinuousTestTrigger(&fakeResolver{}, &fakePool{}, &fakeSmokeLister{}, &fakeRepo{}, nil)
	if err := trig.HandleGitEvent(context.Background(), kafka.CloudEvent{Type: "noop.something"}); err != nil {
		t.Fatalf("unknown events should not error: %v", err)
	}
}
