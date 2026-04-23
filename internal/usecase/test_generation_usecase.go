package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
)

// TestGenerationUseCase produces TestNode definitions from a spec's acceptance
// criteria by delegating to foundry-service for AI-driven code synthesis.
type TestGenerationUseCase interface {
	GenerateTests(ctx context.Context, input TestGenerationInput) (*TestGenerationOutput, error)
}

// TestGenerationInput is the use-case input.
type TestGenerationInput struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	SpecID         string    `json:"spec_id,omitempty"`
	Criteria       []string  `json:"criteria"`
	Framework      string    `json:"framework"`
	Language       string    `json:"language"`
	Context        string    `json:"context,omitempty"`
	// SourceCode is the function body or class under test. When provided
	// the pattern matcher (§8.2) inspects it and upgrades the generator
	// from the keyword-based smoke template to CRUD / auth / validation
	// / boundary when confidence ≥ 0.6. Optional — when empty we fall
	// back to the keyword classifier against Context + Criteria.
	SourceCode string `json:"source_code,omitempty"`
}

// TestGenerationOutput carries the generated tests plus one or more TestNode
// definitions that the canvas/runtime can materialize.
type TestGenerationOutput struct {
	Framework string         `json:"framework"`
	Language  string         `json:"language"`
	Code      string         `json:"code"`
	TestNodes []TestNodeSpec `json:"test_nodes"`
	Notes     string         `json:"notes,omitempty"`
}

// TestNodeSpec is a minimal, canvas-friendly description of a generated test
// suitable for turning into a node on a canvas.
type TestNodeSpec struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Criterion string `json:"criterion"`
	Framework string `json:"framework"`
	Language  string `json:"language"`
	Code      string `json:"code"`
}

type testGenerationService struct {
	foundry foundry.FoundryClient
	// pattern is the §8.2 pattern-based classifier. Holds pre-compiled
	// regexes and is safe for concurrent use.
	pattern *PatternMatcher
}

// NewTestGenerationService constructs the use case. If foundryClient is nil,
// a no-op scaffold generator is used so the endpoint still works.
func NewTestGenerationService(foundryClient foundry.FoundryClient) TestGenerationUseCase {
	if foundryClient == nil {
		foundryClient = foundry.NewNoopClient()
	}
	return &testGenerationService{
		foundry: foundryClient,
		pattern: NewPatternMatcher(),
	}
}

// patternConfidenceThreshold is the min match score before the pattern
// matcher overrides the keyword classifier. Kept at 0.6 to match the
// spec; anything lower falls back to smoke by design.
const patternConfidenceThreshold = 0.6

// GenerateTests validates the input, classifies intent into a test
// category (CRUD | auth | smoke), dispatches to a framework-specific
// foundry call, and returns both the raw generated source and one
// TestNodeSpec per acceptance criterion.
//
// §8.1 gap-closure: the original path treated every request as a generic
// smoke test. Intent-aware routing lets foundry produce entity-level
// CRUD suites and login/permission suites without forcing callers to
// hand-write the prompt template.
func (s *testGenerationService) GenerateTests(ctx context.Context, input TestGenerationInput) (*TestGenerationOutput, error) {
	framework := strings.ToLower(strings.TrimSpace(input.Framework))
	language := strings.ToLower(strings.TrimSpace(input.Language))
	if framework == "" {
		return nil, fmt.Errorf("framework is required (pytest|jest|go)")
	}
	if !isSupportedFramework(framework) {
		return nil, fmt.Errorf("unsupported framework %q (supported: pytest, jest, go)", framework)
	}
	if language == "" {
		language = defaultLanguageFor(framework)
	}
	if len(input.Criteria) == 0 {
		return nil, fmt.Errorf("at least one acceptance criterion is required")
	}

	// §8.2 — classify via pattern matcher first (when source code is
	// supplied); fall back to the keyword classifier when confidence is
	// below the threshold or the caller didn't ship source code at all.
	category := ClassifyWithPattern(s.pattern, input.SourceCode, input.Context, input.Criteria, patternConfidenceThreshold)

	// Route to a framework-specific call shape. The foundry client
	// contract accepts a Category hint via the existing request
	// structure; if the downstream client ignores it we still get a
	// sensible smoke test back.
	resp, err := s.foundry.GenerateTests(ctx, input.OrganizationID.String(), foundry.GenerateTestRequest{
		Criteria:  input.Criteria,
		Framework: framework,
		Language:  language,
		Context:   buildCategoryContext(category, input.Context),
	})
	if err != nil {
		return nil, err
	}

	nodes := make([]TestNodeSpec, 0, len(input.Criteria))
	for i, c := range input.Criteria {
		nodes = append(nodes, TestNodeSpec{
			ID:        uuid.NewString(),
			Name:      fmt.Sprintf("%s_test_%d", category, i+1),
			Criterion: c,
			Framework: framework,
			Language:  language,
			Code:      resp.Code,
		})
	}

	return &TestGenerationOutput{
		Framework: resp.Framework,
		Language:  resp.Language,
		Code:      resp.Code,
		TestNodes: nodes,
		Notes:     resp.Notes,
	}, nil
}

// TestCategory identifies which generation template the spec should
// hit. Adding a new category only needs a keyword set in
// classifyTestCategory and a prompt prefix in buildCategoryContext.
type TestCategory string

const (
	// TestCategoryCRUD produces entity-level create/read/update/delete
	// tests that exercise the repository + handler layers.
	TestCategoryCRUD TestCategory = "crud"
	// TestCategoryAuth produces login / permission / token-scope tests.
	TestCategoryAuth TestCategory = "auth"
	// TestCategorySmoke is the default fallback — a single happy-path
	// invocation per acceptance criterion.
	TestCategorySmoke TestCategory = "smoke"
)

// classifyTestCategory walks the spec Context and Criteria looking for
// keywords that imply a specialised test shape. The check is case
// insensitive and substring-based so callers can use natural phrasing
// ("CRUD endpoints" / "Ensure auth is required").
func classifyTestCategory(specContext string, criteria []string) TestCategory {
	blob := strings.ToLower(specContext)
	for _, c := range criteria {
		blob += "\n" + strings.ToLower(c)
	}
	// Auth-related terms take precedence over CRUD because login flows
	// often involve create/update verbs too.
	authKeywords := []string{"auth", "login", "permission", "rbac", "role", "token", "jwt", "oauth", "session"}
	for _, k := range authKeywords {
		if strings.Contains(blob, k) {
			return TestCategoryAuth
		}
	}
	crudKeywords := []string{"crud", "create", "read", "update", "delete", "entity", "repository"}
	for _, k := range crudKeywords {
		if strings.Contains(blob, k) {
			return TestCategoryCRUD
		}
	}
	return TestCategorySmoke
}

// buildCategoryContext prefixes the operator-supplied context with a
// framework-aware instruction so the downstream generator lines up on
// the right template. The prefix is intentionally short and stable —
// foundry prompts expect deterministic input.
func buildCategoryContext(category TestCategory, raw string) string {
	var prefix string
	switch category {
	case TestCategoryCRUD:
		prefix = "[generator=crud] Produce entity-level create/read/update/delete tests. Exercise the repository + handler layers.\n"
	case TestCategoryAuth:
		prefix = "[generator=auth] Produce login + permission tests covering unauthenticated, authenticated, and over-privileged request paths.\n"
	default:
		prefix = "[generator=smoke] Produce one happy-path invocation per acceptance criterion.\n"
	}
	if raw == "" {
		return prefix
	}
	return prefix + raw
}

func isSupportedFramework(f string) bool {
	switch f {
	case "pytest", "jest", "go":
		return true
	}
	return false
}

func defaultLanguageFor(framework string) string {
	switch framework {
	case "pytest":
		return "python"
	case "jest":
		return "javascript"
	case "go":
		return "go"
	}
	return ""
}
