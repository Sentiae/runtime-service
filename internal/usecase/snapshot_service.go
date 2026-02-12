package usecase

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

type snapshotService struct {
	snapshotRepo repository.SnapshotRepository
	vmRepo       repository.MicroVMRepository
	vmProvider   VMProvider
}

// NewSnapshotService creates a new snapshot use case service
func NewSnapshotService(
	snapshotRepo repository.SnapshotRepository,
	vmRepo repository.MicroVMRepository,
	vmProvider VMProvider,
) SnapshotUseCase {
	return &snapshotService{
		snapshotRepo: snapshotRepo,
		vmRepo:       vmRepo,
		vmProvider:   vmProvider,
	}
}

func (s *snapshotService) CreateSnapshot(ctx context.Context, vmID uuid.UUID, description string) (*domain.Snapshot, error) {
	vm, err := s.vmRepo.FindByID(ctx, vmID)
	if err != nil {
		return nil, err
	}

	if vm.Status != domain.VMStatusRunning && vm.Status != domain.VMStatusReady {
		return nil, domain.ErrVMNotReady
	}

	// Pause the VM for a consistent snapshot
	if err := s.vmProvider.Pause(ctx, vm.SocketPath); err != nil {
		return nil, fmt.Errorf("failed to pause VM for snapshot: %w", err)
	}

	start := time.Now()
	snapshotID := uuid.New()

	// Call the real Firecracker snapshot API via the provider
	result, err := s.vmProvider.CreateSnapshot(ctx, vm.SocketPath, snapshotID)
	if err != nil {
		// Resume the VM even if snapshot fails
		if resumeErr := s.vmProvider.Resume(ctx, vm.SocketPath); resumeErr != nil {
			log.Printf("Warning: failed to resume VM %s after snapshot failure: %v", vmID, resumeErr)
		}
		return nil, fmt.Errorf("failed to create snapshot via Firecracker API: %w", err)
	}

	createTimeMS := time.Since(start).Milliseconds()

	snapshot := &domain.Snapshot{
		ID:             snapshotID,
		VMID:           vmID,
		ExecutionID:    vm.ExecutionID,
		Language:       vm.Language,
		MemoryFilePath: result.MemoryFilePath,
		StateFilePath:  result.StateFilePath,
		SizeBytes:      result.SizeBytes,
		VCPU:           vm.VCPU,
		MemoryMB:       vm.MemoryMB,
		Description:    description,
		RestoreTimeMS:  &createTimeMS,
		CreatedAt:      time.Now().UTC(),
	}

	if err := s.snapshotRepo.Create(ctx, snapshot); err != nil {
		// Resume VM even if snapshot record save fails
		if resumeErr := s.vmProvider.Resume(ctx, vm.SocketPath); resumeErr != nil {
			log.Printf("Warning: failed to resume VM %s after snapshot record failure: %v", vmID, resumeErr)
		}
		// Clean up snapshot files since we could not persist the record
		if delErr := s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath); delErr != nil {
			log.Printf("Warning: failed to clean up snapshot files for %s: %v", snapshotID, delErr)
		}
		return nil, fmt.Errorf("failed to save snapshot record: %w", err)
	}

	// Resume the VM after successful snapshot
	if err := s.vmProvider.Resume(ctx, vm.SocketPath); err != nil {
		log.Printf("Warning: failed to resume VM %s after snapshot: %v", vmID, err)
	}

	log.Printf("Snapshot created: %s (vm=%s, lang=%s, size=%d bytes, time=%dms)",
		snapshotID, vmID, vm.Language, result.SizeBytes, createTimeMS)
	return snapshot, nil
}

func (s *snapshotService) RestoreSnapshot(ctx context.Context, snapshotID uuid.UUID) (*domain.MicroVM, error) {
	snapshot, err := s.snapshotRepo.FindByID(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	// Create a new VM record for the restored instance
	vmID := uuid.New()
	vm := &domain.MicroVM{
		ID:          vmID,
		Status:      domain.VMStatusCreating,
		VCPU:        snapshot.VCPU,
		MemoryMB:    snapshot.MemoryMB,
		KernelPath:  "", // Restored from snapshot state
		RootfsPath:  "", // Restored from snapshot state
		NetworkMode: domain.NetworkModeIsolated,
		Language:    snapshot.Language,
		CreatedAt:   time.Now().UTC(),
	}

	if err := s.vmRepo.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to create VM record for restore: %w", err)
	}

	start := time.Now()

	// Boot a fresh Firecracker process (no VM config needed -- snapshot will provide it)
	bootResult, err := s.vmProvider.Boot(ctx, VMBootConfig{
		VMID:        vmID,
		Language:    snapshot.Language,
		VCPU:        snapshot.VCPU,
		MemoryMB:    snapshot.MemoryMB,
		NetworkMode: domain.NetworkModeIsolated,
	})
	if err != nil {
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to boot Firecracker for restore: %w", err)
	}

	// Load the snapshot into the fresh Firecracker instance
	socketPath := vm.SocketPath
	if socketPath == "" {
		// Derive the socket path the same way the provider does
		socketPath = fmt.Sprintf("/tmp/firecracker/%s.sock", vmID.String())
	}

	if err := s.vmProvider.RestoreSnapshot(ctx, socketPath, snapshot.MemoryFilePath, snapshot.StateFilePath); err != nil {
		// Terminate the fresh Firecracker process on failure
		if bootResult.PID > 0 {
			_ = s.vmProvider.Terminate(ctx, socketPath, bootResult.PID)
		}
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to restore snapshot: %w", err)
	}

	restoreTimeMS := time.Since(start).Milliseconds()

	// Update VM with boot/restore results
	vm.Status = domain.VMStatusReady
	vm.PID = &bootResult.PID
	vm.IPAddress = bootResult.IPAddress
	vm.BootTimeMS = &restoreTimeMS
	vm.SocketPath = socketPath

	if err := s.vmRepo.Update(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to update VM after restore: %w", err)
	}

	log.Printf("Snapshot restored: %s -> VM %s (restore=%dms)", snapshotID, vmID, restoreTimeMS)
	return vm, nil
}

func (s *snapshotService) GetSnapshot(ctx context.Context, id uuid.UUID) (*domain.Snapshot, error) {
	return s.snapshotRepo.FindByID(ctx, id)
}

func (s *snapshotService) ListSnapshotsByExecution(ctx context.Context, executionID uuid.UUID) ([]domain.Snapshot, error) {
	return s.snapshotRepo.FindByExecution(ctx, executionID)
}

func (s *snapshotService) GetBaseSnapshot(ctx context.Context, language domain.Language) (*domain.Snapshot, error) {
	return s.snapshotRepo.FindBaseByLanguage(ctx, language)
}

func (s *snapshotService) DeleteSnapshot(ctx context.Context, id uuid.UUID) error {
	// Retrieve snapshot to get file paths before deleting
	snapshot, err := s.snapshotRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}

	// Delete snapshot files from disk
	if err := s.vmProvider.DeleteSnapshotFiles(snapshot.MemoryFilePath, snapshot.StateFilePath); err != nil {
		log.Printf("Warning: failed to delete snapshot files for %s: %v", id, err)
	}

	return s.snapshotRepo.Delete(ctx, id)
}
