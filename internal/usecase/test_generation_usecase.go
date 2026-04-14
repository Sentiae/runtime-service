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
}

// NewTestGenerationService constructs the use case. If foundryClient is nil,
// a no-op scaffold generator is used so the endpoint still works.
func NewTestGenerationService(foundryClient foundry.FoundryClient) TestGenerationUseCase {
	if foundryClient == nil {
		foundryClient = foundry.NewNoopClient()
	}
	return &testGenerationService{foundry: foundryClient}
}

// GenerateTests validates the input, dispatches to foundry, and returns both
// the raw generated source and one TestNodeSpec per acceptance criterion.
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

	resp, err := s.foundry.GenerateTests(ctx, input.OrganizationID.String(), foundry.GenerateTestRequest{
		Criteria:  input.Criteria,
		Framework: framework,
		Language:  language,
		Context:   input.Context,
	})
	if err != nil {
		return nil, err
	}

	nodes := make([]TestNodeSpec, 0, len(input.Criteria))
	for i, c := range input.Criteria {
		nodes = append(nodes, TestNodeSpec{
			ID:        uuid.NewString(),
			Name:      fmt.Sprintf("test_%d", i+1),
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
