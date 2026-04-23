// Package usecase — SessionLifecycleHandler watches
// `sentiae.git.session.merged` CloudEvents and kicks off a full test
// suite run for every TestRun associated with the merged session's
// canvas (§8.6).
//
// Rationale: post-merge we want to catch regressions that the
// commit-level affected-tests resolver might miss (e.g. integration
// coverage that spans multiple files not in the diff). Re-running
// every test on the canvas is the belt-and-suspenders safety net.
package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// SessionLifecycleHandler reacts to git session state transitions. The
// only event currently handled is `session.merged` — other transitions
// (created, closed, code_ready) are handled by other services.
type SessionLifecycleHandler struct {
	testRunRepo *postgres.TestRunRepo
	publisher   EventPublisher
}

// NewSessionLifecycleHandler constructs the handler. testRunRepo and
// publisher may be nil; the handler degrades gracefully with a warning
// log so a misconfigured deployment can't wedge the consumer loop.
func NewSessionLifecycleHandler(testRunRepo *postgres.TestRunRepo, publisher EventPublisher) *SessionLifecycleHandler {
	return &SessionLifecycleHandler{
		testRunRepo: testRunRepo,
		publisher:   publisher,
	}
}

// HandleGitEvent is the Kafka entry point. It filters by event type and
// delegates to the merge handler. Non-merge events are ignored.
func (h *SessionLifecycleHandler) HandleGitEvent(ctx context.Context, event kafka.CloudEvent) error {
	if h == nil {
		return nil
	}
	switch event.Type {
	case "sentiae.git.session.merged", "git.session.merged":
		return h.onSessionMerged(ctx, event)
	}
	return nil
}

// gitSessionMergedPayload mirrors the metadata that git-service emits
// on session merge. `canvas_id` may or may not be present depending on
// when the session was created; we handle both.
type gitSessionMergedPayload struct {
	SessionID     any    `json:"session_id"` // can be numeric or string
	CanvasID      string `json:"canvas_id"`
	RepositoryID  string `json:"repository_id"`
	LinkedSpecID  string `json:"linked_spec_id"`
	OrgID         string `json:"organization_id"`
	MergedBy      string `json:"merged_by"`
	PreviousState string `json:"previous_state"`
}

func (h *SessionLifecycleHandler) onSessionMerged(ctx context.Context, event kafka.CloudEvent) error {
	if h.testRunRepo == nil {
		log.Printf("[SESSION-MERGE] test run repo not wired; ignoring %s", event.Type)
		return nil
	}

	payload, orgID, err := parseGitSessionPayload(event)
	if err != nil {
		log.Printf("[SESSION-MERGE] unmarshal %s: %v", event.Type, err)
		return nil
	}
	if payload.CanvasID == "" {
		// Without a canvas id we can't locate the TestRun rows to
		// enqueue. Log and bail — this is expected for repos not yet
		// linked to a canvas.
		log.Printf("[SESSION-MERGE] skipping: no canvas_id on session=%v", payload.SessionID)
		return nil
	}
	canvasUUID, err := uuid.Parse(payload.CanvasID)
	if err != nil {
		log.Printf("[SESSION-MERGE] non-uuid canvas_id %q; skipping", payload.CanvasID)
		return nil
	}

	// Pull every TestRun linked to this canvas. We use a generous limit
	// because post-merge re-runs intentionally cover the full suite.
	existing, _, err := h.testRunRepo.FindByCanvas(ctx, canvasUUID, 500, 0)
	if err != nil {
		log.Printf("[SESSION-MERGE] find by canvas %s: %v", canvasUUID, err)
		return nil
	}
	if len(existing) == 0 {
		log.Printf("[SESSION-MERGE] no test runs attached to canvas=%s; nothing to enqueue", canvasUUID)
		return nil
	}

	// De-duplicate by (test_node_id, code_node_id): we only need one
	// fresh run per unique test, even if the canvas has many historical
	// rows.
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
		if err := h.testRunRepo.Create(ctx, fresh); err != nil {
			log.Printf("[SESSION-MERGE] create test run for node=%s: %v", prior.TestNodeID, err)
			continue
		}
		newIDs = append(newIDs, fresh.ID.String())
	}

	h.publishTestQueued(ctx, newIDs, "session_merged", map[string]any{
		"canvas_id":     canvasUUID.String(),
		"session_id":    fmt.Sprintf("%v", payload.SessionID),
		"repository_id": payload.RepositoryID,
	})
	log.Printf("[SESSION-MERGE] queued %d post-merge test run(s) for canvas=%s", len(newIDs), canvasUUID)
	return nil
}

func (h *SessionLifecycleHandler) publishTestQueued(ctx context.Context, testRunIDs []string, trigger string, extra map[string]any) {
	if h.publisher == nil || len(testRunIDs) == 0 {
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
	if err := h.publisher.Publish(ctx, EventTestQueued, key, payload); err != nil {
		log.Printf("[SESSION-MERGE] publish %s: %v", EventTestQueued, err)
	}
}

func parseGitSessionPayload(event kafka.CloudEvent) (gitSessionMergedPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return gitSessionMergedPayload{}, uuid.Nil, fmt.Errorf("marshal event data: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload gitSessionMergedPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return gitSessionMergedPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return gitSessionMergedPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
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
