// Package usecase — RegressionTestGenerator turns production traces
// captured by ops-service into automated reproducer tests. It is the
// runtime-side half of the "ops.trace.captured_for_regression" event
// flow:
//
//	ops-service ──event──▶ runtime.RegressionTestGenerator
//	                           │
//	                           ▼
//	                    TraceFetcher (HTTP)
//	                           │
//	                           ▼
//	                    foundry.GenerateTests
//	                           │
//	                           ▼
//	                    RegressionTestRepo.Create
//	                           │
//	                           ▼
//	                    runtime.regression_test.created event
//
// The generator is also exposed via a synchronous HTTP endpoint so
// engineers can manually request a regression test for any trace.
package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// ProductionTrace is the runtime-internal projection of an ops trace.
// Only the fields used by the generator are modelled; ops-service may
// carry additional metadata that we ignore.
type ProductionTrace struct {
	TraceID        string            `json:"trace_id"`
	ServiceID      string            `json:"service_id"`
	OrganizationID uuid.UUID         `json:"organization_id"`
	HTTPMethod     string            `json:"http_method,omitempty"`
	HTTPPath       string            `json:"http_path,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`
	ResponseStatus int               `json:"response_status,omitempty"`
	ResponseBody   string            `json:"response_body,omitempty"`
	DBQueries      []string          `json:"db_queries,omitempty"`
	SideEffects    []string          `json:"side_effects,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Language       string            `json:"language,omitempty"`
	Framework      string            `json:"framework,omitempty"`
}

// TraceFetcher loads a trace by ID. The default implementation talks
// HTTP to ops-service; tests inject a stub.
type TraceFetcher interface {
	FetchTrace(ctx context.Context, traceID, serviceID string) (*ProductionTrace, error)
}

// RegressionTestGenerator orchestrates fetch → generate → persist.
type RegressionTestGenerator struct {
	traceFetcher    TraceFetcher
	foundryClient   foundry.FoundryClient
	regressionRepo  *postgres.RegressionTestRepo
	eventPublisher  EventPublisher
	defaultLanguage string
	defaultFramwork string
}

// NewRegressionTestGenerator wires the dependencies. fetcher may be
// nil — in that case generation requests will fail fast with a
// descriptive error so the misconfiguration is obvious. foundry must
// not be nil; callers pass foundry.NewNoopClient() to get a scaffold
// when no LLM is configured.
func NewRegressionTestGenerator(
	fetcher TraceFetcher,
	foundryClient foundry.FoundryClient,
	regressionRepo *postgres.RegressionTestRepo,
	eventPublisher EventPublisher,
) *RegressionTestGenerator {
	if foundryClient == nil {
		foundryClient = foundry.NewNoopClient()
	}
	if eventPublisher == nil {
		eventPublisher = noopEventPublisher{}
	}
	return &RegressionTestGenerator{
		traceFetcher:    fetcher,
		foundryClient:   foundryClient,
		regressionRepo:  regressionRepo,
		eventPublisher:  eventPublisher,
		defaultLanguage: "python",
		defaultFramwork: "pytest",
	}
}

// GenerateFromTrace is the public entry point. It pulls the trace from
// ops, asks foundry to synthesise a test, persists the template, and
// emits runtime.regression_test.created.
func (g *RegressionTestGenerator) GenerateFromTrace(ctx context.Context, traceID, serviceID string) (*domain.RegressionTestTemplate, error) {
	if g.regressionRepo == nil {
		return nil, fmt.Errorf("regression repo not wired")
	}
	if g.traceFetcher == nil {
		return nil, fmt.Errorf("trace fetcher not configured")
	}
	if traceID == "" {
		return nil, fmt.Errorf("trace_id is required")
	}

	// Idempotency: if we already generated for this trace, return it.
	if existing, _ := g.regressionRepo.FindByTrace(ctx, traceID); existing != nil {
		return existing, nil
	}

	trace, err := g.traceFetcher.FetchTrace(ctx, traceID, serviceID)
	if err != nil {
		return nil, fmt.Errorf("fetch trace %s: %w", traceID, err)
	}
	if trace == nil {
		return nil, fmt.Errorf("trace %s not found", traceID)
	}

	language := strings.ToLower(strings.TrimSpace(trace.Language))
	if language == "" {
		language = g.defaultLanguage
	}
	framework := strings.ToLower(strings.TrimSpace(trace.Framework))
	if framework == "" {
		framework = defaultFrameworkFor(language, g.defaultFramwork)
	}

	criteria := buildCriteria(trace)
	contextBlob := buildContext(trace)

	resp, err := g.foundryClient.GenerateTests(ctx, trace.OrganizationID.String(), foundry.GenerateTestRequest{
		Criteria:  criteria,
		Framework: framework,
		Language:  language,
		Context:   contextBlob,
	})
	if err != nil {
		return nil, fmt.Errorf("foundry: %w", err)
	}

	now := time.Now().UTC()
	row := &domain.RegressionTestTemplate{
		ID:             uuid.New(),
		OrganizationID: trace.OrganizationID,
		TraceID:        traceID,
		ServiceID:      serviceID,
		Language:       domain.Language(language),
		Framework:      framework,
		GeneratedCode:  resp.Code,
		TraceSummary:   summariseTrace(trace),
		Notes:          resp.Notes,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := g.regressionRepo.Create(ctx, row); err != nil {
		return nil, fmt.Errorf("persist regression test: %w", err)
	}

	_ = g.eventPublisher.Publish(ctx, "runtime.regression_test.created", row.ID.String(), map[string]any{
		"trace_id":    traceID,
		"service_id":  serviceID,
		"test_run_id": row.ID.String(),
		"language":    language,
		"framework":   framework,
	})
	log.Printf("[REGRESSION] Generated test %s from trace %s (lang=%s, framework=%s)", row.ID, traceID, language, framework)
	return row, nil
}

// HandleTraceCapturedEvent is the kafka consumer adapter. Registers
// against the ops.trace.captured_for_regression topic.
func (g *RegressionTestGenerator) HandleTraceCapturedEvent(ctx context.Context, event kafka.CloudEvent) error {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	var payload struct {
		TraceID        string `json:"trace_id"`
		ServiceID      string `json:"service_id"`
		OrganizationID string `json:"organization_id"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return fmt.Errorf("unmarshal metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("unmarshal payload: %w", err)
		}
	}
	if payload.TraceID == "" {
		return fmt.Errorf("event missing trace_id")
	}
	_, err = g.GenerateFromTrace(ctx, payload.TraceID, payload.ServiceID)
	return err
}

// buildCriteria converts a production trace into a list of acceptance
// criteria suitable for foundry's prompt template. Each criterion is a
// short, declarative sentence so the LLM can hang assertions off it.
func buildCriteria(t *ProductionTrace) []string {
	var out []string
	if t.HTTPMethod != "" || t.HTTPPath != "" {
		out = append(out, fmt.Sprintf(
			"Reproduce a %s request to %s and assert the response status is %d",
			defaultIfEmpty(t.HTTPMethod, "GET"),
			defaultIfEmpty(t.HTTPPath, "/"),
			defaultIntIfZero(t.ResponseStatus, 200),
		))
	}
	for _, q := range t.DBQueries {
		out = append(out, fmt.Sprintf("After execution, the following SQL was run: %s", truncate(q, 240)))
	}
	for _, e := range t.SideEffects {
		out = append(out, fmt.Sprintf("Side effect observed: %s", truncate(e, 240)))
	}
	if len(out) == 0 {
		out = append(out, "Reproduce the captured production behaviour and assert it matches the recorded response")
	}
	return out
}

// buildContext renders the request/response payload as a single string
// so foundry can include it verbatim in the generated code.
func buildContext(t *ProductionTrace) string {
	var sb strings.Builder
	sb.WriteString("Captured production trace ")
	sb.WriteString(t.TraceID)
	sb.WriteString(" for service ")
	sb.WriteString(t.ServiceID)
	sb.WriteString("\n\n")
	if t.HTTPMethod != "" {
		fmt.Fprintf(&sb, "Request: %s %s\n", t.HTTPMethod, t.HTTPPath)
	}
	if t.RequestBody != "" {
		fmt.Fprintf(&sb, "Request body:\n%s\n\n", truncate(t.RequestBody, 4096))
	}
	if t.ResponseStatus != 0 {
		fmt.Fprintf(&sb, "Response status: %d\n", t.ResponseStatus)
	}
	if t.ResponseBody != "" {
		fmt.Fprintf(&sb, "Response body:\n%s\n\n", truncate(t.ResponseBody, 4096))
	}
	if len(t.DBQueries) > 0 {
		sb.WriteString("Database queries:\n")
		for _, q := range t.DBQueries {
			fmt.Fprintf(&sb, " - %s\n", truncate(q, 240))
		}
	}
	return sb.String()
}

func summariseTrace(t *ProductionTrace) string {
	if t.HTTPMethod != "" || t.HTTPPath != "" {
		return fmt.Sprintf("%s %s -> %d", t.HTTPMethod, t.HTTPPath, t.ResponseStatus)
	}
	return fmt.Sprintf("trace %s on %s", t.TraceID, t.ServiceID)
}

func defaultFrameworkFor(language, fallback string) string {
	switch language {
	case "python":
		return "pytest"
	case "javascript", "typescript":
		return "jest"
	case "go":
		return "go"
	}
	return fallback
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func defaultIntIfZero(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}
