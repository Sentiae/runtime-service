package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// warmManager is the narrow slice of WarmManager the pool depends on. Defining
// it as an interface lets the pool's template-caching / clone / index logic be
// unit-tested with a fake (no KVM host). *WarmManager satisfies it.
type warmManager interface {
	BootWarm(ctx context.Context, language domain.Language) (*WarmVM, error)
	CreateTemplateSnapshot(ctx context.Context, warm *WarmVM) (*TemplateSnapshot, error)
	DestroyWarm(ctx context.Context, warm *WarmVM) error
	CloneFromSnapshot(ctx context.Context, snap *TemplateSnapshot, n int) (*Clone, error)
	DestroyClone(ctx context.Context, clone *Clone) error
}

// codeAgent is the narrow slice of AgentClient the pool depends on: POST code to
// a clone's endpoint. *AgentClient satisfies it.
type codeAgent interface {
	RunCode(ctx context.Context, endpoint string, req AgentRunRequest) (AgentRunResult, error)
}

// maxCloneIndex is the upper bound of the clone index space (1..254). It mirrors
// the constraint in WarmManager.CloneFromSnapshot (the 10.200.N.x /24 scheme).
const maxCloneIndex = 254

// templateSource values reported by Fleet introspection.
const (
	templateSourceLocal       = "local"
	templateSourceObjectStore = "object-store"
	templateSourceNone        = "none"
)

// WarmPool owns the warm-template lifecycle and runs each code execution on a
// fresh warm CLONE. Per language it lazily builds the template snapshot once
// (boot warm VM → snapshot → destroy warm VM; the snapshot files persist), then
// every RunCode clones from that snapshot, POSTs the code to the clone's agent,
// and always tears the clone down (releasing its index) on return.
//
// WarmPool implements usecase.WarmCodeRunner.
type WarmPool struct {
	mgr   warmManager
	agent codeAgent

	// store, when non-nil, persists built template snapshots (state+mem) to a
	// durable object store and pulls them on a cache miss — so a runtime
	// restart or a different host reuses a template instead of rebuilding it
	// (a pull is faster than BootWarm, and works where KVM can't rebuild).
	// nil ⇒ today's local-only behavior, unchanged.
	store usecase.ArtifactStore
	// templateDir is the local directory the pulled template files land in
	// (and that built files are read from for the durable Put). Stable per
	// language so concurrent clones all restore from the same local mem file
	// (CoW). Empty ⇒ a system temp dir.
	templateDir string

	mu        sync.Mutex
	templates map[domain.Language]*TemplateSnapshot
	building  map[domain.Language]chan struct{} // per-language in-flight build guard
	active    map[int]bool                      // allocated clone indices

	// templateSource records HOW each cached template was obtained — "local"
	// (built via BootWarm→snapshot) or "object-store" (pulled from the durable
	// store) — purely for fleet introspection. Set under mu when a template is
	// cached; never read on the execution path.
	templateSource map[domain.Language]string

	// readyN is the per-language pre-warm buffer depth. 0 (the default) ⇒ no
	// buffer, no replenisher goroutines, RunCode behaves exactly as before.
	readyN int

	// ready holds the per-language buffered channel of pre-restored clones. A
	// background replenisher keeps each at depth readyN; RunCode does a
	// non-blocking receive to grab one off the critical path. Lazily created
	// (guarded by replenish) once the language's template exists.
	ready map[domain.Language]chan *Clone
	// replenish guards lazy startup of the per-language replenisher goroutine.
	replenish map[domain.Language]*sync.Once
	// taken is poked (non-blocking) on every take so the replenisher wakes
	// immediately to refill rather than waiting out its poll interval.
	taken map[domain.Language]chan struct{}

	// rootCtx/cancel scope every replenisher goroutine; Close() cancels them.
	// wg waits for the replenishers to exit before the ready buffers are
	// drained. closeOnce makes Close idempotent.
	rootCtx   context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// Compile-time assertion: WarmPool implements the usecase port.
var _ usecase.WarmCodeRunner = (*WarmPool)(nil)

// NewWarmPool builds a WarmPool over a WarmManager and AgentClient. store may be
// nil (local-only template builds, no durable persistence); when non-nil the
// pool persists/restores templates via the object store. templateDir is where
// pulled/persisted template files live locally (stable per language). readyN is
// the per-language pre-warm buffer depth — 0 keeps the on-demand-only behavior
// (no background goroutines, RunCode identical to before).
func NewWarmPool(mgr *WarmManager, agent *AgentClient, store usecase.ArtifactStore, templateDir string, readyN int) *WarmPool {
	p := newWarmPool(mgr, agent)
	p.store = store
	p.templateDir = templateDir
	p.readyN = readyN
	return p
}

// newWarmPool is the dependency-injected constructor used by tests (interfaces).
func newWarmPool(mgr warmManager, agent codeAgent) *WarmPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &WarmPool{
		mgr:            mgr,
		agent:          agent,
		templates:      make(map[domain.Language]*TemplateSnapshot),
		building:       make(map[domain.Language]chan struct{}),
		active:         make(map[int]bool),
		templateSource: make(map[domain.Language]string),
		ready:          make(map[domain.Language]chan *Clone),
		replenish:      make(map[domain.Language]*sync.Once),
		taken:          make(map[domain.Language]chan struct{}),
		rootCtx:        ctx,
		cancel:         cancel,
	}
}

// RunCode runs code on a fresh warm clone of the language's template. It ensures
// the template exists (building it at most once per language), then obtains a
// clone — TAKEN instantly off the pre-warm buffer when readyN>0 and one is
// available, else CLONED on-demand (the original path). EITHER way the clone is
// ALWAYS destroyed and its index freed (defer) — even when the agent run errors
// — because a used clone carries execution side effects and is never reused. The
// replenisher restores a fresh CoW clone to replace what was taken, off the
// request's critical path. Returns the flattened stdout/stderr/exit-code triple.
func (p *WarmPool) RunCode(ctx context.Context, language domain.Language, code, stdin string) (usecase.WarmRunResult, error) {
	snap, err := p.ensureTemplate(ctx, language)
	if err != nil {
		return usecase.WarmRunResult{}, fmt.Errorf("ensure template for %s: %w", language, err)
	}

	clone, n, err := p.acquireClone(ctx, language, snap)
	if err != nil {
		return usecase.WarmRunResult{}, err
	}
	// A used clone is destroyed (with its index freed) on every path —
	// success, agent error, or panic — so no VM/index leaks past one run.
	defer p.freeIndex(n)
	defer func() {
		if derr := p.mgr.DestroyClone(ctx, clone); derr != nil {
			logger.FromContext(ctx).Warn("warm-pool: destroy used clone failed",
				"clone_id", n, "language", language, "err", derr)
		}
	}()

	res, err := p.agent.RunCode(ctx, clone.Endpoint, AgentRunRequest{
		Language: string(language),
		Code:     code,
		Stdin:    stdin,
	})
	if err != nil {
		return usecase.WarmRunResult{}, fmt.Errorf("run code on clone %d: %w", n, err)
	}

	return usecase.WarmRunResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}

// acquireClone returns a clone to run on plus its index n. When readyN>0 it
// first ensures the language's replenisher is running, then NON-BLOCKING-receives
// a pre-warmed clone from the buffer (the ~0ms take path); the taken clone
// already owns its index (Clone.ID, allocated by the replenisher). On a buffer
// miss — or readyN==0 — it falls back to the on-demand path: allocIndex +
// CloneFromSnapshot (the original behavior, unchanged). The caller always frees
// n and destroys the clone.
func (p *WarmPool) acquireClone(ctx context.Context, language domain.Language, snap *TemplateSnapshot) (*Clone, int, error) {
	if p.readyN > 0 {
		p.ensureReplenisher(language, snap)
		ch := p.readyChan(language)
		select {
		case c := <-ch:
			// Buffer hit: the clone already holds its index (Clone.ID). Poke
			// the replenisher to refill the slot we just drained.
			p.pokeTaken(language)
			return c, c.ID, nil
		default:
			// Buffer empty under burst — fall through to on-demand.
		}
	}

	n, err := p.allocIndex()
	if err != nil {
		return nil, 0, fmt.Errorf("allocate clone index: %w", err)
	}
	clone, err := p.mgr.CloneFromSnapshot(ctx, snap, n)
	if err != nil {
		p.freeIndex(n)
		return nil, 0, fmt.Errorf("clone %s template (index %d): %w", language, n, err)
	}
	return clone, n, nil
}

// ensureTemplate returns the cached template snapshot for a language, building
// it (boot → snapshot → destroy warm VM) at most once even under concurrent
// first-calls. A per-language build channel serializes builders without holding
// the pool mutex across the (slow) boot+snapshot, so other languages still
// proceed concurrently.
func (p *WarmPool) ensureTemplate(ctx context.Context, language domain.Language) (*TemplateSnapshot, error) {
	for {
		p.mu.Lock()
		if snap, ok := p.templates[language]; ok {
			p.mu.Unlock()
			return snap, nil
		}
		if ch, building := p.building[language]; building {
			// Another goroutine is building this language. Wait for it
			// (without holding the lock), then re-check the cache.
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// We own the build for this language.
		ch := make(chan struct{})
		p.building[language] = ch
		p.mu.Unlock()

		snap, source, err := p.buildTemplate(ctx, language)

		p.mu.Lock()
		delete(p.building, language)
		if err == nil {
			p.templates[language] = snap
			p.templateSource[language] = source
		}
		p.mu.Unlock()
		close(ch) // wake any waiters so they re-check the cache

		if err != nil {
			return nil, err
		}
		return snap, nil
	}
}

// buildTemplate returns a template snapshot for a language. Runs outside the
// pool mutex (the per-language build guard in ensureTemplate already serializes
// concurrent first-callers). When a durable store is wired it first tries to
// PULL a previously-persisted template (cross-host / post-restart fast path,
// skipping BootWarm entirely); otherwise it BUILDS one (boot warm VM → snapshot
// → destroy warm VM) and, best-effort, PERSISTS it for next time. With no store
// it always builds — today's behavior, unchanged. The returned source is
// "object-store" on a durable pull or "local" on a fresh build (fleet
// introspection only).
func (p *WarmPool) buildTemplate(ctx context.Context, language domain.Language) (*TemplateSnapshot, string, error) {
	// Whatever this call resolves to, any other-version file for this language is
	// already unreachable (the names carry templateFormatVersion) — drop it here,
	// before a pull writes the current-version file beside it.
	p.reclaimOtherVersionTemplates(ctx, language)

	if p.store != nil {
		if snap, ok := p.pullTemplate(ctx, language); ok {
			return snap, templateSourceObjectStore, nil
		}
	}

	snap, err := p.buildTemplateLocal(ctx, language)
	if err != nil {
		return nil, "", err
	}

	if p.store != nil {
		p.persistTemplate(ctx, snap, language)
	}
	return snap, templateSourceLocal, nil
}

// buildTemplateLocal boots a warm VM, snapshots it, and destroys the warm VM
// (the snapshot files persist locally under the Provider's SnapshotPath).
func (p *WarmPool) buildTemplateLocal(ctx context.Context, language domain.Language) (*TemplateSnapshot, error) {
	warm, err := p.mgr.BootWarm(ctx, language)
	if err != nil {
		return nil, fmt.Errorf("boot warm VM: %w", err)
	}
	snap, err := p.mgr.CreateTemplateSnapshot(ctx, warm)
	if err != nil {
		// Best-effort teardown of the warm VM we just booted.
		_ = p.mgr.DestroyWarm(ctx, warm)
		return nil, fmt.Errorf("create template snapshot: %w", err)
	}
	if derr := p.mgr.DestroyWarm(ctx, warm); derr != nil {
		logger.FromContext(ctx).Warn("warm-pool: destroy warm template VM failed",
			"language", language, "vm_id", warm.ID, "err", derr)
	}
	return snap, nil
}

// templateObjectKeys returns the durable object-store keys for a language's
// template state + mem files: templates/<language>/v<N>/{state,mem}.
//
// The templateFormatVersion segment is what makes a template baked with different
// device/machine semantics UNREACHABLE rather than merely wrong: pullTemplate asks
// Exists() for the CURRENT version's keys only, so after a bump the store looks
// empty for that language and the pool bakes fresh.
//
// ⚠ The superseded object is NOT deleted: usecase.ArtifactStore has no Delete
// (Put/Get/Exists/VerifyHash only). It is unreachable dead weight in the bucket;
// reclaiming it needs a store-side lifecycle rule or a new port method.
func templateObjectKeys(language domain.Language) (stateKey, memKey string) {
	base := "templates/" + string(language) + "/" + templateVersionTag()
	return base + "/state", base + "/mem"
}

// templateLocalPaths returns the stable local paths a pulled template's files
// land in: <templateDir>/template-<lang>.v<N>.{state,mem}. Stable per language so
// concurrent clones all CoW-restore from the same local mem file, and per
// templateFormatVersion so a file left by an older binary can never be restored.
func (p *WarmPool) templateLocalPaths(language domain.Language) (statePath, memPath string) {
	dir := p.templateLocalDir()
	base := templateLocalPrefix(language) + templateVersionTag()
	return filepath.Join(dir, base+".state"), filepath.Join(dir, base+".mem")
}

// templateLocalDir is where this pool's per-language template files live.
func (p *WarmPool) templateLocalDir() string {
	if p.templateDir == "" {
		return os.TempDir()
	}
	return p.templateDir
}

// templateLocalPrefix is the base-name prefix every version of one language's
// local template files shares, INCLUDING the separating dot. The trailing dot is
// load-bearing for reclaimOtherVersionTemplates: it makes the match exact, so
// "python" can never match "python3"'s files.
func templateLocalPrefix(language domain.Language) string {
	return "template-" + string(language) + "."
}

// reclaimOtherVersionTemplates removes THIS language's local template files that
// belong to any other templateFormatVersion — including the pre-versioning
// "template-<lang>.{state,mem}" names. Called once per template build/pull so a
// version bump does not leak a full guest-memory image per language forever.
//
// Safety of the removal itself: a template file in use is hard-linked into every
// live clone's chroot, so unlinking this path drops a name, not the inode — a
// running clone keeps reading its snapshot. And it can only ever unlink an
// OTHER-version file, which by construction no current-version restore uses.
//
// Matching is a LITERAL PREFIX+SUFFIX test over the directory listing,
// deliberately NOT filepath.Glob (same reasoning as volume.BackingStore's sibling
// reclaim): a language string containing '*', '?', '[' or '\' would otherwise be
// read as pattern syntax, and there is no filepath.QuoteMeta to defend with. A
// candidate must start with "template-<lang>." and end in ".state" or ".mem", so
// it cannot match another language's template, pullObject's ".template-*.tmp"
// staging files, or anything outside this directory. (The park/wake VM snapshots
// live in SnapshotPath's ROOT, not in this templates/ subdirectory.)
//
// Best-effort: failures are logged, never fatal — a stale file is wasted disk,
// not a correctness problem, because it is already unreachable by name.
func (p *WarmPool) reclaimOtherVersionTemplates(ctx context.Context, language domain.Language) {
	dir := p.templateLocalDir()
	keepState, keepMem := p.templateLocalPaths(language)
	keep := map[string]struct{}{
		filepath.Base(keepState): {},
		filepath.Base(keepMem):   {},
	}
	prefix := templateLocalPrefix(language)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.FromContext(ctx).Warn("warm-pool: list template dir for version reclaim failed",
				"language", language, "dir", dir, "err", err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		if !strings.HasSuffix(name, ".state") && !strings.HasSuffix(name, ".mem") {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			logger.FromContext(ctx).Warn("warm-pool: reclaim superseded template file failed",
				"language", language, "file", name, "err", err)
			continue
		}
		logger.FromContext(ctx).Info("warm-pool: reclaimed superseded template file",
			"language", language, "file", name, "current_version", templateFormatVersion)
	}
}

// pullTemplate restores a persisted template from the durable store onto stable
// local paths and returns it, skipping BootWarm. Reports ok=false (so the
// caller falls back to a local build) when either object is absent or any step
// fails — pulling is a best-effort fast path, never a hard dependency.
func (p *WarmPool) pullTemplate(ctx context.Context, language domain.Language) (*TemplateSnapshot, bool) {
	stateKey, memKey := templateObjectKeys(language)

	stateExists, err := p.store.Exists(stateKey)
	if err != nil || !stateExists {
		return nil, false
	}
	memExists, err := p.store.Exists(memKey)
	if err != nil || !memExists {
		return nil, false
	}

	statePath, memPath := p.templateLocalPaths(language)
	if err := os.MkdirAll(filepath.Dir(statePath), 0750); err != nil {
		logger.FromContext(ctx).Warn("warm-pool: pull template mkdir local dir failed",
			"language", language, "path", filepath.Dir(statePath), "err", err)
		return nil, false
	}
	if err := p.pullObject(stateKey, statePath); err != nil {
		logger.FromContext(ctx).Warn("warm-pool: pull template state failed",
			"language", language, "object_key", stateKey, "err", err)
		return nil, false
	}
	if err := p.pullObject(memKey, memPath); err != nil {
		logger.FromContext(ctx).Warn("warm-pool: pull template mem failed",
			"language", language, "object_key", memKey, "err", err)
		return nil, false
	}

	logger.FromContext(ctx).Info("warm-pool: template restored from object store",
		"language", language, "state_path", statePath, "mem_path", memPath)
	return &TemplateSnapshot{StatePath: statePath, MemPath: memPath, Language: language}, true
}

// pullObject streams one stored object to a local path via a temp file + rename
// so a concurrent restore never observes a partial file.
func (p *WarmPool) pullObject(key, localPath string) error {
	rc, err := p.store.Get(key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".template-*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, rc); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	// os.CreateTemp makes the file 0600 root-only, but a pulled template is read
	// by the JAILED clones (as the unprivileged warm uid) through hard links into
	// their chroots — without this a pulled template restores nothing. It matches
	// the mode a locally-built template gets when it is moved out of the template
	// VM's chroot; the containing directory stays 0750 root-owned.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// persistTemplate uploads a freshly-built template's state + mem files to the
// durable store under the language's template keys. Best-effort: a failed
// upload is logged but never fails the build (the template is usable locally;
// the next process that needs it just rebuilds).
func (p *WarmPool) persistTemplate(ctx context.Context, snap *TemplateSnapshot, language domain.Language) {
	stateKey, memKey := templateObjectKeys(language)
	if err := p.putObject(stateKey, snap.StatePath); err != nil {
		logger.FromContext(ctx).Warn("warm-pool: persist template state failed",
			"language", language, "object_key", stateKey, "err", err)
		return
	}
	if err := p.putObject(memKey, snap.MemPath); err != nil {
		logger.FromContext(ctx).Warn("warm-pool: persist template mem failed",
			"language", language, "object_key", memKey, "err", err)
		return
	}
	logger.FromContext(ctx).Info("warm-pool: template persisted to object store",
		"language", language, "state_key", stateKey, "mem_key", memKey)
}

// putObject streams a local file into the durable store under key.
func (p *WarmPool) putObject(key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()
	if err := p.store.Put(key, f); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// replenishPoll bounds how long a replenisher sleeps when its buffer is full
// before re-checking; the taken signal wakes it sooner on every take.
const replenishPoll = 250 * time.Millisecond

// replenishBackoff is the brief pause after a CloneFromSnapshot failure before
// the replenisher retries, so a transient host error doesn't spin the loop.
const replenishBackoff = 500 * time.Millisecond

// readyChan returns the buffered ready channel for a language, lazily creating
// it (cap readyN) under the pool mutex.
func (p *WarmPool) readyChan(language domain.Language) chan *Clone {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.ready[language]
	if !ok {
		ch = make(chan *Clone, p.readyN)
		p.ready[language] = ch
	}
	return ch
}

// pokeTaken does a non-blocking send on the language's taken signal so the
// replenisher wakes immediately to refill the drained slot.
func (p *WarmPool) pokeTaken(language domain.Language) {
	p.mu.Lock()
	sig := p.taken[language]
	p.mu.Unlock()
	if sig == nil {
		return
	}
	select {
	case sig <- struct{}{}:
	default:
	}
}

// ensureReplenisher starts the per-language replenisher goroutine exactly once
// (after the template snapshot exists). No-op when readyN==0 or already started.
func (p *WarmPool) ensureReplenisher(language domain.Language, snap *TemplateSnapshot) {
	if p.readyN <= 0 {
		return
	}
	p.mu.Lock()
	once, ok := p.replenish[language]
	if !ok {
		once = &sync.Once{}
		p.replenish[language] = once
	}
	if _, ok := p.taken[language]; !ok {
		p.taken[language] = make(chan struct{}, 1)
	}
	p.mu.Unlock()

	once.Do(func() {
		p.wg.Add(1)
		go p.replenishLoop(language, snap)
	})
}

// replenishLoop keeps the language's ready buffer filled to readyN. It clones a
// fresh CoW VM whenever the buffer is below depth and block-sends it (respecting
// root-ctx cancellation); when full it waits for a take signal or a short poll.
// One clone is built per iteration so it never spawns faster than the host can
// absorb. Exits when the root ctx is cancelled (Close). ctx-aware + recover per
// CLAUDE.md §9.
func (p *WarmPool) replenishLoop(language domain.Language, snap *TemplateSnapshot) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(p.rootCtx).Error("warm-pool: replenisher panicked",
				"language", language, "panic", r)
		}
	}()

	ctx := p.rootCtx
	ch := p.readyChan(language)
	p.mu.Lock()
	sig := p.taken[language]
	p.mu.Unlock()

	for {
		if ctx.Err() != nil {
			return
		}

		// Buffer full → wait for a take (or a short poll) before re-checking.
		if len(ch) >= p.readyN {
			select {
			case <-ctx.Done():
				return
			case <-sig:
			case <-time.After(replenishPoll):
			}
			continue
		}

		n, err := p.allocIndex()
		if err != nil {
			// Index space exhausted (ready + active). Back off and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(replenishBackoff):
			}
			continue
		}
		clone, err := p.mgr.CloneFromSnapshot(ctx, snap, n)
		if err != nil {
			p.freeIndex(n)
			if ctx.Err() != nil {
				return
			}
			logger.FromContext(ctx).Warn("warm-pool: replenish clone failed",
				"language", language, "clone_id", n, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(replenishBackoff):
			}
			continue
		}

		// Block-send the ready clone; if Close cancels mid-send, destroy the
		// clone we just made and free its index so nothing leaks.
		select {
		case ch <- clone:
		case <-ctx.Done():
			if derr := p.mgr.DestroyClone(ctx, clone); derr != nil {
				logger.FromContext(ctx).Warn("warm-pool: destroy ready clone on shutdown failed",
					"language", language, "clone_id", n, "err", derr)
			}
			p.freeIndex(n)
			return
		}
	}
}

// Close cancels the root context, waits for every replenisher goroutine to
// exit, then drains each ready buffer — destroying every buffered clone and
// freeing its index so no VMs / netns / indices leak on shutdown. Idempotent.
func (p *WarmPool) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		p.wg.Wait()

		// Snapshot the ready channels under lock, then drain them. Replenishers
		// have exited, so no new clones are pushed after the wait above.
		p.mu.Lock()
		channels := make([]chan *Clone, 0, len(p.ready))
		for _, ch := range p.ready {
			channels = append(channels, ch)
		}
		p.mu.Unlock()

		for _, ch := range channels {
			for {
				select {
				case c := <-ch:
					if derr := p.mgr.DestroyClone(p.rootCtx, c); derr != nil {
						logger.FromContext(p.rootCtx).Warn("warm-pool: destroy buffered clone on close failed",
							"clone_id", c.ID, "err", derr)
					}
					p.freeIndex(c.ID)
				default:
					goto nextChan
				}
			}
		nextChan:
		}
	})
	return nil
}

// allocIndex returns a free clone index in 1..maxCloneIndex and marks it active.
// Errors when the index space is exhausted.
func (p *WarmPool) allocIndex() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for n := 1; n <= maxCloneIndex; n++ {
		if !p.active[n] {
			p.active[n] = true
			return n, nil
		}
	}
	return 0, fmt.Errorf("clone index space exhausted (max %d concurrent clones)", maxCloneIndex)
}

// freeIndex returns a clone index to the pool.
func (p *WarmPool) freeIndex(n int) {
	p.mu.Lock()
	delete(p.active, n)
	p.mu.Unlock()
}

// --- fleet introspection + control ----------------------------------
//
// These methods give an operator visibility into and direct control over the
// live warm state — which templates are ready, how many clones are buffered vs
// in-flight, and the ability to kill a buffered clone or force a template
// rebuild. They are additive: they never change the RunCode / replenisher /
// ensureTemplate semantics, only read the existing state (under the same mutex)
// and, for KillClone, surgically remove ONE buffered ready clone.

// CloneInfo is one buffered ready clone, exposed for fleet monitoring.
type CloneInfo struct {
	ID        int    `json:"id"`
	Endpoint  string `json:"endpoint"`
	Namespace string `json:"namespace"`
	PID       int    `json:"pid"`
}

// LanguageFleet is the warm state for one language profile.
type LanguageFleet struct {
	Language       string      `json:"language"`
	TemplateReady  bool        `json:"template_ready"`
	TemplateSource string      `json:"template_source"` // local|object-store|none
	ReadyClones    []CloneInfo `json:"ready_clones"`
	ReadyTarget    int         `json:"ready_target"`
	ActiveCount    int         `json:"active_count"` // indices in-flight (allocated, not buffered)
}

// FleetSnapshot is the whole warm pool's state at one instant, per language.
type FleetSnapshot struct {
	ReadyTarget int             `json:"ready_target"` // configured per-language buffer depth
	Languages   []LanguageFleet `json:"languages"`
	TotalReady  int             `json:"total_ready"`
	TotalActive int             `json:"total_active"`
}

// Fleet returns a consistent snapshot of the warm pool's live state under the
// pool mutex: per language the template readiness + source, the buffered ready
// clones (read WITHOUT consuming them — see below), the configured target depth,
// and the in-flight (active-but-not-buffered) clone count. Safe to call
// concurrently with RunCode and the replenisher.
//
// Reading the ready buffer without losing clones: each ready channel is drained
// fully into a local slice and then every clone is pushed straight back in the
// same order, all while holding p.mu. Because RunCode's take and the
// replenisher's send also need p.mu only for their bookkeeping — but actually
// receive/send on the channel itself — we hold the lock for the whole
// drain-then-refill so no concurrent take can interleave and observe an empty
// buffer mid-read. The channel cap == readyN >= len, so the refill never blocks.
func (p *WarmPool) Fleet() FleetSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Collect every language we know about (templates seen + ready buffers
	// created), de-duplicated, for a stable union.
	langSet := make(map[domain.Language]struct{})
	for lang := range p.templates {
		langSet[lang] = struct{}{}
	}
	for lang := range p.ready {
		langSet[lang] = struct{}{}
	}

	snap := FleetSnapshot{ReadyTarget: p.readyN}
	bufferedIndices := make(map[int]struct{}) // ready clone indices (not in-flight)

	for lang := range langSet {
		lf := LanguageFleet{
			Language:       string(lang),
			TemplateSource: templateSourceNone,
			ReadyTarget:    p.readyN,
		}
		if _, ok := p.templates[lang]; ok {
			lf.TemplateReady = true
			if src, ok := p.templateSource[lang]; ok && src != "" {
				lf.TemplateSource = src
			} else {
				lf.TemplateSource = templateSourceLocal
			}
		}

		// Drain-and-refill the ready channel to read its buffered clones without
		// removing them. cap == readyN >= len so the push-back never blocks.
		if ch := p.ready[lang]; ch != nil {
			n := len(ch)
			drained := make([]*Clone, 0, n)
			for i := 0; i < n; i++ {
				c := <-ch
				drained = append(drained, c)
			}
			for _, c := range drained {
				ch <- c // refill in original order
				bufferedIndices[c.ID] = struct{}{}
				lf.ReadyClones = append(lf.ReadyClones, CloneInfo{
					ID:        c.ID,
					Endpoint:  c.Endpoint,
					Namespace: c.Namespace,
					PID:       c.PID,
				})
			}
		}

		snap.TotalReady += len(lf.ReadyClones)
		snap.Languages = append(snap.Languages, lf)
	}

	// ActiveCount per language is hard to attribute (the active index map isn't
	// language-keyed), but the platform-wide in-flight count is precise: any
	// allocated index that is NOT sitting in a ready buffer is an in-flight
	// execution clone. Report that total and surface it on each language entry as
	// the shared in-flight figure so the UI can show "N executing".
	inFlight := 0
	for idx := range p.active {
		if _, buffered := bufferedIndices[idx]; !buffered {
			inFlight++
		}
	}
	snap.TotalActive = inFlight
	for i := range snap.Languages {
		snap.Languages[i].ActiveCount = inFlight
	}

	return snap
}

// KillClone removes ONE buffered ready clone with the given index from its
// language buffer, destroys it, and frees its index. It returns true when a
// buffered clone was found and killed, false when no READY clone has that id
// (in-flight clones owned by a live RunCode are never touched). The replenisher
// refills the drained slot afterward. Concurrency-safe.
//
// It drains each ready channel under p.mu, pulling out the matching clone and
// pushing the rest back (same order), so the take/replenish paths never observe
// a torn buffer. DestroyClone (the only host call) runs AFTER the lock is
// released so a slow teardown doesn't block the pool.
func (p *WarmPool) KillClone(ctx context.Context, id int) (bool, error) {
	p.mu.Lock()
	var victim *Clone
	for _, ch := range p.ready {
		if ch == nil {
			continue
		}
		n := len(ch)
		kept := make([]*Clone, 0, n)
		for i := 0; i < n; i++ {
			c := <-ch
			if victim == nil && c.ID == id {
				victim = c
				continue // drop from the buffer
			}
			kept = append(kept, c)
		}
		for _, c := range kept {
			ch <- c // refill (cap >= len, never blocks)
		}
		if victim != nil {
			break
		}
	}
	p.mu.Unlock()

	if victim == nil {
		return false, nil
	}

	// Destroy outside the lock; free the index regardless so it never leaks.
	derr := p.mgr.DestroyClone(ctx, victim)
	p.freeIndex(victim.ID)
	if derr != nil {
		return true, fmt.Errorf("destroy clone %d: %w", id, derr)
	}
	return true, nil
}

// RefreshTemplate drops the cached template (and its recorded source) for a
// language so the NEXT ensureTemplate rebuilds or re-pulls it. Best-effort and
// non-disruptive: live clones and the ready buffer are left untouched (they keep
// serving from the snapshot files already on disk); only the in-memory cache
// entry is cleared. Always returns nil today — the signature carries an error
// for forward compatibility (e.g. a future variant that also evicts files).
func (p *WarmPool) RefreshTemplate(language domain.Language) error {
	// Reject an out-of-allowlist language before any object-store key / local
	// path helper derives a template location from it (path-traversal guard),
	// regardless of caller.
	if !language.IsValid() {
		return fmt.Errorf("refresh template %q: %w", language, domain.ErrInvalidLanguage)
	}
	p.mu.Lock()
	delete(p.templates, language)
	delete(p.templateSource, language)
	p.mu.Unlock()
	return nil
}
