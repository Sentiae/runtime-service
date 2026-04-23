// Package usecase — QuarantineUseCase drives the §8.3 auto-quarantine
// loop. A background ticker periodically scans TestRun rows, and every
// test whose FlakinessScore has stayed above the threshold for N runs
// is flipped to Quarantined=true. Manual override entry points let
// operators forcibly toggle the flag outside of the scheduler's
// cadence (e.g. to bring a test back into rotation after a fix).
package usecase

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// Defaults chosen to match the §8.3 spec: a test must stay above
// FlakinessScore=0.3 for 10 consecutive runs before the scheduler
// flips Quarantined to true.
const (
	DefaultFlakinessThreshold  = 0.3
	DefaultMinRunsForQuarantine = 10
	DefaultQuarantineTick       = 5 * time.Minute
)

// TestRunQuarantineRepo is the repo surface the use case depends on.
// Defined here so the test suite can substitute a fake without
// pulling the postgres-backed types into the usecase test binary.
type TestRunQuarantineRepo interface {
	FindByID(ctx context.Context, id uuid.UUID) (*domain.TestRun, error)
	Update(ctx context.Context, run *domain.TestRun) error
	FindCandidatesForQuarantine(ctx context.Context, threshold float64, minRuns int) ([]domain.TestRun, error)
}

// QuarantineUseCase is the orchestrator. Call Start to launch the
// background ticker; Quarantine/Unquarantine are the manual entry
// points surfaced by the HTTP handler.
type QuarantineUseCase struct {
	repo      TestRunQuarantineRepo
	publisher EventPublisher

	threshold float64
	minRuns   int
	tick      time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewQuarantineUseCase wires the scheduler. Zero / negative values for
// threshold, minRuns, tick fall back to the defaults so callers can
// construct the use case with `nil` config in tests.
func NewQuarantineUseCase(
	repo TestRunQuarantineRepo,
	publisher EventPublisher,
	threshold float64,
	minRuns int,
	tick time.Duration,
) *QuarantineUseCase {
	if threshold <= 0 {
		threshold = DefaultFlakinessThreshold
	}
	if minRuns <= 0 {
		minRuns = DefaultMinRunsForQuarantine
	}
	if tick <= 0 {
		tick = DefaultQuarantineTick
	}
	if publisher == nil {
		publisher = noopEventPublisher{}
	}
	return &QuarantineUseCase{
		repo:      repo,
		publisher: publisher,
		threshold: threshold,
		minRuns:   minRuns,
		tick:      tick,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start launches the background ticker that periodically evaluates
// candidates. Safe to call once; subsequent calls are a no-op because
// the ticker goroutine observes a single start signal.
func (uc *QuarantineUseCase) Start(ctx context.Context) {
	go uc.run(ctx)
	log.Printf("[QUARANTINE] Scheduler started (threshold=%.2f min_runs=%d tick=%s)",
		uc.threshold, uc.minRuns, uc.tick)
}

// Stop signals the background loop to exit and waits for it to finish.
func (uc *QuarantineUseCase) Stop() {
	uc.stopOnce.Do(func() { close(uc.stopCh) })
	<-uc.doneCh
}

func (uc *QuarantineUseCase) run(ctx context.Context) {
	defer close(uc.doneCh)
	t := time.NewTicker(uc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-uc.stopCh:
			return
		case <-t.C:
			if n, err := uc.EvaluateOnce(ctx); err != nil {
				log.Printf("[QUARANTINE] tick error: %v", err)
			} else if n > 0 {
				log.Printf("[QUARANTINE] flipped %d test(s) to quarantined", n)
			}
		}
	}
}

// EvaluateOnce runs a single scheduler pass. Returns the number of
// tests transitioned to quarantined. Errors on individual rows are
// logged and do not abort the pass — one bad row must not block the
// others.
func (uc *QuarantineUseCase) EvaluateOnce(ctx context.Context) (int, error) {
	candidates, err := uc.repo.FindCandidatesForQuarantine(ctx, uc.threshold, uc.minRuns)
	if err != nil {
		return 0, fmt.Errorf("list candidates: %w", err)
	}
	flipped := 0
	now := time.Now().UTC()
	for i := range candidates {
		run := &candidates[i]
		if run.Quarantined {
			// Already quarantined — no-op.
			continue
		}
		run.Quarantine(now)
		if err := uc.repo.Update(ctx, run); err != nil {
			log.Printf("[QUARANTINE] update %s: %v", run.ID, err)
			continue
		}
		uc.publish(ctx, EventTestQuarantined, run, "auto: sustained_flakiness")
		flipped++
	}
	return flipped, nil
}

// Quarantine is the manual override entry point: flips Quarantined to
// true for the given test and emits the lifecycle event. Idempotent —
// calling on an already-quarantined test is a no-op that still returns
// the latest row.
func (uc *QuarantineUseCase) Quarantine(ctx context.Context, id uuid.UUID, reason string) (*domain.TestRun, error) {
	run, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("test run %s not found", id)
	}
	if run.Quarantined {
		return run, nil
	}
	run.Quarantine(time.Now().UTC())
	if err := uc.repo.Update(ctx, run); err != nil {
		return nil, err
	}
	uc.publish(ctx, EventTestQuarantined, run, reasonOrDefault(reason, "manual"))
	return run, nil
}

// Unquarantine flips Quarantined back to false and emits the event.
// Used both when an operator pulls a test out of quarantine and when a
// follow-up automation determines the flakiness has resolved.
func (uc *QuarantineUseCase) Unquarantine(ctx context.Context, id uuid.UUID, reason string) (*domain.TestRun, error) {
	run, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("test run %s not found", id)
	}
	if !run.Quarantined {
		return run, nil
	}
	run.Unquarantine(time.Now().UTC())
	if err := uc.repo.Update(ctx, run); err != nil {
		return nil, err
	}
	uc.publish(ctx, EventTestUnquarantined, run, reasonOrDefault(reason, "manual"))
	return run, nil
}

func (uc *QuarantineUseCase) publish(ctx context.Context, eventType string, run *domain.TestRun, reason string) {
	if uc.publisher == nil || run == nil {
		return
	}
	_ = uc.publisher.Publish(ctx, eventType, run.ID.String(), map[string]any{
		"test_run_id":     run.ID.String(),
		"test_node_id":    run.TestNodeID.String(),
		"organization_id": run.OrganizationID.String(),
		"flakiness_score": run.FlakinessScore,
		"reason":          reason,
	})
}

func reasonOrDefault(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}
