package http

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TestQuarantineHandler exposes the §8.3 manual-override endpoints on
// top of the QuarantineUseCase. Routes:
//
//	POST /api/v1/tests/{id}/quarantine        # force on
//	DELETE /api/v1/tests/{id}/quarantine      # force off
//
// Responses mirror the persisted TestRun shape so callers see the
// transitioned row (including QuarantinedAt) without a second fetch.
type TestQuarantineHandler struct {
	uc *usecase.QuarantineUseCase
}

// NewTestQuarantineHandler wires the handler.
func NewTestQuarantineHandler(uc *usecase.QuarantineUseCase) *TestQuarantineHandler {
	return &TestQuarantineHandler{uc: uc}
}

// RegisterRoutes registers the quarantine toggles.
func (h *TestQuarantineHandler) RegisterRoutes(r chi.Router) {
	r.Route("/tests/{id}/quarantine", func(r chi.Router) {
		r.Post("/", h.Quarantine)
		r.Delete("/", h.Unquarantine)
	})
}

type quarantineBody struct {
	Reason string `json:"reason,omitempty"`
}

// Quarantine flips Quarantined=true on the identified test.
func (h *TestQuarantineHandler) Quarantine(w http.ResponseWriter, r *http.Request) {
	if h.uc == nil {
		RespondInternalError(w, "Quarantine scheduler not configured")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}
	var body quarantineBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	run, err := h.uc.Quarantine(r.Context(), id, body.Reason)
	if err != nil {
		RespondInternalError(w, err.Error())
		return
	}
	RespondSuccess(w, run)
}

// Unquarantine flips Quarantined=false on the identified test.
func (h *TestQuarantineHandler) Unquarantine(w http.ResponseWriter, r *http.Request) {
	if h.uc == nil {
		RespondInternalError(w, "Quarantine scheduler not configured")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid test run ID", nil)
		return
	}
	var body quarantineBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	run, err := h.uc.Unquarantine(r.Context(), id, body.Reason)
	if err != nil {
		RespondInternalError(w, err.Error())
		return
	}
	RespondSuccess(w, run)
}
