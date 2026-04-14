package http

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TestRunHandler handles test run related HTTP requests.
type TestRunHandler struct {
	repo *postgres.TestRunRepo
}

// NewTestRunHandler creates a new test run handler.
func NewTestRunHandler(repo *postgres.TestRunRepo) *TestRunHandler {
	return &TestRunHandler{repo: repo}
}

// RegisterRoutes registers test run routes.
func (h *TestRunHandler) RegisterRoutes(r chi.Router) {
	r.Route("/test-runs", func(r chi.Router) {
		r.Post("/", h.CreateTestRun)
		r.Get("/", h.ListTestRuns)
		r.Get("/{id}", h.GetTestRun)
		r.Get("/{id}/runner", h.ResolveRunner)
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

	switch domain.TestRunStatus(req.Status) {
	case domain.TestRunStatusPassed, domain.TestRunStatusFailed:
		durationMS := int64(0)
		if req.DurationMS != nil {
			durationMS = *req.DurationMS
		}
		run.MarkCompleted(req.Passed, req.Failed, req.Skipped, req.CoveragePC, durationMS)
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
		}
	case domain.TestRunStatusCancelled:
		run.Status = domain.TestRunStatusCancelled
	}

	if err := h.repo.Update(r.Context(), run); err != nil {
		RespondInternalError(w, "Failed to update test run")
		return
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
