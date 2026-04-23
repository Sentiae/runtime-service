// Package usecase — §8.3 PoolScheduler. A lightweight, cancellation-aware
// worker pool that runs TestRun jobs in parallel against a fixed number
// of workers. Each worker acquires a VM from the existing ExecutionUseCase
// (via TestRunDispatcher), which preserves the per-test isolation
// invariant — fresh DB, fresh kernel, fresh rootfs.
//
// The scheduler keeps the API surface tiny on purpose:
//
//	ps := NewPoolScheduler(dispatcher, 4)
//	ps.Start(ctx)
//	ps.Submit(job)
//	ps.Wait()   // blocks until every submitted job terminates
//	ps.Stop()   // tears down the workers
//
// Callers that only need "run N jobs concurrently and return when all
// are done" can use RunAll which wires Submit + Wait + Stop in one
// call. This is what execution_processor.go flips onto when a batch
// larger than 1 arrives and VMs are available.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// DefaultPoolWorkers is the fallback worker count when the caller
// doesn't pin one explicitly. Four tracks the typical host concurrency
// ceiling for small Firecracker deployments and matches the §8.3
// "default 4" specification.
const DefaultPoolWorkers = 4

// PoolJob is a single unit of work. The scheduler does not assume the
// job carries any specific data — only that Run takes a context and
// returns an error when the job itself failed. Cancellation is carried
// via ctx.
type PoolJob struct {
	Run *domain.TestRun
	// Profile captures the resolved test runner profile the dispatcher
	// needs. Carried on the job so the scheduler itself stays agnostic
	// of runner selection.
	Profile TestRunnerProfile
	// OnComplete, when non-nil, fires once the job terminates (success
	// or error). Callbacks run on the worker goroutine so they should
	// be fast; queue work back to another goroutine for long paths.
	OnComplete func(result PoolResult)
}

// PoolResult is the per-job outcome the OnComplete callback (and
// RunAll) observes.
type PoolResult struct {
	Run        *domain.TestRun
	Err        error
	DurationMS int64
}

// ErrPoolStopped is returned from Submit after Stop has been called.
var ErrPoolStopped = errors.New("pool scheduler stopped")

// PoolScheduler is the concrete worker pool. Zero-value is not usable
// — callers must use NewPoolScheduler.
type PoolScheduler struct {
	dispatcher TestRunDispatcher
	workers    int

	queue   chan PoolJob
	results chan PoolResult
	wg      sync.WaitGroup

	// started flips to 1 after Start runs so repeat calls are no-ops.
	started atomic.Int32

	// stopped flips to 1 on Stop. Submit honours the flag so late
	// callers see ErrPoolStopped instead of panicking on a closed chan.
	stopped atomic.Int32

	// inFlight tracks the number of submitted-but-not-completed jobs
	// so Wait can block correctly.
	inFlight sync.WaitGroup
}

// TestRunDispatcher is the minimal contract PoolScheduler needs to run
// a test against a freshly acquired VM. Implemented by
// FirecrackerTestRunDispatcher + any in-memory fake for tests.
type TestRunDispatcher interface {
	DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error
}

// TestRunnerProfile mirrors the field the existing dispatcher expects.
// Redeclared here (not imported) only to keep the scheduler file
// self-contained; the real struct lives alongside the HTTP handler but
// this package already has a matching definition in test_runner_dispatch.go
// so the build picks up the canonical one.

// NewPoolScheduler builds a PoolScheduler. workers <= 0 defaults to
// DefaultPoolWorkers.
func NewPoolScheduler(dispatcher TestRunDispatcher, workers int) *PoolScheduler {
	if workers <= 0 {
		workers = DefaultPoolWorkers
	}
	return &PoolScheduler{
		dispatcher: dispatcher,
		workers:    workers,
		queue:      make(chan PoolJob, workers*4),
		results:    make(chan PoolResult, workers*4),
	}
}

// Start launches the worker goroutines. Safe to call multiple times —
// subsequent calls are no-ops.
func (p *PoolScheduler) Start(ctx context.Context) {
	if !p.started.CompareAndSwap(0, 1) {
		return
	}
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.workerLoop(ctx, i)
	}
}

// Submit enqueues a job. Blocks when the queue is at capacity so
// back-pressure propagates to producers. Returns ErrPoolStopped after
// Stop.
func (p *PoolScheduler) Submit(ctx context.Context, job PoolJob) error {
	if p.stopped.Load() == 1 {
		return ErrPoolStopped
	}
	p.inFlight.Add(1)
	select {
	case p.queue <- job:
		return nil
	case <-ctx.Done():
		p.inFlight.Done()
		return ctx.Err()
	}
}

// Wait blocks until every submitted job has terminated. Does not stop
// workers — call Stop for that.
func (p *PoolScheduler) Wait() {
	p.inFlight.Wait()
}

// Stop signals the workers to drain and exit. Call exactly once.
// Subsequent Submit calls return ErrPoolStopped.
func (p *PoolScheduler) Stop() {
	if !p.stopped.CompareAndSwap(0, 1) {
		return
	}
	close(p.queue)
	p.wg.Wait()
	close(p.results)
}

// Results is the read-only stream of completed jobs. Consumers can
// range over it — it closes after Stop finishes draining workers.
func (p *PoolScheduler) Results() <-chan PoolResult {
	return p.results
}

// RunAll is the synchronous entry point for the common "run N jobs,
// wait for all, tear down" flow. Workers equal to the min of jobs and
// poolSize. Returns the per-job results in submission order.
func RunAll(ctx context.Context, dispatcher TestRunDispatcher, poolSize int, jobs []PoolJob) []PoolResult {
	if len(jobs) == 0 {
		return nil
	}
	size := poolSize
	if size <= 0 {
		size = DefaultPoolWorkers
	}
	if size > len(jobs) {
		size = len(jobs)
	}
	ps := NewPoolScheduler(dispatcher, size)
	ps.Start(ctx)

	// Preallocate a slice we fill by index so RunAll callers get
	// deterministic ordering regardless of worker interleaving.
	results := make([]PoolResult, len(jobs))
	var resMu sync.Mutex
	for i, j := range jobs {
		idx := i
		originalCB := j.OnComplete
		j.OnComplete = func(r PoolResult) {
			resMu.Lock()
			results[idx] = r
			resMu.Unlock()
			if originalCB != nil {
				originalCB(r)
			}
		}
		if err := ps.Submit(ctx, j); err != nil {
			// Submit shouldn't fail here — the pool is fresh — but
			// we honour the contract and surface any error as a
			// failed result so the caller doesn't lose a job.
			resMu.Lock()
			results[idx] = PoolResult{Run: j.Run, Err: err}
			resMu.Unlock()
		}
	}
	ps.Wait()
	ps.Stop()
	return results
}

// workerLoop is the body each worker goroutine runs. It pulls jobs off
// the queue, invokes the dispatcher, and reports the result. Errors
// don't kill the worker — they're surfaced on the result channel so the
// caller can decide.
func (p *PoolScheduler) workerLoop(ctx context.Context, id int) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-p.queue:
			if !ok {
				return
			}
			start := time.Now()
			err := p.safeDispatch(ctx, job)
			res := PoolResult{
				Run:        job.Run,
				Err:        err,
				DurationMS: time.Since(start).Milliseconds(),
			}
			// Fire the job-local callback first, then the pool-wide
			// results stream. Either can be nil-checked without harm.
			if job.OnComplete != nil {
				job.OnComplete(res)
			}
			select {
			case p.results <- res:
			default:
				// Buffered channel full — drop the outbound event
				// rather than block the worker. The callback + the
				// dispatcher's own persistence are the authoritative
				// record; the stream is a convenience for tests.
			}
			p.inFlight.Done()
			_ = id
		}
	}
}

// safeDispatch is a thin wrapper that recovers from dispatcher panics
// so one bad runner doesn't take the whole pool down with it.
func (p *PoolScheduler) safeDispatch(ctx context.Context, job PoolJob) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errRecovered("dispatcher panic: %v", r)
			log.Printf("[POOL] worker recovered from panic: %v", r)
		}
	}()
	return p.dispatcher.DispatchInVM(ctx, job.Run, job.Profile)
}

// errRecovered wraps the panic value into a stable error type for
// callers that want to distinguish panics from ordinary dispatch
// errors.
func errRecovered(format string, a ...any) error {
	return &dispatcherPanicError{msg: fmt.Sprintf(format, a...)}
}

type dispatcherPanicError struct{ msg string }

func (e *dispatcherPanicError) Error() string { return e.msg }
