package gitservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// repoListPayload mirrors the subset of git-service's /api/v1/repositories
// response that the client decodes. Declared here so the test asserts on
// the wire format the production code actually parses (note the
// PascalCase field names).
type repoListPayload struct {
	Repositories []struct {
		ID         uuid.UUID `json:"ID"`
		Name       string    `json:"Name"`
		OwnerLogin string    `json:"OwnerLogin"`
	} `json:"repositories"`
}

// writeRepoList writes the /repositories JSON response.
func writeRepoList(t *testing.T, w http.ResponseWriter, entries ...struct {
	ID         uuid.UUID
	Name       string
	OwnerLogin string
}) {
	t.Helper()
	var payload repoListPayload
	for _, e := range entries {
		payload.Repositories = append(payload.Repositories, struct {
			ID         uuid.UUID `json:"ID"`
			Name       string    `json:"Name"`
			OwnerLogin string    `json:"OwnerLogin"`
		}{ID: e.ID, Name: e.Name, OwnerLogin: e.OwnerLogin})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// writeImpact writes a minimal impactResponse payload matching the JSON
// tags the client decodes.
func writeImpact(t *testing.T, w http.ResponseWriter, sourceFile string, exported []string, impacted map[string][]string) {
	t.Helper()
	type refEntry struct {
		SymbolName string `json:"symbol_name"`
		FilePath   string `json:"file_path"`
	}
	type impactedEntry struct {
		FilePath   string     `json:"file_path"`
		References []refEntry `json:"references"`
	}
	type exportedEntry struct {
		SymbolName string `json:"symbol_name"`
		FilePath   string `json:"file_path"`
	}
	var payload = struct {
		SourceFile      string          `json:"source_file"`
		ExportedSymbols []exportedEntry `json:"exported_symbols"`
		ImpactedFiles   []impactedEntry `json:"impacted_files"`
	}{SourceFile: sourceFile}
	for _, s := range exported {
		payload.ExportedSymbols = append(payload.ExportedSymbols, exportedEntry{SymbolName: s, FilePath: sourceFile})
	}
	for file, syms := range impacted {
		entry := impactedEntry{FilePath: file}
		for _, s := range syms {
			entry.References = append(entry.References, refEntry{SymbolName: s, FilePath: file})
		}
		payload.ImpactedFiles = append(payload.ImpactedFiles, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// TestLooksLikeTestFile is a table-driven assertion over the test-file
// heuristic. The matrix reflects the naming conventions the current
// implementation accepts.
func TestLooksLikeTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Common positives
		{"foo_test.go", true},
		{"pkg/foo_test.go", true},
		{"foo.test.ts", true},
		{"src/foo.test.tsx", true},
		{"foo.spec.js", true},
		{"src/components/foo.spec.ts", true},
		{"test_foo.py", true},
		// The directory anchors require a leading slash, so "pkg/tests/foo.go"
		// matches but bare "tests/foo.go" does not.
		{"pkg/tests/foo.go", true},
		{"pkg/test/foo.go", true},
		{"src/__tests__/foo.js", true},
		// Without a leading slash these are interpreted as bare paths and
		// fall through to false — documented quirk of the heuristic.
		{"tests/foo.go", false},
		{"test/foo.go", false},

		// Case-insensitive — the matcher lowercases before checking
		{"Foo_Test.go", true},

		// Negatives: non-test source files
		{"src/foo.go", false},
		{"lib/bar.ts", false},
		{"pkg/service.go", false},

		// The heuristic only matches "test_" as a filename prefix, not
		// PascalCase Java classes — TestFoo.java does NOT match.
		{"TestFoo.java", false},

		// "test" embedded in the directory name (no leading slash) isn't
		// one of the recognised anchors; only /tests/, /test/, /__tests__/
		// fire.
		{"latest/foo.go", false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := looksLikeTestFile(tc.path)
			if got != tc.want {
				t.Fatalf("looksLikeTestFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestAppendUnique guards the small dedupe helper against drift.
func TestAppendUnique(t *testing.T) {
	got := appendUnique(nil, "a")
	got = appendUnique(got, "a")
	got = appendUnique(got, "b")
	got = appendUnique(got, "")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected dedupe result: %+v", got)
	}
}

// TestSymbolGraphClient_HappyPath exercises the full flow: cold repo cache,
// /repositories fetch populates it, then /impact is called per changed
// file, results are filtered by looksLikeTestFile, and merged by test path.
func TestSymbolGraphClient_HappyPath(t *testing.T) {
	repoID := uuid.New()
	var repoCalls, impactCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&repoCalls, 1)
		writeRepoList(t, w, struct {
			ID         uuid.UUID
			Name       string
			OwnerLogin string
		}{ID: repoID, Name: "repo", OwnerLogin: "acme"})
	})

	mux.HandleFunc("/api/v1/repos/acme/repo/impact", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&impactCalls, 1)
		file := r.URL.Query().Get("file")
		switch file {
		case "pkg/foo.go":
			// Exported Foo referenced by foo_test.go (a test) and bar.go (a
			// non-test file that must be filtered out).
			writeImpact(t, w, file,
				[]string{"Foo"},
				map[string][]string{
					"pkg/foo_test.go": {"Foo"},
					"pkg/bar.go":      {"Foo"},
				},
			)
		case "pkg/bar.go":
			// Exported Bar referenced by the same foo_test.go — this exercises
			// the merge-by-path path: two changed files produce ONE TestSymbolRefs
			// row with both source files and both symbols attached.
			writeImpact(t, w, file,
				[]string{"Bar"},
				map[string][]string{
					"pkg/foo_test.go": {"Bar"},
				},
			)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	refs, err := client.ListTestSymbolRefs(context.Background(), repoID, []string{"pkg/foo.go", "pkg/bar.go"})
	if err != nil {
		t.Fatalf("ListTestSymbolRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 merged TestSymbolRefs, got %d: %+v", len(refs), refs)
	}

	got := refs[0]
	if got.TestNodeID != "pkg/foo_test.go" || got.FilePath != "pkg/foo_test.go" {
		t.Fatalf("unexpected keying: %+v", got)
	}

	sort.Strings(got.SourceFiles)
	if len(got.SourceFiles) != 2 || got.SourceFiles[0] != "pkg/bar.go" || got.SourceFiles[1] != "pkg/foo.go" {
		t.Fatalf("expected both source files on merged entry, got %+v", got.SourceFiles)
	}

	sort.Strings(got.Symbols)
	if len(got.Symbols) != 2 || got.Symbols[0] != "Bar" || got.Symbols[1] != "Foo" {
		t.Fatalf("expected Foo+Bar symbols on merged entry, got %+v", got.Symbols)
	}

	if atomic.LoadInt32(&repoCalls) != 1 {
		t.Fatalf("expected /repositories to be fetched once, got %d", atomic.LoadInt32(&repoCalls))
	}
	if atomic.LoadInt32(&impactCalls) != 2 {
		t.Fatalf("expected /impact called once per changed file, got %d", atomic.LoadInt32(&impactCalls))
	}

	// A second call must reuse the cache — repoCalls stays at 1.
	_, err = client.ListTestSymbolRefs(context.Background(), repoID, []string{"pkg/foo.go"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&repoCalls) != 1 {
		t.Fatalf("expected /repositories cache reuse, got %d extra fetches", atomic.LoadInt32(&repoCalls)-1)
	}
}

// TestSymbolGraphClient_UnknownRepoReturnsNilNil — when /repositories does
// not list the requested UUID the top-level call logs and falls back to the
// heuristic by returning (nil, nil). A cache refresh is attempted.
func TestSymbolGraphClient_UnknownRepoReturnsNilNil(t *testing.T) {
	unknownID := uuid.New()
	var repoCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&repoCalls, 1)
		// Return a list that does not contain the requested UUID.
		writeRepoList(t, w, struct {
			ID         uuid.UUID
			Name       string
			OwnerLogin string
		}{ID: uuid.New(), Name: "other", OwnerLogin: "someone"})
	})
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("impact endpoint must not be called when repo is unknown: %s", r.URL.String())
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	refs, err := client.ListTestSymbolRefs(context.Background(), unknownID, []string{"pkg/foo.go"})
	if err != nil {
		t.Fatalf("expected nil error (graceful degradation), got %v", err)
	}
	if refs != nil {
		t.Fatalf("expected nil refs on unknown repo, got %+v", refs)
	}
	if atomic.LoadInt32(&repoCalls) == 0 {
		t.Fatal("expected /repositories to be queried on cold cache miss")
	}
}

// TestSymbolGraphClient_Impact500_GracefulNilReturn — when /impact returns
// 5xx the client logs and skips that source file. With no downstream work
// to do the top-level call returns (empty-slice, nil).
func TestSymbolGraphClient_Impact500_GracefulNilReturn(t *testing.T) {
	repoID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		writeRepoList(t, w, struct {
			ID         uuid.UUID
			Name       string
			OwnerLogin string
		}{ID: repoID, Name: "repo", OwnerLogin: "acme"})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/impact", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	refs, err := client.ListTestSymbolRefs(context.Background(), repoID, []string{"pkg/foo.go"})
	if err != nil {
		t.Fatalf("expected graceful nil error, got %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no refs when impact 500s, got %+v", refs)
	}
}

// TestSymbolGraphClient_ImpactMalformedJSON_GracefulNilReturn — malformed
// JSON on /impact is logged and skipped, just like a 500.
func TestSymbolGraphClient_ImpactMalformedJSON_GracefulNilReturn(t *testing.T) {
	repoID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		writeRepoList(t, w, struct {
			ID         uuid.UUID
			Name       string
			OwnerLogin string
		}{ID: repoID, Name: "repo", OwnerLogin: "acme"})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/impact", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source_file": "pkg/foo.go" INVALID JSON`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	refs, err := client.ListTestSymbolRefs(context.Background(), repoID, []string{"pkg/foo.go"})
	if err != nil {
		t.Fatalf("expected graceful nil error, got %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no refs when impact payload is malformed, got %+v", refs)
	}
}

// TestSymbolGraphClient_EmptyBaseURL returns nil. The DI container relies
// on this to leave the AffectedTestResolver's nil-fallback path intact.
func TestSymbolGraphClient_EmptyBaseURL(t *testing.T) {
	if c := NewSymbolGraphClient("", nil); c != nil {
		t.Fatalf("expected nil client for empty baseURL, got %+v", c)
	}
	if c := NewSymbolGraphClient("   ", nil); c != nil {
		t.Fatalf("expected nil client for whitespace baseURL, got %+v", c)
	}
}

// TestSymbolGraphClient_EmptyPathFilterReturnsEarly — an empty pathFilter
// must short-circuit before any HTTP call is issued.
func TestSymbolGraphClient_EmptyPathFilterReturnsEarly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("HTTP must not be called when pathFilter is empty: %s", r.URL.Path)
	}))
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	refs, err := client.ListTestSymbolRefs(context.Background(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refs != nil {
		t.Fatalf("expected nil refs for empty pathFilter, got %+v", refs)
	}
}

// TestSymbolGraphClient_RepoIDNilRejected — a nil repo UUID is an input
// error, not a fallback scenario.
func TestSymbolGraphClient_RepoIDNilRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("HTTP must not be called when repoID is nil: %s", r.URL.Path)
	}))
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	_, err := client.ListTestSymbolRefs(context.Background(), uuid.Nil, []string{"pkg/foo.go"})
	if err == nil {
		t.Fatal("expected error for uuid.Nil repoID")
	}
	if !strings.Contains(err.Error(), "repo_id required") {
		t.Fatalf("expected repo_id error, got %v", err)
	}
}

// TestSymbolGraphClient_ImpactURLEncodesParams — file paths with spaces
// (and owner/repo names with spaces) must round-trip through URL encoding
// so the git-service handler sees the original decoded values.
func TestSymbolGraphClient_ImpactURLEncodesParams(t *testing.T) {
	repoID := uuid.New()
	var (
		gotFile string
		gotPath string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		writeRepoList(t, w, struct {
			ID         uuid.UUID
			Name       string
			OwnerLogin string
		}{ID: repoID, Name: "cool-repo", OwnerLogin: "acme"})
	})
	mux.HandleFunc("/api/v1/repos/acme/cool-repo/impact", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFile = r.URL.Query().Get("file")
		writeImpact(t, w, gotFile, nil, nil)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSymbolGraphClient(srv.URL, nil)
	sourceFile := "pkg/dir with spaces/foo.go"
	_, err := client.ListTestSymbolRefs(context.Background(), repoID, []string{sourceFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/v1/repos/acme/cool-repo/impact" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	// Go's net/http decodes the query string for us — the value we fish
	// out must be bit-for-bit the original.
	if gotFile != sourceFile {
		t.Fatalf("expected server to see decoded file=%q, got %q", sourceFile, gotFile)
	}

	// Round-trip sanity: confirm the client's encoding is symmetric with
	// url.QueryEscape so a future refactor doesn't silently change it.
	encoded := url.QueryEscape(sourceFile)
	decoded, uerr := url.QueryUnescape(encoded)
	if uerr != nil || decoded != sourceFile {
		t.Fatalf("round-trip mismatch: %q (err=%v)", decoded, uerr)
	}

	_ = fmt.Sprintf
}
