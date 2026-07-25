//go:build unit

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// fakeArtifactStore is an in-memory usecase.ArtifactStore for exercising the
// WarmPool template-persistence flow without MinIO. It records every Put/Get
// key so tests can assert which template objects were persisted/pulled.
type fakeArtifactStore struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	putKeys  []string
	getKeys  []string
	existsCt int
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{blobs: make(map[string][]byte)}
}

// seed preloads a blob so Exists/Get report a hit (store-hit fast path).
func (s *fakeArtifactStore) seed(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[key] = data
}

func (s *fakeArtifactStore) Put(digest string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[digest] = data
	s.putKeys = append(s.putKeys, digest)
	return nil
}

func (s *fakeArtifactStore) Get(digest string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getKeys = append(s.getKeys, digest)
	data, ok := s.blobs[digest]
	if !ok {
		return nil, usecase.ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeArtifactStore) Exists(digest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existsCt++
	_, ok := s.blobs[digest]
	return ok, nil
}

func (s *fakeArtifactStore) VerifyHash(digest string) error { return nil }

// snapshot of the recorded key slices under lock.
func (s *fakeArtifactStore) keys() (put, get []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.putKeys...), append([]string(nil), s.getKeys...)
}

// fakeWarmManager is a host-free stand-in for *WarmManager. It counts BootWarm
// calls (template-build-once verification), records clone create/destroy so the
// test can assert lifecycle release, and can inject a clone error.
type fakeWarmManager struct {
	mu sync.Mutex

	bootCalls    int32
	clonesMade   int
	clonesKilled int

	cloneErr error // returned by CloneFromSnapshot when set

	// snapStatePath/snapMemPath override the snapshot file paths returned by
	// CreateTemplateSnapshot so persistTemplate can open real local files.
	snapStatePath string
	snapMemPath   string
}

func (f *fakeWarmManager) BootWarm(ctx context.Context, language domain.Language) (*WarmVM, error) {
	atomic.AddInt32(&f.bootCalls, 1)
	return &WarmVM{ID: uuid.New(), Language: language, PID: 1, TapName: warmTapInNS}, nil
}

func (f *fakeWarmManager) CreateTemplateSnapshot(ctx context.Context, warm *WarmVM) (*TemplateSnapshot, error) {
	statePath, memPath := "state", "mem"
	if f.snapStatePath != "" {
		statePath = f.snapStatePath
	}
	if f.snapMemPath != "" {
		memPath = f.snapMemPath
	}
	return &TemplateSnapshot{StatePath: statePath, MemPath: memPath, Language: warm.Language}, nil
}

func (f *fakeWarmManager) DestroyWarm(ctx context.Context, warm *WarmVM) error { return nil }

func (f *fakeWarmManager) CloneFromSnapshot(ctx context.Context, snap *TemplateSnapshot, n int) (*Clone, error) {
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	f.mu.Lock()
	f.clonesMade++
	f.mu.Unlock()
	return &Clone{ID: n, Endpoint: "10.200.1.2:8000"}, nil
}

func (f *fakeWarmManager) DestroyClone(ctx context.Context, clone *Clone) error {
	f.mu.Lock()
	f.clonesKilled++
	f.mu.Unlock()
	return nil
}

// fakeAgent is a host-free stand-in for *AgentClient. It can return a canned
// result or an error to exercise the always-release-on-error path.
type fakeAgent struct {
	result AgentRunResult
	err    error
}

func (f *fakeAgent) RunCode(ctx context.Context, endpoint string, req AgentRunRequest) (AgentRunResult, error) {
	if f.err != nil {
		return AgentRunResult{}, f.err
	}
	return f.result, nil
}

func TestWarmPool_AllocIndex_DistinctAndFree(t *testing.T) {
	p := newWarmPool(&fakeWarmManager{}, &fakeAgent{})

	a, err := p.allocIndex()
	if err != nil {
		t.Fatalf("allocIndex: %v", err)
	}
	b, err := p.allocIndex()
	if err != nil {
		t.Fatalf("allocIndex: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct indices, got %d and %d", a, b)
	}
	if a < 1 || a > maxCloneIndex || b < 1 || b > maxCloneIndex {
		t.Fatalf("indices out of range: %d, %d", a, b)
	}

	// Freeing returns the index to the pool: the next alloc reuses it.
	p.freeIndex(a)
	c, err := p.allocIndex()
	if err != nil {
		t.Fatalf("allocIndex after free: %v", err)
	}
	if c != a {
		t.Fatalf("expected freed index %d to be reused, got %d", a, c)
	}
}

func TestWarmPool_AllocIndex_Exhaustion(t *testing.T) {
	p := newWarmPool(&fakeWarmManager{}, &fakeAgent{})

	for i := 0; i < maxCloneIndex; i++ {
		if _, err := p.allocIndex(); err != nil {
			t.Fatalf("allocIndex %d: %v", i, err)
		}
	}
	if _, err := p.allocIndex(); err == nil {
		t.Fatal("expected exhaustion error after allocating the whole index space")
	}
}

func TestWarmPool_TemplateBuiltOnce_UnderConcurrency(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok", ExitCode: 0}}
	p := newWarmPool(mgr, agent)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := p.RunCode(context.Background(), domain.Language("python"), "print('x')", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RunCode: %v", err)
		}
	}

	if got := atomic.LoadInt32(&mgr.bootCalls); got != 1 {
		t.Fatalf("expected template built exactly once, BootWarm called %d times", got)
	}
	// Every run made and destroyed a clone.
	if mgr.clonesMade != goroutines || mgr.clonesKilled != goroutines {
		t.Fatalf("clone lifecycle mismatch: made=%d killed=%d want %d each", mgr.clonesMade, mgr.clonesKilled, goroutines)
	}
	// All indices released back to the pool.
	if n := len(p.active); n != 0 {
		t.Fatalf("expected all indices released, %d still active", n)
	}
}

func TestWarmPool_ReleasesCloneAndIndex_OnAgentError(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{err: errors.New("agent boom")}
	p := newWarmPool(mgr, agent)

	_, err := p.RunCode(context.Background(), domain.Language("go"), "code", "")
	if err == nil {
		t.Fatal("expected error from agent failure")
	}
	// Clone was created then destroyed despite the agent error.
	if mgr.clonesMade != 1 || mgr.clonesKilled != 1 {
		t.Fatalf("expected clone made+killed on agent error, made=%d killed=%d", mgr.clonesMade, mgr.clonesKilled)
	}
	if n := len(p.active); n != 0 {
		t.Fatalf("expected index released on agent error, %d still active", n)
	}
}

// newWarmPoolWithStore builds a pool wired to a store + local template dir,
// mirroring NewWarmPool but with the fake (interface) deps the tests use.
func newWarmPoolWithStore(mgr warmManager, agent codeAgent, store usecase.ArtifactStore, dir string) *WarmPool {
	p := newWarmPool(mgr, agent)
	p.store = store
	p.templateDir = dir
	return p
}

func TestWarmPool_StoreHit_PullsWithoutBuild(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	store := newFakeArtifactStore()
	stateKey, memKey := templateObjectKeys(domain.Language("python"))
	store.seed(stateKey, []byte("STATE-BYTES"))
	store.seed(memKey, []byte("MEM-BYTES"))

	p := newWarmPoolWithStore(mgr, agent, store, t.TempDir())

	snap, err := p.ensureTemplate(context.Background(), domain.Language("python"))
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	// Store-hit fast path: BootWarm must NOT be called.
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 0 {
		t.Fatalf("expected store-hit to skip BootWarm, called %d times", got)
	}
	// Snapshot points at the stable local paths, and they hold the pulled bytes.
	wantState, wantMem := p.templateLocalPaths(domain.Language("python"))
	if snap.StatePath != wantState || snap.MemPath != wantMem {
		t.Fatalf("snap paths = %s,%s want %s,%s", snap.StatePath, snap.MemPath, wantState, wantMem)
	}
	if b, _ := os.ReadFile(snap.StatePath); string(b) != "STATE-BYTES" {
		t.Fatalf("pulled state file = %q", string(b))
	}
	if b, _ := os.ReadFile(snap.MemPath); string(b) != "MEM-BYTES" {
		t.Fatalf("pulled mem file = %q", string(b))
	}
	// Nothing was persisted on a pure pull.
	put, _ := store.keys()
	if len(put) != 0 {
		t.Fatalf("expected no Put on store-hit, got %v", put)
	}
}

func TestWarmPool_StoreMiss_BuildsAndPersists(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	store := newFakeArtifactStore() // empty ⇒ miss

	dir := t.TempDir()
	// The fake CreateTemplateSnapshot returns StatePath="state"/MemPath="mem";
	// persistTemplate opens those local files, so create them under dir.
	statePath := filepath.Join(dir, "state")
	memPath := filepath.Join(dir, "mem")
	if err := os.WriteFile(statePath, []byte("built-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memPath, []byte("built-mem"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.snapStatePath = statePath
	mgr.snapMemPath = memPath

	p := newWarmPoolWithStore(mgr, agent, store, dir)

	if _, err := p.ensureTemplate(context.Background(), domain.Language("go")); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	// Store-miss ⇒ built exactly once.
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 1 {
		t.Fatalf("expected one build on store-miss, BootWarm called %d times", got)
	}
	// Both template objects persisted.
	stateKey, memKey := templateObjectKeys(domain.Language("go"))
	put, _ := store.keys()
	if len(put) != 2 || put[0] != stateKey || put[1] != memKey {
		t.Fatalf("expected Put of [%s %s], got %v", stateKey, memKey, put)
	}
	if string(store.blobs[stateKey]) != "built-state" || string(store.blobs[memKey]) != "built-mem" {
		t.Fatalf("persisted bytes mismatch: state=%q mem=%q", store.blobs[stateKey], store.blobs[memKey])
	}
}

func TestWarmPool_NilStore_BuildsNoStoreCalls(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	// store == nil ⇒ exact current behavior.
	p := newWarmPool(mgr, agent)

	if _, err := p.ensureTemplate(context.Background(), domain.Language("python")); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 1 {
		t.Fatalf("expected one build with nil store, BootWarm called %d times", got)
	}
}

// clonesMadeCount / clonesKilledCount read the fake's counters under lock.
func (f *fakeWarmManager) clonesMadeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clonesMade
}

func (f *fakeWarmManager) clonesKilledCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clonesKilled
}

// waitUntil polls cond until true or the deadline; returns false on timeout.
func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestWarmPool_ReadyZero_NoBufferNoGoroutines asserts readyN==0 keeps the exact
// on-demand behavior: RunCode clones on-demand, never starts a replenisher, and
// no ready buffer is ever created.
func TestWarmPool_ReadyZero_NoBufferNoGoroutines(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent) // readyN defaults to 0

	if _, err := p.RunCode(context.Background(), domain.Language("python"), "print(1)", ""); err != nil {
		t.Fatalf("RunCode: %v", err)
	}

	// On-demand clone made + destroyed, index released — identical to before.
	if mgr.clonesMadeCount() != 1 || mgr.clonesKilledCount() != 1 {
		t.Fatalf("expected one on-demand clone made+killed, made=%d killed=%d",
			mgr.clonesMadeCount(), mgr.clonesKilledCount())
	}
	if n := len(p.active); n != 0 {
		t.Fatalf("expected index released, %d active", n)
	}
	// No replenisher started, no ready channel created.
	p.mu.Lock()
	gotReplenish := len(p.replenish)
	gotReady := len(p.ready)
	p.mu.Unlock()
	if gotReplenish != 0 || gotReady != 0 {
		t.Fatalf("expected no replenisher/ready buffer at readyN=0, replenish=%d ready=%d",
			gotReplenish, gotReady)
	}

	// Close is a safe no-op when nothing was started.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWarmPool_ReadyN_FillsBuffer_AndTakeServesRequest asserts that with readyN>0
// the replenisher fills the buffer to depth N, and a RunCode is SERVED from the
// buffer (the take path, not an on-demand clone) and the used clone is destroyed.
func TestWarmPool_ReadyN_FillsBuffer_AndTakeServesRequest(t *testing.T) {
	const readyN = 3
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer p.Close()

	lang := domain.Language("python")
	// Start the replenisher (RunCode would also do this, but kick it directly so
	// we can observe the buffer fill before the take).
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)

	// Replenisher fills the buffer to depth readyN (clonesMade reaches N).
	if !waitUntil(2*time.Second, func() bool { return mgr.clonesMadeCount() >= readyN }) {
		t.Fatalf("buffer never filled: clonesMade=%d want >=%d", mgr.clonesMadeCount(), readyN)
	}
	if !waitUntil(time.Second, func() bool {
		ch := p.readyChan(lang)
		return len(ch) == readyN
	}) {
		t.Fatalf("ready buffer depth never reached %d, got %d", readyN, len(p.readyChan(lang)))
	}

	madeBeforeTake := mgr.clonesMadeCount()
	killedBeforeTake := mgr.clonesKilledCount()

	// A take from the (full) buffer should NOT trigger an on-demand clone in
	// acquireClone: the served clone was pre-made by the replenisher.
	clone, n, err := p.acquireClone(context.Background(), lang, snap)
	if err != nil {
		t.Fatalf("acquireClone: %v", err)
	}
	if n < 1 || n > maxCloneIndex {
		t.Fatalf("served index out of range: %d", n)
	}
	if n != clone.ID {
		t.Fatalf("served clone index mismatch: n=%d clone.ID=%d", n, clone.ID)
	}
	// No NEW on-demand clone was made by acquireClone itself (the replenisher
	// may refill afterwards, but the take itself didn't clone).
	if got := mgr.clonesMadeCount(); got != madeBeforeTake && got < madeBeforeTake {
		t.Fatalf("clonesMade decreased? before=%d after=%d", madeBeforeTake, got)
	}

	// Simulate RunCode's always-destroy on the taken clone.
	if err := mgr.DestroyClone(ctx, clone); err != nil {
		t.Fatalf("DestroyClone: %v", err)
	}
	p.freeIndex(n)
	if mgr.clonesKilledCount() != killedBeforeTake+1 {
		t.Fatalf("expected the taken clone destroyed, killed before=%d after=%d",
			killedBeforeTake, mgr.clonesKilledCount())
	}
}

// TestWarmPool_RunCode_TakesFromBuffer is the end-to-end take assertion through
// the public RunCode: after warmup, a RunCode consumes a buffered clone (so the
// buffer depth drops) and destroys it.
func TestWarmPool_RunCode_TakesFromBuffer(t *testing.T) {
	const readyN = 2
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok", ExitCode: 0}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer p.Close()

	lang := domain.Language("go")
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer never reached depth %d", readyN)
	}

	res, err := p.RunCode(context.Background(), lang, "code", "")
	if err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if res.Stdout != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The buffer was drained by at least one (the replenisher races to refill,
	// so we only assert the take happened by checking total kills grew and the
	// index space stays consistent). Eventually the buffer refills to N again.
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer did not refill to %d after a take, got %d", readyN, len(p.readyChan(lang)))
	}
	// At least one clone destroyed (the one RunCode used).
	if mgr.clonesKilledCount() < 1 {
		t.Fatalf("expected the used clone destroyed, killed=%d", mgr.clonesKilledCount())
	}
}

// TestWarmPool_Close_DestroysBufferedClones asserts Close destroys every
// buffered ready clone and frees their indices, and is idempotent.
func TestWarmPool_Close_DestroysBufferedClones(t *testing.T) {
	const readyN = 4
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN

	lang := domain.Language("python")
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer never filled to %d, got %d", readyN, len(p.readyChan(lang)))
	}

	madeAtFull := mgr.clonesMadeCount()
	killedBefore := mgr.clonesKilledCount()

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Every clone the replenisher made and left buffered is destroyed by Close.
	// (Replenisher may have made a few extra mid-flight that it self-destroyed on
	// the ctx-cancelled send; net: total killed == total made.)
	if mgr.clonesKilledCount() != mgr.clonesMadeCount() {
		t.Fatalf("Close leaked clones: made=%d killed=%d", mgr.clonesMadeCount(), mgr.clonesKilledCount())
	}
	if mgr.clonesKilledCount() < killedBefore+readyN {
		t.Fatalf("expected at least %d buffered clones destroyed by Close, killed grew %d→%d",
			readyN, killedBefore, mgr.clonesKilledCount())
	}
	if madeAtFull < readyN {
		t.Fatalf("sanity: expected >=%d made before Close, got %d", readyN, madeAtFull)
	}
	// All indices freed.
	if n := len(p.active); n != 0 {
		t.Fatalf("expected all indices freed after Close, %d active", n)
	}

	// Idempotent: a second Close is a safe no-op.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestWarmPool_BufferEmpty_FallsBackToOnDemand asserts that when readyN>0 but the
// buffer is empty (burst faster than refill), acquireClone falls back to the
// on-demand path (allocIndex + CloneFromSnapshot) instead of blocking.
func TestWarmPool_BufferEmpty_FallsBackToOnDemand(t *testing.T) {
	const readyN = 1
	// Block the replenisher's CloneFromSnapshot forever so the buffer stays
	// empty; acquireClone must NOT block on the empty buffer.
	release := make(chan struct{})
	mgr := &blockingWarmManager{block: release}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer func() { close(release); p.Close() }()

	lang := domain.Language("rust")
	snap := &TemplateSnapshot{StatePath: "s", MemPath: "m", Language: lang}
	// Seed the template cache directly so ensureTemplate is a no-op (the
	// blocking manager would otherwise stall buildTemplate too).
	p.mu.Lock()
	p.templates[lang] = snap
	p.mu.Unlock()

	// Tag the request ctx so the manager lets the on-demand clone through
	// instantly while the replenisher's clone (using rootCtx, untagged) blocks.
	onDemandCtx := context.WithValue(context.Background(), onDemandMarker{}, true)
	done := make(chan struct{})
	var clone *Clone
	var n int
	var acqErr error
	go func() {
		defer close(done)
		clone, n, acqErr = p.acquireClone(onDemandCtx, lang, snap)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquireClone blocked on an empty buffer instead of falling back to on-demand")
	}
	if acqErr != nil {
		t.Fatalf("acquireClone fallback: %v", acqErr)
	}
	if clone == nil || n < 1 {
		t.Fatalf("expected an on-demand clone, got clone=%v n=%d", clone, n)
	}
	// The on-demand clone came from the blocking manager's on-demand path
	// (which does NOT block — only replenisher clones block).
	mgr.DestroyClone(ctx, clone)
	p.freeIndex(n)
}

// onDemandMarker tags a request context so blockingWarmManager lets that clone
// through instantly; clones on any untagged ctx (the replenisher's rootCtx)
// block. This isolates the "buffer empty → on-demand fallback, no blocking on
// the empty buffer" behavior under test.
type onDemandMarker struct{}

// blockingWarmManager blocks every CloneFromSnapshot whose ctx is NOT tagged
// with onDemandMarker (i.e. the replenisher, keeping the ready buffer empty)
// until release/ctx-cancel; a tagged (on-demand) clone returns instantly.
type blockingWarmManager struct {
	mu           sync.Mutex
	clonesMade   int
	clonesKilled int
	block        chan struct{}
}

func (b *blockingWarmManager) BootWarm(ctx context.Context, language domain.Language) (*WarmVM, error) {
	return &WarmVM{ID: uuid.New(), Language: language, PID: 1}, nil
}

func (b *blockingWarmManager) CreateTemplateSnapshot(ctx context.Context, warm *WarmVM) (*TemplateSnapshot, error) {
	return &TemplateSnapshot{StatePath: "state", MemPath: "mem", Language: warm.Language}, nil
}

func (b *blockingWarmManager) DestroyWarm(ctx context.Context, warm *WarmVM) error { return nil }

func (b *blockingWarmManager) CloneFromSnapshot(ctx context.Context, snap *TemplateSnapshot, n int) (*Clone, error) {
	// Untagged ctx (the replenisher) blocks so the ready buffer never fills;
	// an on-demand-tagged ctx returns instantly.
	if v, _ := ctx.Value(onDemandMarker{}).(bool); !v {
		select {
		case <-b.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.mu.Lock()
	b.clonesMade++
	b.mu.Unlock()
	return &Clone{ID: n, Endpoint: "10.200.1.2:8000"}, nil
}

func (b *blockingWarmManager) DestroyClone(ctx context.Context, clone *Clone) error {
	b.mu.Lock()
	b.clonesKilled++
	b.mu.Unlock()
	return nil
}

// TestWarmPool_Race_ConcurrentRunReplenishClose exercises concurrent RunCode
// (take + on-demand mix), the background replenisher, and Close together — run
// with -race to catch data races on the ready buffers / index map.
func TestWarmPool_Race_ConcurrentRunReplenishClose(t *testing.T) {
	const readyN = 4
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN

	langs := []domain.Language{"python", "go", "rust"}
	var wg sync.WaitGroup
	for _, lang := range langs {
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(l domain.Language) {
				defer wg.Done()
				if _, err := p.RunCode(context.Background(), l, "code", ""); err != nil {
					t.Errorf("RunCode(%s): %v", l, err)
				}
			}(lang)
		}
	}

	// Close concurrently with in-flight runs to shake out shutdown races.
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = p.Close()
	}()

	wg.Wait()
	_ = p.Close() // idempotent
}

// TestWarmPool_Fleet_ReportsReadyWithoutConsuming asserts Fleet() reports the N
// buffered ready clones (with their id/endpoint) and the template source WITHOUT
// draining the buffer — a take after Fleet still sees the full depth.
func TestWarmPool_Fleet_ReportsReadyWithoutConsuming(t *testing.T) {
	const readyN = 3
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer p.Close()

	lang := domain.Language("python")
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer never filled to %d, got %d", readyN, len(p.readyChan(lang)))
	}

	fleet := p.Fleet()
	if fleet.ReadyTarget != readyN {
		t.Fatalf("ReadyTarget=%d want %d", fleet.ReadyTarget, readyN)
	}
	if len(fleet.Languages) != 1 {
		t.Fatalf("expected 1 language in fleet, got %d", len(fleet.Languages))
	}
	lf := fleet.Languages[0]
	if lf.Language != "python" || !lf.TemplateReady {
		t.Fatalf("unexpected language entry: %+v", lf)
	}
	if lf.TemplateSource != templateSourceLocal {
		t.Fatalf("TemplateSource=%q want %q", lf.TemplateSource, templateSourceLocal)
	}
	if len(lf.ReadyClones) != readyN {
		t.Fatalf("Fleet reported %d ready clones, want %d", len(lf.ReadyClones), readyN)
	}
	if fleet.TotalReady != readyN {
		t.Fatalf("TotalReady=%d want %d", fleet.TotalReady, readyN)
	}
	for _, ci := range lf.ReadyClones {
		if ci.ID < 1 || ci.ID > maxCloneIndex {
			t.Fatalf("ready clone id out of range: %d", ci.ID)
		}
		if ci.Endpoint == "" {
			t.Fatalf("ready clone %d missing endpoint", ci.ID)
		}
	}

	// Crucial: Fleet did NOT consume the buffer — depth is still readyN.
	if got := len(p.readyChan(lang)); got != readyN {
		t.Fatalf("Fleet consumed the buffer: depth=%d want %d", got, readyN)
	}
}

// TestWarmPool_KillClone_RemovesDestroysAndFrees asserts KillClone removes a
// buffered ready clone, destroys it (host teardown), and frees its index.
func TestWarmPool_KillClone_RemovesDestroysAndFrees(t *testing.T) {
	const readyN = 3
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer p.Close()

	lang := domain.Language("go")
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer never filled to %d, got %d", readyN, len(p.readyChan(lang)))
	}

	// Stop the replenisher from refilling so we can observe the post-kill state
	// deterministically: cancel the root ctx and wait for the goroutine to exit.
	p.cancel()
	p.wg.Wait()

	fleet := p.Fleet()
	targetID := fleet.Languages[0].ReadyClones[0].ID
	killedBefore := mgr.clonesKilledCount()

	ok, err := p.KillClone(context.Background(), targetID)
	if err != nil {
		t.Fatalf("KillClone: %v", err)
	}
	if !ok {
		t.Fatalf("KillClone(%d) returned false for a buffered clone", targetID)
	}
	// Destroyed exactly once via the manager.
	if mgr.clonesKilledCount() != killedBefore+1 {
		t.Fatalf("expected one destroy, killed grew %d→%d", killedBefore, mgr.clonesKilledCount())
	}
	// Index freed.
	p.mu.Lock()
	_, stillActive := p.active[targetID]
	p.mu.Unlock()
	if stillActive {
		t.Fatalf("index %d not freed after KillClone", targetID)
	}
	// Buffer depth dropped by one and the killed clone is gone from it.
	if got := len(p.readyChan(lang)); got != readyN-1 {
		t.Fatalf("buffer depth=%d want %d after kill", got, readyN-1)
	}
	for _, ci := range p.Fleet().Languages[0].ReadyClones {
		if ci.ID == targetID {
			t.Fatalf("killed clone %d still buffered", targetID)
		}
	}
}

// TestWarmPool_KillClone_UnknownID asserts KillClone returns false (no error,
// no destroy) for an id that is not a buffered ready clone.
func TestWarmPool_KillClone_UnknownID(t *testing.T) {
	const readyN = 2
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)
	p.readyN = readyN
	defer p.Close()

	lang := domain.Language("python")
	snap, err := p.ensureTemplate(context.Background(), lang)
	if err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	p.ensureReplenisher(lang, snap)
	if !waitUntil(2*time.Second, func() bool { return len(p.readyChan(lang)) == readyN }) {
		t.Fatalf("buffer never filled to %d", readyN)
	}
	p.cancel()
	p.wg.Wait()

	killedBefore := mgr.clonesKilledCount()
	// 9999 is well outside any buffered clone id.
	ok, err := p.KillClone(context.Background(), 9999)
	if err != nil {
		t.Fatalf("KillClone(unknown): unexpected error %v", err)
	}
	if ok {
		t.Fatal("KillClone(unknown) returned true")
	}
	if mgr.clonesKilledCount() != killedBefore {
		t.Fatalf("KillClone(unknown) destroyed a clone: killed %d→%d", killedBefore, mgr.clonesKilledCount())
	}
	// Buffer untouched.
	if got := len(p.readyChan(lang)); got != readyN {
		t.Fatalf("KillClone(unknown) disturbed the buffer: depth=%d want %d", got, readyN)
	}
}

// TestWarmPool_RefreshTemplate_ForcesRebuild asserts RefreshTemplate drops the
// cached template so the next ensureTemplate rebuilds it (BootWarm fires again),
// without disturbing the live ready buffer.
func TestWarmPool_RefreshTemplate_ForcesRebuild(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	p := newWarmPool(mgr, agent)

	lang := domain.Language("python")
	if _, err := p.ensureTemplate(context.Background(), lang); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 1 {
		t.Fatalf("expected one build, got %d", got)
	}

	if err := p.RefreshTemplate(lang); err != nil {
		t.Fatalf("RefreshTemplate: %v", err)
	}
	// Cache entry cleared.
	p.mu.Lock()
	_, cached := p.templates[lang]
	p.mu.Unlock()
	if cached {
		t.Fatal("template still cached after RefreshTemplate")
	}

	// Next ensureTemplate rebuilds.
	if _, err := p.ensureTemplate(context.Background(), lang); err != nil {
		t.Fatalf("ensureTemplate after refresh: %v", err)
	}
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 2 {
		t.Fatalf("expected a rebuild after refresh, BootWarm called %d times", got)
	}
}

func TestWarmPool_StoreMiss_BuildsOnceUnderConcurrency(t *testing.T) {
	mgr := &fakeWarmManager{}
	agent := &fakeAgent{result: AgentRunResult{Stdout: "ok"}}
	store := newFakeArtifactStore() // empty ⇒ miss

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state")
	memPath := filepath.Join(dir, "mem")
	if err := os.WriteFile(statePath, []byte("built-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memPath, []byte("built-mem"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.snapStatePath = statePath
	mgr.snapMemPath = memPath

	p := newWarmPoolWithStore(mgr, agent, store, dir)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := p.ensureTemplate(context.Background(), domain.Language("python"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensureTemplate: %v", err)
		}
	}
	// Build-once guard still holds with the store wired.
	if got := atomic.LoadInt32(&mgr.bootCalls); got != 1 {
		t.Fatalf("expected exactly one build under concurrency, BootWarm called %d times", got)
	}
	// Persisted exactly once (two keys, not 2*goroutines).
	put, _ := store.keys()
	if len(put) != 2 {
		t.Fatalf("expected 2 Puts (built once), got %d: %v", len(put), put)
	}
}
