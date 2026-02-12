package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// SnapshotHandler handles snapshot-related HTTP requests
type SnapshotHandler struct {
	snapshotUC usecase.SnapshotUseCase
}

// NewSnapshotHandler creates a new snapshot handler
func NewSnapshotHandler(uc usecase.SnapshotUseCase) *SnapshotHandler {
	return &SnapshotHandler{snapshotUC: uc}
}

// RegisterRoutes registers snapshot routes on the router
func (h *SnapshotHandler) RegisterRoutes(r chi.Router) {
	r.Route("/snapshots", func(r chi.Router) {
		r.Post("/", h.CreateSnapshot)
		r.Get("/{snapshot_id}", h.GetSnapshot)
		r.Delete("/{snapshot_id}", h.DeleteSnapshot)
		r.Post("/{snapshot_id}/restore", h.RestoreSnapshot)
	})
}

// CreateSnapshotRequest represents the request body for creating a snapshot
type CreateSnapshotRequest struct {
	VMID        string `json:"vm_id"`
	Description string `json:"description,omitempty"`
}

// CreateSnapshot handles POST /api/v1/snapshots
func (h *SnapshotHandler) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req CreateSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondBadRequest(w, "Invalid request body", nil)
		return
	}

	vmID, err := parseUUID(req.VMID)
	if err != nil {
		RespondBadRequest(w, "Invalid VM ID", nil)
		return
	}

	snapshot, err := h.snapshotUC.CreateSnapshot(r.Context(), vmID, req.Description)
	if err != nil {
		if errors.Is(err, domain.ErrVMNotFound) {
			RespondNotFound(w, "VM not found")
			return
		}
		if errors.Is(err, domain.ErrVMNotReady) {
			RespondBadRequest(w, "VM is not in a snapshotable state", nil)
			return
		}
		RespondInternalError(w, "Failed to create snapshot")
		return
	}

	RespondCreated(w, snapshotToResponse(snapshot))
}

// GetSnapshot handles GET /api/v1/snapshots/{snapshot_id}
func (h *SnapshotHandler) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := GetUUIDParam(r, "snapshot_id")
	if err != nil {
		RespondBadRequest(w, "Invalid snapshot ID", nil)
		return
	}

	snapshot, err := h.snapshotUC.GetSnapshot(r.Context(), snapshotID)
	if err != nil {
		if errors.Is(err, domain.ErrSnapshotNotFound) {
			RespondNotFound(w, "Snapshot not found")
			return
		}
		RespondInternalError(w, "Failed to get snapshot")
		return
	}

	RespondSuccess(w, snapshotToResponse(snapshot))
}

// DeleteSnapshot handles DELETE /api/v1/snapshots/{snapshot_id}
func (h *SnapshotHandler) DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := GetUUIDParam(r, "snapshot_id")
	if err != nil {
		RespondBadRequest(w, "Invalid snapshot ID", nil)
		return
	}

	if err := h.snapshotUC.DeleteSnapshot(r.Context(), snapshotID); err != nil {
		if errors.Is(err, domain.ErrSnapshotNotFound) {
			RespondNotFound(w, "Snapshot not found")
			return
		}
		RespondInternalError(w, "Failed to delete snapshot")
		return
	}

	RespondNoContent(w)
}

// RestoreSnapshot handles POST /api/v1/snapshots/{snapshot_id}/restore
func (h *SnapshotHandler) RestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := GetUUIDParam(r, "snapshot_id")
	if err != nil {
		RespondBadRequest(w, "Invalid snapshot ID", nil)
		return
	}

	vm, err := h.snapshotUC.RestoreSnapshot(r.Context(), snapshotID)
	if err != nil {
		if errors.Is(err, domain.ErrSnapshotNotFound) {
			RespondNotFound(w, "Snapshot not found")
			return
		}
		if errors.Is(err, domain.ErrSnapshotRestoreFail) {
			RespondInternalError(w, "Failed to restore from snapshot")
			return
		}
		RespondInternalError(w, "Failed to restore snapshot")
		return
	}

	RespondCreated(w, vmToResponse(vm))
}

// SnapshotResponse is the API response for a snapshot
type SnapshotResponse struct {
	ID            string  `json:"id"`
	VMID          string  `json:"vm_id"`
	ExecutionID   *string `json:"execution_id,omitempty"`
	Language      string  `json:"language"`
	SizeBytes     int64   `json:"size_bytes"`
	VCPU          int     `json:"vcpu"`
	MemoryMB      int     `json:"memory_mb"`
	Description   string  `json:"description,omitempty"`
	IsBaseImage   bool    `json:"is_base_image"`
	RestoreTimeMS *int64  `json:"restore_time_ms,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

func snapshotToResponse(s *domain.Snapshot) SnapshotResponse {
	resp := SnapshotResponse{
		ID:            s.ID.String(),
		VMID:          s.VMID.String(),
		Language:      string(s.Language),
		SizeBytes:     s.SizeBytes,
		VCPU:          s.VCPU,
		MemoryMB:      s.MemoryMB,
		Description:   s.Description,
		IsBaseImage:   s.IsBaseImage,
		RestoreTimeMS: s.RestoreTimeMS,
		CreatedAt:     s.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if s.ExecutionID != nil {
		e := s.ExecutionID.String()
		resp.ExecutionID = &e
	}

	return resp
}

func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
