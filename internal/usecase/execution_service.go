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

type executionService struct {
	execRepo    repository.ExecutionRepository
	metricsRepo repository.ExecutionMetricsRepository
	vmUC        VMUseCase
	runner      ExecutionRunner
	vmProvider  VMProvider
}

// ExecutionRunner abstracts the actual code execution within a microVM
type ExecutionRunner interface {
	// Run executes code inside a microVM and returns the result
	Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*RunResult, error)
}

// RunResult represents the result of a code execution
type RunResult struct {
	ExitCode      int
	Stdout        string
	Stderr        string
	CPUTimeMS     int64
	MemoryPeakMB  float64
	MemoryAvgMB   float64
	IOReadBytes   int64
	IOWriteBytes  int64
	CompileTimeMS *int64
	ExecTimeMS    int64
}

// NewExecutionService creates a new execution use case service
func NewExecutionService(
	execRepo repository.ExecutionRepository,
	metricsRepo repository.ExecutionMetricsRepository,
	vmUC VMUseCase,
	runner ExecutionRunner,
	vmProvider ...VMProvider,
) ExecutionUseCase {
	svc := &executionService{
		execRepo:    execRepo,
		metricsRepo: metricsRepo,
		vmUC:        vmUC,
		runner:      runner,
	}
	if len(vmProvider) > 0 {
		svc.vmProvider = vmProvider[0]
	}
	return svc
}

func (s *executionService) CreateExecution(ctx context.Context, input CreateExecutionInput) (*domain.Execution, error) {
	resources := domain.ResourceLimit{}
	if input.Resources != nil {
		resources = *input.Resources
	}
	resources.ApplyDefaults()

	if err := resources.Validate(); err != nil {
		return nil, err
	}

	execution := &domain.Execution{
		ID:             uuid.New(),
		OrganizationID: input.OrganizationID,
		RequestedBy:    input.RequestedBy,
		NodeID:         input.NodeID,
		WorkflowID:     input.WorkflowID,
		Language:       input.Language,
		Code:           input.Code,
		Stdin:          input.Stdin,
		Args:           input.Args,
		EnvVars:        input.EnvVars,
		Status:         domain.ExecutionStatusPending,
		Resources:      resources,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	if err := execution.Validate(); err != nil {
		return nil, err
	}

	if err := s.execRepo.Create(ctx, execution); err != nil {
		return nil, fmt.Errorf("failed to create execution: %w", err)
	}

	log.Printf("Execution created: %s (lang=%s, org=%s)", execution.ID, execution.Language, execution.OrganizationID)
	return execution, nil
}

func (s *executionService) GetExecution(ctx context.Context, id uuid.UUID) (*domain.Execution, error) {
	return s.execRepo.FindByID(ctx, id)
}

func (s *executionService) ListExecutions(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return s.execRepo.FindByOrganization(ctx, orgID, limit, offset)
}

func (s *executionService) CancelExecution(ctx context.Context, id uuid.UUID) error {
	execution, err := s.execRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}

	if execution.IsTerminal() {
		return domain.ErrExecutionAlreadyDone
	}

	now := time.Now().UTC()
	execution.Status = domain.ExecutionStatusCancelled
	execution.CompletedAt = &now
	execution.UpdatedAt = now
	if execution.StartedAt != nil {
		dur := now.Sub(*execution.StartedAt).Milliseconds()
		execution.DurationMS = &dur
	}

	if err := s.execRepo.Update(ctx, execution); err != nil {
		return fmt.Errorf("failed to cancel execution: %w", err)
	}

	// If a VM is assigned, release it
	if execution.VMID != nil {
		if err := s.vmUC.ReleaseVM(ctx, *execution.VMID); err != nil {
			log.Printf("Warning: failed to release VM %s after cancel: %v", execution.VMID, err)
		}
	}

	log.Printf("Execution cancelled: %s", id)
	return nil
}

func (s *executionService) GetExecutionMetrics(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error) {
	return s.metricsRepo.FindByExecution(ctx, executionID)
}

func (s *executionService) ExecuteSync(ctx context.Context, input CreateExecutionInput) (*domain.Execution, error) {
	execution, err := s.CreateExecution(ctx, input)
	if err != nil {
		return nil, err
	}

	if err := s.executeOne(ctx, execution); err != nil {
		// Reload to get the final state (may have been marked failed/timeout)
		reloaded, reloadErr := s.execRepo.FindByID(ctx, execution.ID)
		if reloadErr != nil {
			return nil, fmt.Errorf("execution failed and could not reload: %w", err)
		}
		return reloaded, nil
	}

	// Reload to get the final state
	return s.execRepo.FindByID(ctx, execution.ID)
}

func (s *executionService) ProcessPending(ctx context.Context, limit int) (int, error) {
	pending, err := s.execRepo.FindPending(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("failed to find pending executions: %w", err)
	}

	processed := 0
	for _, exec := range pending {
		if err := s.executeOne(ctx, &exec); err != nil {
			log.Printf("Failed to process execution %s: %v", exec.ID, err)
			continue
		}
		processed++
	}

	return processed, nil
}

func (s *executionService) executeOne(ctx context.Context, execution *domain.Execution) error {
	// Acquire a VM for this execution
	vm, err := s.vmUC.AcquireVM(ctx, execution.Language, execution.Resources)
	if err != nil {
		log.Printf("Failed to acquire VM for execution %s: %v", execution.ID, err)
		return err
	}

	// Mark execution as running
	execution.MarkRunning(vm.ID)
	execution.UpdatedAt = time.Now().UTC()
	if err := s.execRepo.Update(ctx, execution); err != nil {
		// Release the VM if we can't update
		_ = s.vmUC.ReleaseVM(ctx, vm.ID)
		return fmt.Errorf("failed to update execution status: %w", err)
	}

	// Create a timeout context based on resource limits
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(execution.Resources.TimeoutSec)*time.Second)
	defer cancel()

	// Run the execution
	result, runErr := s.runner.Run(execCtx, vm, execution)

	// Collect resource metrics from the VM before releasing it
	var vmMetrics *VMMetrics
	if s.vmProvider != nil && vm.IPAddress != "" {
		metricsCtx, metricsCancel := context.WithTimeout(ctx, 10*time.Second)
		collected, err := s.vmProvider.CollectMetrics(metricsCtx, vm.IPAddress)
		metricsCancel()
		if err != nil {
			log.Printf("Warning: failed to collect metrics for VM %s: %v", vm.ID, err)
		} else {
			vmMetrics = collected
		}
	}

	// Release the VM back to pool
	if releaseErr := s.vmUC.ReleaseVM(ctx, vm.ID); releaseErr != nil {
		log.Printf("Warning: failed to release VM %s: %v", vm.ID, releaseErr)
	}

	// Update execution based on result
	if runErr != nil {
		if ctx.Err() != nil {
			execution.MarkTimeout("", "")
		} else {
			execution.MarkFailed(runErr.Error(), -1, "", "")
		}
	} else if result.ExitCode != 0 {
		execution.MarkFailed(
			fmt.Sprintf("process exited with code %d", result.ExitCode),
			result.ExitCode, result.Stdout, result.Stderr,
		)
	} else {
		execution.MarkCompleted(result.ExitCode, result.Stdout, result.Stderr)
	}

	execution.UpdatedAt = time.Now().UTC()
	if err := s.execRepo.Update(ctx, execution); err != nil {
		return fmt.Errorf("failed to update execution result: %w", err)
	}

	// Record metrics if we have a result
	if result != nil {
		metrics := &domain.ExecutionMetrics{
			ID:            uuid.New(),
			ExecutionID:   execution.ID,
			VMID:          vm.ID,
			CPUTimeMS:     result.CPUTimeMS,
			MemoryPeakMB:  result.MemoryPeakMB,
			MemoryAvgMB:   result.MemoryAvgMB,
			IOReadBytes:   result.IOReadBytes,
			IOWriteBytes:  result.IOWriteBytes,
			CompileTimeMS: result.CompileTimeMS,
			ExecTimeMS:    result.ExecTimeMS,
			CollectedAt:   time.Now().UTC(),
		}

		// Merge in VM-collected resource metrics (these are more accurate
		// than the RunResult fields which may be zero from the provider)
		if vmMetrics != nil {
			if vmMetrics.CPUTimeMS > 0 {
				metrics.CPUTimeMS = vmMetrics.CPUTimeMS
			}
			if vmMetrics.MemoryPeakMB > 0 {
				metrics.MemoryPeakMB = vmMetrics.MemoryPeakMB
			}
			if vmMetrics.MemoryAvgMB > 0 {
				metrics.MemoryAvgMB = vmMetrics.MemoryAvgMB
			}
			if vmMetrics.IOReadBytes > 0 {
				metrics.IOReadBytes = vmMetrics.IOReadBytes
			}
			if vmMetrics.IOWriteBytes > 0 {
				metrics.IOWriteBytes = vmMetrics.IOWriteBytes
			}
			metrics.NetBytesIn = vmMetrics.NetBytesIn
			metrics.NetBytesOut = vmMetrics.NetBytesOut
		}

		if execution.DurationMS != nil {
			metrics.TotalTimeMS = *execution.DurationMS
		}
		if vm.BootTimeMS != nil {
			metrics.BootTimeMS = *vm.BootTimeMS
		}

		if err := s.metricsRepo.Create(ctx, metrics); err != nil {
			log.Printf("Warning: failed to record metrics for execution %s: %v", execution.ID, err)
		}
	}

	log.Printf("Execution completed: %s (status=%s)", execution.ID, execution.Status)
	return nil
}
