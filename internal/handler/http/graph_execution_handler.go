package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// GraphExecutionHandler handles graph execution HTTP requests
type GraphExecutionHandler struct {
	engine *usecase.GraphExecutionEngine
}

// NewGraphExecutionHandler creates a new graph execution handler
func NewGraphExecutionHandler(engine *usecase.GraphExecutionEngine) *GraphExecutionHandler {
	return &GraphExecutionHandler{engine: engine}
}

// RegisterRoutes registers graph execution routes on the router
func (h *GraphExecutionHandler) RegisterRoutes(r chi.Router) {
	r.Post("/graphs/{graph_id}/execute", h.ExecuteGraph)
	r.Get("/graphs/{graph_id}/executions", h.ListExecutions)

	r.Route("/graph-executions", func(r chi.Router) {
		r.Get("/{execution_id}", h.GetExecution)
		r.Post("/{execution_id}/cancel", h.CancelExecution)
		r.Get("/{execution_id}/nodes", h.ListNodeExecutions)
		r.Get("/{execution_id}/nodes/{node_id}", h.GetNodeExecution)
		r.Get("/{execution_id}/parallelism-report", h.GetParallelismReport)
	})

	// Mirror the parallelism-report endpoint at the spec-required path
	// "/executions/{id}/parallelism-report" so external callers don't
	// have to know the runtime distinguishes graph-executions from
	// single-shot executions.
	r.Get("/executions/{execution_id}/parallelism-report", h.GetParallelismReport)
}

// ExecuteGraphRequest represents the request body for executing a graph
type ExecuteGraphRequest struct {
	Input     domain.JSONMap `json:"input,omitempty"`
	DebugMode bool           `json:"debug_mode,omitempty"`
}

// ExecuteGraph handles POST /api/v1/graphs/{graph_id}/execute
func (h *GraphExecutionHandler) ExecuteGraph(w http.ResponseWriter, r *http.Request) {
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

	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	var req ExecuteGraphRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	exec, err := h.engine.ExecuteGraph(r.Context(), graphID, orgID, userID, req.Input, req.DebugMode)
	if err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		if errors.Is(err, domain.ErrGraphNotActive) {
			RespondBadRequest(w, "Graph is not active. Deploy it first.", nil)
			return
		}
		RespondInternalError(w, "Failed to execute graph")
		return
	}

	RespondCreated(w, exec)
}

// GetExecution handles GET /api/v1/graph-executions/{execution_id}
func (h *GraphExecutionHandler) GetExecution(w http.ResponseWriter, r *http.Request) {
	execID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	exec, err := h.engine.GetGraphExecution(r.Context(), execID)
	if err != nil {
		if errors.Is(err, domain.ErrGraphExecutionNotFound) {
			RespondNotFound(w, "Graph execution not found")
			return
		}
		RespondInternalError(w, "Failed to get execution")
		return
	}

	RespondSuccess(w, exec)
}

// ListExecutions handles GET /api/v1/graphs/{graph_id}/executions
func (h *GraphExecutionHandler) ListExecutions(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	execs, total, err := h.engine.ListGraphExecutions(r.Context(), graphID, limit, offset)
	if err != nil {
		RespondInternalError(w, "Failed to list executions")
		return
	}

	RespondSuccess(w, map[string]any{
		"items": execs,
		"total": total,
	})
}

// CancelExecution handles POST /api/v1/graph-executions/{execution_id}/cancel
func (h *GraphExecutionHandler) CancelExecution(w http.ResponseWriter, r *http.Request) {
	execID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	if err := h.engine.CancelGraphExecution(r.Context(), execID); err != nil {
		if errors.Is(err, domain.ErrGraphExecutionNotFound) {
			RespondNotFound(w, "Graph execution not found")
			return
		}
		RespondInternalError(w, "Failed to cancel execution")
		return
	}

	RespondNoContent(w)
}

// ListNodeExecutions handles GET /api/v1/graph-executions/{execution_id}/nodes
func (h *GraphExecutionHandler) ListNodeExecutions(w http.ResponseWriter, r *http.Request) {
	execID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	nodes, err := h.engine.ListNodeExecutions(r.Context(), execID)
	if err != nil {
		RespondInternalError(w, "Failed to list node executions")
		return
	}

	RespondSuccess(w, map[string]any{
		"items": nodes,
		"total": len(nodes),
	})
}

// GetParallelismReport handles GET /api/v1/graph-executions/{execution_id}/parallelism-report
// and the alias /api/v1/executions/{execution_id}/parallelism-report.
func (h *GraphExecutionHandler) GetParallelismReport(w http.ResponseWriter, r *http.Request) {
	execID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}
	report := h.engine.GetParallelismReport(execID)
	RespondSuccess(w, report)
}

// GetNodeExecution handles GET /api/v1/graph-executions/{execution_id}/nodes/{node_id}
func (h *GraphExecutionHandler) GetNodeExecution(w http.ResponseWriter, r *http.Request) {
	nodeID, err := GetUUIDParam(r, "node_id")
	if err != nil {
		RespondBadRequest(w, "Invalid node ID", nil)
		return
	}

	nodeExec, err := h.engine.GetNodeExecution(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, domain.ErrNodeExecutionNotFound) {
			RespondNotFound(w, "Node execution not found")
			return
		}
		RespondInternalError(w, "Failed to get node execution")
		return
	}

	RespondSuccess(w, nodeExec)
}
