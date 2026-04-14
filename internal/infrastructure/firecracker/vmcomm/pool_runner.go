package vmcomm

import (
	"context"
	"fmt"
	"log"

	"github.com/sentiae/runtime-service/internal/domain"
	pb "github.com/sentiae/runtime-service/internal/infrastructure/firecracker/agentpb"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// PoolRunner implements usecase.ExecutionRunner using the warm VM pool.
// It acquires a VM from the pool, sends the task, collects results, and
// returns the VM to the pool for reuse.
type PoolRunner struct {
	pool *Pool
}

// NewPoolRunner creates a runner backed by the warm pool.
func NewPoolRunner(pool *Pool) *PoolRunner {
	return &PoolRunner{pool: pool}
}

// Run implements usecase.ExecutionRunner.
func (r *PoolRunner) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	// Acquire a pooled VM (reuses existing or boots new)
	pooledVM, err := r.pool.AcquireVM(ctx, execution.Language, execution.Resources)
	if err != nil {
		return nil, fmt.Errorf("acquire VM from pool: %w", err)
	}

	// Always return to pool after execution
	defer r.pool.ReleaseVM(pooledVM.VM.ID)

	taskID := execution.ID.String()

	envVars := make(map[string]string)
	if execution.EnvVars != nil {
		for k, v := range execution.EnvVars {
			if s, ok := v.(string); ok {
				envVars[k] = s
			}
		}
	}

	task := &pb.ExecuteTask{
		TaskId:        taskID,
		Language:      string(execution.Language),
		Code:          execution.Code,
		Stdin:         execution.Stdin,
		TimeoutSecs:   int32(execution.Resources.TimeoutSec),
		EnvVars:       envVars,
		MemoryLimitMb: int64(execution.Resources.MemoryMB),
	}

	log.Printf("pool-runner: sending task %s to VM %s (lang=%s)", taskID, pooledVM.VM.ID, execution.Language)

	result, err := pooledVM.Client.ExecuteTask(ctx, task, nil)
	if err != nil {
		// Connection might be broken (broken pipe). Remove from pool
		// and boot a fresh VM.
		log.Printf("pool-runner: task failed on warm VM %s, retrying with fresh VM: %v", pooledVM.VM.ID, err)
		r.pool.ReleaseVM(pooledVM.VM.ID) // release first so defer doesn't double-release
		r.pool.removeVM(pooledVM.VM.ID)  // remove from pool entirely

		// Boot a fresh VM and retry
		freshVM, bootErr := r.pool.bootAndConnect(ctx, execution.Language, execution.Resources)
		if bootErr != nil {
			return nil, fmt.Errorf("retry with fresh VM failed: %w", bootErr)
		}
		defer r.pool.ReleaseVM(freshVM.VM.ID)

		result, err = freshVM.Client.ExecuteTask(ctx, task, nil)
		if err != nil {
			return nil, fmt.Errorf("execute task on fresh VM: %w", err)
		}
	}

	if result.Error != "" {
		return nil, fmt.Errorf("agent error: %s", result.Error)
	}

	return &usecase.RunResult{
		ExitCode:     int(result.ExitCode),
		Stdout:       string(result.Stdout),
		Stderr:       string(result.Stderr),
		CPUTimeMS:    result.CPUTimeMS,
		MemoryPeakMB: float64(result.MemoryPeakKB) / 1024.0,
		ExecTimeMS:   result.DurationMS,
	}, nil
}
