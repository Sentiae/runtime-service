package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// AffectedTestResolver determines which tests are affected by a code diff by
// walking a symbol graph supplied by git-service and intersecting the touched
// symbols with test->symbol references.
type AffectedTestResolver interface {
	Resolve(ctx context.Context, input AffectedTestsInput) (*AffectedTestsOutput, error)
}

// AffectedTestsInput describes a diff: repository ID plus the set of files
// that changed and, optionally, the function/symbol names that changed within
// each file. When FunctionsByFile is empty the resolver falls back to a
// filename-based intersection.
type AffectedTestsInput struct {
	RepoID          uuid.UUID           `json:"repo_id"`
	DiffFiles       []string            `json:"diff_files"`
	FunctionsByFile map[string][]string `json:"functions_by_file,omitempty"`
}

// AffectedTestsOutput returns the set of test node IDs (or, when no symbol
// graph is wired, test file names) that should be re-run.
type AffectedTestsOutput struct {
	TestNodeIDs []string        `json:"test_node_ids"`
	Matched     []AffectedMatch `json:"matched,omitempty"`
	Notes       string          `json:"notes,omitempty"`
}

// AffectedMatch explains why a given test was included.
type AffectedMatch struct {
	TestID       string   `json:"test_id"`
	TestName     string   `json:"test_name"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Symbols      []string `json:"symbols,omitempty"`
}

// SymbolGraphClient is the minimal surface the resolver needs from
// git-service. Kept here so the resolver can be tested independently of HTTP.
type SymbolGraphClient interface {
	// ListTestSymbolRefs returns, for each test in the repo, the set of
	// referenced production symbols (e.g. "pkg/foo.Func") and the
	// production files the test imports or exercises. The optional path
	// filter restricts results to tests touching one of the given files.
	ListTestSymbolRefs(ctx context.Context, repoID uuid.UUID, pathFilter []string) ([]TestSymbolRefs, error)
}

// TestSymbolRefs describes one test node's upstream references.
type TestSymbolRefs struct {
	TestNodeID  string   `json:"test_node_id"`
	TestName    string   `json:"test_name"`
	FilePath    string   `json:"file_path"`
	SourceFiles []string `json:"source_files"`
	Symbols     []string `json:"symbols"`
}

type affectedTestResolver struct {
	symbols SymbolGraphClient
}

// NewAffectedTestResolver constructs the resolver. A nil SymbolGraphClient is
// permitted; the resolver will then do a filename-only match (no symbol-level
// pruning) and annotate its output.
func NewAffectedTestResolver(symbols SymbolGraphClient) AffectedTestResolver {
	return &affectedTestResolver{symbols: symbols}
}

func (r *affectedTestResolver) Resolve(ctx context.Context, input AffectedTestsInput) (*AffectedTestsOutput, error) {
	if input.RepoID == uuid.Nil {
		return nil, fmt.Errorf("repo_id is required")
	}
	if len(input.DiffFiles) == 0 {
		return &AffectedTestsOutput{TestNodeIDs: nil, Notes: "no changed files"}, nil
	}

	changedFiles := make(map[string]struct{}, len(input.DiffFiles))
	for _, f := range input.DiffFiles {
		changedFiles[normalizePath(f)] = struct{}{}
	}

	changedSymbols := make(map[string]struct{})
	for file, syms := range input.FunctionsByFile {
		norm := normalizePath(file)
		for _, s := range syms {
			changedSymbols[norm+":"+s] = struct{}{}
			changedSymbols[s] = struct{}{} // also support unqualified match
		}
	}

	if r.symbols == nil {
		// Fall back to name-based heuristic: any test whose filename shares a
		// stem with a changed source file is considered affected.
		return r.heuristicResolve(input, changedFiles), nil
	}

	refs, err := r.symbols.ListTestSymbolRefs(ctx, input.RepoID, input.DiffFiles)
	if err != nil {
		return nil, fmt.Errorf("symbol graph lookup: %w", err)
	}

	var (
		ids     []string
		matches []AffectedMatch
		seen    = map[string]struct{}{}
	)
	for _, ref := range refs {
		var (
			hitFiles []string
			hitSyms  []string
		)
		for _, f := range ref.SourceFiles {
			if _, ok := changedFiles[normalizePath(f)]; ok {
				hitFiles = append(hitFiles, f)
			}
		}
		if len(changedSymbols) > 0 {
			for _, sym := range ref.Symbols {
				if _, ok := changedSymbols[sym]; ok {
					hitSyms = append(hitSyms, sym)
				}
			}
		}
		if len(hitFiles) == 0 && len(hitSyms) == 0 {
			continue
		}
		if _, dup := seen[ref.TestNodeID]; dup {
			continue
		}
		seen[ref.TestNodeID] = struct{}{}
		ids = append(ids, ref.TestNodeID)
		matches = append(matches, AffectedMatch{
			TestID:       ref.TestNodeID,
			TestName:     ref.TestName,
			ChangedFiles: hitFiles,
			Symbols:      hitSyms,
		})
	}

	return &AffectedTestsOutput{TestNodeIDs: ids, Matched: matches}, nil
}

// heuristicResolve runs when no symbol graph is available. Any test whose
// file path contains the stem of a changed file (e.g. "user.go" ->
// "user_test.go") is considered affected.
func (r *affectedTestResolver) heuristicResolve(input AffectedTestsInput, changedFiles map[string]struct{}) *AffectedTestsOutput {
	stems := make(map[string]struct{})
	for f := range changedFiles {
		stems[stemFor(f)] = struct{}{}
	}
	var ids []string
	var matches []AffectedMatch
	for f := range changedFiles {
		base := stemFor(f)
		if base == "" {
			continue
		}
		candidate := base + "_test"
		ids = append(ids, candidate)
		matches = append(matches, AffectedMatch{
			TestID:       candidate,
			TestName:     candidate,
			ChangedFiles: []string{f},
		})
	}
	return &AffectedTestsOutput{
		TestNodeIDs: ids,
		Matched:     matches,
		Notes:       "symbol graph not available; used filename-stem heuristic",
	}
}

func normalizePath(p string) string {
	return strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
}

// stemFor returns the filename stem without extension or _test suffix, used
// to pair source and test files heuristically.
func stemFor(path string) string {
	norm := normalizePath(path)
	idx := strings.LastIndex(norm, "/")
	name := norm
	if idx >= 0 {
		name = norm[idx+1:]
	}
	if dot := strings.LastIndex(name, "."); dot > 0 {
		name = name[:dot]
	}
	name = strings.TrimSuffix(name, "_test")
	return name
}
