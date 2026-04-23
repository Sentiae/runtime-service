// Package gitservice wraps git-service's HTTP symbol-graph API so the
// runtime-service AffectedTestResolver can walk symbol → reference edges
// instead of relying on the filename-stem heuristic.
//
// Shape: git-service exposes its symbol graph under
//
//	GET /api/v1/repos/{owner}/{repo}/impact?file=<path>
//
// which returns every file that references exported symbols defined in
// the target file. Test files are identified by filename heuristic
// (`_test.`, `.spec.`, `.test.`, `test_`) because git-service does not
// surface a "kind=test" flag today.
//
// UUID→owner/repo resolution: runtime-service only knows a repository's
// UUID (it arrives on `git.commit.pushed` CloudEvents). git-service's
// impact endpoint is keyed on owner/repo, so the client lazily fetches
// `GET /api/v1/repositories` and caches the UUID→(owner,repo) mapping.
// The cache is refreshed on miss so newly-created repos resolve on
// their first lookup.
//
// Graceful degradation: any HTTP error or decode failure returns an
// empty slice plus a logged warning. The resolver then falls back to
// its filename-stem heuristic, so this client is always safe to wire.
package gitservice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// SymbolGraphClient is an HTTP wrapper around git-service's symbol graph
// endpoints. It implements usecase.SymbolGraphClient.
type SymbolGraphClient struct {
	baseURL string
	client  *http.Client

	mu       sync.RWMutex
	repoByID map[uuid.UUID]repoCoords
}

type repoCoords struct {
	Owner string
	Name  string
}

// NewSymbolGraphClient builds a client pointed at git-service's HTTP
// origin (e.g. http://git-service:8085). An empty baseURL returns nil
// so the DI container can keep the AffectedTestResolver's nil-fallback
// path intact without special-casing.
func NewSymbolGraphClient(baseURL string, httpClient *http.Client) *SymbolGraphClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &SymbolGraphClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		client:   httpClient,
		repoByID: make(map[uuid.UUID]repoCoords),
	}
}

// ListTestSymbolRefs implements usecase.SymbolGraphClient. For each file
// in pathFilter the client queries git-service's impact-analysis
// endpoint and keeps the impacted files that look like tests. The
// returned TestSymbolRefs are keyed by test file path (used as the
// TestNodeID) and carry both the upstream source file that triggered
// the match and the symbol names that bridged the edge.
//
// TODO(symbol-graph-v2): TestNodeID is the test file path today because
// git-service's /impact endpoint is file-keyed. The caller
// (AffectedTestTrigger.enqueueTestRuns) will skip any TestNodeID that
// isn't a UUID, so persistent test-run rows still need a future
// git-service endpoint that emits per-test-function UUIDs. Until then
// the resolver returns the file-path matches so callers with a
// heuristic-friendly consumer (e.g. downstream dispatchers keyed on
// file paths) can still use them.
func (c *SymbolGraphClient) ListTestSymbolRefs(ctx context.Context, repoID uuid.UUID, pathFilter []string) ([]usecase.TestSymbolRefs, error) {
	if c == nil {
		return nil, fmt.Errorf("gitservice: symbol graph client not configured")
	}
	if repoID == uuid.Nil {
		return nil, fmt.Errorf("gitservice: repo_id required")
	}
	if len(pathFilter) == 0 {
		return nil, nil
	}

	coords, err := c.resolveRepo(ctx, repoID)
	if err != nil {
		log.Printf("[symbol-graph] resolve repo %s: %v (falling back to heuristic)", repoID, err)
		return nil, nil
	}

	// Merge results from all requested files keyed by test path so a
	// single test referencing multiple changed files dedupes cleanly.
	merged := make(map[string]*usecase.TestSymbolRefs)

	for _, sourceFile := range pathFilter {
		impact, err := c.fetchImpact(ctx, coords.Owner, coords.Name, sourceFile)
		if err != nil {
			log.Printf("[symbol-graph] impact owner=%s repo=%s file=%s: %v", coords.Owner, coords.Name, sourceFile, err)
			continue
		}
		if impact == nil {
			continue
		}

		// Collect exported symbol names for this source file once per
		// file — they annotate every matching test row.
		symbols := make([]string, 0, len(impact.ExportedSymbols))
		for _, sym := range impact.ExportedSymbols {
			symbols = append(symbols, sym.SymbolName)
		}

		for _, impacted := range impact.ImpactedFiles {
			if !looksLikeTestFile(impacted.FilePath) {
				continue
			}
			entry, ok := merged[impacted.FilePath]
			if !ok {
				entry = &usecase.TestSymbolRefs{
					TestNodeID: impacted.FilePath,
					TestName:   impacted.FilePath,
					FilePath:   impacted.FilePath,
				}
				merged[impacted.FilePath] = entry
			}
			entry.SourceFiles = appendUnique(entry.SourceFiles, sourceFile)
			for _, sym := range symbols {
				entry.Symbols = appendUnique(entry.Symbols, sym)
			}
			for _, ref := range impacted.References {
				if ref.SymbolName != "" {
					entry.Symbols = appendUnique(entry.Symbols, ref.SymbolName)
				}
			}
		}
	}

	out := make([]usecase.TestSymbolRefs, 0, len(merged))
	for _, v := range merged {
		out = append(out, *v)
	}
	return out, nil
}

// resolveRepo maps a repository UUID to its (owner, name) pair. Hits
// the in-memory cache first and falls back to GET /api/v1/repositories
// on miss. One refresh is attempted per miss.
func (c *SymbolGraphClient) resolveRepo(ctx context.Context, repoID uuid.UUID) (repoCoords, error) {
	c.mu.RLock()
	coords, ok := c.repoByID[repoID]
	c.mu.RUnlock()
	if ok {
		return coords, nil
	}
	if err := c.refreshRepoCache(ctx); err != nil {
		return repoCoords{}, err
	}
	c.mu.RLock()
	coords, ok = c.repoByID[repoID]
	c.mu.RUnlock()
	if !ok {
		return repoCoords{}, fmt.Errorf("repo %s not known to git-service", repoID)
	}
	return coords, nil
}

// refreshRepoCache walks git-service's /repositories listing and
// rebuilds the UUID→(owner,repo) map. Any transient failure is
// reported; the existing cache is preserved.
func (c *SymbolGraphClient) refreshRepoCache(ctx context.Context) error {
	endpoint := c.baseURL + "/api/v1/repositories"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list repos %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Repositories []struct {
			ID         uuid.UUID `json:"ID"`
			Name       string    `json:"Name"`
			OwnerLogin string    `json:"OwnerLogin"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode repos: %w", err)
	}

	next := make(map[uuid.UUID]repoCoords, len(payload.Repositories))
	for _, r := range payload.Repositories {
		if r.ID == uuid.Nil || r.Name == "" || r.OwnerLogin == "" {
			continue
		}
		next[r.ID] = repoCoords{Owner: r.OwnerLogin, Name: r.Name}
	}

	c.mu.Lock()
	c.repoByID = next
	c.mu.Unlock()
	return nil
}

// impactResponse mirrors the subset of GetImpactAnalysisOutput that the
// client needs to consume. Declared locally so the runtime-service
// package does not have to import git-service's DTOs.
type impactResponse struct {
	SourceFile      string `json:"source_file"`
	ExportedSymbols []struct {
		SymbolName string `json:"symbol_name"`
		FilePath   string `json:"file_path"`
	} `json:"exported_symbols"`
	ImpactedFiles []struct {
		FilePath   string `json:"file_path"`
		References []struct {
			SymbolName string `json:"symbol_name"`
			FilePath   string `json:"file_path"`
		} `json:"references"`
	} `json:"impacted_files"`
}

// fetchImpact calls GET /api/v1/repos/{owner}/{repo}/impact?file=<path>.
// Returns (nil, nil) when the repo is not indexed yet or the file has
// no impacted downstreams — both are expected steady states.
func (c *SymbolGraphClient) fetchImpact(ctx context.Context, owner, repo, filePath string) (*impactResponse, error) {
	endpoint := fmt.Sprintf(
		"%s/api/v1/repos/%s/%s/impact?file=%s",
		c.baseURL,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.QueryEscape(filePath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("impact request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("impact %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var decoded impactResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode impact: %w", err)
	}
	return &decoded, nil
}

// looksLikeTestFile matches common test-file naming conventions across
// languages. TODO: replace with a symbol-graph "kind=test" attribute
// once git-service surfaces it.
func looksLikeTestFile(path string) bool {
	lower := strings.ToLower(path)
	base := lower
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		base = lower[idx+1:]
	}
	switch {
	case strings.Contains(base, "_test."):
		return true
	case strings.Contains(base, ".test."):
		return true
	case strings.Contains(base, ".spec."):
		return true
	case strings.HasPrefix(base, "test_"):
		return true
	case strings.Contains(lower, "/tests/"):
		return true
	case strings.Contains(lower, "/test/"):
		return true
	case strings.Contains(lower, "/__tests__/"):
		return true
	}
	return false
}

func appendUnique(slice []string, s string) []string {
	if s == "" {
		return slice
	}
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}
