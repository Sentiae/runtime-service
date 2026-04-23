// Package firecracker — VMPool keeps a small set of pre-warmed
// Firecracker microVMs around so callers that boot fresh VMs on the
// critical path (e.g. test dispatch, node execution) amortise the
// ~150-300ms kernel+rootfs warm-up cost (§9.1).
//
// Design:
//   - a bounded buffered channel holds pooled VMs, one slot per
//     pool entry.
//   - Acquire() pops a pre-warmed VM from the pool, falling back to a
//     fresh Boot when the channel is empty. A fresh boot is always
//     safe — the pool is an optimisation, not a requirement.
//   - Release() returns the VM to the pool after the caller has torn
//     down any per-execution state. VMs that Release cannot re-home
//     (pool full, language mismatch, shutdown) are terminated instead.
//   - A refill goroutine keeps the pool topped up to the configured
//     size using the provider's Boot method. It stops when Close is
//     called so process shutdown drains cleanly.
//
// Scope: the pool in this first cut is language-agnostic — a single
// bucket with a single default language. The type is designed so a
// future per-language pool can slot in by swapping the channel for a
// map keyed by Language without changing the caller surface.
package firecracker

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// DefaultPoolSize is applied when callers pass 0 or a negative value.
// Matches APP_RUNTIME_VM_POOL_SIZE default in the config spec (§9.1).
const DefaultPoolSize = 4

// PooledVM is the metadata the pool needs to return a VM to a caller.
// It mirrors VMBootResult so callers can use it interchangeably.
type PooledVM struct {
	VMID       uuid.UUID
	PID        int
	IPAddress  string
	SocketPath string
	Language   domain.Language
	BootTimeMS int64
	CreatedAt  time.Time
}

// BootFunc is the slim contract the pool needs from a VM provider.
// We accept a func type rather than depending on *Provider directly so
// the pool is trivially testable with a mock boot function.
type BootFunc func(ctx context.Context, cfg usecase.VMBootConfig) (*usecase.VMBootResult, error)

// TerminateFunc mirrors Provider.Terminate. Callers that discard VMs
// from the pool (pool full, shutdown) use this to free host resources.
type TerminateFunc func(ctx context.Context, socketPath string, pid int) error

// VMPool is a bounded, pre-warmed cache of Firecracker microVMs.
// The zero value is unusable — construct via NewVMPool.
type VMPool struct {
	size     int
	language domain.Language
	vcpu     int
	memoryMB int

	boot      BootFunc
	terminate TerminateFunc

	// available carries ready-to-hand-out VMs. Buffer size == pool size.
	available chan *PooledVM
	// tracked holds every VM the pool has booted so shutdown can drain
	// them all, including those held by a caller between Acquire /
	// Release.
	tracked sync.Map

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
	started   bool
	closed    bool
	mu        sync.Mutex
}

// VMPoolOptions pins the pool's language / resource profile and the
// underlying boot / terminate functions. Language, VCPU and MemoryMB
// pin the warm-up profile used for refills.
type VMPoolOptions struct {
	Size      int
	Language  domain.Language
	VCPU      int
	MemoryMB  int
	Boot      BootFunc
	Terminate TerminateFunc
}

// NewVMPool constructs a pool. Boot + Terminate are required; a missing
// Boot function returns an error instead of panicking on the first
// Acquire call.
func NewVMPool(opts VMPoolOptions) (*VMPool, error) {
	if opts.Boot == nil {
		return nil, errors.New("firecracker pool: Boot is required")
	}
	if opts.Terminate == nil {
		return nil, errors.New("firecracker pool: Terminate is required")
	}
	size := opts.Size
	if size <= 0 {
		size = DefaultPoolSize
	}
	return &VMPool{
		size:      size,
		language:  opts.Language,
		vcpu:      opts.VCPU,
		memoryMB:  opts.MemoryMB,
		boot:      opts.Boot,
		terminate: opts.Terminate,
		available: make(chan *PooledVM, size),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}, nil
}

// Start launches the background refill loop. Safe to call once; the
// goroutine exits when Close is called or ctx is cancelled.
func (p *VMPool) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()
		go p.refillLoop(ctx)
		log.Printf("[VM-POOL] started (size=%d language=%s)", p.size, p.language)
	})
}

// Acquire returns a pre-warmed VM from the pool if one is available,
// otherwise boots a fresh one. A fresh boot may block until the
// provider's Boot call returns; callers should pass a ctx with a
// sensible deadline.
func (p *VMPool) Acquire(ctx context.Context) (*PooledVM, error) {
	select {
	case vm := <-p.available:
		if vm != nil {
			return vm, nil
		}
	default:
	}
	return p.bootFresh(ctx)
}

// Release hands a VM back to the pool. If the pool is full or closed,
// the VM is terminated instead so the host doesn't leak Firecracker
// processes. vm is considered consumed — callers must not read or
// mutate it after Release returns.
func (p *VMPool) Release(ctx context.Context, vm *PooledVM) {
	if vm == nil {
		return
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		p.terminateAndForget(ctx, vm)
		return
	}
	select {
	case p.available <- vm:
		// Returned to pool.
	default:
		// Pool full — discard.
		p.terminateAndForget(ctx, vm)
	}
}

// Close signals the refill goroutine to stop and terminates every
// tracked VM the pool booted (both available and held). Idempotent.
func (p *VMPool) Close(ctx context.Context) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		wasStarted := p.started
		p.mu.Unlock()
		close(p.stopCh)
		if wasStarted {
			<-p.doneCh
		}
	})
	// Drain the buffered channel.
	for {
		select {
		case vm := <-p.available:
			if vm != nil {
				p.terminateAndForget(ctx, vm)
			}
		default:
			// Tear down anything the pool tracked that wasn't in the channel
			// (either held by a caller or orphaned after a provider error).
			p.tracked.Range(func(key, _ any) bool {
				if vm, ok := key.(*PooledVM); ok {
					p.terminateAndForget(ctx, vm)
				}
				return true
			})
			log.Printf("[VM-POOL] closed")
			return
		}
	}
}

// Size returns the number of VMs currently available (not including
// those held by callers). Exposed for tests / metrics.
func (p *VMPool) Size() int {
	return len(p.available)
}

// Capacity returns the configured maximum size.
func (p *VMPool) Capacity() int {
	return p.size
}

func (p *VMPool) refillLoop(ctx context.Context) {
	defer close(p.doneCh)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-t.C:
			p.topUp(ctx)
		}
	}
}

// topUp tops the pool up to capacity. Each Boot call runs
// synchronously — we accept the latency because the refill path is
// off the hot path by design.
func (p *VMPool) topUp(ctx context.Context) {
	for len(p.available) < p.size {
		vm, err := p.bootFresh(ctx)
		if err != nil {
			log.Printf("[VM-POOL] refill boot failed: %v", err)
			return
		}
		select {
		case p.available <- vm:
		default:
			// Race with an Acquire — just terminate the extra.
			p.terminateAndForget(ctx, vm)
			return
		}
	}
}

func (p *VMPool) bootFresh(ctx context.Context) (*PooledVM, error) {
	vmID := uuid.New()
	cfg := usecase.VMBootConfig{
		VMID:     vmID,
		Language: p.language,
		VCPU:     p.vcpu,
		MemoryMB: p.memoryMB,
	}
	result, err := p.boot(ctx, cfg)
	if err != nil {
		return nil, err
	}
	vm := &PooledVM{
		VMID:       vmID,
		PID:        result.PID,
		IPAddress:  result.IPAddress,
		SocketPath: result.SocketPath,
		Language:   p.language,
		BootTimeMS: result.BootTimeMS,
		CreatedAt:  time.Now().UTC(),
	}
	p.tracked.Store(vm, struct{}{})
	return vm, nil
}

func (p *VMPool) terminateAndForget(ctx context.Context, vm *PooledVM) {
	if vm == nil {
		return
	}
	p.tracked.Delete(vm)
	if err := p.terminate(ctx, vm.SocketPath, vm.PID); err != nil {
		log.Printf("[VM-POOL] terminate %s: %v", vm.VMID, err)
	}
}
