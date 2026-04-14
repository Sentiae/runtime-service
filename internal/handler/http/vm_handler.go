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
	vmUC usecase.VMUseCase
}

// NewVMHandler creates a new VM handler
func NewVMHandler(uc usecase.VMUseCase) *VMHandler {
	return &VMHandler{vmUC: uc}
}

// RegisterRoutes registers VM routes on the router
func (h *VMHandler) RegisterRoutes(r chi.Router) {
	r.Route("/vms", func(r chi.Router) {
		r.Get("/", h.ListActiveVMs)
		r.Get("/{vm_id}", h.GetVM)
		r.Post("/{vm_id}/terminate", h.TerminateVM)
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
