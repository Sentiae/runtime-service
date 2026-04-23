// Package usecase — §8.2 pattern-based test generation. The
// PatternMatcher runs a small, pragmatic set of regex + AST-lite heuristics
// against a target symbol's source to decide whether the generator should
// prefer CRUD, auth, validation, or boundary test templates.
//
// The matcher is deliberately lightweight: it doesn't build a full AST
// (tree-sitter or go/ast) because the input is already a parsed + snipped
// symbol body coming from git-service. A sequence of focused regexes over
// the symbol code plus its adjacent receiver types gives us enough signal
// to pick a template with 0.6+ confidence on the common patterns. Anything
// lower falls through to the existing keyword classifier, preserving the
// prior behaviour for corner cases.
package usecase

import (
	"regexp"
	"sort"
	"strings"
)

// PatternKind is the finite set of templates the matcher can route to.
// Each kind maps 1:1 onto a TestCategory so downstream callers don't have
// to carry two enums. The string values are stable — they show up in
// LLM prompts as `[pattern=xyz]` hints.
type PatternKind string

const (
	PatternKindCRUD              PatternKind = "crud"
	PatternKindAuth              PatternKind = "auth"
	PatternKindValidation        PatternKind = "validation"
	PatternKindBoundary          PatternKind = "boundary"
	PatternKindUnknown           PatternKind = "unknown"
)

// AsTestCategory translates a pattern kind into the existing
// TestCategory taxonomy the generator already understands. CRUD/auth map
// directly; validation + boundary collapse into the smoke fallback since
// those templates are a superset of "happy-path" assertions.
func (k PatternKind) AsTestCategory() TestCategory {
	switch k {
	case PatternKindCRUD:
		return TestCategoryCRUD
	case PatternKindAuth:
		return TestCategoryAuth
	}
	return TestCategorySmoke
}

// PatternMatch is the result of a single scan. Confidence is a 0-1
// float; higher means the matcher was more sure of the template. The
// caller decides the threshold (0.6 is the documented default).
type PatternMatch struct {
	Kind       PatternKind
	Confidence float64
	Evidence   []string
}

// PatternMatcher inspects a symbol's source and decides which test
// template the generator should prefer. A single matcher is safe for
// concurrent use — it holds only pre-compiled regex state.
type PatternMatcher struct {
	// precompiled per-pattern regexes, lazily initialised at struct
	// construction so matching stays hot-path-cheap.
	crudRE        []*regexp.Regexp
	authRE        []*regexp.Regexp
	validationRE  []*regexp.Regexp
	boundaryRE    []*regexp.Regexp
	repositoryRE  []*regexp.Regexp
}

// NewPatternMatcher builds the matcher with the default pattern set.
func NewPatternMatcher() *PatternMatcher {
	compile := func(patterns []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(patterns))
		for _, p := range patterns {
			out = append(out, regexp.MustCompile(`(?i)`+p))
		}
		return out
	}
	return &PatternMatcher{
		// CRUD markers: method names that touch a repository or DB layer.
		crudRE: compile([]string{
			`\bfunc\s+\w*\s*create\w*\s*\(`,
			`\bfunc\s+\w*\s*update\w*\s*\(`,
			`\bfunc\s+\w*\s*delete\w*\s*\(`,
			`\bdef\s+create_\w+`,
			`\bdef\s+update_\w+`,
			`\bdef\s+delete_\w+`,
			`\brepo\.Create\s*\(`,
			`\brepo\.Update\s*\(`,
			`\brepo\.Delete\s*\(`,
			`\bdb\.Save\s*\(`,
			`\bdb\.Create\s*\(`,
			`\.FirstOrCreate\s*\(`,
		}),
		// Repository receiver hints — having any of these plus one CRUD
		// verb strongly implies "CRUD on repository".
		repositoryRE: compile([]string{
			`\*?\w+Repository\b`,
			`\brepository\b`,
			`\brepo\s+\*\w+`,
		}),
		// Auth markers: tokens, sessions, permissions, JWT decode.
		authRE: compile([]string{
			`\bjwt\.Parse\b`,
			`\bjwt\.Sign\b`,
			`\bbcrypt\.`,
			`\bargon2\.`,
			`\bRequireAuth\b`,
			`\brequires_auth\b`,
			`\b@require_auth\b`,
			`\bauthorization\b`,
			`\bAuthenticate\b`,
			`\bCheckPermission\b`,
			`\bHasPermission\b`,
			`\brole\s*==`,
			`\bssoprovider\b`,
			`\bscope\s*:=`,
		}),
		// Validation markers: checks that return errors before doing work.
		validationRE: compile([]string{
			`return\s+\w+,?\s*(fmt\.Errorf|errors\.New)`,
			`\bif\s+\w+\s*==\s*""\s*\{`,
			`\bif\s+len\(\w+\)\s*==\s*0\s*\{`,
			`\bif\s+\w+\s*<\s*0\s*\{`,
			`\braise\s+ValueError\b`,
			`\braise\s+ValidationError\b`,
			`\bvalidator\.`,
			`\bgo-playground/validator\b`,
			`\bjsonschema\.`,
		}),
		// Boundary markers: loops over slices, off-by-one edges,
		// pagination limits.
		boundaryRE: compile([]string{
			`\blimit\s*:=`,
			`\boffset\s*:=`,
			`\brange\s+\w+\s*{`,
			`\bfor\s+\w+\s*,\s*\w+\s*:=\s*range\b`,
			`\bif\s+\w+\s*>\s*max\w*\b`,
			`\bif\s+\w+\s*<\s*min\w*\b`,
			`\bmath\.MaxInt\b`,
		}),
	}
}

// Match runs every pattern against src and returns the best match. If no
// pattern clears the minConfidence bar the return is
// (PatternKindUnknown, confidence, evidence).
//
// Scoring rule: each regex hit contributes 0.2 confidence, capped at 1.0.
// CRUD hits get an extra +0.2 bonus when a repository marker is present
// in the same window (strong signal).
func (m *PatternMatcher) Match(src string) PatternMatch {
	if strings.TrimSpace(src) == "" {
		return PatternMatch{Kind: PatternKindUnknown}
	}

	type scoreEntry struct {
		kind     PatternKind
		score    float64
		evidence []string
	}
	scores := []scoreEntry{
		{kind: PatternKindCRUD},
		{kind: PatternKindAuth},
		{kind: PatternKindValidation},
		{kind: PatternKindBoundary},
	}

	hits := func(patterns []*regexp.Regexp) (float64, []string) {
		var ev []string
		var score float64
		for _, re := range patterns {
			if loc := re.FindString(src); loc != "" {
				score += 0.2
				ev = append(ev, loc)
			}
		}
		if score > 1 {
			score = 1
		}
		return score, ev
	}

	crudScore, crudEv := hits(m.crudRE)
	authScore, authEv := hits(m.authRE)
	valScore, valEv := hits(m.validationRE)
	bndScore, bndEv := hits(m.boundaryRE)
	repoScore, repoEv := hits(m.repositoryRE)

	// CRUD + repository presence is a strong combined signal.
	if crudScore > 0 && repoScore > 0 {
		crudScore += 0.2
		if crudScore > 1 {
			crudScore = 1
		}
		crudEv = append(crudEv, repoEv...)
	}

	scores[0].score = crudScore
	scores[0].evidence = crudEv
	scores[1].score = authScore
	scores[1].evidence = authEv
	scores[2].score = valScore
	scores[2].evidence = valEv
	scores[3].score = bndScore
	scores[3].evidence = bndEv

	// Stable order (CRUD first) so equal-confidence ties are
	// deterministic — callers rely on this for snapshot tests.
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].kind < scores[j].kind
		}
		return scores[i].score > scores[j].score
	})

	best := scores[0]
	if best.score == 0 {
		return PatternMatch{Kind: PatternKindUnknown}
	}
	return PatternMatch{
		Kind:       best.kind,
		Confidence: best.score,
		Evidence:   best.evidence,
	}
}

// ClassifyWithPattern is the entry point the test-generation usecase
// calls in place of the old keyword-only classifier. It applies the
// matcher first and, if confidence >= threshold, returns its TestCategory;
// otherwise it falls back to classifyTestCategory for the legacy keyword
// heuristic.
func ClassifyWithPattern(m *PatternMatcher, src, specContext string, criteria []string, threshold float64) TestCategory {
	if m != nil && strings.TrimSpace(src) != "" {
		match := m.Match(src)
		if match.Confidence >= threshold && match.Kind != PatternKindUnknown {
			return match.Kind.AsTestCategory()
		}
	}
	return classifyTestCategory(specContext, criteria)
}
