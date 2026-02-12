package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// VMInstanceHandler handles desired-state VM instance HTTP requests
type VMInstanceHandler struct {
	vmInstanceUC usecase.VMInstanceUseCase
	schedulerUC  usecase.SchedulerUseCase
}

// NewVMInstanceHandler creates a new VM instance handler
func NewVMInstanceHandler(vmInstanceUC usecase.VMInstanceUseCase, schedulerUC usecase.SchedulerUseCase) *VMInstanceHandler {
	return &VMInstanceHandler{
		vmInstanceUC: vmInstanceUC,
		schedulerUC:  schedulerUC,
	}
}

// RegisterRoutes registers VM instance routes on the router
func (h *VMInstanceHandler) RegisterRoutes(r chi.Router) {
	r.Route("/vm-instances", func(r chi.Router) {
		r.Get("/", h.ListVMInstances)
		r.Post("/", h.CreateVMInstance)
		r.Get("/{id}", h.GetVMInstance)
		r.Post("/{id}/desired-state", h.SetDesiredState)
		r.Delete("/{id}", h.RequestTermination)
	})

	r.Route("/hosts", func(r chi.Router) {
		r.Get("/", h.ListHosts)
	})
}

// --- Request/Response Types ---

// CreateVMInstanceRequest represents the request body for creating a VM instance
type CreateVMInstanceRequest struct {
	Language     string `json:"language"`
	BaseImage    string `json:"base_image,omitempty"`
	DesiredState string `json:"desired_state,omitempty"`
	VCPU         int    `json:"vcpu,omitempty"`
	MemoryMB     int    `json:"memory_mb,omitempty"`
	DiskMB       int    `json:"disk_mb,omitempty"`
	ExecutionID  string `json:"execution_id,omitempty"`
}

// SetDesiredStateRequest represents the request body for setting desired state
type SetDesiredStateRequest struct {
	DesiredState string `json:"desired_state"`
}

// VMInstanceResponse is the API response for a VM instance
type VMInstanceResponse struct {
	ID           string  `json:"id"`
	ExecutionID  *string `json:"execution_id,omitempty"`
	HostID       string  `json:"host_id"`
	State        string  `json:"state"`
	DesiredState string  `json:"desired_state"`
	Language     string  `json:"language"`
	BaseImage    string  `json:"base_image,omitempty"`
	VCPU         int     `json:"vcpu"`
	MemoryMB     int     `json:"memory_mb"`
	DiskMB       int     `json:"disk_mb"`
	IPAddress    string  `json:"ip_address,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	TerminatedAt *string `json:"terminated_at,omitempty"`
}

// HostResponse is the API response for a host
type HostResponse struct {
	ID            string            `json:"id"`
	Address       string            `json:"address"`
	TotalVCPU     int               `json:"total_vcpu"`
	TotalMemMB    int               `json:"total_mem_mb"`
	UsedVCPU      int               `json:"used_vcpu"`
	UsedMemMB     int               `json:"used_mem_mb"`
	AvailableVCPU int               `json:"available_vcpu"`
	AvailableMemMB int              `json:"available_mem_mb"`
	Available     bool              `json:"available"`
	Labels        map[string]string `json:"labels,omitempty"`
	LastHeartbeat string            `json:"last_heartbeat"`
}

// --- Handlers ---

// CreateVMInstance handles POST /api/v1/vm-instances
func (h *VMInstanceHandler) CreateVMInstance(w http.ResponseWriter, r *http.Request) {
	var req CreateVMInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	lang := domain.Language(req.Language)
	if !lang.IsValid() {
		RespondBadRequest(w, "Unsupported language", map[string]interface{}{
			"language": req.Language,
		})
		return
	}

	input := usecase.CreateVMInstanceInput{
		Language:  lang,
		BaseImage: req.BaseImage,
		VCPU:      req.VCPU,
		MemoryMB:  req.MemoryMB,
		DiskMB:    req.DiskMB,
	}

	if req.DesiredState != "" {
		ds := domain.VMInstanceState(req.DesiredState)
		if !ds.IsValid() {
			RespondBadRequest(w, "Invalid desired state", nil)
			return
		}
		input.DesiredState = ds
	}

	if req.ExecutionID != "" {
		execID, err := parseUUID(req.ExecutionID)
		if err != nil {
			RespondBadRequest(w, "Invalid execution_id format", nil)
			return
		}
		input.ExecutionID = &execID
	}

	instance, err := h.vmInstanceUC.CreateVMInstance(r.Context(), input)
	if err != nil {
		if errors.Is(err, domain.ErrNoHostAvailable) {
			RespondError(w, http.StatusServiceUnavailable, "NO_HOST_AVAILABLE", "No host available with sufficient resources", nil)
			return
		}
		if errors.Is(err, domain.ErrInvalidLanguage) {
			RespondBadRequest(w, err.Error(), nil)
			return
		}
		RespondInternalError(w, "Failed to create VM instance")
		return
	}

	RespondCreated(w, vmInstanceToResponse(instance))
}

// GetVMInstance handles GET /api/v1/vm-instances/{id}
func (h *VMInstanceHandler) GetVMInstance(w http.ResponseWriter, r *http.Request) {
	id, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM instance ID", nil)
		return
	}

	instance, err := h.vmInstanceUC.GetVMInstance(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrVMInstanceNotFound) {
			RespondNotFound(w, "VM instance not found")
			return
		}
		RespondInternalError(w, "Failed to get VM instance")
		return
	}

	RespondSuccess(w, vmInstanceToResponse(instance))
}

// ListVMInstances handles GET /api/v1/vm-instances
func (h *VMInstanceHandler) ListVMInstances(w http.ResponseWriter, r *http.Request) {
	var statusFilter *domain.VMInstanceState
	if stateParam := r.URL.Query().Get("state"); stateParam != "" {
		state := domain.VMInstanceState(stateParam)
		if !state.IsValid() {
			RespondBadRequest(w, "Invalid state filter", nil)
			return
		}
		statusFilter = &state
	}

	instances, err := h.vmInstanceUC.ListVMInstances(r.Context(), statusFilter)
	if err != nil {
		RespondInternalError(w, "Failed to list VM instances")
		return
	}

	items := make([]VMInstanceResponse, len(instances))
	for i, inst := range instances {
		items[i] = vmInstanceToResponse(&inst)
	}

	RespondSuccess(w, map[string]interface{}{
		"items": items,
		"total": len(items),
	})
}

// SetDesiredState handles POST /api/v1/vm-instances/{id}/desired-state
func (h *VMInstanceHandler) SetDesiredState(w http.ResponseWriter, r *http.Request) {
	id, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM instance ID", nil)
		return
	}

	var req SetDesiredStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	desiredState := domain.VMInstanceState(req.DesiredState)
	if !desiredState.IsValid() {
		RespondBadRequest(w, "Invalid desired state", map[string]interface{}{
			"desired_state": req.DesiredState,
			"valid_states":  []string{"pending", "booting", "running", "paused", "terminated"},
		})
		return
	}

	if err := h.vmInstanceUC.SetDesiredState(r.Context(), id, desiredState); err != nil {
		if errors.Is(err, domain.ErrVMInstanceNotFound) {
			RespondNotFound(w, "VM instance not found")
			return
		}
		if errors.Is(err, domain.ErrInvalidVMState) {
			RespondBadRequest(w, err.Error(), nil)
			return
		}
		RespondInternalError(w, "Failed to set desired state")
		return
	}

	// Return the updated instance
	instance, err := h.vmInstanceUC.GetVMInstance(r.Context(), id)
	if err != nil {
		RespondInternalError(w, "State updated but failed to fetch result")
		return
	}

	RespondSuccess(w, vmInstanceToResponse(instance))
}

// RequestTermination handles DELETE /api/v1/vm-instances/{id}
func (h *VMInstanceHandler) RequestTermination(w http.ResponseWriter, r *http.Request) {
	id, err := GetUUIDParam(r, "id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM instance ID", nil)
		return
	}

	if err := h.vmInstanceUC.RequestTermination(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrVMInstanceNotFound) {
			RespondNotFound(w, "VM instance not found")
			return
		}
		RespondInternalError(w, "Failed to request termination")
		return
	}

	RespondNoContent(w)
}

// ListHosts handles GET /api/v1/hosts
func (h *VMInstanceHandler) ListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := h.schedulerUC.ListHosts(r.Context())
	if err != nil {
		RespondInternalError(w, "Failed to list hosts")
		return
	}

	items := make([]HostResponse, len(hosts))
	for i, host := range hosts {
		items[i] = hostToResponse(&host)
	}

	RespondSuccess(w, map[string]interface{}{
		"items": items,
		"total": len(items),
	})
}

// --- Conversion helpers ---

func vmInstanceToResponse(inst *domain.VMInstance) VMInstanceResponse {
	resp := VMInstanceResponse{
		ID:           inst.ID.String(),
		HostID:       inst.HostID,
		State:        string(inst.State),
		DesiredState: string(inst.DesiredState),
		Language:     string(inst.Language),
		BaseImage:    inst.BaseImage,
		VCPU:         inst.VCPU,
		MemoryMB:     inst.MemoryMB,
		DiskMB:       inst.DiskMB,
		IPAddress:    inst.IPAddress,
		ErrorMessage: inst.ErrorMessage,
		CreatedAt:    inst.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    inst.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if inst.ExecutionID != nil {
		s := inst.ExecutionID.String()
		resp.ExecutionID = &s
	}
	if inst.TerminatedAt != nil {
		s := inst.TerminatedAt.Format("2006-01-02T15:04:05Z")
		resp.TerminatedAt = &s
	}

	return resp
}

func hostToResponse(h *usecase.HostInfo) HostResponse {
	return HostResponse{
		ID:             h.ID,
		Address:        h.Address,
		TotalVCPU:      h.TotalVCPU,
		TotalMemMB:     h.TotalMemMB,
		UsedVCPU:       h.UsedVCPU,
		UsedMemMB:      h.UsedMemMB,
		AvailableVCPU:  h.AvailableVCPU(),
		AvailableMemMB: h.AvailableMemMB(),
		Available:      h.Available,
		Labels:         h.Labels,
		LastHeartbeat:  h.LastHeartbeat.Format("2006-01-02T15:04:05Z"),
	}
}
