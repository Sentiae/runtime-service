package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TestGenHandler exposes test generation + affected-tests resolution.
type TestGenHandler struct {
	genUC      usecase.TestGenerationUseCase
	affectedUC usecase.AffectedTestResolver
}

// NewTestGenHandler constructs the handler.
func NewTestGenHandler(genUC usecase.TestGenerationUseCase, affectedUC usecase.AffectedTestResolver) *TestGenHandler {
	return &TestGenHandler{genUC: genUC, affectedUC: affectedUC}
}

// RegisterRoutes wires up POST /runtime/test-gen and POST /runtime/affected-tests.
func (h *TestGenHandler) RegisterRoutes(r chi.Router) {
	r.Route("/runtime", func(r chi.Router) {
		r.Post("/test-gen", h.GenerateTests)
		r.Post("/affected-tests", h.AffectedTests)
	})
}

// GenerateTests handles POST /api/v1/runtime/test-gen.
func (h *TestGenHandler) GenerateTests(w http.ResponseWriter, r *http.Request) {
	if h.genUC == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "test generation not wired", nil)
		return
	}
	var input usecase.TestGenerationInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		RespondBadRequest(w, "invalid request body", nil)
		return
	}
	out, err := h.genUC.GenerateTests(r.Context(), input)
	if err != nil {
		RespondBadRequest(w, err.Error(), nil)
		return
	}
	RespondSuccess(w, out)
}

// AffectedTests handles POST /api/v1/runtime/affected-tests.
func (h *TestGenHandler) AffectedTests(w http.ResponseWriter, r *http.Request) {
	if h.affectedUC == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "affected tests resolver not wired", nil)
		return
	}
	var input usecase.AffectedTestsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		RespondBadRequest(w, "invalid request body", nil)
		return
	}
	out, err := h.affectedUC.Resolve(r.Context(), input)
	if err != nil {
		RespondBadRequest(w, err.Error(), nil)
		return
	}
	RespondSuccess(w, out)
}
