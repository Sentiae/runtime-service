package vmcomm

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	pb "github.com/sentiae/runtime-service/internal/infrastructure/firecracker/agentpb"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Runner implements usecase.ExecutionRunner by sending code to the guest
// agent over vsock and collecting the result.
type Runner struct {
	fcListener   *FirecrackerListener
	readyTimeout time.Duration
}

// NewRunner creates a Runner that connects to guest agents via Firecracker vsock UDS.
func NewRunner(readyTimeout time.Duration) *Runner {
	if readyTimeout == 0 {
		readyTimeout = 30 * time.Second
	}
	return &Runner{
		fcListener:   NewFirecrackerListener(),
		readyTimeout: readyTimeout,
	}
}

// Run implements usecase.ExecutionRunner. It sends the execution's code to
// the guest agent in the given VM and waits for the result.
func (r *Runner) Run(ctx context.Context, vm *domain.MicroVM, execution *domain.Execution) (*usecase.RunResult, error) {
	client := r.fcListener.GetClient(vm.ID)
	if client == nil {
		// Agent not connected yet — connect to the VM's vsock UDS
		logger.FromContext(ctx).Debug("vmcomm: connecting to guest agent",
			"vm_id", vm.ID, "socket_path", vm.SocketPath)
		connectCtx, cancel := context.WithTimeout(ctx, r.readyTimeout)
		defer cancel()

		var err error
		client, err = r.fcListener.ConnectToVM(connectCtx, vm.ID, vm.SocketPath)
		if err != nil {
			return nil, fmt.Errorf("connect to agent on VM %s: %w", vm.ID, err)
		}

		// Wait for Ready message
		info, err := client.WaitReady(connectCtx)
		if err != nil {
			r.fcListener.RemoveClient(vm.ID)
			return nil, fmt.Errorf("agent not ready on VM %s: %w", vm.ID, err)
		}
		logger.FromContext(ctx).Info("vmcomm: guest agent ready",
			"vm_id", vm.ID, "agent_version", info.AgentVersion, "supported_languages", info.SupportedLanguages)
	}

	taskID := execution.ID.String()

	// Build the ExecuteTask message.
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

	logger.FromContext(ctx).Info("vmcomm: sending task to VM",
		"task_id", taskID, "vm_id", vm.ID, "language", execution.Language)

	result, err := client.ExecuteTask(ctx, task, nil)
	if err != nil {
		return nil, fmt.Errorf("execute task via vsock: %w", err)
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

// ShutdownVM sends a graceful shutdown to the guest agent and cleans up.
func (r *Runner) ShutdownVM(ctx context.Context, vmID uuid.UUID) {
	client := r.fcListener.GetClient(vmID)
	if client != nil {
		if err := client.Shutdown(); err != nil {
			logger.FromContext(ctx).Warn("vmcomm: guest agent shutdown failed", "vm_id", vmID, "err", err)
		}
	}
	r.fcListener.RemoveClient(vmID)
}

// Close cleans up all connections.
func (r *Runner) Close() error {
	return r.fcListener.Close()
}
