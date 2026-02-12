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

// ExecutionHandler handles execution-related HTTP requests
type ExecutionHandler struct {
	executionUC usecase.ExecutionUseCase
}

// NewExecutionHandler creates a new execution handler
func NewExecutionHandler(uc usecase.ExecutionUseCase) *ExecutionHandler {
	return &ExecutionHandler{executionUC: uc}
}

// RegisterRoutes registers execution routes on the router
func (h *ExecutionHandler) RegisterRoutes(r chi.Router) {
	r.Route("/executions", func(r chi.Router) {
		r.Post("/", h.CreateExecution)
		r.Get("/", h.ListExecutions)
		r.Get("/{execution_id}", h.GetExecution)
		r.Post("/{execution_id}/cancel", h.CancelExecution)
		r.Get("/{execution_id}/metrics", h.GetExecutionMetrics)
	})
}

// CreateExecutionRequest represents the request body for creating an execution
type CreateExecutionRequest struct {
	Language  string             `json:"language"`
	Code      string             `json:"code"`
	Stdin     string             `json:"stdin,omitempty"`
	Args      domain.JSONMap     `json:"args,omitempty"`
	EnvVars   domain.JSONMap     `json:"env_vars,omitempty"`
	NodeID    *string            `json:"node_id,omitempty"`
	Resources *ResourceRequest   `json:"resources,omitempty"`
}

// ResourceRequest represents resource limits in the request
type ResourceRequest struct {
	VCPU       int    `json:"vcpu,omitempty"`
	MemoryMB   int    `json:"memory_mb,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Network    string `json:"network,omitempty"`
	DiskMB     int    `json:"disk_mb,omitempty"`
}

// CreateExecution handles POST /api/v1/executions
func (h *ExecutionHandler) CreateExecution(w http.ResponseWriter, r *http.Request) {
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

	var req CreateExecutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	if req.Code == "" {
		RespondBadRequest(w, "Code is required", nil)
		return
	}

	lang := domain.Language(req.Language)
	if !lang.IsValid() {
		RespondBadRequest(w, "Unsupported language", map[string]interface{}{
			"language":  req.Language,
			"supported": []string{"go", "python", "javascript", "typescript", "rust", "c", "cpp", "bash"},
		})
		return
	}

	input := usecase.CreateExecutionInput{
		OrganizationID: orgID,
		RequestedBy:    userID,
		Language:       lang,
		Code:           req.Code,
		Stdin:          req.Stdin,
		Args:           req.Args,
		EnvVars:        req.EnvVars,
	}

	if req.NodeID != nil {
		nodeID, err := uuid.Parse(*req.NodeID)
		if err != nil {
			RespondBadRequest(w, "Invalid node_id format", nil)
			return
		}
		input.NodeID = &nodeID
	}

	if req.Resources != nil {
		input.Resources = &domain.ResourceLimit{
			VCPU:       req.Resources.VCPU,
			MemoryMB:   req.Resources.MemoryMB,
			TimeoutSec: req.Resources.TimeoutSec,
			Network:    domain.NetworkMode(req.Resources.Network),
			DiskMB:     req.Resources.DiskMB,
		}
	}

	execution, err := h.executionUC.CreateExecution(r.Context(), input)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidLanguage) || errors.Is(err, domain.ErrEmptyCode) ||
			errors.Is(err, domain.ErrInvalidVCPU) || errors.Is(err, domain.ErrInvalidMemory) ||
			errors.Is(err, domain.ErrInvalidTimeout) {
			RespondBadRequest(w, err.Error(), nil)
			return
		}
		RespondInternalError(w, "Failed to create execution")
		return
	}

	RespondCreated(w, executionToResponse(execution))
}

// GetExecution handles GET /api/v1/executions/{execution_id}
func (h *ExecutionHandler) GetExecution(w http.ResponseWriter, r *http.Request) {
	executionID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	execution, err := h.executionUC.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, domain.ErrExecutionNotFound) {
			RespondNotFound(w, "Execution not found")
			return
		}
		RespondInternalError(w, "Failed to get execution")
		return
	}

	RespondSuccess(w, executionToResponse(execution))
}

// ListExecutions handles GET /api/v1/executions
func (h *ExecutionHandler) ListExecutions(w http.ResponseWriter, r *http.Request) {
	orgID, ok := GetOrganizationIDFromContext(r.Context())
	if !ok {
		RespondBadRequest(w, "Organization ID required", nil)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	executions, total, err := h.executionUC.ListExecutions(r.Context(), orgID, limit, offset)
	if err != nil {
		RespondInternalError(w, "Failed to list executions")
		return
	}

	items := make([]ExecutionResponse, len(executions))
	for i, e := range executions {
		items[i] = executionToResponse(&e)
	}

	RespondSuccess(w, map[string]interface{}{
		"items": items,
		"total": total,
	})
}

// CancelExecution handles POST /api/v1/executions/{execution_id}/cancel
func (h *ExecutionHandler) CancelExecution(w http.ResponseWriter, r *http.Request) {
	executionID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	if err := h.executionUC.CancelExecution(r.Context(), executionID); err != nil {
		if errors.Is(err, domain.ErrExecutionNotFound) {
			RespondNotFound(w, "Execution not found")
			return
		}
		if errors.Is(err, domain.ErrExecutionAlreadyDone) {
			RespondBadRequest(w, "Execution already completed", nil)
			return
		}
		RespondInternalError(w, "Failed to cancel execution")
		return
	}

	RespondNoContent(w)
}

// GetExecutionMetrics handles GET /api/v1/executions/{execution_id}/metrics
func (h *ExecutionHandler) GetExecutionMetrics(w http.ResponseWriter, r *http.Request) {
	executionID, err := GetUUIDParam(r, "execution_id")
	if err != nil {
		RespondBadRequest(w, "Invalid execution ID", nil)
		return
	}

	metrics, err := h.executionUC.GetExecutionMetrics(r.Context(), executionID)
	if err != nil {
		RespondInternalError(w, "Failed to get execution metrics")
		return
	}
	if metrics == nil {
		RespondNotFound(w, "Metrics not available for this execution")
		return
	}

	RespondSuccess(w, metricsToResponse(metrics))
}

// ExecutionResponse is the API response for an execution
type ExecutionResponse struct {
	ID             string                 `json:"id"`
	OrganizationID string                 `json:"organization_id"`
	Language       string                 `json:"language"`
	Status         string                 `json:"status"`
	ExitCode       *int                   `json:"exit_code,omitempty"`
	Stdout         string                 `json:"stdout,omitempty"`
	Stderr         string                 `json:"stderr,omitempty"`
	Error          string                 `json:"error,omitempty"`
	Resources      ResourceResponse       `json:"resources"`
	DurationMS     *int64                 `json:"duration_ms,omitempty"`
	CreatedAt      string                 `json:"created_at"`
	StartedAt      *string                `json:"started_at,omitempty"`
	CompletedAt    *string                `json:"completed_at,omitempty"`
}

// ResourceResponse is the API response for resource limits
type ResourceResponse struct {
	VCPU       int    `json:"vcpu"`
	MemoryMB   int    `json:"memory_mb"`
	TimeoutSec int    `json:"timeout_sec"`
	Network    string `json:"network"`
	DiskMB     int    `json:"disk_mb"`
}

// MetricsResponse is the API response for execution metrics
type MetricsResponse struct {
	CPUTimeMS     int64   `json:"cpu_time_ms"`
	MemoryPeakMB  float64 `json:"memory_peak_mb"`
	MemoryAvgMB   float64 `json:"memory_avg_mb"`
	IOReadBytes   int64   `json:"io_read_bytes"`
	IOWriteBytes  int64   `json:"io_write_bytes"`
	NetBytesIn    int64   `json:"net_bytes_in"`
	NetBytesOut   int64   `json:"net_bytes_out"`
	BootTimeMS    int64   `json:"boot_time_ms"`
	CompileTimeMS *int64  `json:"compile_time_ms,omitempty"`
	ExecTimeMS    int64   `json:"exec_time_ms"`
	TotalTimeMS   int64   `json:"total_time_ms"`
}

func executionToResponse(e *domain.Execution) ExecutionResponse {
	resp := ExecutionResponse{
		ID:             e.ID.String(),
		OrganizationID: e.OrganizationID.String(),
		Language:       string(e.Language),
		Status:         string(e.Status),
		ExitCode:       e.ExitCode,
		Stdout:         e.Stdout,
		Stderr:         e.Stderr,
		Error:          e.Error,
		Resources: ResourceResponse{
			VCPU:       e.Resources.VCPU,
			MemoryMB:   e.Resources.MemoryMB,
			TimeoutSec: e.Resources.TimeoutSec,
			Network:    string(e.Resources.Network),
			DiskMB:     e.Resources.DiskMB,
		},
		DurationMS: e.DurationMS,
		CreatedAt:  e.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if e.StartedAt != nil {
		s := e.StartedAt.Format("2006-01-02T15:04:05Z")
		resp.StartedAt = &s
	}
	if e.CompletedAt != nil {
		s := e.CompletedAt.Format("2006-01-02T15:04:05Z")
		resp.CompletedAt = &s
	}

	return resp
}

func metricsToResponse(m *domain.ExecutionMetrics) MetricsResponse {
	return MetricsResponse{
		CPUTimeMS:     m.CPUTimeMS,
		MemoryPeakMB:  m.MemoryPeakMB,
		MemoryAvgMB:   m.MemoryAvgMB,
		IOReadBytes:   m.IOReadBytes,
		IOWriteBytes:  m.IOWriteBytes,
		NetBytesIn:    m.NetBytesIn,
		NetBytesOut:   m.NetBytesOut,
		BootTimeMS:    m.BootTimeMS,
		CompileTimeMS: m.CompileTimeMS,
		ExecTimeMS:    m.ExecTimeMS,
		TotalTimeMS:   m.TotalTimeMS,
	}
}
