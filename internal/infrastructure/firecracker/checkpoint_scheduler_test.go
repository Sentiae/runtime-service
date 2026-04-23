package firecracker

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- fake backend ---------------------------------------------------

type fakeSnapshotBackend struct {
	mu         sync.Mutex
	paused     int32
	resumed    int32
	created    int32
	deleted    int32
	deletedSet map[string]struct{}
	createErr  error
	dir        string
}

func newFakeBackend() *fakeSnapshotBackend {
	return &fakeSnapshotBackend{
		deletedSet: make(map[string]struct{}),
		dir:        "/tmp/fake-fc-snapshots",
	}
}

func (f *fakeSnapshotBackend) Pause(_ context.Context, _ string) error {
	atomic.AddInt32(&f.paused, 1)
	return nil
}

func (f *fakeSnapshotBackend) Resume(_ context.Context, _ string) error {
	atomic.AddInt32(&f.resumed, 1)
	return nil
}

func (f *fakeSnapshotBackend) CreateSnapshot(_ context.Context, _ string, id uuid.UUID) (*SnapshotFiles, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	atomic.AddInt32(&f.created, 1)
	return &SnapshotFiles{
		MemoryFilePath: filepath.Join(f.dir, id.String()+".mem"),
		StateFilePath:  filepath.Join(f.dir, id.String()+".state"),
		SizeBytes:      1024,
	}, nil
}

func (f *fakeSnapshotBackend) DeleteSnapshotFiles(memPath, statePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.deleted, 1)
	f.deletedSet[memPath] = struct{}{}
	f.deletedSet[statePath] = struct{}{}
	return nil
}

// --- tests ----------------------------------------------------------

func silentLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

func TestCheckpointScheduler_RegisterDeregister(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())

	vmID := uuid.New()
	sched.Register(context.Background(), VMRegistration{
		VMID:                      vmID,
		SocketPath:                "/tmp/sock",
		CheckpointIntervalMinutes: 1,
		MaxCheckpointsPerVM:       5,
	})
	if sched.Tracking() != 1 {
		t.Errorf("tracking should be 1, got %d", sched.Tracking())
	}

	sched.Deregister(vmID)
	if sched.Tracking() != 0 {
		t.Errorf("tracking should be 0 after deregister, got %d", sched.Tracking())
	}
}

func TestCheckpointScheduler_RegisterIdempotent(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())
	defer sched.Close()

	vmID := uuid.New()
	reg := VMRegistration{VMID: vmID, SocketPath: "/tmp/sock", CheckpointIntervalMinutes: 1}
	sched.Register(context.Background(), reg)
	sched.Register(context.Background(), reg)
	if sched.Tracking() != 1 {
		t.Errorf("duplicate register should be idempotent, got %d", sched.Tracking())
	}
}

func TestCheckpointScheduler_ProducesSnapshotsAndPrunes(t *testing.T) {
	backend := newFakeBackend()
	sched := &CheckpointScheduler{
		backend: backend,
		log:     silentLogger(),
		tracked: make(map[uuid.UUID]*vmState),
	}
	vmID := uuid.New()
	state := &vmState{
		reg: VMRegistration{
			VMID:                      vmID,
			SocketPath:                "/tmp/sock",
			CheckpointIntervalMinutes: 15,
			MaxCheckpointsPerVM:       3,
		},
		done: make(chan struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	sched.tracked[vmID] = state

	// Drive checkpointOnce five times without the goroutine → easier
	// to assert pruning deterministically.
	for i := 0; i < 5; i++ {
		if err := sched.checkpointOnce(context.Background(), state); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&backend.created) != 5 {
		t.Errorf("expected 5 snapshot creates, got %d", backend.created)
	}
	// 5 created, max=3 → 2 pruned.
	if atomic.LoadInt32(&backend.deleted) != 2 {
		t.Errorf("expected 2 prune calls, got %d", backend.deleted)
	}
	snaps := sched.SnapshotsFor(vmID)
	if len(snaps) != 3 {
		t.Errorf("kept snapshots should be 3 (MaxCheckpointsPerVM), got %d", len(snaps))
	}
	// Pair invariants: Pause and Resume called once per tick.
	if atomic.LoadInt32(&backend.paused) != 5 {
		t.Errorf("expected 5 pause calls, got %d", backend.paused)
	}
	if atomic.LoadInt32(&backend.resumed) != 5 {
		t.Errorf("expected 5 resume calls, got %d", backend.resumed)
	}
}

func TestCheckpointScheduler_DefaultsApplied(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())
	defer sched.Close()

	vmID := uuid.New()
	sched.Register(context.Background(), VMRegistration{VMID: vmID, SocketPath: "/tmp/sock"})
	sched.mu.Lock()
	state := sched.tracked[vmID]
	sched.mu.Unlock()
	if state == nil {
		t.Fatal("state not stored")
	}
	if state.reg.CheckpointIntervalMinutes != DefaultCheckpointIntervalMinutes {
		t.Errorf("interval default not applied: %d", state.reg.CheckpointIntervalMinutes)
	}
	if state.reg.MaxCheckpointsPerVM != DefaultMaxCheckpointsPerVM {
		t.Errorf("max default not applied: %d", state.reg.MaxCheckpointsPerVM)
	}
}

func TestCheckpointScheduler_RejectsInvalidRegistration(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())
	defer sched.Close()

	// Nil UUID
	sched.Register(context.Background(), VMRegistration{SocketPath: "/tmp/sock"})
	// Empty socket
	sched.Register(context.Background(), VMRegistration{VMID: uuid.New()})
	if sched.Tracking() != 0 {
		t.Errorf("invalid registrations should be rejected, got %d", sched.Tracking())
	}
}

func TestCheckpointScheduler_CloseStopsAll(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())

	for i := 0; i < 3; i++ {
		sched.Register(context.Background(), VMRegistration{
			VMID: uuid.New(), SocketPath: "/tmp/s",
			CheckpointIntervalMinutes: 60,
		})
	}
	done := make(chan struct{})
	go func() {
		sched.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within timeout")
	}
	if sched.Tracking() != 0 {
		t.Errorf("Close should clear tracked; got %d", sched.Tracking())
	}
	// Register after close → no-op.
	sched.Register(context.Background(), VMRegistration{VMID: uuid.New(), SocketPath: "/tmp/x"})
	if sched.Tracking() != 0 {
		t.Errorf("Register after Close should be a no-op; got %d", sched.Tracking())
	}
}
