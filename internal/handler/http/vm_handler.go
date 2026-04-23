package http

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// VMHandler handles microVM-related HTTP requests
type VMHandler struct {
	vmUC        usecase.VMUseCase
	checkpoints *usecase.CheckpointScheduler
}

// NewVMHandler creates a new VM handler. The optional checkpoint
// scheduler enables the POST /vms/{id}/pause endpoint (§9.3); pass
// nil when the caller hasn't wired snapshotting yet and the endpoint
// will respond with 503.
func NewVMHandler(uc usecase.VMUseCase, cp *usecase.CheckpointScheduler) *VMHandler {
	return &VMHandler{vmUC: uc, checkpoints: cp}
}

// RegisterRoutes registers VM routes on the router
func (h *VMHandler) RegisterRoutes(r chi.Router) {
	r.Route("/vms", func(r chi.Router) {
		r.Get("/", h.ListActiveVMs)
		r.Get("/{vm_id}", h.GetVM)
		r.Post("/{vm_id}/terminate", h.TerminateVM)
		r.Post("/{vm_id}/pause", h.PauseVM)
	})
}

// GetVM handles GET /api/v1/vms/{vm_id}
func (h *VMHandler) GetVM(w http.ResponseWriter, r *http.Request) {
	vmID, err := GetUUIDParam(r, "vm_id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM ID", nil)
		return
	}

	vm, err := h.vmUC.GetVM(r.Context(), vmID)
	if err != nil {
		if errors.Is(err, domain.ErrVMNotFound) {
			RespondNotFound(w, "VM not found")
			return
		}
		RespondInternalError(w, "Failed to get VM")
		return
	}

	RespondSuccess(w, vmToResponse(vm))
}

// ListActiveVMs handles GET /api/v1/vms
func (h *VMHandler) ListActiveVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := h.vmUC.ListActiveVMs(r.Context())
	if err != nil {
		RespondInternalError(w, "Failed to list VMs")
		return
	}

	items := make([]VMResponse, len(vms))
	for i, vm := range vms {
		items[i] = vmToResponse(&vm)
	}

	RespondSuccess(w, map[string]any{
		"items": items,
		"total": len(items),
	})
}

// TerminateVM handles POST /api/v1/vms/{vm_id}/terminate
func (h *VMHandler) TerminateVM(w http.ResponseWriter, r *http.Request) {
	vmID, err := GetUUIDParam(r, "vm_id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM ID", nil)
		return
	}

	if err := h.vmUC.TerminateVM(r.Context(), vmID); err != nil {
		if errors.Is(err, domain.ErrVMNotFound) {
			RespondNotFound(w, "VM not found")
			return
		}
		if errors.Is(err, domain.ErrVMAlreadyTerminated) {
			RespondBadRequest(w, "VM already terminated", nil)
			return
		}
		RespondInternalError(w, "Failed to terminate VM")
		return
	}

	RespondNoContent(w)
}

// PauseVM handles POST /api/v1/vms/{vm_id}/pause.
//
// §9.3 gap-closure: when an agent asks its sandbox VM to pause we
// snapshot immediately (not on the scheduler cadence) and leave the
// CPU stopped. Callers restore from the returned snapshot to resume.
func (h *VMHandler) PauseVM(w http.ResponseWriter, r *http.Request) {
	vmID, err := GetUUIDParam(r, "vm_id")
	if err != nil {
		RespondBadRequest(w, "Invalid VM ID", nil)
		return
	}
	if h.checkpoints == nil {
		RespondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "checkpoint scheduler not configured", nil)
		return
	}
	snap, err := h.checkpoints.CheckpointNow(r.Context(), vmID)
	if err != nil {
		if errors.Is(err, domain.ErrVMNotFound) {
			RespondNotFound(w, "VM not found")
			return
		}
		RespondInternalError(w, err.Error())
		return
	}
	RespondSuccess(w, map[string]any{
		"vm_id":       vmID.String(),
		"snapshot_id": snap.ID.String(),
		"size_bytes":  snap.SizeBytes,
		"created_at":  snap.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

// VMResponse is the API response for a microVM
type VMResponse struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	VCPU        int     `json:"vcpu"`
	MemoryMB    int     `json:"memory_mb"`
	Language    string  `json:"language"`
	NetworkMode string  `json:"network_mode"`
	IPAddress   string  `json:"ip_address,omitempty"`
	BootTimeMS  *int64  `json:"boot_time_ms,omitempty"`
	ExecutionID *string `json:"execution_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

func vmToResponse(vm *domain.MicroVM) VMResponse {
	resp := VMResponse{
		ID:          vm.ID.String(),
		Status:      string(vm.Status),
		VCPU:        vm.VCPU,
		MemoryMB:    vm.MemoryMB,
		Language:    string(vm.Language),
		NetworkMode: string(vm.NetworkMode),
		IPAddress:   vm.IPAddress,
		BootTimeMS:  vm.BootTimeMS,
		CreatedAt:   vm.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if vm.ExecutionID != nil {
		s := vm.ExecutionID.String()
		resp.ExecutionID = &s
	}

	return resp
}
