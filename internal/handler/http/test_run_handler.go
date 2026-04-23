package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/canvasservice"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TestRunHandler handles test run related HTTP requests.
type TestRunHandler struct {
	repo         *postgres.TestRunRepo
	publisher    usecase.EventPublisher
	dispatcher   TestRunDispatcher
	canvasClient *canvasservice.Client
}

// NewTestRunHandler creates a new test run handler.
func NewTestRunHandler(repo *postgres.TestRunRepo) *TestRunHandler {
	return &TestRunHandler{repo: repo}
}

// SetCanvasClient wires the canvas-service HTTP client. Called by the
// DI container when CANVAS_SERVICE_URL is configured. Closes §19.1
// flow 1E by adding a direct HTTP push alongside the Kafka dual-write.
func (h *TestRunHandler) SetCanvasClient(c *canvasservice.Client) {
	h.canvasClient = c
}

// SetEventPublisher wires the Kafka publisher used to emit
// `sentiae.runtime.test.completed` CloudEvents when a run transitions
// to a terminal status. Publishing is best-effort — a publisher error
// is logged but does not fail the HTTP response so test results are
// still persisted reliably even when Kafka is degraded.
func (h *TestRunHandler) SetEventPublisher(pub usecase.EventPublisher) {
	h.publisher = pub
}

// publishTestCompleted emits sentiae.runtime.test.completed. The payload
// shape is the contract consumed by foundry-service's spec-driven saga
// and by Pulse / canvas gates. `failed_assertions` is best-effort: most
// runners don't surface individual assertion text to the update
// endpoint, so an empty slice is normal.
func (h *TestRunHandler) publishTestCompleted(ctx context.Context, run *domain.TestRun) {
	if h.publisher == nil || run == nil {
		return
	}
	status := string(run.Status)
	switch run.Status {
	case domain.TestRunStatusPassed:
		status = "passed"
	case domain.TestRunStatusFailed:
		status = "failed"
	case domain.TestRunStatusError:
		status = "error"
	}
	var durationMS int64
	if run.DurationMS != nil {
		durationMS = *run.DurationMS
	}
	var canvasID, codeNodeID string
	if run.CanvasID != nil {
		canvasID = run.CanvasID.String()
	}
	if run.CodeNodeID != nil {
		codeNodeID = run.CodeNodeID.String()
	}
	failedAssertions := []string{}
	if run.ErrorMessage != "" {
		// Runners that surface assertion text pack it in error_message
		// as newline-separated lines. Split defensively so consumers
		// always see a slice.
		for _, line := range strings.Split(run.ErrorMessage, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				failedAssertions = append(failedAssertions, line)
			}
		}
	}
	payload := map[string]any{
		"test_run_id":       run.ID.String(),
		"execution_id":      run.ExecutionID.String(),
		"test_node_id":      run.TestNodeID.String(),
		"code_node_id":      codeNodeID,
		"canvas_id":         canvasID,
		"session_id":        canvasID, // saga coordinator keys off session_id; canvas_id is the closest proxy we have
		"organization_id":   run.OrganizationID.String(),
		"status":            status,
		"passed":            run.Passed,
		"failed":            run.Failed,
		"skipped":           run.Skipped,
		"total":             run.Total,
		"duration_ms":       durationMS,
		"flakiness_score":   run.FlakinessScore,
		"failed_assertions": failedAssertions,
	}
	if err := h.publisher.Publish(ctx, usecase.EventTestCompleted, run.ID.String(), payload); err != nil {
		log.Printf("[TEST-RUN] failed to publish %s for run %s: %v", usecase.EventTestCompleted, run.ID, err)
	}
	// §19.1 1E — POST to canvas-service alongside the Kafka fan-out
	// so the node badge updates at the latency floor, not at the Kafka
	// consumer-lag floor. Best-effort: Kafka is authoritative.
	h.pushTestResultToCanvas(ctx, run)
}

// pushTestResultToCanvas forwards the terminal verdict to canvas-service
// via HTTP. Safe to call with no client wired (noop). Canvas-service
// de-duplicates on run_id so the Kafka path replaying this later is a
// no-op on the node.
func (h *TestRunHandler) pushTestResultToCanvas(ctx context.Context, run *domain.TestRun) {
	if h.canvasClient == nil || run == nil || run.CanvasID == nil || run.TestNodeID == uuid.Nil {
		return
	}
	status := string(run.Status)
	switch run.Status {
	case domain.TestRunStatusPassed:
		status = "passed"
	case domain.TestRunStatusFailed:
		status = "failed"
	case domain.TestRunStatusError:
		status = "error"
	}
	var durationMS int64
	if run.DurationMS != nil {
		durationMS = *run.DurationMS
	}
	payload := canvasservice.TestResultPayload{
		RunID:      run.ID.String(),
		Status:     status,
		Passing:    run.Passed,
		Failed:     run.Failed,
		Skipped:    run.Skipped,
		Total:      run.Total,
		DurationMS: durationMS,
	}
	if run.CoveragePC != nil {
		v := *run.CoveragePC
		payload.CoveragePct = &v
	}
	// Keep the HTTP call bounded so a slow canvas doesn't wedge the
	// update handler.
	pushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := h.canvasClient.ApplyTestResult(pushCtx, *run.CanvasID, run.TestNodeID, payload); err != nil {
		log.Printf("[TEST-RUN] canvas push failed for run %s: %v", run.ID, err)
	}
}

// RegisterRoutes registers test run routes.
func (h *TestRunHandler) RegisterRoutes(r chi.Router) {
	r.Route("/test-runs", func(r chi.Router) {
		r.Post("/", h.CreateTestRun)
		r.Get("/", h.ListTestRuns)
		r.Get("/{id}", h.GetTestRun)
		r.Get("/{id}/runner", h.ResolveRunner)
		r.Post("/{id}/dispatch", h.DispatchTestRun)
		r.Put("/{id}", h.UpdateTestRun)
		r.Get("/node/{node_id}", h.ListByTestNode)
		r.Get("/node/{node_id}/latest", h.GetLatestForNodes)
		r.Get("/canvas/{canvas_id}/summary", h.GetCanvasSummary)
	})
}

type createTestRunRequest struct {
	ExecutionID uuid.UUID  `json:"execution_id"`
	TestNodeID  uuid.UUID  `json:"test_node_id"`
	CodeNodeID  *uuid.UUID `json:"code_node_id,omitempty"`
	CanvasID    *uuid.UUID `json:"canvas_id,omitempty"`
	Language    string     `json:"language"`
	// TestType classifies the run (unit / integration / e2e / perf /
	// security / contract / visual / accessibility). The runtime uses
	// it to pick the right command + VM profile via
	// usecase.ResolveTestRunner. Empty defaults to "unit".
	TestType   string `json:"test_type,omitempty"`
	MaxRetries int    `json:"max_retries,omitempty"`
}

// CreateTestRun handles POST /api/v1/test-runs
func (h *TestRunHandler) CreateTestRun(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}

	var req createTestRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = domain.DefaultMaxTestRetries
	}
	run := &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		ExecutionID:    req.ExecutionID,
		TestNodeID:     req.TestNodeID,
		CodeNodeID:     req.CodeNodeID,
		CanvasID:       req.CanvasID,
		Language:       domain.Language(req.Language),
		TestType:       domain.TestType(req.TestType).Normalize(),
		MaxRetries:     maxRetries,
		Status:         domain.TestRunStatusRunning,
	}

	if err := h.repo.Create(r.Context(), run); err != nil {
		RespondInternalError(w, "Failed to create test run")
		return
	}

	RespondCreated(w, run)
}

// TestRunDispatcher is the optional VM-booting hook. When set, the
// handler asks it to execute the resolved runner profile inside a
// microVM. When nil, the handler returns the queued shape only.
// §8.3 — closes the "dispatch stops at profile resolution" gap by
// offering a one-line DI wire-up to bind a real Firecracker runner.
type TestRunDispatcher interface {
	DispatchInVM(ctx context.Context, run *domain.TestRun, profile usecase.TestRunnerProfile) error
}

// WithDispatcher injects an optional VM dispatcher. Safe to call
// multiple times; the last value wins.
func (h *TestRunHandler) WithDispatcher(d TestRunDispatcher) *TestRunHandler {
	h.dispatcher = d
	return h
}

// DispatchTestRun handles POST /api/v1/test-runs/{id}/dispatch. §8.3.
// When WithDispatcher has been called, the handler calls into it so
// the test boots in a real Firecracker VM. Otherwise the handler
// returns the resolved profile as a queued hand-off for a worker.
func (h *TestRunHandler) DispatchTestRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}
	run, err := h.repo.FindByID(r.Context(), id)
	if err != nil || run == nil {
		RespondNotFound(w, "Test run not found")
		return
	}
	profile, matched := usecase.ResolveTestRunner(run.Language, run.TestType)

	status := "queued"
	if h.dispatcher != nil {
		// Kick off in a goroutine so the caller isn't blocked on VM boot.
		// Errors flow into TestRun.ErrorMessage by the dispatcher impl.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := h.dispatcher.DispatchInVM(ctx, run, profile); err != nil {
				// Best effort — dispatcher is responsible for persisting
				// the failure on the TestRun row.
				_ = err
			}
		}()
		status = "dispatched"
	}

	RespondSuccess(w, map[string]any{
		"test_run_id":    run.ID,
		"execution_id":   run.ExecutionID,
		"status":         status,
		"runner_matched": matched,
		"command":        profile.Command,
		"vm_profile":     profile.VMProfile,
		"network":        profile.Network,
	})
}

// ResolveRunner handles GET /api/v1/test-runs/{id}/runner
// It returns the command template and VM profile the execution layer
// would use for the run's (language, type) pair, plus a "matched" flag
// indicating whether an exact row was found or fallback to unit
// happened. Kept on the run resource rather than a free-form query so
// the resolution is always bound to a real TestRun (no ad-hoc
// discovery that leaks the dispatch table).
func (h *TestRunHandler) ResolveRunner(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}
	run, err := h.repo.FindByID(r.Context(), id)
	if err != nil || run == nil {
		RespondNotFound(w, "Test run not found")
		return
	}
	profile, matched := usecase.ResolveTestRunner(run.Language, run.TestType)
	RespondSuccess(w, map[string]any{
		"command":    profile.Command,
		"vm_profile": profile.VMProfile,
		"network":    profile.Network,
		"matched":    matched,
	})
}

type updateTestRunRequest struct {
	Status       string   `json:"status"`
	Passed       int      `json:"passed"`
	Failed       int      `json:"failed"`
	Skipped      int      `json:"skipped"`
	CoveragePC   *float64 `json:"coverage_pc,omitempty"`
	DurationMS   *int64   `json:"duration_ms,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

// UpdateTestRun handles PUT /api/v1/test-runs/{id}
func (h *TestRunHandler) UpdateTestRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}

	run, err := h.repo.FindByID(r.Context(), id)
	if err != nil || run == nil {
		RespondNotFound(w, "Test run not found")
		return
	}

	var req updateTestRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	// terminalTransition tracks whether this update moved the run to a
	// status that should emit `sentiae.runtime.test.completed`. Retries
	// intentionally do NOT emit: callers see a queued re-run, not a
	// final verdict.
	terminalTransition := false

	switch domain.TestRunStatus(req.Status) {
	case domain.TestRunStatusPassed, domain.TestRunStatusFailed:
		durationMS := int64(0)
		if req.DurationMS != nil {
			durationMS = *req.DurationMS
		}
		run.MarkCompleted(req.Passed, req.Failed, req.Skipped, req.CoveragePC, durationMS)
		terminalTransition = true
	case domain.TestRunStatusError:
		// A runner reporting an error may have hit a transient
		// infrastructure failure (network, VM provisioning, rate limit).
		// If the error is classifiable as transient and we still have
		// retry budget, reset the run to "running" so the next worker
		// pick-up re-executes instead of marking a flaky test as broken.
		if run.TryRetry(req.ErrorMessage) {
			// Intentionally do not MarkError — the run is re-queued.
		} else {
			run.MarkError(req.ErrorMessage)
			terminalTransition = true
		}
	case domain.TestRunStatusCancelled:
		run.Status = domain.TestRunStatusCancelled
	}

	if err := h.repo.Update(r.Context(), run); err != nil {
		RespondInternalError(w, "Failed to update test run")
		return
	}

	if terminalTransition {
		h.publishTestCompleted(r.Context(), run)
	}

	RespondSuccess(w, run)
}

// GetTestRun handles GET /api/v1/test-runs/{id}
func (h *TestRunHandler) GetTestRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}

	run, err := h.repo.FindByID(r.Context(), id)
	if err != nil || run == nil {
		RespondNotFound(w, "Test run not found")
		return
	}

	RespondSuccess(w, run)
}

// ListTestRuns handles GET /api/v1/test-runs
func (h *TestRunHandler) ListTestRuns(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	canvasIDStr := r.URL.Query().Get("canvas_id")
	if canvasIDStr != "" {
		canvasID, err := uuid.Parse(canvasIDStr)
		if err != nil {
			RespondBadRequest(w, "Invalid canvas_id", nil)
			return
		}
		runs, total, err := h.repo.FindByCanvas(r.Context(), canvasID, limit, offset)
		if err != nil {
			RespondInternalError(w, "Failed to list test runs")
			return
		}
		RespondSuccess(w, map[string]any{
			"data": runs, "total": total,
		})
		return
	}

	// Fallback: list all for org (not ideal but workable)
	_ = orgID
	RespondSuccess(w, map[string]any{"data": []any{}, "total": 0})
}

// ListByTestNode handles GET /api/v1/test-runs/node/{node_id}
func (h *TestRunHandler) ListByTestNode(w http.ResponseWriter, r *http.Request) {
	nodeID, err := uuid.Parse(chi.URLParam(r, "node_id"))
	if err != nil {
		RespondBadRequest(w, "Invalid node ID", nil)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	runs, total, err := h.repo.FindByTestNode(r.Context(), nodeID, limit, offset)
	if err != nil {
		RespondInternalError(w, "Failed to list test runs")
		return
	}

	RespondSuccess(w, map[string]any{
		"data": runs, "total": total,
	})
}

// GetLatestForNodes handles GET /api/v1/test-runs/node/{node_id}/latest
func (h *TestRunHandler) GetLatestForNodes(w http.ResponseWriter, r *http.Request) {
	nodeID, err := uuid.Parse(chi.URLParam(r, "node_id"))
	if err != nil {
		RespondBadRequest(w, "Invalid node ID", nil)
		return
	}

	runs, err := h.repo.FindLatestByTestNode(r.Context(), []uuid.UUID{nodeID})
	if err != nil {
		RespondInternalError(w, "Failed to get latest test run")
		return
	}
	if len(runs) == 0 {
		RespondNotFound(w, "No test runs found")
		return
	}

	RespondSuccess(w, runs[0])
}

// GetCanvasSummary handles GET /api/v1/test-runs/canvas/{canvas_id}/summary
func (h *TestRunHandler) GetCanvasSummary(w http.ResponseWriter, r *http.Request) {
	canvasID, err := uuid.Parse(chi.URLParam(r, "canvas_id"))
	if err != nil {
		RespondBadRequest(w, "Invalid canvas ID", nil)
		return
	}

	summary, err := h.repo.SummaryByCanvas(r.Context(), canvasID)
	if err != nil {
		RespondInternalError(w, "Failed to get test summary")
		return
	}

	RespondSuccess(w, summary)
}
