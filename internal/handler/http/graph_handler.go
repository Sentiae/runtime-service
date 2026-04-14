package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// GraphHandler handles graph definition HTTP requests
type GraphHandler struct {
	graphUC usecase.GraphUseCase
}

// NewGraphHandler creates a new graph handler
func NewGraphHandler(graphUC usecase.GraphUseCase) *GraphHandler {
	return &GraphHandler{graphUC: graphUC}
}

// RegisterRoutes registers graph routes on the router
func (h *GraphHandler) RegisterRoutes(r chi.Router) {
	r.Route("/graphs", func(r chi.Router) {
		r.Post("/", h.CreateGraph)
		r.Get("/", h.ListGraphs)
		r.Get("/{graph_id}", h.GetGraph)
		r.Put("/{graph_id}", h.UpdateGraph)
		r.Delete("/{graph_id}", h.DeleteGraph)
		r.Post("/{graph_id}/deploy", h.DeployGraph)
		r.Post("/{graph_id}/validate", h.ValidateGraph)
	})
}

// CreateGraphRequest represents the request body for creating a graph
type CreateGraphRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	CanvasID    *string             `json:"canvas_id,omitempty"`
	WorkflowID  *string             `json:"workflow_id,omitempty"`
	Nodes       []CreateNodeRequest `json:"nodes"`
	Edges       []CreateEdgeRequest `json:"edges"`
}

// CreateNodeRequest represents a node in the create graph request
type CreateNodeRequest struct {
	NodeType  string           `json:"node_type"`
	Name      string           `json:"name"`
	Config    domain.JSONMap   `json:"config,omitempty"`
	Language  string           `json:"language,omitempty"`
	Code      string           `json:"code,omitempty"`
	Resources *ResourceRequest `json:"resources,omitempty"`
	Position  domain.JSONMap   `json:"position,omitempty"`
	SortOrder int              `json:"sort_order"`
}

// CreateEdgeRequest represents an edge in the create graph request
type CreateEdgeRequest struct {
	SourceNodeIndex int    `json:"source_node_index"`
	TargetNodeIndex int    `json:"target_node_index"`
	SourcePort      string `json:"source_port,omitempty"`
	TargetPort      string `json:"target_port,omitempty"`
	ConditionExpr   string `json:"condition_expr,omitempty"`
}

// UpdateGraphRequest represents the request body for updating a graph
type UpdateGraphRequest struct {
	Name        *string             `json:"name,omitempty"`
	Description *string             `json:"description,omitempty"`
	Nodes       []CreateNodeRequest `json:"nodes,omitempty"`
	Edges       []CreateEdgeRequest `json:"edges,omitempty"`
}

// CreateGraph handles POST /api/v1/graphs
func (h *GraphHandler) CreateGraph(w http.ResponseWriter, r *http.Request) {
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

	var req CreateGraphRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}
	if req.Name == "" {
		RespondBadRequest(w, "Name is required", nil)
		return
	}

	input := usecase.CreateGraphInput{
		OrganizationID: orgID,
		Name:           req.Name,
		Description:    req.Description,
		CreatedBy:      userID,
	}

	if req.CanvasID != nil {
		cid, err := uuid.Parse(*req.CanvasID)
		if err != nil {
			RespondBadRequest(w, "Invalid canvas_id", nil)
			return
		}
		input.CanvasID = &cid
	}
	if req.WorkflowID != nil {
		wid, err := uuid.Parse(*req.WorkflowID)
		if err != nil {
			RespondBadRequest(w, "Invalid workflow_id", nil)
			return
		}
		input.WorkflowID = &wid
	}

	for _, n := range req.Nodes {
		nodeInput := usecase.CreateGraphNodeInput{
			NodeType:  domain.GraphNodeType(n.NodeType),
			Name:      n.Name,
			Config:    n.Config,
			Code:      n.Code,
			Position:  n.Position,
			SortOrder: n.SortOrder,
		}
		if n.Language != "" {
			lang := domain.Language(n.Language)
			nodeInput.Language = &lang
		}
		if n.Resources != nil {
			nodeInput.Resources = &domain.ResourceLimit{
				VCPU:       n.Resources.VCPU,
				MemoryMB:   n.Resources.MemoryMB,
				TimeoutSec: n.Resources.TimeoutSec,
				Network:    domain.NetworkMode(n.Resources.Network),
				DiskMB:     n.Resources.DiskMB,
			}
		}
		input.Nodes = append(input.Nodes, nodeInput)
	}

	for _, e := range req.Edges {
		input.Edges = append(input.Edges, usecase.CreateGraphEdgeInput{
			SourceNodeIndex: e.SourceNodeIndex,
			TargetNodeIndex: e.TargetNodeIndex,
			SourcePort:      e.SourcePort,
			TargetPort:      e.TargetPort,
			ConditionExpr:   e.ConditionExpr,
		})
	}

	graph, err := h.graphUC.CreateGraph(r.Context(), input)
	if err != nil {
		RespondInternalError(w, "Failed to create graph")
		return
	}

	RespondCreated(w, graph)
}

// GetGraph handles GET /api/v1/graphs/{graph_id}
func (h *GraphHandler) GetGraph(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	graph, nodes, edges, err := h.graphUC.GetGraph(r.Context(), graphID)
	if err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		RespondInternalError(w, "Failed to get graph")
		return
	}

	RespondSuccess(w, map[string]any{
		"graph": graph,
		"nodes": nodes,
		"edges": edges,
	})
}

// ListGraphs handles GET /api/v1/graphs
func (h *GraphHandler) ListGraphs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondBadRequest(w, "Organization ID required", nil)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	graphs, total, err := h.graphUC.ListGraphs(r.Context(), orgID, limit, offset)
	if err != nil {
		RespondInternalError(w, "Failed to list graphs")
		return
	}

	RespondSuccess(w, map[string]any{
		"items": graphs,
		"total": total,
	})
}

// UpdateGraph handles PUT /api/v1/graphs/{graph_id}
func (h *GraphHandler) UpdateGraph(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	var req UpdateGraphRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	input := usecase.UpdateGraphInput{
		Name:        req.Name,
		Description: req.Description,
	}

	if req.Nodes != nil {
		var nodes []usecase.CreateGraphNodeInput
		for _, n := range req.Nodes {
			nodeInput := usecase.CreateGraphNodeInput{
				NodeType:  domain.GraphNodeType(n.NodeType),
				Name:      n.Name,
				Config:    n.Config,
				Code:      n.Code,
				Position:  n.Position,
				SortOrder: n.SortOrder,
			}
			if n.Language != "" {
				lang := domain.Language(n.Language)
				nodeInput.Language = &lang
			}
			if n.Resources != nil {
				nodeInput.Resources = &domain.ResourceLimit{
					VCPU:       n.Resources.VCPU,
					MemoryMB:   n.Resources.MemoryMB,
					TimeoutSec: n.Resources.TimeoutSec,
					Network:    domain.NetworkMode(n.Resources.Network),
					DiskMB:     n.Resources.DiskMB,
				}
			}
			nodes = append(nodes, nodeInput)
		}
		input.Nodes = nodes

		if req.Edges != nil {
			var edges []usecase.CreateGraphEdgeInput
			for _, e := range req.Edges {
				edges = append(edges, usecase.CreateGraphEdgeInput{
					SourceNodeIndex: e.SourceNodeIndex,
					TargetNodeIndex: e.TargetNodeIndex,
					SourcePort:      e.SourcePort,
					TargetPort:      e.TargetPort,
					ConditionExpr:   e.ConditionExpr,
				})
			}
			input.Edges = edges
		}
	}

	graph, err := h.graphUC.UpdateGraph(r.Context(), graphID, input)
	if err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		RespondInternalError(w, "Failed to update graph")
		return
	}

	RespondSuccess(w, graph)
}

// DeleteGraph handles DELETE /api/v1/graphs/{graph_id}
func (h *GraphHandler) DeleteGraph(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	if err := h.graphUC.DeleteGraph(r.Context(), graphID); err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		RespondInternalError(w, "Failed to delete graph")
		return
	}

	RespondNoContent(w)
}

// DeployGraph handles POST /api/v1/graphs/{graph_id}/deploy
func (h *GraphHandler) DeployGraph(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	graph, err := h.graphUC.DeployGraph(r.Context(), graphID)
	if err != nil {
		if errors.Is(err, domain.ErrGraphNotFound) {
			RespondNotFound(w, "Graph not found")
			return
		}
		if errors.Is(err, domain.ErrGraphHasCycle) {
			RespondBadRequest(w, "Graph contains a cycle", nil)
			return
		}
		RespondBadRequest(w, err.Error(), nil)
		return
	}

	RespondSuccess(w, graph)
}

// ValidateGraph handles POST /api/v1/graphs/{graph_id}/validate
func (h *GraphHandler) ValidateGraph(w http.ResponseWriter, r *http.Request) {
	graphID, err := GetUUIDParam(r, "graph_id")
	if err != nil {
		RespondBadRequest(w, "Invalid graph ID", nil)
		return
	}

	if err := h.graphUC.ValidateGraph(r.Context(), graphID); err != nil {
		RespondBadRequest(w, err.Error(), nil)
		return
	}

	RespondSuccess(w, map[string]any{
		"valid":   true,
		"message": "Graph validation passed",
	})
}
