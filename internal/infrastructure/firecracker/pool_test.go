package firecracker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// fakeBoot records every boot request and returns a deterministic
// VMBootResult. BootFails toggles error returns for failure-path tests.
type fakeBootState struct {
	mu        sync.Mutex
	bootCalls atomic.Int32
	termCalls atomic.Int32
	bootFails bool
}

func (f *fakeBootState) boot(_ context.Context, cfg usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	f.bootCalls.Add(1)
	if f.bootFails {
		return nil, errors.New("boot failed")
	}
	return &usecase.VMBootResult{
		PID:        1000 + int(f.bootCalls.Load()),
		IPAddress:  "10.0.0.1",
		SocketPath: "/tmp/" + cfg.VMID.String() + ".sock",
		BootTimeMS: 42,
	}, nil
}

func (f *fakeBootState) terminate(_ context.Context, _ string, _ int) error {
	f.termCalls.Add(1)
	return nil
}

func newFakePool(t *testing.T, size int) (*VMPool, *fakeBootState) {
	t.Helper()
	fake := &fakeBootState{}
	pool, err := NewVMPool(VMPoolOptions{
		Size:      size,
		Language:  domain.LanguagePython,
		VCPU:      1,
		MemoryMB:  512,
		Boot:      fake.boot,
		Terminate: fake.terminate,
	})
	if err != nil {
		t.Fatalf("NewVMPool: %v", err)
	}
	return pool, fake
}

func TestVMPool_NewValidation(t *testing.T) {
	_, err := NewVMPool(VMPoolOptions{Terminate: func(context.Context, string, int) error { return nil }})
	if err == nil {
		t.Fatal("expected error for missing Boot")
	}
	_, err = NewVMPool(VMPoolOptions{Boot: func(context.Context, usecase.VMBootConfig) (*usecase.VMBootResult, error) { return nil, nil }})
	if err == nil {
		t.Fatal("expected error for missing Terminate")
	}
}

func TestVMPool_AcquireColdBootsWhenEmpty(t *testing.T) {
	pool, fake := newFakePool(t, 2)
	// Never Start — the pool is empty and Acquire must boot cold.
	vm, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if vm == nil || vm.VMID == uuid.Nil {
		t.Fatalf("expected a VM, got %+v", vm)
	}
	if fake.bootCalls.Load() != 1 {
		t.Fatalf("expected 1 boot, got %d", fake.bootCalls.Load())
	}
}

func TestVMPool_FillAcquireReleaseDrain(t *testing.T) {
	pool, fake := newFakePool(t, 2)
	ctx := context.Background()
	// Fill manually (skip refill goroutine so we can reason about counts).
	pool.topUp(ctx)
	if got := pool.Size(); got != 2 {
		t.Fatalf("Size after topUp: got %d want 2", got)
	}
	if got := fake.bootCalls.Load(); got != 2 {
		t.Fatalf("boot calls after topUp: got %d want 2", got)
	}

	// Acquire pops from the pool — boot count stays at 2.
	vm1, err := pool.Acquire(ctx)
	if err != nil || vm1 == nil {
		t.Fatalf("Acquire #1: %v", err)
	}
	if got := fake.bootCalls.Load(); got != 2 {
		t.Fatalf("boot count after warm acquire: got %d want 2", got)
	}
	// Pool went from 2 to 1 (we popped one).
	if got := pool.Size(); got != 1 {
		t.Fatalf("Size after acquire: got %d want 1", got)
	}

	// Release returns it to the pool.
	pool.Release(ctx, vm1)
	if got := pool.Size(); got != 2 {
		t.Fatalf("Size after release: got %d want 2", got)
	}

	// Drain terminates every pooled VM.
	pool.Close(ctx)
	if fake.termCalls.Load() < 2 {
		t.Fatalf("Close did not terminate VMs: term calls = %d", fake.termCalls.Load())
	}
}

func TestVMPool_ReleaseOverflowTerminates(t *testing.T) {
	pool, fake := newFakePool(t, 1)
	ctx := context.Background()
	// Fill to capacity (1 slot).
	pool.topUp(ctx)
	if got := pool.Size(); got != 1 {
		t.Fatalf("want size 1, got %d", got)
	}
	// Manually boot a VM without going through the pool channel so we
	// can Release into a full pool and trigger the overflow path.
	extra, err := pool.bootFresh(ctx)
	if err != nil {
		t.Fatalf("bootFresh: %v", err)
	}
	pool.Release(ctx, extra)
	// Overflow must terminate extra.
	if fake.termCalls.Load() == 0 {
		t.Fatal("expected overflow terminate, got 0")
	}
	pool.Close(ctx)
}

func TestVMPool_ReleaseAfterCloseTerminates(t *testing.T) {
	pool, fake := newFakePool(t, 1)
	ctx := context.Background()
	vm, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Close(ctx)
	pool.Release(ctx, vm)
	if fake.termCalls.Load() == 0 {
		t.Fatal("release after close must terminate")
	}
}

// TestProvider_BootPullsFromPool asserts that when a Provider has a
// warm VMPool wired via SetPool and the pool has a pre-warmed VM,
// Provider.Boot returns the pooled VM's metadata without invoking any
// cold-boot Firecracker process. §9.1.
func TestProvider_BootPullsFromPool(t *testing.T) {
	// Fake boot function that populates the pool with a known VM.
	fake := &fakeBootState{}
	pool, err := NewVMPool(VMPoolOptions{
		Size:      1,
		Language:  domain.LanguagePython,
		VCPU:      1,
		MemoryMB:  512,
		Boot:      fake.boot,
		Terminate: fake.terminate,
	})
	if err != nil {
		t.Fatalf("NewVMPool: %v", err)
	}
	// Manually top up so the pool has 1 warm VM.
	pool.topUp(context.Background())

	// Construct a Provider with no real Firecracker binary. Because the
	// pool channel already has a VM, Provider.Boot must hit the warm
	// path and NEVER call bootCold (which would require a Firecracker
	// binary on PATH and would fail synchronously).
	p := &Provider{}
	p.SetPool(pool)

	want := uuid.New()
	result, err := p.Boot(context.Background(), usecase.VMBootConfig{VMID: want, Language: domain.LanguagePython})
	if err != nil {
		t.Fatalf("Provider.Boot: %v", err)
	}
	if result == nil || result.PID == 0 {
		t.Fatalf("Boot returned empty result: %+v", result)
	}
	// Pool must now be empty — the VM was handed over to the caller.
	if got := pool.Size(); got != 0 {
		t.Fatalf("pool size after Boot: got %d want 0", got)
	}
	// boot calls stay at 1 (the topUp call) — no cold boot was issued.
	if got := fake.bootCalls.Load(); got != 1 {
		t.Fatalf("cold boot leaked through: bootCalls=%d want 1", got)
	}
}

// TestProvider_BootFallsThroughWhenPoolEmpty — with the pool wired but
// empty, Provider.Boot falls through to bootCold. We assert it does
// not block on the empty channel (the select-default branch is the
// whole point of the non-blocking pool check).
func TestProvider_BootFallsThroughWhenPoolEmpty(t *testing.T) {
	fake := &fakeBootState{}
	pool, err := NewVMPool(VMPoolOptions{
		Size:      1,
		Language:  domain.LanguagePython,
		Boot:      fake.boot,
		Terminate: fake.terminate,
	})
	if err != nil {
		t.Fatalf("NewVMPool: %v", err)
	}
	p := &Provider{}
	p.SetPool(pool)

	// bootCold would actually shell out to the Firecracker binary —
	// we can't exercise it here without real infra, so assert instead
	// that the pool-empty select didn't block: we race against a short
	// timeout. If the select blocked we'd deadlock forever.
	done := make(chan struct{})
	go func() {
		_, _ = p.Boot(context.Background(), usecase.VMBootConfig{VMID: uuid.New(), Language: domain.LanguagePython})
		close(done)
	}()
	select {
	case <-done:
		// bootCold returned (probably with an error) — that's fine, the
		// point is Boot didn't block on an empty pool channel.
	case <-time.After(3 * time.Second):
		t.Fatal("Provider.Boot blocked on empty pool instead of falling through")
	}
}

func TestVMPool_RefillGoroutineStops(t *testing.T) {
	pool, _ := newFakePool(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	// Give the refill loop a tick to run.
	time.Sleep(10 * time.Millisecond)
	// Close should stop the refill goroutine without deadlocking.
	pool.Close(ctx)
}
