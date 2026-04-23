// Package firecracker — per-VM checkpoint scheduler.
//
// Unlike the usecase-layer CheckpointScheduler (which polls the VM
// table and checkpoints rows whose last_checkpoint_at has aged past the
// configured interval), this infrastructure-layer scheduler spawns one
// goroutine PER long-running VM and wakes up every
// CheckpointIntervalMinutes to call Provider.CreateSnapshot directly.
//
// Lifecycle:
//   - Boot() registers a new VM with Register(); the scheduler
//     launches its goroutine.
//   - Terminate() calls Deregister(); the goroutine exits and any
//     local snapshot files it produced are left to the snapshot
//     repository / lifecycle manager.
//
// Pruning: the scheduler keeps MaxCheckpointsPerVM snapshots on disk
// for each VM. Older snapshot file pairs are unlinked via the
// SnapshotBackend provided at construction so the host doesn't fill
// with stale memory dumps.
//
// Testing: both the snapshot call and the pause/resume cycle are
// abstracted behind SnapshotBackend so unit tests can verify the
// cadence, pruning, and cancellation semantics without a real
// Firecracker process.
package firecracker

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Defaults for the scheduler. These match the §9.3 spec.
const (
	DefaultCheckpointIntervalMinutes = 15
	DefaultMaxCheckpointsPerVM       = 5
)

// SnapshotRecord is the bookkeeping row for each snapshot the
// scheduler produces. Held in memory per-VM so pruning can unlink
// older files without a DB round-trip. The snapshot service (usecase
// layer) remains the source of truth for cross-process snapshot
// state; this record only drives the host-local prune cadence.
type SnapshotRecord struct {
	ID             uuid.UUID
	MemoryFilePath string
	StateFilePath  string
	SizeBytes      int64
	CreatedAt      time.Time
}

// SnapshotBackend is the narrow surface the scheduler needs. The real
// implementation wraps a *Provider; tests supply a fake.
type SnapshotBackend interface {
	// Pause halts the guest so the snapshot is crash-consistent.
	Pause(ctx context.Context, socketPath string) error
	// Resume wakes the guest after Pause.
	Resume(ctx context.Context, socketPath string) error
	// CreateSnapshot writes the memory + state files under the VM id.
	CreateSnapshot(ctx context.Context, socketPath string, snapshotID uuid.UUID) (*SnapshotFiles, error)
	// DeleteSnapshotFiles unlinks a snapshot pair for pruning.
	DeleteSnapshotFiles(memPath, statePath string) error
}

// SnapshotFiles mirrors the relevant subset of usecase.SnapshotResult
// — kept local so this package doesn't depend on usecase and can
// be imported by tests without pulling the whole dep graph.
type SnapshotFiles struct {
	MemoryFilePath string
	StateFilePath  string
	SizeBytes      int64
}

// VMRegistration describes a VM the scheduler is watching.
type VMRegistration struct {
	VMID                      uuid.UUID
	SocketPath                string
	CheckpointIntervalMinutes int // 0 → DefaultCheckpointIntervalMinutes
	MaxCheckpointsPerVM       int // 0 → DefaultMaxCheckpointsPerVM
}

// CheckpointScheduler runs one goroutine per registered VM.
type CheckpointScheduler struct {
	backend SnapshotBackend
	log     *log.Logger

	mu      sync.Mutex
	tracked map[uuid.UUID]*vmState
	closed  bool
}

type vmState struct {
	reg        VMRegistration
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	snapshots  []SnapshotRecord
}

// NewCheckpointScheduler constructs the scheduler. backend must be
// non-nil; logger defaults to the std logger when nil.
func NewCheckpointScheduler(backend SnapshotBackend, logger *log.Logger) *CheckpointScheduler {
	if logger == nil {
		logger = log.Default()
	}
	return &CheckpointScheduler{
		backend: backend,
		log:     logger,
		tracked: make(map[uuid.UUID]*vmState),
	}
}

// Register starts the per-VM checkpoint goroutine. Idempotent: a
// second call for the same VMID is a no-op. Calls after Close are
// rejected silently.
func (s *CheckpointScheduler) Register(ctx context.Context, reg VMRegistration) {
	if s == nil || s.backend == nil {
		return
	}
	if reg.VMID == uuid.Nil || reg.SocketPath == "" {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, exists := s.tracked[reg.VMID]; exists {
		s.mu.Unlock()
		return
	}
	normalized := reg
	if normalized.CheckpointIntervalMinutes <= 0 {
		normalized.CheckpointIntervalMinutes = DefaultCheckpointIntervalMinutes
	}
	if normalized.MaxCheckpointsPerVM <= 0 {
		normalized.MaxCheckpointsPerVM = DefaultMaxCheckpointsPerVM
	}
	state := &vmState{reg: normalized, done: make(chan struct{})}
	childCtx, cancel := context.WithCancel(ctx)
	state.cancel = cancel
	s.tracked[reg.VMID] = state
	s.mu.Unlock()

	go s.run(childCtx, state)
}

// Deregister stops the goroutine for a VM and waits for it to exit.
// Called from Provider.Terminate.
func (s *CheckpointScheduler) Deregister(vmID uuid.UUID) {
	if s == nil {
		return
	}
	s.mu.Lock()
	state, ok := s.tracked[vmID]
	if ok {
		delete(s.tracked, vmID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	state.cancel()
	<-state.done
}

// Close stops every goroutine and marks the scheduler closed. Safe to
// call from a shutdown hook.
func (s *CheckpointScheduler) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	states := make([]*vmState, 0, len(s.tracked))
	for _, state := range s.tracked {
		states = append(states, state)
	}
	s.tracked = make(map[uuid.UUID]*vmState)
	s.mu.Unlock()
	for _, state := range states {
		state.cancel()
	}
	for _, state := range states {
		<-state.done
	}
}

// Tracking counts for observability / tests.
func (s *CheckpointScheduler) Tracking() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tracked)
}

// SnapshotsFor returns a copy of the snapshot history recorded for a
// VM. Used by tests to assert on the prune invariant.
func (s *CheckpointScheduler) SnapshotsFor(vmID uuid.UUID) []SnapshotRecord {
	s.mu.Lock()
	state, ok := s.tracked[vmID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	out := make([]SnapshotRecord, len(state.snapshots))
	copy(out, state.snapshots)
	return out
}

// run is the per-VM goroutine.
func (s *CheckpointScheduler) run(ctx context.Context, state *vmState) {
	defer close(state.done)
	interval := time.Duration(state.reg.CheckpointIntervalMinutes) * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	s.log.Printf("[FC-CHECKPOINT] VM %s registered (interval=%s, max=%d)",
		state.reg.VMID, interval, state.reg.MaxCheckpointsPerVM)
	for {
		select {
		case <-ctx.Done():
			s.log.Printf("[FC-CHECKPOINT] VM %s deregistered", state.reg.VMID)
			return
		case <-t.C:
			if err := s.checkpointOnce(ctx, state); err != nil {
				s.log.Printf("[FC-CHECKPOINT] VM %s tick: %v", state.reg.VMID, err)
			}
		}
	}
}

// checkpointOnce performs one pause-snapshot-resume cycle and prunes.
// Always resumes on exit so a failed snapshot doesn't leave the VM
// stuck paused.
func (s *CheckpointScheduler) checkpointOnce(ctx context.Context, state *vmState) error {
	if err := s.backend.Pause(ctx, state.reg.SocketPath); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	defer func() {
		if err := s.backend.Resume(ctx, state.reg.SocketPath); err != nil {
			s.log.Printf("[FC-CHECKPOINT] resume VM %s failed: %v", state.reg.VMID, err)
		}
	}()

	snapshotID := uuid.New()
	files, err := s.backend.CreateSnapshot(ctx, state.reg.SocketPath, snapshotID)
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	record := SnapshotRecord{
		ID:             snapshotID,
		MemoryFilePath: files.MemoryFilePath,
		StateFilePath:  files.StateFilePath,
		SizeBytes:      files.SizeBytes,
		CreatedAt:      time.Now().UTC(),
	}

	state.mu.Lock()
	state.snapshots = append(state.snapshots, record)
	sort.Slice(state.snapshots, func(i, j int) bool {
		return state.snapshots[i].CreatedAt.Before(state.snapshots[j].CreatedAt)
	})
	var toPrune []SnapshotRecord
	if len(state.snapshots) > state.reg.MaxCheckpointsPerVM {
		excess := len(state.snapshots) - state.reg.MaxCheckpointsPerVM
		toPrune = append(toPrune, state.snapshots[:excess]...)
		state.snapshots = state.snapshots[excess:]
	}
	state.mu.Unlock()

	for _, rec := range toPrune {
		if err := s.backend.DeleteSnapshotFiles(rec.MemoryFilePath, rec.StateFilePath); err != nil {
			s.log.Printf("[FC-CHECKPOINT] prune %s failed: %v", rec.ID, err)
		}
	}
	s.log.Printf("[FC-CHECKPOINT] VM %s → snapshot %s (%d bytes, kept=%d pruned=%d)",
		state.reg.VMID, snapshotID, files.SizeBytes, state.reg.MaxCheckpointsPerVM, len(toPrune))
	return nil
}

// ProviderSnapshotBackend adapts the firecracker Provider into the
// SnapshotBackend surface the scheduler needs. Keeping the adapter
// here makes the scheduler's test surface stay trivially mockable
// without anyone having to construct a real Provider.
type ProviderSnapshotBackend struct {
	P *Provider
}

func (b *ProviderSnapshotBackend) Pause(ctx context.Context, socketPath string) error {
	return b.P.Pause(ctx, socketPath)
}

func (b *ProviderSnapshotBackend) Resume(ctx context.Context, socketPath string) error {
	return b.P.Resume(ctx, socketPath)
}

func (b *ProviderSnapshotBackend) CreateSnapshot(ctx context.Context, socketPath string, snapshotID uuid.UUID) (*SnapshotFiles, error) {
	res, err := b.P.CreateSnapshot(ctx, socketPath, snapshotID)
	if err != nil {
		return nil, err
	}
	return &SnapshotFiles{
		MemoryFilePath: res.MemoryFilePath,
		StateFilePath:  res.StateFilePath,
		SizeBytes:      res.SizeBytes,
	}, nil
}

func (b *ProviderSnapshotBackend) DeleteSnapshotFiles(memPath, statePath string) error {
	return b.P.DeleteSnapshotFiles(memPath, statePath)
}
