// Package usecase — CheckpointScheduler runs in a single goroutine and
// drives the automatic checkpoint feature for long-running VMs.
//
// On every tick it asks the VM-instance repository which VMs are due
// (CheckpointIntervalSeconds > 0 and last_checkpoint_at + interval <=
// NOW()), pauses each, asks the VM provider to write Firecracker memory +
// state files, persists a Snapshot row of kind "checkpoint", resumes the
// VM, and emits a runtime.checkpoint.created event.
//
// On startup (RestoreLatest) it inspects each VM that is in "running"
// desired-state but has no live socket and tries to restore from its
// most recent checkpoint, emitting runtime.checkpoint.restored on
// success.
package usecase

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// DefaultCheckpointInterval is the cadence at which the scheduler wakes
// up to look for due checkpoints. The actual per-VM interval comes from
// VMInstance.CheckpointIntervalSeconds; this is just the tick rate.
const DefaultCheckpointInterval = 30 * time.Second

// CheckpointScheduler drives automatic snapshotting of long-running VMs.
type CheckpointScheduler struct {
	vmRepo         repository.VMInstanceRepository
	snapshotRepo   repository.SnapshotRepository
	vmProvider     VMProvider
	eventPublisher EventPublisher

	tick     time.Duration
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewCheckpointScheduler wires the scheduler with the data plane it
// needs. tick is how often it polls for due VMs; pass 0 to use
// DefaultCheckpointInterval. The scheduler does not start until Start
// is called.
func NewCheckpointScheduler(
	vmRepo repository.VMInstanceRepository,
	snapshotRepo repository.SnapshotRepository,
	vmProvider VMProvider,
	eventPublisher EventPublisher,
	tick time.Duration,
) *CheckpointScheduler {
	if tick <= 0 {
		tick = DefaultCheckpointInterval
	}
	if eventPublisher == nil {
		eventPublisher = noopEventPublisher{}
	}
	return &CheckpointScheduler{
		vmRepo:         vmRepo,
		snapshotRepo:   snapshotRepo,
		vmProvider:     vmProvider,
		eventPublisher: eventPublisher,
		tick:           tick,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

// Start launches the background loop. Safe to call only once. The loop
// exits when ctx is cancelled or Stop() is invoked.
func (s *CheckpointScheduler) Start(ctx context.Context) {
	go s.run(ctx)
	log.Printf("[CHECKPOINT] Scheduler started (tick=%s)", s.tick)
}

// Stop signals the background loop to exit and waits for it to finish.
func (s *CheckpointScheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	<-s.doneCh
}

func (s *CheckpointScheduler) run(ctx context.Context) {
	defer close(s.doneCh)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			if err := s.runOnce(ctx); err != nil {
				log.Printf("[CHECKPOINT] tick error: %v", err)
			}
		}
	}
}

// runOnce does a single pass: list due VMs, snapshot each. Errors on
// individual VMs are logged and do not abort the pass — one bad VM
// must not block the others.
func (s *CheckpointScheduler) runOnce(ctx context.Context) error {
	vms, err := s.vmRepo.FindCheckpointable(ctx)
	if err != nil {
		return fmt.Errorf("list checkpointable: %w", err)
	}
	if len(vms) == 0 {
		return nil
	}
	log.Printf("[CHECKPOINT] %d VM(s) due for checkpoint", len(vms))
	for i := range vms {
		vm := &vms[i]
		if err := s.checkpointOne(ctx, vm); err != nil {
			log.Printf("[CHECKPOINT] VM %s failed: %v", vm.ID, err)
		}
	}
	return nil
}

// checkpointOne pauses the VM, snapshots it via the VM provider,
// records the metadata, resumes the VM, and emits an event. We always
// resume on the way out, even if the snapshot fails, so a transient
// error does not leave the VM stuck in paused state.
func (s *CheckpointScheduler) checkpointOne(ctx context.Context, vm *domain.VMInstance) error {
	if vm.SocketPath == "" {
		return fmt.Errorf("VM %s has no socket path", vm.ID)
	}
	if err := s.vmProvider.Pause(ctx, vm.SocketPath); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() {
		if err := s.vmProvider.Resume(ctx, vm.SocketPath); err != nil {
			log.Printf("[CHECKPOINT] resume of VM %s after checkpoint failed: %v", vm.ID, err)
		}
	}()

	snapshotID := uuid.New()
	start := time.Now()
	result, err := s.vmProvider.CreateSnapshot(ctx, vm.SocketPath, snapshotID)
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	createMS := time.Since(start).Milliseconds()
	now := time.Now().UTC()

	row := &domain.Snapshot{
		ID:             snapshotID,
		VMID:           vm.ID,
		ExecutionID:    vm.ExecutionID,
		Language:       vm.Language,
		MemoryFilePath: result.MemoryFilePath,
		StateFilePath:  result.StateFilePath,
		SizeBytes:      result.SizeBytes,
		VCPU:           vm.VCPU,
		MemoryMB:       vm.MemoryMB,
		Description:    fmt.Sprintf("auto checkpoint @ %s", now.Format(time.RFC3339)),
		Kind:           domain.SnapshotKindCheckpoint,
		RestoreTimeMS:  &createMS,
		CreatedAt:      now,
	}
	if err := s.snapshotRepo.Create(ctx, row); err != nil {
		// Files exist on disk but the row didn't persist — clean up
		// to prevent unbounded disk growth.
		_ = s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath)
		return fmt.Errorf("persist snapshot row: %w", err)
	}

	vm.LastCheckpointAt = &now
	vm.UpdatedAt = now
	if err := s.vmRepo.Update(ctx, vm); err != nil {
		// Non-fatal: row exists, files exist, only the VM tracking
		// timestamp failed. Log and move on.
		log.Printf("[CHECKPOINT] failed to update last_checkpoint_at for VM %s: %v", vm.ID, err)
	}

	_ = s.eventPublisher.Publish(ctx, "runtime.checkpoint.created", vm.ID.String(), map[string]any{
		"vm_id":            vm.ID.String(),
		"snapshot_id":      snapshotID.String(),
		"size_bytes":       result.SizeBytes,
		"interval_seconds": vm.CheckpointIntervalSeconds,
		"created_at":       now.Format(time.RFC3339),
	})
	log.Printf("[CHECKPOINT] VM %s -> snapshot %s (%d bytes, %dms)", vm.ID, snapshotID, result.SizeBytes, createMS)
	return nil
}

// RestoreLatest finds the most recent checkpoint for each VM that is in
// running desired-state but lacks a live PID/socket and asks the VM
// provider to restore it. Intended to be called once at process startup
// after the DI container is built but before HTTP traffic is accepted.
//
// The function returns the number of VMs successfully restored. Errors
// on individual VMs are logged and do not abort the loop.
func (s *CheckpointScheduler) RestoreLatest(ctx context.Context) (int, error) {
	state := domain.VMInstanceStateRunning
	vms, err := s.vmRepo.FindAll(ctx, &state)
	if err != nil {
		return 0, fmt.Errorf("list vms: %w", err)
	}
	restored := 0
	for i := range vms {
		vm := &vms[i]
		// A VM with a live PID does not need restoring. We treat the
		// presence of PID + socket as evidence the runtime is still up.
		if vm.PID != nil && vm.SocketPath != "" {
			continue
		}
		snap, err := s.snapshotRepo.FindLatestCheckpointByVM(ctx, vm.ID)
		if err != nil {
			continue
		}
		start := time.Now()
		// Boot a fresh Firecracker process; provider.RestoreSnapshot
		// loads the saved memory + state and resumes the guest.
		bootResult, err := s.vmProvider.Boot(ctx, VMBootConfig{
			VMID:     vm.ID,
			Language: vm.Language,
			VCPU:     vm.VCPU,
			MemoryMB: vm.MemoryMB,
			NetworkPolicy: domain.NetworkPolicy{
				Mode: domain.NetworkPolicyAllowHost,
			},
		})
		if err != nil {
			log.Printf("[CHECKPOINT] boot for restore (VM %s) failed: %v", vm.ID, err)
			continue
		}
		if err := s.vmProvider.RestoreSnapshot(ctx, bootResult.SocketPath, snap.MemoryFilePath, snap.StateFilePath); err != nil {
			log.Printf("[CHECKPOINT] restore (VM %s) failed: %v", vm.ID, err)
			_ = s.vmProvider.Terminate(ctx, bootResult.SocketPath, bootResult.PID)
			continue
		}
		restoreMS := time.Since(start).Milliseconds()

		vm.PID = &bootResult.PID
		vm.SocketPath = bootResult.SocketPath
		vm.IPAddress = bootResult.IPAddress
		vm.UpdatedAt = time.Now().UTC()
		if err := s.vmRepo.Update(ctx, vm); err != nil {
			log.Printf("[CHECKPOINT] update VM %s after restore failed: %v", vm.ID, err)
		}

		_ = s.eventPublisher.Publish(ctx, "runtime.checkpoint.restored", vm.ID.String(), map[string]any{
			"vm_id":           vm.ID.String(),
			"snapshot_id":     snap.ID.String(),
			"restore_time_ms": restoreMS,
		})
		restored++
		log.Printf("[CHECKPOINT] restored VM %s from snapshot %s in %dms", vm.ID, snap.ID, restoreMS)
	}
	return restored, nil
}

// CheckpointNow snapshots the VM immediately and leaves it paused.
// (§9.3) This is the path an agent takes when it sends a "pause"
// signal to its sandbox VM: the snapshot captures the full state for
// later restore, and the CPU stays stopped so no work happens while
// the operator inspects the machine. Unlike checkpointOne, this does
// NOT resume the VM afterwards — callers must issue Resume explicitly
// (or restore from the snapshot).
//
// The fast-path bypasses the interval check — an agent can always
// pause regardless of the scheduler cadence.
func (s *CheckpointScheduler) CheckpointNow(ctx context.Context, vmID uuid.UUID) (*domain.Snapshot, error) {
	vm, err := s.vmRepo.FindByID(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("load vm: %w", err)
	}
	if vm.SocketPath == "" {
		return nil, fmt.Errorf("VM %s has no socket path", vm.ID)
	}
	if err := s.vmProvider.Pause(ctx, vm.SocketPath); err != nil {
		return nil, fmt.Errorf("pause: %w", err)
	}

	snapshotID := uuid.New()
	start := time.Now()
	result, err := s.vmProvider.CreateSnapshot(ctx, vm.SocketPath, snapshotID)
	if err != nil {
		// Resume so the agent-requested pause doesn't leave the VM
		// stuck if the snapshot fails. The caller sees the error and
		// can retry.
		_ = s.vmProvider.Resume(ctx, vm.SocketPath)
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	createMS := time.Since(start).Milliseconds()
	now := time.Now().UTC()

	row := &domain.Snapshot{
		ID:             snapshotID,
		VMID:           vm.ID,
		ExecutionID:    vm.ExecutionID,
		Language:       vm.Language,
		MemoryFilePath: result.MemoryFilePath,
		StateFilePath:  result.StateFilePath,
		SizeBytes:      result.SizeBytes,
		VCPU:           vm.VCPU,
		MemoryMB:       vm.MemoryMB,
		Description:    fmt.Sprintf("agent pause @ %s", now.Format(time.RFC3339)),
		Kind:           domain.SnapshotKindCheckpoint,
		RestoreTimeMS:  &createMS,
		CreatedAt:      now,
	}
	if err := s.snapshotRepo.Create(ctx, row); err != nil {
		_ = s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath)
		_ = s.vmProvider.Resume(ctx, vm.SocketPath)
		return nil, fmt.Errorf("persist snapshot row: %w", err)
	}

	vm.LastCheckpointAt = &now
	vm.UpdatedAt = now
	if err := s.vmRepo.Update(ctx, vm); err != nil {
		log.Printf("[CHECKPOINT] last_checkpoint_at update for VM %s failed: %v", vm.ID, err)
	}

	_ = s.eventPublisher.Publish(ctx, "runtime.checkpoint.paused", vm.ID.String(), map[string]any{
		"vm_id":       vm.ID.String(),
		"snapshot_id": snapshotID.String(),
		"size_bytes":  result.SizeBytes,
		"reason":      "agent_pause",
		"created_at":  now.Format(time.RFC3339),
	})
	log.Printf("[CHECKPOINT] agent-paused VM %s -> snapshot %s (%d bytes, %dms)", vm.ID, snapshotID, result.SizeBytes, createMS)
	return row, nil
}

// noopEventPublisher is the local fallback used when no publisher is
// wired (e.g. in tests). It satisfies EventPublisher with empty methods.
type noopEventPublisher struct{}

func (noopEventPublisher) Publish(_ context.Context, _, _ string, _ any) error { return nil }
func (noopEventPublisher) Close() error                                        { return nil }
