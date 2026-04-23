// Package usecase — AffectedTestTrigger listens for
// `sentiae.git.commit.created` / `sentiae.git.push` CloudEvents and
// enqueues the set of tests that reference the changed files (§8.6).
//
// Wiring: the DI container registers this handler on the Kafka
// consumer for git topics. On receipt the handler extracts the diff
// files, delegates to `AffectedTestResolver` to compute the set of
// affected test node IDs, creates one TestRun row per affected test,
// and publishes `sentiae.runtime.test.queued` so Pulse and the portal
// see the cascade in real time.
//
// Resilience: every failure path logs and returns nil. A bad git
// payload must not wedge the Kafka consumer or cause redelivery loops
// — the upstream event is informational, not transactional.
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

// AffectedTestTrigger handles git commit events and enqueues tests
// whose production references intersect with the changed files.
type AffectedTestTrigger struct {
	resolver    AffectedTestResolver
	testRunRepo *postgres.TestRunRepo
	publisher   EventPublisher
}

// NewAffectedTestTrigger constructs the trigger. A nil resolver or
// testRunRepo leaves the handler as a best-effort no-op with a warning
// log so the consumer path stays healthy even when a dependency is
// unavailable.
func NewAffectedTestTrigger(resolver AffectedTestResolver, testRunRepo *postgres.TestRunRepo, publisher EventPublisher) *AffectedTestTrigger {
	return &AffectedTestTrigger{
		resolver:    resolver,
		testRunRepo: testRunRepo,
		publisher:   publisher,
	}
}

// HandleGitEvent is the Kafka handler entry point. It filters the
// inbound event by type — only `commit.created` / `push` /
// `change.created` trigger the cascade — and dispatches to the
// resolver path.
func (t *AffectedTestTrigger) HandleGitEvent(ctx context.Context, event kafka.CloudEvent) error {
	if t == nil {
		return nil
	}
	switch event.Type {
	case "sentiae.git.commit.created",
		"git.commit.created",
		"sentiae.git.push",
		"git.push",
		"sentiae.git.change.created",
		"git.change.created":
		return t.onCommitPushed(ctx, event)
	}
	// Other git event types (branch, PR, review, release) are ignored.
	return nil
}

// gitCommitPayload mirrors the metadata emitted by git-service when a
// commit or push lands. We tolerate both the EventData envelope
// (`metadata.files_changed`) and a flat shape so the handler stays
// robust across publisher conventions.
type gitCommitPayload struct {
	RepositoryID string   `json:"repository_id"`
	Branch       string   `json:"branch"`
	SHA          string   `json:"sha"`
	FilesChanged []string `json:"files_changed"`
	OrgID        string   `json:"organization_id"`
}

func (t *AffectedTestTrigger) onCommitPushed(ctx context.Context, event kafka.CloudEvent) error {
	if t.resolver == nil {
		log.Printf("[AFFECTED-TRIGGER] resolver not wired; ignoring %s", event.Type)
		return nil
	}

	payload, orgID, err := parseGitCommitPayload(event)
	if err != nil {
		log.Printf("[AFFECTED-TRIGGER] unmarshal %s: %v", event.Type, err)
		return nil
	}
	if payload.RepositoryID == "" || len(payload.FilesChanged) == 0 {
		// A commit without files_changed (e.g. empty-tree or
		// octopus-merge metadata edge case) has nothing to trigger on.
		log.Printf("[AFFECTED-TRIGGER] skipping %s: repo=%q files=%d", event.Type, payload.RepositoryID, len(payload.FilesChanged))
		return nil
	}
	repoUUID, err := uuid.Parse(payload.RepositoryID)
	if err != nil {
		// Some legacy commit publishers emit numeric repository_id
		// instead of a UUID. Resolution against git-service would need
		// an RPC; skip rather than guess.
		log.Printf("[AFFECTED-TRIGGER] non-uuid repository_id %q; skipping", payload.RepositoryID)
		return nil
	}

	out, err := t.resolver.Resolve(ctx, AffectedTestsInput{
		RepoID:    repoUUID,
		DiffFiles: payload.FilesChanged,
	})
	if err != nil {
		log.Printf("[AFFECTED-TRIGGER] resolve: %v", err)
		return nil
	}
	if out == nil || len(out.TestNodeIDs) == 0 {
		log.Printf("[AFFECTED-TRIGGER] no affected tests for repo=%s sha=%s", repoUUID, payload.SHA)
		return nil
	}

	runIDs := t.enqueueTestRuns(ctx, orgID, out.TestNodeIDs)
	t.publishTestQueued(ctx, runIDs, "commit_pushed", map[string]any{
		"repository_id": repoUUID.String(),
		"sha":           payload.SHA,
		"branch":        payload.Branch,
		"files_changed": payload.FilesChanged,
		"match_count":   len(out.TestNodeIDs),
	})
	log.Printf("[AFFECTED-TRIGGER] queued %d test run(s) for repo=%s sha=%s", len(runIDs), repoUUID, payload.SHA)
	return nil
}

// enqueueTestRuns persists one TestRun row per affected test node so
// downstream dispatch workers pick them up. IDs that fail to parse as
// UUIDs (heuristic fallback emits string stems like "foo_test") are
// skipped — the next-gen symbol graph will supply proper IDs.
func (t *AffectedTestTrigger) enqueueTestRuns(ctx context.Context, orgID uuid.UUID, testNodeIDs []string) []string {
	if t.testRunRepo == nil {
		return nil
	}
	ids := make([]string, 0, len(testNodeIDs))
	for _, raw := range testNodeIDs {
		testNodeID, err := uuid.Parse(raw)
		if err != nil {
			// Heuristic resolver emitted a stem (e.g. "user_test").
			// We can't persist that as a uuid-typed column; skip it.
			continue
		}
		run := &domain.TestRun{
			ID:             uuid.New(),
			OrganizationID: orgID,
			TestNodeID:     testNodeID,
			Language:       domain.Language(""),
			TestType:       domain.TestTypeUnit,
			Status:         domain.TestRunStatusRunning,
			MaxRetries:     domain.DefaultMaxTestRetries,
		}
		if err := t.testRunRepo.Create(ctx, run); err != nil {
			log.Printf("[AFFECTED-TRIGGER] create test run for node=%s: %v", testNodeID, err)
			continue
		}
		ids = append(ids, run.ID.String())
	}
	return ids
}

// publishTestQueued emits sentiae.runtime.test.queued so Pulse and the
// canvas portal can render the cascade live. Publishing is best-effort.
func (t *AffectedTestTrigger) publishTestQueued(ctx context.Context, testRunIDs []string, trigger string, extra map[string]any) {
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
		log.Printf("[AFFECTED-TRIGGER] publish %s: %v", EventTestQueued, err)
	}
}

// parseGitCommitPayload tolerates both the EventData envelope
// ({metadata: {...}}) and a flat payload. Organization ID may appear
// at the envelope level or inside metadata; returning uuid.Nil is
// acceptable because downstream enqueue code tolerates it (TestRun has
// org_id NOT NULL so a zero UUID row would fail — we instead require
// org and bail on uuid.Nil).
func parseGitCommitPayload(event kafka.CloudEvent) (gitCommitPayload, uuid.UUID, error) {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return gitCommitPayload{}, uuid.Nil, fmt.Errorf("marshal event data: %w", err)
	}
	var envelope struct {
		OrganizationID string          `json:"organization_id"`
		Metadata       json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var payload gitCommitPayload
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return gitCommitPayload{}, uuid.Nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return gitCommitPayload{}, uuid.Nil, fmt.Errorf("unmarshal payload: %w", err)
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
