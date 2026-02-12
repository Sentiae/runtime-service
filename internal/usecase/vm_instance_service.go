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

// vmInstanceService implements VMInstanceUseCase for the desired state store.
type vmInstanceService struct {
	instanceRepo repository.VMInstanceRepository
	vmProvider   VMProvider
	scheduler    SchedulerUseCase
}

// NewVMInstanceService creates a new VM instance service for desired-state management.
func NewVMInstanceService(
	instanceRepo repository.VMInstanceRepository,
	vmProvider VMProvider,
	scheduler SchedulerUseCase,
) VMInstanceUseCase {
	return &vmInstanceService{
		instanceRepo: instanceRepo,
		vmProvider:   vmProvider,
		scheduler:    scheduler,
	}
}

func (s *vmInstanceService) GetVMInstance(ctx context.Context, id uuid.UUID) (*domain.VMInstance, error) {
	return s.instanceRepo.FindByID(ctx, id)
}

func (s *vmInstanceService) ListVMInstances(ctx context.Context, statusFilter *domain.VMInstanceState) ([]domain.VMInstance, error) {
	return s.instanceRepo.FindAll(ctx, statusFilter)
}

func (s *vmInstanceService) SetDesiredState(ctx context.Context, id uuid.UUID, desiredState domain.VMInstanceState) error {
	if !desiredState.IsValid() {
		return domain.ErrInvalidVMState
	}

	instance, err := s.instanceRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}

	// Cannot change desired state of a terminated instance
	if instance.State.IsTerminal() && desiredState != domain.VMInstanceStateTerminated {
		return fmt.Errorf("cannot change desired state of terminated VM instance %s", id)
	}

	instance.DesiredState = desiredState
	instance.UpdatedAt = time.Now().UTC()

	if err := s.instanceRepo.Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to update desired state: %w", err)
	}

	log.Printf("VM instance %s desired state set to %s", id, desiredState)
	return nil
}

func (s *vmInstanceService) RequestTermination(ctx context.Context, id uuid.UUID) error {
	return s.SetDesiredState(ctx, id, domain.VMInstanceStateTerminated)
}

func (s *vmInstanceService) CreateVMInstance(ctx context.Context, input CreateVMInstanceInput) (*domain.VMInstance, error) {
	if !input.Language.IsValid() {
		return nil, domain.ErrInvalidLanguage
	}

	vcpu := input.VCPU
	if vcpu == 0 {
		vcpu = 1
	}
	memoryMB := input.MemoryMB
	if memoryMB == 0 {
		memoryMB = 128
	}
	diskMB := input.DiskMB
	if diskMB == 0 {
		diskMB = 256
	}

	desiredState := input.DesiredState
	if desiredState == "" {
		desiredState = domain.VMInstanceStateRunning
	}

	// Use the scheduler to select a host
	host, err := s.scheduler.SelectHost(ctx, vcpu, memoryMB)
	if err != nil {
		return nil, fmt.Errorf("scheduler failed to select host: %w", err)
	}

	now := time.Now().UTC()
	instance := &domain.VMInstance{
		ID:           uuid.New(),
		ExecutionID:  input.ExecutionID,
		HostID:       host.ID,
		State:        domain.VMInstanceStatePending,
		DesiredState: desiredState,
		Language:     input.Language,
		BaseImage:    input.BaseImage,
		VCPU:         vcpu,
		MemoryMB:     memoryMB,
		DiskMB:       diskMB,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.instanceRepo.Create(ctx, instance); err != nil {
		return nil, fmt.Errorf("failed to create VM instance: %w", err)
	}

	// Update host resource usage
	if err := s.scheduler.UpdateHostUsage(ctx, host.ID, vcpu, memoryMB); err != nil {
		log.Printf("Warning: failed to update host %s usage: %v", host.ID, err)
	}

	log.Printf("VM instance created: %s (host=%s, lang=%s, vcpu=%d, mem=%dMB, desired=%s)",
		instance.ID, host.ID, input.Language, vcpu, memoryMB, desiredState)
	return instance, nil
}

// Reconcile runs a single reconciliation pass over all VM instances.
// It compares each instance's current state to its desired state and
// takes corrective action to converge.
func (s *vmInstanceService) Reconcile(ctx context.Context) error {
	instances, err := s.instanceRepo.FindNeedingReconciliation(ctx)
	if err != nil {
		return fmt.Errorf("failed to find instances needing reconciliation: %w", err)
	}

	if len(instances) == 0 {
		return nil
	}

	log.Printf("Reconciliation: %d instances need attention", len(instances))

	for i := range instances {
		instance := &instances[i]
		if err := s.reconcileOne(ctx, instance); err != nil {
			log.Printf("Reconciliation failed for VM instance %s: %v", instance.ID, err)
			instance.SetError(err.Error())
			if updateErr := s.instanceRepo.Update(ctx, instance); updateErr != nil {
				log.Printf("Warning: failed to update error state for %s: %v", instance.ID, updateErr)
			}
		}
	}

	return nil
}

// reconcileOne handles reconciliation for a single VM instance.
func (s *vmInstanceService) reconcileOne(ctx context.Context, instance *domain.VMInstance) error {
	switch instance.DesiredState {
	case domain.VMInstanceStateRunning:
		return s.reconcileToRunning(ctx, instance)
	case domain.VMInstanceStatePaused:
		return s.reconcileToPaused(ctx, instance)
	case domain.VMInstanceStateTerminated:
		return s.reconcileToTerminated(ctx, instance)
	default:
		return fmt.Errorf("unsupported desired state: %s", instance.DesiredState)
	}
}

// reconcileToRunning ensures the VM is in running state.
func (s *vmInstanceService) reconcileToRunning(ctx context.Context, instance *domain.VMInstance) error {
	switch instance.State {
	case domain.VMInstanceStatePending:
		// Boot the VM
		instance.SetState(domain.VMInstanceStateBooting)
		if err := s.instanceRepo.Update(ctx, instance); err != nil {
			return err
		}

		bootResult, err := s.vmProvider.Boot(ctx, VMBootConfig{
			VMID:        instance.ID,
			Language:    instance.Language,
			VCPU:        instance.VCPU,
			MemoryMB:    instance.MemoryMB,
			NetworkMode: domain.NetworkModeIsolated,
		})
		if err != nil {
			return fmt.Errorf("boot VM: %w", err)
		}

		instance.PID = &bootResult.PID
		instance.IPAddress = bootResult.IPAddress
		instance.SetState(domain.VMInstanceStateRunning)
		return s.instanceRepo.Update(ctx, instance)

	case domain.VMInstanceStateBooting:
		// Still booting -- check if it has come up (a real implementation
		// would ping the agent; for now we leave it for the next pass).
		log.Printf("VM instance %s still booting, will retry next pass", instance.ID)
		return nil

	case domain.VMInstanceStatePaused:
		// Resume the VM
		if instance.SocketPath != "" {
			if err := s.vmProvider.Resume(ctx, instance.SocketPath); err != nil {
				return fmt.Errorf("resume VM: %w", err)
			}
		}
		instance.SetState(domain.VMInstanceStateRunning)
		return s.instanceRepo.Update(ctx, instance)

	case domain.VMInstanceStateError:
		// Attempt to restart: terminate the old process and re-boot
		if instance.PID != nil {
			_ = s.vmProvider.Terminate(ctx, instance.SocketPath, *instance.PID)
		}
		instance.SetState(domain.VMInstanceStatePending)
		return s.instanceRepo.Update(ctx, instance)

	default:
		return nil
	}
}

// reconcileToPaused ensures the VM is in paused state.
func (s *vmInstanceService) reconcileToPaused(ctx context.Context, instance *domain.VMInstance) error {
	if instance.State == domain.VMInstanceStateRunning {
		if instance.SocketPath != "" {
			if err := s.vmProvider.Pause(ctx, instance.SocketPath); err != nil {
				return fmt.Errorf("pause VM: %w", err)
			}
		}
		instance.SetState(domain.VMInstanceStatePaused)
		return s.instanceRepo.Update(ctx, instance)
	}
	return nil
}

// reconcileToTerminated ensures the VM is terminated.
func (s *vmInstanceService) reconcileToTerminated(ctx context.Context, instance *domain.VMInstance) error {
	if instance.State.IsTerminal() {
		return nil
	}

	// Terminate the VM process
	if instance.PID != nil {
		if err := s.vmProvider.Terminate(ctx, instance.SocketPath, *instance.PID); err != nil {
			log.Printf("Warning: failed to terminate VM process for %s: %v", instance.ID, err)
		}
	}

	// Release host resources
	if err := s.scheduler.ReleaseHostResources(ctx, instance.HostID, instance.VCPU, instance.MemoryMB); err != nil {
		log.Printf("Warning: failed to release resources on host %s: %v", instance.HostID, err)
	}

	instance.MarkTerminated()
	return s.instanceRepo.Update(ctx, instance)
}

// ReconciliationController runs the reconciliation loop periodically.
type ReconciliationController struct {
	vmInstanceUC VMInstanceUseCase
	interval     time.Duration
	stopCh       chan struct{}
}

// NewReconciliationController creates a new reconciliation controller.
func NewReconciliationController(vmInstanceUC VMInstanceUseCase, interval time.Duration) *ReconciliationController {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ReconciliationController{
		vmInstanceUC: vmInstanceUC,
		interval:     interval,
		stopCh:       make(chan struct{}),
	}
}

// Start begins the reconciliation loop in a background goroutine.
func (c *ReconciliationController) Start(ctx context.Context) {
	go func() {
		log.Printf("Reconciliation controller started (interval=%s)", c.interval)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("Reconciliation controller stopped (context cancelled)")
				return
			case <-c.stopCh:
				log.Println("Reconciliation controller stopped")
				return
			case <-ticker.C:
				if err := c.vmInstanceUC.Reconcile(ctx); err != nil {
					log.Printf("Reconciliation error: %v", err)
				}
			}
		}
	}()
}

// Stop signals the reconciliation loop to stop.
func (c *ReconciliationController) Stop() {
	close(c.stopCh)
}
