package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// GraphDebugHandler handles graph debug HTTP requests
type GraphDebugHandler struct {
	debugUC usecase.GraphDebugUseCase
}

// NewGraphDebugHandler creates a new graph debug handler
func NewGraphDebugHandler(debugUC usecase.GraphDebugUseCase) *GraphDebugHandler {
	return &GraphDebugHandler{debugUC: debugUC}
}

// RegisterRoutes registers graph debug routes on the router
func (h *GraphDebugHandler) RegisterRoutes(r chi.Router) {
	r.Route("/graphs/debug/sessions", func(r chi.Router) {
		r.Post("/", h.CreateSession)
		r.Get("/{session_id}", h.GetSession)
		r.Post("/{session_id}/start", h.StartSession)
		r.Post("/{session_id}/step", h.StepOver)
		r.Post("/{session_id}/continue", h.ContinueSession)
		r.Post("/{session_id}/pause", h.PauseSession)
		r.Post("/{session_id}/cancel", h.CancelSession)
		r.Put("/{session_id}/breakpoints", h.SetBreakpoints)
	})
}

// CreateDebugSessionRequest represents the request body for creating a debug session
type CreateDebugSessionRequest struct {
	GraphID     string         `json:"graph_id"`
	Mode        string         `json:"mode"`
	Input       domain.JSONMap `json:"input,omitempty"`
	Breakpoints domain.JSONMap `json:"breakpoints,omitempty"`
}

// CreateSession handles POST /api/v1/graphs/debug/sessions
func (h *GraphDebugHandler) CreateSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := GetUserIDFromContext(r.Context())
	if !ok {
		RespondUnauthorized(w, "User not authenticated")
		return
	}
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondBadRequest(w, "Organization ID required", nil)
		return
	}

	var req CreateDebugSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	graphID, err := parseUUID(req.GraphID)
	if err != nil {
		RespondBadRequest(w, "Invalid graph_id", nil)
		return
	}

	mode := domain.DebugMode(req.Mode)
	if !mode.IsValid() {
		RespondBadRequest(w, "Invalid mode. Use 'step_through' or 'breakpoint'", nil)
		return
	}

	session, err := h.debugUC.CreateSession(r.Context(), usecase.CreateDebugSessionInput{
		GraphID:        graphID,
		OrganizationID: orgID,
		UserID:         userID,
		Mode:           mode,
		Input:          req.Input,
		Breakpoints:    req.Breakpoints,
	})
	if err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		if errors.Is(err, domain.ErrGraphNotActive) {
			RespondBadRequest(w, "Graph is not active. Deploy it first.", nil)
			return
		}
		RespondInternalError(w, "Failed to create debug session")
		return
	}

	RespondCreated(w, session)
}

// GetSession handles GET /api/v1/graphs/debug/sessions/{session_id}
func (h *GraphDebugHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	session, err := h.debugUC.GetSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		RespondInternalError(w, "Failed to get debug session")
		return
	}

	RespondSuccess(w, session)
}

// StartSession handles POST /api/v1/graphs/debug/sessions/{session_id}/start
func (h *GraphDebugHandler) StartSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.debugUC.StartSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		RespondBadRequest(w, err.Error(), nil)
		return
	}

	RespondSuccess(w, map[string]any{"status": "started"})
}

// StepOver handles POST /api/v1/graphs/debug/sessions/{session_id}/step
func (h *GraphDebugHandler) StepOver(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	session, err := h.debugUC.StepOver(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		if errors.Is(err, domain.ErrDebugSessionNotPaused) {
			RespondBadRequest(w, "Session is not paused", nil)
			return
		}
		RespondInternalError(w, "Failed to step")
		return
	}

	RespondSuccess(w, session)
}

// ContinueSession handles POST /api/v1/graphs/debug/sessions/{session_id}/continue
func (h *GraphDebugHandler) ContinueSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.debugUC.Continue(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		if errors.Is(err, domain.ErrDebugSessionNotPaused) {
			RespondBadRequest(w, "Session is not paused", nil)
			return
		}
		RespondInternalError(w, "Failed to continue")
		return
	}

	RespondSuccess(w, map[string]any{"status": "continuing"})
}

// PauseSession handles POST /api/v1/graphs/debug/sessions/{session_id}/pause
func (h *GraphDebugHandler) PauseSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.debugUC.PauseSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		RespondBadRequest(w, err.Error(), nil)
		return
	}

	RespondSuccess(w, map[string]any{"status": "pausing"})
}

// CancelSession handles POST /api/v1/graphs/debug/sessions/{session_id}/cancel
func (h *GraphDebugHandler) CancelSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.debugUC.CancelSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		RespondInternalError(w, "Failed to cancel session")
		return
	}

	RespondNoContent(w)
}

// SetBreakpointsRequest represents the request body for setting breakpoints
type SetBreakpointsRequest struct {
	Breakpoints domain.JSONMap `json:"breakpoints"`
}

// SetBreakpoints handles PUT /api/v1/graphs/debug/sessions/{session_id}/breakpoints
func (h *GraphDebugHandler) SetBreakpoints(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	var req SetBreakpointsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	if err := h.debugUC.SetBreakpoints(r.Context(), sessionID, req.Breakpoints); err != nil {
		if errors.Is(err, domain.ErrDebugSessionNotFound) {
			RespondNotFound(w, "Debug session not found")
			return
		}
		RespondInternalError(w, "Failed to set breakpoints")
		return
	}

	RespondSuccess(w, map[string]any{"status": "updated"})
}
