// Package messaging — SessionCommitAddedConsumer listens for
// `sentiae.git.session.commit_added` CloudEvents and submits the
// session's linked test set to the PoolScheduler (§8.6 continuous
// testing). Every commit that lands on a session's head branch triggers
// a fresh run of every test wired to the session's canvas.
//
// Wiring: the DI container registers this handler on the runtime Kafka
// consumer; each receipt creates one TestRun row per unique
// (test_node_id, code_node_id) on the session's canvas and hands the
// job to the PoolScheduler which provides the fresh-VM isolation the
// §8.3 spec requires.
//
// Resilience: every failure path logs and returns nil — a bad git
// payload must not wedge the consumer or cause redelivery loops. If
// canvas_id is missing the handler no-ops because we have no stable
// way to enumerate the linked test set without work-service/canvas
// round-trips that would add cross-service coupling we don't need on
// the critical path.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// EventTypeSessionCommitAdded mirrors the git-service constant. Kept
// local so the messaging package doesn't pull in git-service as a hard
// dependency.
const EventTypeSessionCommitAdded = "sentiae.git.session.commit_added"

// TestPoolSubmitter is the minimal surface the consumer needs from
// PoolScheduler. Declared as an interface so tests can pass a fake
// without spinning up the real worker pool.
type TestPoolSubmitter interface {
	Submit(ctx context.Context, job usecase.PoolJob) error
}

// SessionCommitTestRepo is the narrow slice of TestRunRepo the consumer
// uses. Declared as an interface so tests can swap a pure in-memory
// fake without needing a real Postgres container for the unit layer.
type SessionCommitTestRepo interface {
	FindByCanvas(ctx context.Context, canvasID uuid.UUID, limit, offset int) ([]domain.TestRun, int64, error)
	Create(ctx context.Context, run *domain.TestRun) error
}

// SessionCommitAddedConsumer routes session.commit_added events into
// the PoolScheduler.
type SessionCommitAddedConsumer struct {
	testRunRepo SessionCommitTestRepo
	pool        TestPoolSubmitter
	publisher   EventPublisher
}

// NewSessionCommitAddedConsumer constructs the consumer. A nil pool or
// test-run repo leaves the handler as a best-effort no-op with a
// warning log, matching the defensive pattern used by the §8.6
// siblings.
func NewSessionCommitAddedConsumer(testRunRepo SessionCommitTestRepo, pool TestPoolSubmitter, publisher EventPublisher) *SessionCommitAddedConsumer {
	return &SessionCommitAddedConsumer{
		testRunRepo: testRunRepo,
		pool:        pool,
		publisher:   publisher,
	}
}

// Register wires HandleGitEvent onto every event-type alias the
// consumer recognises. Tolerant of the bare type (without the
// sentiae. prefix) because some internal publishers strip it.
func (c *SessionCommitAddedConsumer) Register(consumer *EventConsumer) {
	if consumer == nil || c == nil {
		return
	}
	consumer.Handle(EventTypeSessionCommitAdded, c.HandleGitEvent)
	consumer.Handle("git.session.commit_added", c.HandleGitEvent)
}

// HandleGitEvent is the Kafka entry point. It filters by event type
// and delegates to the dispatch path.
func (c *SessionCommitAddedConsumer) HandleGitEvent(ctx context.Context, event kafka.CloudEvent) error {
	if c == nil {
		return nil
	}
	switch event.Type {
	case EventTypeSessionCommitAdded, "git.session.commit_added":
		return c.onCommitAdded(ctx, event)
	}
	return nil
}

// sessionCommitPayload mirrors the metadata git-service emits on
// session.commit_added. `canvas_id` may or may not be present — when
// absent the consumer no-ops because we cannot locate the linked
// test set without it.
type sessionCommitPayload struct {
	SessionID    any    `json:"session_id"`
	RepositoryID string `json:"repository_id"`
	CanvasID     string `json:"canvas_id"`
	LinkedSpecID string `json:"linked_spec_id"`
	CommitSHA    string `json:"commit_sha"`
	HeadBranch   string `json:"head_branch"`
	OrgID        string `json:"organization_id"`
}

func (c *SessionCommitAddedConsumer) onCommitAdded(ctx context.Context, event kafka.CloudEvent) error {
	if c.testRunRepo == nil {
		log.Printf("[SESSION-COMMIT] test run repo not wired; ignoring %s", event.Type)
		return nil
	}

	payload, orgID, err := parseSessionCommitPayload(event)
	if err != nil {
		log.Printf("[SESSION-COMMIT] unmarshal %s: %v", event.Type, err)
		return nil
	}
	if payload.CanvasID == "" {
		log.Printf("[SESSION-COMMIT] skipping: no canvas_id on session=%v sha=%s",
			payload.SessionID, payload.CommitSHA)
		return nil
	}
	canvasUUID, err := uuid.Parse(payload.CanvasID)
	if err != nil {
		log.Printf("[SESSION-COMMIT] non-uuid canvas_id %q; skipping", payload.CanvasID)
		return nil
	}

	// Pull every TestRun row attached to this canvas; we materialise a
	// fresh TestRun per unique (test_node_id, code_node_id) tuple so
	// history is preserved and the dispatcher can reuse prior metadata
	// (language, framework).
	existing, _, err := c.testRunRepo.FindByCanvas(ctx, canvasUUID, 500, 0)
	if err != nil {
		log.Printf("[SESSION-COMMIT] find tests for canvas=%s: %v", canvasUUID, err)
		return nil
	}
	if len(existing) == 0 {
		log.Printf("[SESSION-COMMIT] no test runs attached to canvas=%s; nothing to enqueue", canvasUUID)
		return nil
	}

	seen := make(map[string]struct{}, len(existing))
	newIDs := make([]string, 0, len(existing))
	for _, prior := range existing {
		key := prior.TestNodeID.String()
		if prior.CodeNodeID != nil {
			key += ":" + prior.CodeNodeID.String()
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		orgForRow := orgID
		if orgForRow == uuid.Nil {
			orgForRow = prior.OrganizationID
		}
		fresh := &domain.TestRun{
			ID:             uuid.New(),
			OrganizationID: orgForRow,
			TestNodeID:     prior.TestNodeID,
			CodeNodeID:     prior.CodeNodeID,
			CanvasID:       &canvasUUID,
			Language:       prior.Language,
			TestType:       prior.TestType,
			Status:         domain.TestRunStatusRunning,
			MaxRetries:     domain.DefaultMaxTestRetries,
			Framework:      prior.Framework,
		}
		if err := c.testRunRepo.Create(ctx, fresh); err != nil {
			log.Printf("[SESSION-COMMIT] create test run for node=%s: %v", prior.TestNodeID, err)
			continue
		}
		newIDs = append(newIDs, fresh.ID.String())

		// Submit to the pool scheduler if we have one; otherwise the
		// fresh TestRun row sits in "running" state and will be picked
		// up by the existing pending-processor cycle as a safety net.
		if c.pool != nil {
			job := usecase.PoolJob{Run: fresh}
			if err := c.pool.Submit(ctx, job); err != nil {
				log.Printf("[SESSION-COMMIT] pool submit for run=%s: %v", fresh.ID, err)
			}
		}
	}

	c.publishTestQueued(ctx, newIDs, "session_commit_added", map[string]any{
		"canvas_id":     canvasUUID.String(),
		"session_id":    fmt.Sprintf("%v", payload.SessionID),
		"repository_id": payload.RepositoryID,
		"commit_sha":    payload.CommitSHA,
	})
	log.Printf("[SESSION-COMMIT] queued %d test run(s) for canvas=%s sha=%s",
		len(newIDs), canvasUUID, payload.CommitSHA)
	return nil
}

func (c *SessionCommitAddedConsumer) publishTestQueued(ctx context.Context, testRunIDs []string, trigger string, extra map[string]any) {
	if c.publisher == nil || len(testRunIDs) == 0 {
		return
	}
	payload := map[string]any{
		"test_run_ids": testRunIDs,
		"trigger":      trigger,
	}
	for k, v := range extra {
		payload[k] = v
	}
	key := trigger
	if len(testRunIDs) > 0 {
		key = testRunIDs[0]
	}
	if err := c.publisher.Publish(ctx, usecase.EventTestQueued, key, payload); err != nil {
		log.Printf("[SESSION-COMMIT] publish %s: %v", usecase.EventTestQueued, err)
	}
}

func parseSessionCommitPayload(event kafka.CloudEvent) (sessionCommitPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return sessionCommitPayload{}, uuid.Nil, fmt.Errorf("marshal event data: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload sessionCommitPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return sessionCommitPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return sessionCommitPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
		}
	}
	var orgID uuid.UUID
	if envelope.OrganizationID != "" {
		if id, err := uuid.Parse(envelope.OrganizationID); err == nil {
			orgID = id
		}
	}
	if orgID == uuid.Nil && payload.OrgID != "" {
		if id, err := uuid.Parse(payload.OrgID); err == nil {
			orgID = id
		}
	}
	return payload, orgID, nil
}
