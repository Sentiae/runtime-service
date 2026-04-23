package usecase

import (
	"context"
	"encoding/json"
	"testing"

	kafka "github.com/sentiae/platform-kit/kafka"
)

// Filter behaviour — only merge.completed events should route to the
// onMergeCompleted path. Other git events must be ignored.
func TestPostMergeFullRunTrigger_FilterEventType(t *testing.T) {
	trig := NewPostMergeFullRunTrigger(nil, nil)

	cases := []struct {
		name      string
		eventType string
		wantErr   bool
	}{
		{"canonical merge completed", "sentiae.git.merge.completed", false},
		{"short merge completed", "git.merge.completed", false},
		{"unrelated push event", "sentiae.git.push", false},
		{"unrelated commit event", "git.commit.created", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := trig.HandleGitEvent(context.Background(), kafka.CloudEvent{Type: tc.eventType})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// The parser must gracefully handle both envelope-wrapped and flat
// payloads. We cover both shapes below.
func TestParsePostMergePayload_Shapes(t *testing.T) {
	envelope := map[string]any{
		"organization_id": "00000000-0000-0000-0000-000000000000",
		"metadata": map[string]any{
			"repository_id": "repo-1",
			"canvas_id":     "canvas-1",
		},
	}
	flat := map[string]any{
		"repository_id": "repo-2",
		"canvas_id":     "canvas-2",
	}

	cases := []struct {
		name     string
		payload  map[string]any
		wantRepo string
	}{
		{name: "envelope-wrapped", payload: envelope, wantRepo: "repo-1"},
		{name: "flat", payload: flat, wantRepo: "repo-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			payload, _, err := parsePostMergePayload(kafka.CloudEvent{Data: raw})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if payload.RepositoryID != tc.wantRepo {
				t.Fatalf("repo: got %q want %q", payload.RepositoryID, tc.wantRepo)
			}
		})
	}
}

// §B36 — pr.merged should route through the same onMergeCompleted path
// so external PR merges kick off a full-suite sweep too.
func TestPostMergeFullRunTrigger_HandlesPRMerged(t *testing.T) {
	trig := NewPostMergeFullRunTrigger(nil, nil)
	for _, typ := range []string{"sentiae.git.pr.merged", "git.pr.merged"} {
		if err := trig.HandleGitEvent(context.Background(), kafka.CloudEvent{Type: typ}); err != nil {
			t.Fatalf("handler should accept %s, got err=%v", typ, err)
		}
	}
}

// Idempotency: two events with the same commit sha must claim only
// once. A fresh sha must re-claim.
func TestPostMergeFullRunTrigger_ClaimFullSuiteIdempotent(t *testing.T) {
	trig := NewPostMergeFullRunTrigger(nil, nil)
	if !trig.claimFullSuite("sha-1") {
		t.Fatalf("first claim should succeed")
	}
	if trig.claimFullSuite("sha-1") {
		t.Fatalf("second claim on same sha must be blocked")
	}
	if !trig.claimFullSuite("sha-2") {
		t.Fatalf("different sha must claim fresh")
	}
}

// Ensure merge_sha on pr.merged payloads is normalized into SHA so the
// claim key matches whichever producer convention is used.
func TestParsePostMergePayload_MergeSHAFallback(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"merge_sha":     "abc123",
			"repository_id": "r",
			"canvas_id":     "c",
			"pr_number":     42,
		},
	})
	payload, _, err := parsePostMergePayload(kafka.CloudEvent{Data: raw})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if payload.SHA != "abc123" {
		t.Fatalf("merge_sha should populate SHA; got %q", payload.SHA)
	}
	if payload.PRNumber != 42 {
		t.Fatalf("pr_number should parse; got %d", payload.PRNumber)
	}
}

func TestFullRunEventTypes_CoverBothProducerConventions(t *testing.T) {
	must := map[string]bool{
		"sentiae.git.merge.completed": false,
		"git.merge.completed":         false,
		"sentiae.git.pr.merged":       false,
		"git.pr.merged":               false,
	}
	for _, typ := range FullRunEventTypes {
		if _, ok := must[typ]; ok {
			must[typ] = true
		}
	}
	for k, v := range must {
		if !v {
			t.Fatalf("FullRunEventTypes missing %q", k)
		}
	}
}
