package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// RegressionTestHandler exposes the regression-test generator over HTTP.
// It is only registered when the DI container actually wired a
// generator; otherwise the route 503s so callers see the misconfig.
type RegressionTestHandler struct {
	generator *usecase.RegressionTestGenerator
	repo      *postgres.RegressionTestRepo
}

// NewRegressionTestHandler builds the handler.
func NewRegressionTestHandler(g *usecase.RegressionTestGenerator, repo *postgres.RegressionTestRepo) *RegressionTestHandler {
	return &RegressionTestHandler{generator: g, repo: repo}
}

// RegisterRoutes mounts /regression-tests endpoints.
func (h *RegressionTestHandler) RegisterRoutes(r chi.Router) {
	r.Route("/regression-tests", func(r chi.Router) {
		r.Post("/generate-from-trace", h.GenerateFromTrace)
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
	})
}

type generateFromTraceRequest struct {
	TraceID   string `json:"trace_id"`
	ServiceID string `json:"service_id"`
}

// GenerateFromTrace handles POST /api/v1/regression-tests/generate-from-trace.
func (h *RegressionTestHandler) GenerateFromTrace(w http.ResponseWriter, r *http.Request) {
	if h.generator == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "regression test generator not wired", nil)
		return
	}
	var req generateFromTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "invalid request body", nil)
		return
	}
	if req.TraceID == "" {
		RespondBadRequest(w, "trace_id is required", nil)
		return
	}
	out, err := h.generator.GenerateFromTrace(r.Context(), req.TraceID, req.ServiceID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, "GENERATION_FAILED", err.Error(), nil)
		return
	}
	RespondCreated(w, out)
}

// List handles GET /api/v1/regression-tests.
func (h *RegressionTestHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.repo == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "regression test repo not wired", nil)
		return
	}
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "Organization not found")
		return
	}
	rows, total, err := h.repo.ListByOrganization(r.Context(), orgID, 50, 0)
	if err != nil {
		RespondInternalError(w, "failed to list regression tests")
		return
	}
	RespondSuccess(w, map[string]any{"items": rows, "total": total})
}

// Get handles GET /api/v1/regression-tests/{id}.
func (h *RegressionTestHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.repo == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "regression test repo not wired", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "invalid id", nil)
		return
	}
	row, err := h.repo.FindByID(r.Context(), id)
	if err != nil {
		RespondInternalError(w, "lookup failed")
		return
	}
	if row == nil {
		RespondNotFound(w, "regression test not found")
		return
	}
	RespondSuccess(w, row)
}
