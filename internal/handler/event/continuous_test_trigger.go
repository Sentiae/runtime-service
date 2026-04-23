// Package event — ContinuousTestTrigger bridges git lifecycle events
// into runtime test dispatch (CS-2 G2.7 §8.6 continuous testing).
//
//	git.push.received     → affected-tests dispatch
//	git.session.created   → "smoke" test set dispatch (critical tests
//	                        on the base branch)
//
// Idempotency: every dispatch is keyed by (commit_sha, test_node_id).
// A duplicate event within the in-memory cache window is a no-op so
// consumer re-delivery doesn't spam the pool.
package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Event type aliases. Tolerant of both the prefixed and bare forms —
// publishers across the platform use different conventions.
const (
	eventPushReceived       = "sentiae.git.push.received"
	eventPushReceivedBare   = "git.push.received"
	eventPushLegacy         = "sentiae.git.push"
	eventSessionCreated     = "sentiae.git.session.created"
	eventSessionCreatedBare = "git.session.created"
)

// AffectedTestResolver is the narrow surface of
// usecase.AffectedTestResolver used by the push path. Declared locally
// to avoid an import cycle between handler/event and usecase in tests.
type AffectedTestResolver interface {
	Resolve(ctx context.Context, input usecase.AffectedTestsInput) (*usecase.AffectedTestsOutput, error)
}

// TestDispatcher is the slice of PoolScheduler.Submit the trigger
// needs. Any implementation that turns a PoolJob into an actual run
// satisfies the contract.
type TestDispatcher interface {
	Submit(ctx context.Context, job usecase.PoolJob) error
}

// SmokeTestLister returns the list of critical tests tagged to the
// base branch for a session's canvas. Implementations pull from the
// TestRun repo (critical=true, quarantined=false) and filter by the
// base branch + canvas id the session points to.
type SmokeTestLister interface {
	ListSmokeTests(ctx context.Context, canvasID uuid.UUID, baseBranch string) ([]domain.TestRun, error)
}

// EventPublisher is the minimal publish surface.
type EventPublisher interface {
	Publish(ctx context.Context, eventType, key string, data any) error
}

// ContinuousTestTrigger wires resolver + dispatcher together and
// exposes HandleGitEvent for kafka registration.
type ContinuousTestTrigger struct {
	resolver    AffectedTestResolver
	pool        TestDispatcher
	smokeLister SmokeTestLister
	publisher   EventPublisher
	testRunRepo TestRunPersister

	mu          sync.Mutex
	seenKeys    map[string]time.Time
	cacheWindow time.Duration
}

// TestRunPersister persists a fresh TestRun row so the pool has a
// concrete row to execute against. Kept narrow so tests can fake it.
type TestRunPersister interface {
	Create(ctx context.Context, run *domain.TestRun) error
}

// DefaultIdempotencyWindow is how long (commit_sha, test_id) keys
// stay in the seen set. One hour is comfortably longer than the
// longest kafka redelivery window we observe in practice.
const DefaultIdempotencyWindow = time.Hour

// NewContinuousTestTrigger wires the trigger with all dependencies.
func NewContinuousTestTrigger(
	resolver AffectedTestResolver,
	pool TestDispatcher,
	smokeLister SmokeTestLister,
	testRunRepo TestRunPersister,
	publisher EventPublisher,
) *ContinuousTestTrigger {
	return &ContinuousTestTrigger{
		resolver:    resolver,
		pool:        pool,
		smokeLister: smokeLister,
		testRunRepo: testRunRepo,
		publisher:   publisher,
		seenKeys:    make(map[string]time.Time),
		cacheWindow: DefaultIdempotencyWindow,
	}
}

// Register attaches the trigger to all event aliases we care about.
// Safe to call with a nil consumer (no-op).
func (t *ContinuousTestTrigger) Register(consumer interface {
	Handle(eventType string, handler kafka.EventHandler)
}) {
	if consumer == nil || t == nil {
		return
	}
	for _, typ := range []string{
		eventPushReceived, eventPushReceivedBare, eventPushLegacy,
		eventSessionCreated, eventSessionCreatedBare,
	} {
		consumer.Handle(typ, t.HandleGitEvent)
	}
}

// HandleGitEvent dispatches on event.Type. A nil trigger returns nil
// so wiring-order bugs are fail-open.
func (t *ContinuousTestTrigger) HandleGitEvent(ctx context.Context, event kafka.CloudEvent) error {
	if t == nil {
		return nil
	}
	switch event.Type {
	case eventPushReceived, eventPushReceivedBare, eventPushLegacy:
		return t.onPush(ctx, event)
	case eventSessionCreated, eventSessionCreatedBare:
		return t.onSessionCreated(ctx, event)
	}
	return nil
}

// pushPayload mirrors the fields we need off a push.received event.
type pushPayload struct {
	RepositoryID   string   `json:"repository_id"`
	Branch         string   `json:"branch"`
	SHA            string   `json:"sha"`
	FilesChanged   []string `json:"files_changed"`
	OrganizationID string   `json:"organization_id"`
}

type sessionPayload struct {
	SessionID      string `json:"session_id"`
	CanvasID       string `json:"canvas_id"`
	BaseBranch     string `json:"base_branch"`
	OrganizationID string `json:"organization_id"`
}

// onPush resolves affected tests and enqueues each one. Idempotency
// key is (commit_sha, test_node_id).
func (t *ContinuousTestTrigger) onPush(ctx context.Context, event kafka.CloudEvent) error {
	if t.resolver == nil || t.pool == nil || t.testRunRepo == nil {
		log.Printf("[CONTINUOUS-TEST] push handler: dependencies not wired; skipping")
		return nil
	}
	payload, orgID, err := parsePushPayload(event)
	if err != nil {
		log.Printf("[CONTINUOUS-TEST] push parse: %v", err)
		return nil
	}
	if payload.RepositoryID == "" || len(payload.FilesChanged) == 0 {
		return nil
	}
	repoID, err := uuid.Parse(payload.RepositoryID)
	if err != nil {
		return nil
	}
	out, err := t.resolver.Resolve(ctx, usecase.AffectedTestsInput{
		RepoID:    repoID,
		DiffFiles: payload.FilesChanged,
	})
	if err != nil || out == nil {
		if err != nil {
			log.Printf("[CONTINUOUS-TEST] resolver: %v", err)
		}
		return nil
	}

	runIDs := make([]string, 0, len(out.TestNodeIDs))
	for _, raw := range out.TestNodeIDs {
		testNodeID, perr := uuid.Parse(raw)
		if perr != nil {
			continue
		}
		if !t.claim(payload.SHA, raw) {
			continue
		}
		fresh := &domain.TestRun{
			ID:             uuid.New(),
			OrganizationID: orgID,
			TestNodeID:     testNodeID,
			Language:       domain.Language(""),
			TestType:       domain.TestTypeUnit,
			Status:         domain.TestRunStatusRunning,
			MaxRetries:     domain.DefaultMaxTestRetries,
		}
		if err := t.testRunRepo.Create(ctx, fresh); err != nil {
			log.Printf("[CONTINUOUS-TEST] create run for node=%s: %v", testNodeID, err)
			continue
		}
		if err := t.pool.Submit(ctx, usecase.PoolJob{Run: fresh}); err != nil {
			log.Printf("[CONTINUOUS-TEST] submit run %s: %v", fresh.ID, err)
			continue
		}
		runIDs = append(runIDs, fresh.ID.String())
	}

	t.publishQueued(ctx, runIDs, "push_received", map[string]any{
		"repository_id": repoID.String(),
		"sha":           payload.SHA,
		"branch":        payload.Branch,
		"files_changed": payload.FilesChanged,
	})
	log.Printf("[CONTINUOUS-TEST] push → enqueued %d run(s) repo=%s sha=%s", len(runIDs), repoID, payload.SHA)
	return nil
}

// onSessionCreated enqueues the smoke test set for the session's
// canvas. Idempotency key is (session_id, test_node_id) so two copies
// of the same session.created event don't double-run.
func (t *ContinuousTestTrigger) onSessionCreated(ctx context.Context, event kafka.CloudEvent) error {
	if t.smokeLister == nil || t.pool == nil || t.testRunRepo == nil {
		log.Printf("[CONTINUOUS-TEST] session handler: dependencies not wired; skipping")
		return nil
	}
	payload, orgID, err := parseSessionPayload(event)
	if err != nil {
		log.Printf("[CONTINUOUS-TEST] session parse: %v", err)
		return nil
	}
	if payload.CanvasID == "" || payload.SessionID == "" {
		return nil
	}
	canvasID, err := uuid.Parse(payload.CanvasID)
	if err != nil {
		return nil
	}
	baseBranch := payload.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	smoke, err := t.smokeLister.ListSmokeTests(ctx, canvasID, baseBranch)
	if err != nil {
		log.Printf("[CONTINUOUS-TEST] smoke list: %v", err)
		return nil
	}

	runIDs := make([]string, 0, len(smoke))
	for i := range smoke {
		prior := &smoke[i]
		key := payload.SessionID + ":" + prior.TestNodeID.String()
		if !t.claim(payload.SessionID, prior.TestNodeID.String()) {
			_ = key
			continue
		}
		orgForRow := orgID
		if orgForRow == uuid.Nil {
			orgForRow = prior.OrganizationID
		}
		fresh := &domain.TestRun{
			ID:             uuid.New(),
			OrganizationID: orgForRow,
			TestNodeID:     prior.TestNodeID,
			CodeNodeID:     prior.CodeNodeID,
			CanvasID:       &canvasID,
			Language:       prior.Language,
			TestType:       prior.TestType,
			Status:         domain.TestRunStatusRunning,
			MaxRetries:     domain.DefaultMaxTestRetries,
			Critical:       prior.Critical,
			Framework:      prior.Framework,
		}
		if err := t.testRunRepo.Create(ctx, fresh); err != nil {
			log.Printf("[CONTINUOUS-TEST] create smoke run for node=%s: %v", prior.TestNodeID, err)
			continue
		}
		if err := t.pool.Submit(ctx, usecase.PoolJob{Run: fresh}); err != nil {
			log.Printf("[CONTINUOUS-TEST] submit smoke run %s: %v", fresh.ID, err)
			continue
		}
		runIDs = append(runIDs, fresh.ID.String())
	}

	t.publishQueued(ctx, runIDs, "session_created", map[string]any{
		"session_id":  payload.SessionID,
		"canvas_id":   canvasID.String(),
		"base_branch": baseBranch,
		"smoke_count": len(runIDs),
	})
	log.Printf("[CONTINUOUS-TEST] session.created → enqueued %d smoke run(s) canvas=%s session=%s",
		len(runIDs), canvasID, payload.SessionID)
	return nil
}

func (t *ContinuousTestTrigger) publishQueued(ctx context.Context, runIDs []string, trigger string, extra map[string]any) {
	if t.publisher == nil || len(runIDs) == 0 {
		return
	}
	payload := map[string]any{
		"test_run_ids": runIDs,
		"trigger":      trigger,
	}
	for k, v := range extra {
		payload[k] = v
	}
	key := trigger
	if len(runIDs) > 0 {
		key = runIDs[0]
	}
	if err := t.publisher.Publish(ctx, usecase.EventTestQueued, key, payload); err != nil {
		log.Printf("[CONTINUOUS-TEST] publish %s: %v", usecase.EventTestQueued, err)
	}
}

// claim records a (sha, testID) pair as seen. Returns true if this is
// the first time we've seen it within the cache window, false if
// already claimed.
func (t *ContinuousTestTrigger) claim(sha, testID string) bool {
	key := sha + "|" + testID
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if ts, ok := t.seenKeys[key]; ok && now.Sub(ts) < t.cacheWindow {
		return false
	}
	t.seenKeys[key] = now
	// Opportunistic eviction: drop keys older than the cache window
	// so the map doesn't grow unbounded in long-lived processes.
	if len(t.seenKeys) > 1024 {
		for k, ts := range t.seenKeys {
			if now.Sub(ts) >= t.cacheWindow {
				delete(t.seenKeys, k)
			}
		}
	}
	return true
}

// --- payload parsing ------------------------------------------------

func parsePushPayload(event kafka.CloudEvent) (pushPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return pushPayload{}, uuid.Nil, fmt.Errorf("marshal: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload pushPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return pushPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return pushPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
		}
	}
	var orgID uuid.UUID
	if envelope.OrganizationID != "" {
		if id, err := uuid.Parse(envelope.OrganizationID); err == nil {
			orgID = id
		}
	}
	if orgID == uuid.Nil && payload.OrganizationID != "" {
		if id, err := uuid.Parse(payload.OrganizationID); err == nil {
			orgID = id
		}
	}
	return payload, orgID, nil
}

func parseSessionPayload(event kafka.CloudEvent) (sessionPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return sessionPayload{}, uuid.Nil, fmt.Errorf("marshal: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload sessionPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return sessionPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return sessionPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
		}
	}
	var orgID uuid.UUID
	if envelope.OrganizationID != "" {
		if id, err := uuid.Parse(envelope.OrganizationID); err == nil {
			orgID = id
		}
	}
	if orgID == uuid.Nil && payload.OrganizationID != "" {
		if id, err := uuid.Parse(payload.OrganizationID); err == nil {
			orgID = id
		}
	}
	return payload, orgID, nil
}
