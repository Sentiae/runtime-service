package usecase

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// bindingKeys returns the top-level JSON keys of a marshalled binding, sorted.
func bindingKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal binding: %v", err)
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// T3.3 — the binding is the ONE document a sidecar is handed, and its shape is
// a contract with cmd/node-sidecar: exactly invocation, run, node, secrets, and
// egress only when there IS egress. A node that declared no egress must receive
// a document with no egress member at all, so "no proxy" is the absence of the
// field rather than a null the sidecar has to interpret.
//
// The plaintext travels here and nowhere else: the value is carried inside
// secrets (anchor — it IS in the document) and appears in no other member.
//
// Control: drop the omitempty on Egress ⇒ a secret-only binding emits
// "egress": null and the exact-keys assertion fails.
func TestSidecarBinding_Shape(t *testing.T) {
	const canary = "p4-canary-secret-value"
	run := uuid.New()

	base := SidecarBinding{
		Invocation: "inv-" + uuid.New().String(),
		Run:        run.String(),
		Node:       "greet",
		Secrets: map[string]SecretAnswer{
			"greeting_suffix": {Handle: "handle:aaaa", Found: true, Value: canary},
			"absent":          {Handle: "handle:bbbb"},
		},
	}

	t.Run("no egress declared", func(t *testing.T) {
		raw, err := json.Marshal(base)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := bindingKeys(t, raw)
		want := []string{"invocation", "node", "run", "secrets"}
		if !slices.Equal(got, want) {
			t.Fatalf("binding keys = %v, want %v", got, want)
		}
	})

	t.Run("egress declared", func(t *testing.T) {
		withEgress := base
		withEgress.Egress = &EgressBinding{
			Patterns: []string{"httpbin.org"},
			Token:    strings.Repeat("ab", 32),
			Subnet:   "10.201.0.8/29",
		}
		raw, err := json.Marshal(withEgress)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := bindingKeys(t, raw)
		want := []string{"egress", "invocation", "node", "run", "secrets"}
		if !slices.Equal(got, want) {
			t.Fatalf("binding keys = %v, want %v", got, want)
		}

		var doc struct {
			Egress EgressBinding `json:"egress"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal egress: %v", err)
		}
		if !slices.Equal(doc.Egress.Patterns, []string{"httpbin.org"}) ||
			doc.Egress.Token != strings.Repeat("ab", 32) || doc.Egress.Subnet != "10.201.0.8/29" {
			t.Fatalf("egress = %#v, want the declared patterns, token and subnet", doc.Egress)
		}
	})

	t.Run("the value is carried by secrets and by nothing else", func(t *testing.T) {
		raw, err := json.Marshal(base)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), canary) {
			t.Fatalf("anchor: the binding does not carry the value at all: %s", raw)
		}

		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		delete(doc, "secrets")
		rest, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if strings.Contains(string(rest), canary) {
			t.Fatalf("the value leaks outside secrets: %s", rest)
		}
	})
}
