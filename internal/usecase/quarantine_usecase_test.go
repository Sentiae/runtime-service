package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeQuarantineRepo implements TestRunQuarantineRepo with an
// in-memory map so the tests don't need a real postgres.
type fakeQuarantineRepo struct {
	mu         sync.Mutex
	runs       map[uuid.UUID]*domain.TestRun
	candidates []domain.TestRun
	listErr    error
}

func newFakeQuarantineRepo() *fakeQuarantineRepo {
	return &fakeQuarantineRepo{runs: map[uuid.UUID]*domain.TestRun{}}
}

func (f *fakeQuarantineRepo) add(r *domain.TestRun) {
	f.runs[r.ID] = r
}

func (f *fakeQuarantineRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.TestRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, nil
}

func (f *fakeQuarantineRepo) Update(_ context.Context, run *domain.TestRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[run.ID] = run
	return nil
}

func (f *fakeQuarantineRepo) FindCandidatesForQuarantine(_ context.Context, threshold float64, minRuns int) ([]domain.TestRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]domain.TestRun, 0, len(f.candidates))
	for _, r := range f.candidates {
		if r.FlakinessScore > threshold {
			out = append(out, r)
		}
	}
	_ = minRuns
	return out, nil
}

func TestQuarantineUseCase_EvaluateOnceFlipsFlakyTests(t *testing.T) {
	repo := newFakeQuarantineRepo()

	flaky := domain.TestRun{ID: uuid.New(), TestNodeID: uuid.New(), FlakinessScore: 0.45}
	stable := domain.TestRun{ID: uuid.New(), TestNodeID: uuid.New(), FlakinessScore: 0.05}

	repo.add(&flaky)
	repo.add(&stable)
	repo.candidates = []domain.TestRun{flaky, stable}

	uc := NewQuarantineUseCase(repo, nil, 0.3, 10, time.Second)
	flipped, err := uc.EvaluateOnce(context.Background())
	if err != nil {
		t.Fatalf("EvaluateOnce: %v", err)
	}
	if flipped != 1 {
		t.Fatalf("expected 1 flip, got %d", flipped)
	}
	if !repo.runs[flaky.ID].Quarantined {
		t.Fatal("expected flaky test to be quarantined")
	}
	if repo.runs[stable.ID].Quarantined {
		t.Fatal("stable test should not be quarantined")
	}
	if repo.runs[flaky.ID].QuarantinedAt == nil {
		t.Fatal("QuarantinedAt must be set")
	}
}

func TestQuarantineUseCase_EvaluateOnceNoOpWhenAlreadyQuarantined(t *testing.T) {
	repo := newFakeQuarantineRepo()
	now := time.Now().UTC()
	already := domain.TestRun{
		ID: uuid.New(), TestNodeID: uuid.New(),
		FlakinessScore: 0.9,
		Quarantined:    true,
		QuarantinedAt:  &now,
	}
	repo.add(&already)
	repo.candidates = []domain.TestRun{already}

	uc := NewQuarantineUseCase(repo, nil, 0.3, 10, time.Second)
	flipped, err := uc.EvaluateOnce(context.Background())
	if err != nil {
		t.Fatalf("EvaluateOnce: %v", err)
	}
	if flipped != 0 {
		t.Fatalf("expected 0 flips, got %d", flipped)
	}
}

func TestQuarantineUseCase_EvaluateOnceSurfacesRepoError(t *testing.T) {
	repo := newFakeQuarantineRepo()
	repo.listErr = errors.New("boom")
	uc := NewQuarantineUseCase(repo, nil, 0.3, 10, time.Second)
	_, err := uc.EvaluateOnce(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestQuarantineUseCase_ManualToggleRoundTrip(t *testing.T) {
	repo := newFakeQuarantineRepo()
	run := &domain.TestRun{ID: uuid.New(), TestNodeID: uuid.New()}
	repo.add(run)

	uc := NewQuarantineUseCase(repo, nil, 0.3, 10, time.Second)

	// Quarantine.
	after, err := uc.Quarantine(context.Background(), run.ID, "flaky in CI")
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if !after.Quarantined || after.QuarantinedAt == nil {
		t.Fatal("expected Quarantined=true with timestamp")
	}

	// Idempotent — second call returns the same row.
	after2, _ := uc.Quarantine(context.Background(), run.ID, "x")
	if !after2.Quarantined {
		t.Fatal("second Quarantine must keep Quarantined=true")
	}

	// Unquarantine clears the flag.
	after3, err := uc.Unquarantine(context.Background(), run.ID, "fixed")
	if err != nil {
		t.Fatalf("Unquarantine: %v", err)
	}
	if after3.Quarantined || after3.QuarantinedAt != nil {
		t.Fatal("expected Quarantined=false after Unquarantine")
	}
}

func TestQuarantineUseCase_StartStopDrains(t *testing.T) {
	repo := newFakeQuarantineRepo()
	uc := NewQuarantineUseCase(repo, nil, 0.3, 10, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	uc.Start(ctx)
	time.Sleep(25 * time.Millisecond)
	uc.Stop()
}
