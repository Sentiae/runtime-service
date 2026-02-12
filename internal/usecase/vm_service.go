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

type vmService struct {
	vmRepo      repository.MicroVMRepository
	vmProvider  VMProvider
}

// VMProvider abstracts the Firecracker microVM lifecycle
type VMProvider interface {
	// Boot starts a new microVM and returns when it's ready
	Boot(ctx context.Context, config VMBootConfig) (*VMBootResult, error)

	// Terminate kills a running microVM
	Terminate(ctx context.Context, socketPath string, pid int) error

	// Pause pauses a running microVM (for snapshotting)
	Pause(ctx context.Context, socketPath string) error

	// Resume resumes a paused microVM
	Resume(ctx context.Context, socketPath string) error

	// CreateSnapshot creates a snapshot of a paused VM
	CreateSnapshot(ctx context.Context, socketPath string, snapshotID uuid.UUID) (*SnapshotResult, error)

	// RestoreSnapshot restores a VM from snapshot files
	RestoreSnapshot(ctx context.Context, socketPath, memPath, statePath string) error

	// CollectMetrics gathers resource usage metrics from a running VM
	CollectMetrics(ctx context.Context, ip string) (*VMMetrics, error)

	// DeleteSnapshotFiles removes snapshot files from disk
	DeleteSnapshotFiles(memPath, statePath string) error
}

// VMBootConfig holds configuration for booting a new microVM
type VMBootConfig struct {
	VMID        uuid.UUID
	Language    domain.Language
	VCPU        int
	MemoryMB    int
	NetworkMode domain.NetworkMode
	KernelPath  string
	RootfsPath  string
	SocketPath  string
}

// VMBootResult holds the result of booting a microVM
type VMBootResult struct {
	PID        int
	IPAddress  string
	BootTimeMS int64
}

// NewVMService creates a new VM use case service
func NewVMService(
	vmRepo repository.MicroVMRepository,
	vmProvider VMProvider,
) VMUseCase {
	return &vmService{
		vmRepo:     vmRepo,
		vmProvider: vmProvider,
	}
}

func (s *vmService) CreateVM(ctx context.Context, language domain.Language, vcpu, memMB int) (*domain.MicroVM, error) {
	vmID := uuid.New()

	vm := &domain.MicroVM{
		ID:          vmID,
		Status:      domain.VMStatusCreating,
		VCPU:        vcpu,
		MemoryMB:    memMB,
		KernelPath:  "", // Set by provider
		RootfsPath:  "", // Set by provider
		NetworkMode: domain.NetworkModeIsolated,
		Language:    language,
		CreatedAt:   time.Now().UTC(),
	}

	if err := s.vmRepo.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to create VM record: %w", err)
	}

	// Boot the VM via the provider
	bootResult, err := s.vmProvider.Boot(ctx, VMBootConfig{
		VMID:        vmID,
		Language:    language,
		VCPU:        vcpu,
		MemoryMB:    memMB,
		NetworkMode: domain.NetworkModeIsolated,
	})
	if err != nil {
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to boot VM: %w", err)
	}

	// Update VM with boot results
	vm.Status = domain.VMStatusReady
	vm.PID = &bootResult.PID
	vm.IPAddress = bootResult.IPAddress
	vm.BootTimeMS = &bootResult.BootTimeMS

	if err := s.vmRepo.Update(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to update VM after boot: %w", err)
	}

	log.Printf("MicroVM created: %s (lang=%s, vcpu=%d, mem=%dMB, boot=%dms)",
		vm.ID, language, vcpu, memMB, bootResult.BootTimeMS)
	return vm, nil
}

func (s *vmService) AcquireVM(ctx context.Context, language domain.Language, resources domain.ResourceLimit) (*domain.MicroVM, error) {
	// Try to find an available VM from the pool
	vm, err := s.vmRepo.FindAvailable(ctx, language)
	if err == nil {
		vm.AssignExecution(uuid.Nil) // Will be set by caller
		if err := s.vmRepo.Update(ctx, vm); err != nil {
			return nil, err
		}
		return vm, nil
	}

	// No available VM — create a new one
	return s.CreateVM(ctx, language, resources.VCPU, resources.MemoryMB)
}

func (s *vmService) ReleaseVM(ctx context.Context, vmID uuid.UUID) error {
	vm, err := s.vmRepo.FindByID(ctx, vmID)
	if err != nil {
		return err
	}

	vm.Release()
	return s.vmRepo.Update(ctx, vm)
}

func (s *vmService) TerminateVM(ctx context.Context, vmID uuid.UUID) error {
	vm, err := s.vmRepo.FindByID(ctx, vmID)
	if err != nil {
		return err
	}

	if vm.Status == domain.VMStatusTerminated {
		return domain.ErrVMAlreadyTerminated
	}

	// Terminate via provider
	if vm.PID != nil {
		if err := s.vmProvider.Terminate(ctx, vm.SocketPath, *vm.PID); err != nil {
			log.Printf("Warning: failed to terminate VM process %d: %v", *vm.PID, err)
		}
	}

	vm.Terminate()
	return s.vmRepo.Update(ctx, vm)
}

func (s *vmService) GetVM(ctx context.Context, id uuid.UUID) (*domain.MicroVM, error) {
	return s.vmRepo.FindByID(ctx, id)
}

func (s *vmService) ListActiveVMs(ctx context.Context) ([]domain.MicroVM, error) {
	return s.vmRepo.FindActive(ctx)
}

func (s *vmService) EnsurePoolSize(ctx context.Context, language domain.Language, targetSize int) error {
	readyCount, err := s.vmRepo.CountByStatus(ctx, domain.VMStatusReady)
	if err != nil {
		return err
	}

	needed := int64(targetSize) - readyCount
	if needed <= 0 {
		return nil
	}

	log.Printf("Pool replenishment: creating %d VMs for language %s", needed, language)
	for i := int64(0); i < needed; i++ {
		if _, err := s.CreateVM(ctx, language, 1, 128); err != nil {
			log.Printf("Warning: failed to create pool VM: %v", err)
		}
	}

	return nil
}
