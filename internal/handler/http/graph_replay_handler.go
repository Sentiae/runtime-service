package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// GraphReplayHandler handles graph replay and trace HTTP requests
type GraphReplayHandler struct {
	replayUC usecase.GraphReplayUseCase
}

// NewGraphReplayHandler creates a new graph replay handler
func NewGraphReplayHandler(replayUC usecase.GraphReplayUseCase) *GraphReplayHandler {
	return &GraphReplayHandler{replayUC: replayUC}
}

// RegisterRoutes registers graph replay routes on the router
func (h *GraphReplayHandler) RegisterRoutes(r chi.Router) {
	r.Route("/graphs/traces", func(r chi.Router) {
		r.Get("/", h.ListTraces)
		r.Get("/{trace_id}", h.GetTrace)
		r.Get("/{trace_id}/snapshots", h.GetSnapshots)
		r.Delete("/{trace_id}", h.DeleteTrace)
		r.Get("/{trace_id}/step/{index}", h.ReplayAtStep)
		r.Post("/{trace_id}/sessions", h.StartReplay)
	})

	r.Route("/graphs/replay", func(r chi.Router) {
		r.Get("/sessions/{session_id}", h.GetReplayState)
		r.Post("/sessions/{session_id}/forward", h.Forward)
		r.Post("/sessions/{session_id}/backward", h.Backward)
		r.Post("/sessions/{session_id}/jump", h.JumpTo)
		r.Delete("/sessions/{session_id}", h.StopReplay)
		r.Get("/compare", h.CompareTraces)
	})
}

// ListTraces handles GET /api/v1/graphs/traces?graph_id=X
func (h *GraphReplayHandler) ListTraces(w http.ResponseWriter, r *http.Request) {
	graphIDStr := r.URL.Query().Get("graph_id")
	if graphIDStr == "" {
		RespondBadRequest(w, "graph_id query parameter required", nil)
		return
	}

	graphID, err := parseUUID(graphIDStr)
	if err != nil {
		RespondBadRequest(w, "Invalid graph_id", nil)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	traces, total, err := h.replayUC.ListTraces(r.Context(), graphID, limit, offset)
	if err != nil {
		RespondInternalError(w, "Failed to list traces")
		return
	}

	RespondSuccess(w, map[string]any{
		"items": traces,
		"total": total,
	})
}

// GetTrace handles GET /api/v1/graphs/traces/{trace_id}
func (h *GraphReplayHandler) GetTrace(w http.ResponseWriter, r *http.Request) {
	traceID, err := GetUUIDParam(r, "trace_id")
	if err != nil {
		RespondBadRequest(w, "Invalid trace ID", nil)
		return
	}

	trace, err := h.replayUC.GetTrace(r.Context(), traceID)
	if err != nil {
		if errors.Is(err, domain.ErrTraceNotFound) {
			RespondNotFound(w, "Trace not found")
			return
		}
		RespondInternalError(w, "Failed to get trace")
		return
	}

	RespondSuccess(w, trace)
}

// GetSnapshots handles GET /api/v1/graphs/traces/{trace_id}/snapshots
func (h *GraphReplayHandler) GetSnapshots(w http.ResponseWriter, r *http.Request) {
	traceID, err := GetUUIDParam(r, "trace_id")
	if err != nil {
		RespondBadRequest(w, "Invalid trace ID", nil)
		return
	}

	snapshots, err := h.replayUC.GetTraceSnapshots(r.Context(), traceID)
	if err != nil {
		RespondInternalError(w, "Failed to get trace snapshots")
		return
	}

	RespondSuccess(w, map[string]any{
		"items": snapshots,
		"total": len(snapshots),
	})
}

// DeleteTrace handles DELETE /api/v1/graphs/traces/{trace_id}
func (h *GraphReplayHandler) DeleteTrace(w http.ResponseWriter, r *http.Request) {
	traceID, err := GetUUIDParam(r, "trace_id")
	if err != nil {
		RespondBadRequest(w, "Invalid trace ID", nil)
		return
	}

	if err := h.replayUC.DeleteTrace(r.Context(), traceID); err != nil {
		if errors.Is(err, domain.ErrTraceNotFound) {
			RespondNotFound(w, "Trace not found")
			return
		}
		RespondInternalError(w, "Failed to delete trace")
		return
	}

	RespondNoContent(w)
}

// ReplayAtStep handles GET /api/v1/graphs/traces/{trace_id}/step/{index}
func (h *GraphReplayHandler) ReplayAtStep(w http.ResponseWriter, r *http.Request) {
	traceID, err := GetUUIDParam(r, "trace_id")
	if err != nil {
		RespondBadRequest(w, "Invalid trace ID", nil)
		return
	}

	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil {
		RespondBadRequest(w, "Invalid index", nil)
		return
	}

	// Create a temporary session, jump to step, return state, stop session
	state, err := h.replayUC.StartReplay(r.Context(), traceID)
	if err != nil {
		if errors.Is(err, domain.ErrTraceNotFound) {
			RespondNotFound(w, "Trace not found")
			return
		}
		RespondInternalError(w, "Failed to start replay")
		return
	}

	if index > 0 {
		state, err = h.replayUC.JumpTo(r.Context(), state.SessionID, index)
		if err != nil {
			_ = h.replayUC.StopReplay(r.Context(), state.SessionID)
			if errors.Is(err, domain.ErrReplayIndexOutOfBounds) {
				RespondBadRequest(w, "Index out of bounds", nil)
				return
			}
			RespondInternalError(w, "Failed to jump to step")
			return
		}
	}

	// Clean up session
	_ = h.replayUC.StopReplay(r.Context(), state.SessionID)

	RespondSuccess(w, state)
}

// StartReplay handles POST /api/v1/graphs/traces/{trace_id}/sessions
func (h *GraphReplayHandler) StartReplay(w http.ResponseWriter, r *http.Request) {
	traceID, err := GetUUIDParam(r, "trace_id")
	if err != nil {
		RespondBadRequest(w, "Invalid trace ID", nil)
		return
	}

	state, err := h.replayUC.StartReplay(r.Context(), traceID)
	if err != nil {
		if errors.Is(err, domain.ErrTraceNotFound) {
			RespondNotFound(w, "Trace not found")
			return
		}
		RespondInternalError(w, "Failed to start replay")
		return
	}

	RespondCreated(w, state)
}

// GetReplayState handles GET /api/v1/graphs/replay/sessions/{session_id}
func (h *GraphReplayHandler) GetReplayState(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	state, err := h.replayUC.GetReplayState(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrReplaySessionNotFound) {
			RespondNotFound(w, "Replay session not found")
			return
		}
		RespondInternalError(w, "Failed to get replay state")
		return
	}

	RespondSuccess(w, state)
}

// Forward handles POST /api/v1/graphs/replay/sessions/{session_id}/forward
func (h *GraphReplayHandler) Forward(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	state, err := h.replayUC.Forward(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrReplaySessionNotFound) {
			RespondNotFound(w, "Replay session not found")
			return
		}
		if errors.Is(err, domain.ErrReplayIndexOutOfBounds) {
			RespondBadRequest(w, "Already at the last step", nil)
			return
		}
		RespondInternalError(w, "Failed to advance replay")
		return
	}

	RespondSuccess(w, state)
}

// Backward handles POST /api/v1/graphs/replay/sessions/{session_id}/backward
func (h *GraphReplayHandler) Backward(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	state, err := h.replayUC.Backward(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrReplaySessionNotFound) {
			RespondNotFound(w, "Replay session not found")
			return
		}
		if errors.Is(err, domain.ErrReplayIndexOutOfBounds) {
			RespondBadRequest(w, "Already at the first step", nil)
			return
		}
		RespondInternalError(w, "Failed to go backward")
		return
	}

	RespondSuccess(w, state)
}

// JumpToRequest represents the request body for jumping to a specific step
type JumpToRequest struct {
	Index int `json:"index"`
}

// JumpTo handles POST /api/v1/graphs/replay/sessions/{session_id}/jump
func (h *GraphReplayHandler) JumpTo(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	indexStr := r.URL.Query().Get("index")
	if indexStr == "" {
		RespondBadRequest(w, "index query parameter required", nil)
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		RespondBadRequest(w, "Invalid index", nil)
		return
	}

	state, err := h.replayUC.JumpTo(r.Context(), sessionID, index)
	if err != nil {
		if errors.Is(err, domain.ErrReplaySessionNotFound) {
			RespondNotFound(w, "Replay session not found")
			return
		}
		if errors.Is(err, domain.ErrReplayIndexOutOfBounds) {
			RespondBadRequest(w, "Index out of bounds", nil)
			return
		}
		RespondInternalError(w, "Failed to jump to step")
		return
	}

	RespondSuccess(w, state)
}

// StopReplay handles DELETE /api/v1/graphs/replay/sessions/{session_id}
func (h *GraphReplayHandler) StopReplay(w http.ResponseWriter, r *http.Request) {
	sessionID, err := GetUUIDParam(r, "session_id")
	if err != nil {
		RespondBadRequest(w, "Invalid session ID", nil)
		return
	}

	if err := h.replayUC.StopReplay(r.Context(), sessionID); err != nil {
		if errors.Is(err, domain.ErrReplaySessionNotFound) {
			RespondNotFound(w, "Replay session not found")
			return
		}
		RespondInternalError(w, "Failed to stop replay")
		return
	}

	RespondNoContent(w)
}

// CompareTraces handles GET /api/v1/graphs/replay/compare?trace1=X&trace2=Y
func (h *GraphReplayHandler) CompareTraces(w http.ResponseWriter, r *http.Request) {
	trace1Str := r.URL.Query().Get("trace1")
	trace2Str := r.URL.Query().Get("trace2")

	if trace1Str == "" || trace2Str == "" {
		RespondBadRequest(w, "trace1 and trace2 query parameters required", nil)
		return
	}

	trace1ID, err := parseUUID(trace1Str)
	if err != nil {
		RespondBadRequest(w, "Invalid trace1 ID", nil)
		return
	}
	trace2ID, err := parseUUID(trace2Str)
	if err != nil {
		RespondBadRequest(w, "Invalid trace2 ID", nil)
		return
	}

	comparison, err := h.replayUC.CompareTraces(r.Context(), trace1ID, trace2ID)
	if err != nil {
		if errors.Is(err, domain.ErrTraceNotFound) {
			RespondNotFound(w, "Trace not found")
			return
		}
		if errors.Is(err, domain.ErrTracesNotComparable) {
			RespondBadRequest(w, "Traces must be from the same graph", nil)
			return
		}
		RespondInternalError(w, "Failed to compare traces")
		return
	}

	RespondSuccess(w, map[string]any{
		"steps": comparison,
		"total": len(comparison),
	})
}
