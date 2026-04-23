// Package usecase — PostMergeFullRunTrigger listens for
// `sentiae.git.merge.completed` CloudEvents and kicks off a full-suite
// test run for every test node known to the affected canvas (§8.6).
//
// This differs from AffectedTestTrigger (commit-level) and
// SessionLifecycleHandler (session.merged) — merge.completed is the
// authoritative post-merge event from git-service, and we treat it as
// the trigger for the belt-and-suspenders "run everything" sweep that
// catches regressions not visible in the commit diff.
package usecase

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
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// PostMergeFullRunTrigger handles merge.completed events by enqueuing
// a fresh TestRun row for every unique test node previously seen on the
// canvas. The design is intentionally symmetric with
// SessionLifecycleHandler so the two paths converge on the same
// test.queued contract.
//
// §B36 — the trigger also listens for `sentiae.git.pr.merged` events so
// GitHub / GitLab style PR merges (distinct from the internal session
// merge path) also kick off a full suite. Idempotency keyed by
// (commit_sha, full_suite) prevents a retried Kafka delivery from
// double-running.
type PostMergeFullRunTrigger struct {
	testRunRepo *postgres.TestRunRepo
	publisher   EventPublisher

	mu       sync.Mutex
	seenKeys map[string]time.Time
}

// NewPostMergeFullRunTrigger wires the handler. Nil inputs degrade to
// a best-effort no-op that logs so a misconfigured deployment never
// wedges the Kafka consumer.
func NewPostMergeFullRunTrigger(testRunRepo *postgres.TestRunRepo, publisher EventPublisher) *PostMergeFullRunTrigger {
	return &PostMergeFullRunTrigger{
		testRunRepo: testRunRepo,
		publisher:   publisher,
		seenKeys:    make(map[string]time.Time),
	}
}

// postMergeIdempotencyWindow is long enough to absorb typical Kafka
// consumer re-delivery (minutes) without starving genuine re-runs
// across deploy boundaries.
const postMergeIdempotencyWindow = time.Hour

// claimFullSuite returns true iff this is the first time we've seen
// (commit_sha, full_suite) within the idempotency window — so duplicate
// events (Kafka redelivery, downstream retry, double-producer) do not
// double-dispatch the full suite.
func (t *PostMergeFullRunTrigger) claimFullSuite(sha string) bool {
	key := sha + "|full_suite"
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if ts, ok := t.seenKeys[key]; ok && now.Sub(ts) < postMergeIdempotencyWindow {
		return false
	}
	t.seenKeys[key] = now
	// Opportunistic eviction.
	if len(t.seenKeys) > 1024 {
		for k, ts := range t.seenKeys {
			if now.Sub(ts) >= postMergeIdempotencyWindow {
				delete(t.seenKeys, k)
			}
		}
	}
	return true
}

// HandleGitEvent is the Kafka entry point. Filter on event type so this
// handler is safe to register on the shared consumer.
func (t *PostMergeFullRunTrigger) HandleGitEvent(ctx context.Context, event kafka.CloudEvent) error {
	if t == nil {
		return nil
	}
	switch event.Type {
	case "sentiae.git.merge.completed", "git.merge.completed",
		"sentiae.git.pr.merged", "git.pr.merged":
		return t.onMergeCompleted(ctx, event)
	}
	return nil
}

// FullRunEventTypes is the set of Kafka event types the trigger
// consumes. Exposed so the DI container can register all of them in one
// place without the handler package leaking strings.
var FullRunEventTypes = []string{
	"sentiae.git.merge.completed",
	"git.merge.completed",
	"sentiae.git.pr.merged",
	"git.pr.merged",
}

// gitMergeCompletedPayload mirrors the metadata git-service emits on
// merge.completed. Both repository_id and canvas_id are expected; we
// tolerate missing canvas_id with a log-and-skip path.
type gitMergeCompletedPayload struct {
	MergeID      string `json:"merge_id"`
	PRNumber     int    `json:"pr_number"`
	RepositoryID string `json:"repository_id"`
	CanvasID     string `json:"canvas_id"`
	Branch       string `json:"branch"`
	// SHA covers both the merge-completed and pr.merged shapes. pr.merged
	// producers emit merge_sha; we normalize into SHA below.
	SHA      string `json:"sha"`
	MergeSHA string `json:"merge_sha"`
	OrgID    string `json:"organization_id"`
	MergedBy string `json:"merged_by"`
}

func (t *PostMergeFullRunTrigger) onMergeCompleted(ctx context.Context, event kafka.CloudEvent) error {
	if t.testRunRepo == nil {
		log.Printf("[POST-MERGE] test run repo not wired; ignoring %s", event.Type)
		return nil
	}
	payload, orgID, err := parsePostMergePayload(event)
	if err != nil {
		log.Printf("[POST-MERGE] unmarshal %s: %v", event.Type, err)
		return nil
	}
	if payload.CanvasID == "" {
		log.Printf("[POST-MERGE] skipping: no canvas_id on merge=%s repo=%s", payload.MergeID, payload.RepositoryID)
		return nil
	}
	canvasUUID, err := uuid.Parse(payload.CanvasID)
	if err != nil {
		log.Printf("[POST-MERGE] non-uuid canvas_id %q; skipping", payload.CanvasID)
		return nil
	}

	if payload.SHA != "" && !t.claimFullSuite(payload.SHA) {
		log.Printf("[POST-MERGE] idempotency hit: full_suite already dispatched for sha=%s", payload.SHA)
		return nil
	}

	existing, _, err := t.testRunRepo.FindByCanvas(ctx, canvasUUID, 500, 0)
	if err != nil {
		log.Printf("[POST-MERGE] find by canvas %s: %v", canvasUUID, err)
		return nil
	}
	if len(existing) == 0 {
		log.Printf("[POST-MERGE] no test nodes attached to canvas=%s; nothing to enqueue", canvasUUID)
		return nil
	}

	// De-duplicate by test_node_id + code_node_id so we only enqueue one
	// fresh run per unique test even if the canvas has many historical
	// rows for the same test.
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
		if err := t.testRunRepo.Create(ctx, fresh); err != nil {
			log.Printf("[POST-MERGE] create test run for node=%s: %v", prior.TestNodeID, err)
			continue
		}
		newIDs = append(newIDs, fresh.ID.String())
	}

	t.publishTestQueued(ctx, newIDs, "merge_completed", map[string]any{
		"canvas_id":     canvasUUID.String(),
		"merge_id":      payload.MergeID,
		"repository_id": payload.RepositoryID,
		"sha":           payload.SHA,
		"branch":        payload.Branch,
	})
	log.Printf("[POST-MERGE] queued %d full-suite test run(s) for canvas=%s", len(newIDs), canvasUUID)
	return nil
}

func (t *PostMergeFullRunTrigger) publishTestQueued(ctx context.Context, testRunIDs []string, trigger string, extra map[string]any) {
	if t.publisher == nil || len(testRunIDs) == 0 {
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
	if err := t.publisher.Publish(ctx, EventTestQueued, key, payload); err != nil {
		log.Printf("[POST-MERGE] publish %s: %v", EventTestQueued, err)
	}
}

func parsePostMergePayload(event kafka.CloudEvent) (gitMergeCompletedPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return gitMergeCompletedPayload{}, uuid.Nil, fmt.Errorf("marshal event data: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload gitMergeCompletedPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return gitMergeCompletedPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return gitMergeCompletedPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
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
	if payload.SHA == "" && payload.MergeSHA != "" {
		payload.SHA = payload.MergeSHA
	}
	return payload, orgID, nil
}
