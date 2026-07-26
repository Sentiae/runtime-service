package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

const netTestUIDBase = 100000
const netTestUIDSpan = 8192

// ─────────────────────────────────────────────────────────────────────
// Fakes. The lease store enforces the SAME five unique fences the DDL does, so a
// test can never pass on a store that is more permissive than Postgres.
// ─────────────────────────────────────────────────────────────────────

type netFakeLeaseRepo struct {
	mu       sync.Mutex
	leases   []domain.NetLease
	ordinals map[uuid.UUID]int
	// conflictSlots makes Acquire refuse a slot ONCE, simulating a lost race with
	// another process (which is otherwise unreachable from a single-process test).
	conflictSlots map[int]int
	usedSlotsErr  error
	listErr       error
	releaseErr    error
	ordinalErr    error
	acquires      int
	releases      []string
}

var _ repository.NetLeaseRepository = (*netFakeLeaseRepo)(nil)

func newNetFakeLeaseRepo() *netFakeLeaseRepo {
	return &netFakeLeaseRepo{ordinals: map[uuid.UUID]int{}, conflictSlots: map[int]int{}}
}

func (f *netFakeLeaseRepo) Acquire(_ context.Context, lease *domain.NetLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if n := f.conflictSlots[lease.LocalSlot]; n > 0 {
		f.conflictSlots[lease.LocalSlot] = n - 1
		return fmt.Errorf("%w: injected race on slot %d", domain.ErrNetLeaseConflict, lease.LocalSlot)
	}
	for _, held := range f.leases {
		switch {
		case held.NetIndex == lease.NetIndex,
			held.HostID == lease.HostID && held.LocalSlot == lease.LocalSlot,
			held.HostID == lease.HostID && held.VMUID == lease.VMUID,
			held.HostID == lease.HostID && held.TapName == lease.TapName,
			held.OwnerKind == lease.OwnerKind && held.OwnerID == lease.OwnerID:
			return fmt.Errorf("%w: fence", domain.ErrNetLeaseConflict)
		}
	}
	f.leases = append(f.leases, *lease)
	return nil
}

func (f *netFakeLeaseRepo) UsedSlots(_ context.Context, hostID uuid.UUID) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usedSlotsErr != nil {
		return nil, f.usedSlotsErr
	}
	var slots []int
	for _, l := range f.leases {
		if l.HostID == hostID {
			slots = append(slots, l.LocalSlot)
		}
	}
	return slots, nil
}

func (f *netFakeLeaseRepo) FindByOwner(_ context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (*domain.NetLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.leases {
		if f.leases[i].OwnerKind == kind && f.leases[i].OwnerID == ownerID {
			cp := f.leases[i]
			return &cp, nil
		}
	}
	return nil, domain.ErrNetLeaseNotFound
}

func (f *netFakeLeaseRepo) ListByHost(_ context.Context, hostID uuid.UUID) ([]domain.NetLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.NetLease
	for _, l := range f.leases {
		if l.HostID == hostID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *netFakeLeaseRepo) Release(_ context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.releases = append(f.releases, string(kind)+":"+ownerID.String())
	kept := f.leases[:0]
	for _, l := range f.leases {
		if l.OwnerKind == kind && l.OwnerID == ownerID {
			continue
		}
		kept = append(kept, l)
	}
	f.leases = kept
	return nil
}

func (f *netFakeLeaseRepo) EnsureHostOrdinal(_ context.Context, hostID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ordinalErr != nil {
		return 0, f.ordinalErr
	}
	if ord, ok := f.ordinals[hostID]; ok {
		return ord, nil
	}
	used := map[int]bool{}
	for _, o := range f.ordinals {
		used[o] = true
	}
	for candidate := 0; candidate <= domain.NetMaxOrdinal; candidate++ {
		if !used[candidate] {
			f.ordinals[hostID] = candidate
			return candidate, nil
		}
	}
	return 0, domain.ErrNetOrdinalExhausted
}

func (f *netFakeLeaseRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.leases)
}

// seed inserts a lease directly (bypassing the allocator) for reconcile tests.
func (f *netFakeLeaseRepo) seed(t *testing.T, hostID uuid.UUID, slot int, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID, created time.Time) domain.NetLease {
	t.Helper()
	return f.seedAt(t, hostID, 0, slot, kind, ownerID, created)
}

// seedAt is seed for a host on an arbitrary ordinal — the multi-host cases.
func (f *netFakeLeaseRepo) seedAt(t *testing.T, hostID uuid.UUID, ordinal, slot int, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID, created time.Time) domain.NetLease {
	t.Helper()
	lease, err := domain.DeriveNetLease(ordinal, slot, netTestUIDBase)
	if err != nil {
		t.Fatalf("derive seed lease: %v", err)
	}
	lease.ID = uuid.New()
	lease.HostID = hostID
	lease.OwnerKind, lease.OwnerID = kind, ownerID
	lease.CreatedAt, lease.UpdatedAt = created, created
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, lease)
	return lease
}

type netFakeHostRepo struct {
	host *domain.Host
	err  error
}

var _ repository.HostRepository = (*netFakeHostRepo)(nil)

func (f *netFakeHostRepo) Create(context.Context, *domain.Host) error { return nil }
func (f *netFakeHostRepo) Update(context.Context, *domain.Host) error { return nil }
func (f *netFakeHostRepo) FindByID(_ context.Context, _ uuid.UUID) (*domain.Host, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.host == nil {
		return nil, domain.ErrFleetHostNotFound
	}
	cp := *f.host
	return &cp, nil
}
func (f *netFakeHostRepo) List(context.Context) ([]domain.Host, error)       { return nil, nil }
func (f *netFakeHostRepo) ListActive(context.Context) ([]domain.Host, error) { return nil, nil }
func (f *netFakeHostRepo) ListByStatus(context.Context, domain.HostStatus) ([]domain.Host, error) {
	return nil, nil
}
func (f *netFakeHostRepo) Delete(context.Context, uuid.UUID) error { return nil }

type netFakeReplicaRepo struct {
	byID    map[uuid.UUID]domain.Replica
	byState map[domain.ReplicaState][]domain.Replica
	findErr error
	listErr error
}

var _ repository.ReplicaRepository = (*netFakeReplicaRepo)(nil)

func newNetFakeReplicaRepo() *netFakeReplicaRepo {
	return &netFakeReplicaRepo{byID: map[uuid.UUID]domain.Replica{}, byState: map[domain.ReplicaState][]domain.Replica{}}
}

func (f *netFakeReplicaRepo) put(r domain.Replica) {
	f.byID[r.ID] = r
	f.byState[r.State] = append(f.byState[r.State], r)
}

func (f *netFakeReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (f *netFakeReplicaRepo) Update(context.Context, *domain.Replica) error { return nil }
func (f *netFakeReplicaRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Replica, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrReplicaNotFound
	}
	return &r, nil
}
func (f *netFakeReplicaRepo) ListByApp(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *netFakeReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *netFakeReplicaRepo) ListByState(_ context.Context, state domain.ReplicaState) ([]domain.Replica, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byState[state], nil
}
func (f *netFakeReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }

type netFakeWorkloadRepo struct {
	byID   map[uuid.UUID]domain.ImageWorkload
	active []domain.ImageWorkload
}

var _ repository.ImageWorkloadRepository = (*netFakeWorkloadRepo)(nil)

func newNetFakeWorkloadRepo() *netFakeWorkloadRepo {
	return &netFakeWorkloadRepo{byID: map[uuid.UUID]domain.ImageWorkload{}}
}

func (f *netFakeWorkloadRepo) put(w domain.ImageWorkload) {
	f.byID[w.ID] = w
	if w.State == domain.ImageWorkloadStateBooting || w.State == domain.ImageWorkloadStateRunning {
		f.active = append(f.active, w)
	}
}

func (f *netFakeWorkloadRepo) Create(context.Context, *domain.ImageWorkload) error { return nil }
func (f *netFakeWorkloadRepo) Update(context.Context, *domain.ImageWorkload) error { return nil }
func (f *netFakeWorkloadRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.ImageWorkload, error) {
	w, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrWorkloadNotFound
	}
	return &w, nil
}
func (f *netFakeWorkloadRepo) FindActive(context.Context) ([]domain.ImageWorkload, error) {
	return f.active, nil
}
func (f *netFakeWorkloadRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *netFakeWorkloadRepo) FindByIdempotencyKey(context.Context, string, string) (*domain.ImageWorkload, error) {
	return nil, domain.ErrWorkloadNotFound
}
func (f *netFakeWorkloadRepo) IsDuplicateKey(error) bool { return false }

// netRecordingBooter records the teardown handles it was called with.
type netRecordingBooter struct {
	mu          sync.Mutex
	decommonned []ImageDecommissionInput
	err         error
}

var _ ImageBooter = (*netRecordingBooter)(nil)

func (b *netRecordingBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, errors.New("not used")
}
func (b *netRecordingBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, errors.New("not used")
}
func (b *netRecordingBooter) Decommission(_ context.Context, in ImageDecommissionInput) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decommonned = append(b.decommonned, in)
	return b.err
}
func (b *netRecordingBooter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.decommonned)
}

type netRecordingReclaimer struct {
	mu       sync.Mutex
	reclaims []domain.NetLease
	err      error
}

var _ NetLeaseReclaimer = (*netRecordingReclaimer)(nil)

func (r *netRecordingReclaimer) ReclaimLeaseArtifacts(_ context.Context, lease domain.NetLease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reclaims = append(r.reclaims, lease)
	return r.err
}
func (r *netRecordingReclaimer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reclaims)
}

// ─────────────────────────────────────────────────────────────────────
// Allocator
// ─────────────────────────────────────────────────────────────────────

func TestFleetNetAllocatorAllocatesLowestFreeSlot(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	host := uuid.New()
	alloc := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)

	first, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// The live-continuity coordinates: the first allocation on the ordinal-0 host
	// must be exactly what the existing resident replica is running on.
	if first.NetIndex != 1 || first.GuestIP != "10.201.0.6" || first.TapName != "img1" || first.VMUID != 100001 {
		t.Fatalf("first lease = {index:%d guest:%s tap:%s uid:%d}, want {1 10.201.0.6 img1 100001}",
			first.NetIndex, first.GuestIP, first.TapName, first.VMUID)
	}
	second, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerWorkload, uuid.New())
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if second.LocalSlot != 2 {
		t.Fatalf("second slot = %d, want 2", second.LocalSlot)
	}
}

// A retried boot must re-use ITS OWN addressing. Allocating a second lease would
// burn the first slot forever (nothing would ever release it) and leave two rows
// describing one owner.
func TestFleetNetAllocatorIsIdempotentPerOwner(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	host := uuid.New()
	alloc := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)
	owner := uuid.New()

	first, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, owner)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	again, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, owner)
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	if again.ID != first.ID || again.NetIndex != first.NetIndex {
		t.Fatalf("re-Acquire returned a DIFFERENT lease: %+v vs %+v", again, first)
	}
	if repo.count() != 1 {
		t.Fatalf("lease rows = %d, want 1", repo.count())
	}
}

// A lost race must be retried with the NEXT slot, never assumed-successful. The
// fence is the DB's; the allocator's job is to respect the refusal.
func TestFleetNetAllocatorRetriesOnConflict(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	// Slots 1 and 2 lose a race once each (another process won them).
	repo.conflictSlots[1] = 1
	repo.conflictSlots[2] = 1
	host := uuid.New()
	alloc := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)

	lease, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Slot 1 conflicted, then slot 1 was retried-past (it is in `tried`), so slot 2,
	// which also conflicted, then slot 3.
	if lease.LocalSlot != 3 {
		t.Fatalf("slot after two lost races = %d, want 3", lease.LocalSlot)
	}
	if repo.acquires != 3 {
		t.Fatalf("insert attempts = %d, want 3", repo.acquires)
	}
}

// Permanent conflict must end in a REFUSAL, not a hang and not a boot.
func TestFleetNetAllocatorGivesUpOnPermanentConflict(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	for slot := 1; slot <= domain.NetMaxSlot; slot++ {
		repo.conflictSlots[slot] = 1000
	}
	alloc := NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, netTestUIDSpan)
	_, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if !errors.Is(err, domain.ErrNetLeaseConflict) {
		t.Fatalf("Acquire under permanent conflict = %v, want ErrNetLeaseConflict", err)
	}
}

// Exhaustion must REFUSE, never wrap around: wrapping hands a running VM's uid and
// chroot to a second tenant.
func TestFleetNetAllocatorExhaustionRefusesRatherThanWrapping(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	host := uuid.New()
	alloc := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)
	for slot := 1; slot <= domain.NetMaxSlot; slot++ {
		if _, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New()); err != nil {
			t.Fatalf("Acquire slot %d: %v", slot, err)
		}
	}
	_, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if !errors.Is(err, domain.ErrNetLeaseExhausted) {
		t.Fatalf("Acquire past the last slot = %v, want ErrNetLeaseExhausted", err)
	}
	if repo.count() != domain.NetMaxSlot {
		t.Fatalf("lease rows = %d, want %d (nothing may be overwritten)", repo.count(), domain.NetMaxSlot)
	}
}

// The uid span is the jail's isolation budget. A derived uid outside it must refuse
// the boot rather than be handed to the jailer.
func TestFleetNetAllocatorRefusesUIDOutsideSpan(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	// span 1 ⇒ only uid base itself is inside [base, base+1), and slot 1 derives
	// base+1, which is out.
	alloc := NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, 1)
	_, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if !errors.Is(err, domain.ErrNetCoordinateOutOfRange) {
		t.Fatalf("Acquire with a 1-wide uid span = %v, want ErrNetCoordinateOutOfRange", err)
	}
	if repo.count() != 0 {
		t.Fatal("a refused allocation still wrote a lease row")
	}
}

func TestFleetNetAllocatorRefusesWithoutIdentity(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	tests := []struct {
		name     string
		alloc    *FleetNetAllocator
		kind     domain.NetLeaseOwnerKind
		owner    uuid.UUID
		wantSent error
	}{
		{
			name:  "no host identity",
			alloc: NewFleetNetAllocator(repo, uuid.Nil, 0, netTestUIDBase, netTestUIDSpan),
			kind:  domain.NetLeaseOwnerReplica, owner: uuid.New(),
			wantSent: domain.ErrHostNetOrdinalUnset,
		},
		{
			name:  "no assigned ordinal",
			alloc: NewFleetNetAllocator(repo, uuid.New(), -1, netTestUIDBase, netTestUIDSpan),
			kind:  domain.NetLeaseOwnerReplica, owner: uuid.New(),
			wantSent: domain.ErrHostNetOrdinalUnset,
		},
		{
			name:  "no owner kind",
			alloc: NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, netTestUIDSpan),
			kind:  "", owner: uuid.New(),
			wantSent: domain.ErrNetCoordinateOutOfRange,
		},
		{
			name:  "no owner id",
			alloc: NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, netTestUIDSpan),
			kind:  domain.NetLeaseOwnerReplica, owner: uuid.Nil,
			wantSent: domain.ErrNetCoordinateOutOfRange,
		},
		{
			name:  "no lease store",
			alloc: NewFleetNetAllocator(nil, uuid.New(), 0, netTestUIDBase, netTestUIDSpan),
			kind:  domain.NetLeaseOwnerReplica, owner: uuid.New(),
			wantSent: domain.ErrNetPlaneUnreconciled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.alloc.Acquire(context.Background(), tt.kind, tt.owner)
			if !errors.Is(err, tt.wantSent) {
				t.Fatalf("Acquire = %v, want %v", err, tt.wantSent)
			}
		})
	}
	if repo.count() != 0 {
		t.Fatal("a refused allocation wrote a lease row")
	}
}

// An owner holding a lease on ANOTHER host must not have it adopted here: two
// machines would end up configured on one address.
func TestFleetNetAllocatorRefusesForeignHostLease(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	otherHost := uuid.New()
	owner := uuid.New()
	repo.seed(t, otherHost, 5, domain.NetLeaseOwnerReplica, owner, time.Now())

	alloc := NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, netTestUIDSpan)
	_, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, owner)
	if !errors.Is(err, domain.ErrNetLeaseConflict) {
		t.Fatalf("Acquire of a foreign-host lease = %v, want ErrNetLeaseConflict", err)
	}
}

// 64 concurrent acquires through two allocator instances must yield 64 distinct
// slots. This is the property the deleted in-memory map could not provide across
// processes; here it proves the allocator does not fight the fence.
func TestFleetNetAllocatorConcurrentAcquiresAreDistinct(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	host := uuid.New()
	a := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)
	b := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)

	const n = 64
	var wg sync.WaitGroup
	results := make([]domain.NetLease, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alloc := a
			if i%2 == 1 {
				alloc = b
			}
			results[i], errs[i] = alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
		}(i)
	}
	wg.Wait()

	seenSlot := map[int]bool{}
	seenIndex := map[int]bool{}
	seenUID := map[int]bool{}
	seenTap := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Acquire #%d: %v", i, err)
		}
		l := results[i]
		if seenSlot[l.LocalSlot] || seenIndex[l.NetIndex] || seenUID[l.VMUID] || seenTap[l.TapName] {
			t.Fatalf("duplicate allocation #%d: slot=%d index=%d uid=%d tap=%s", i, l.LocalSlot, l.NetIndex, l.VMUID, l.TapName)
		}
		seenSlot[l.LocalSlot], seenIndex[l.NetIndex], seenUID[l.VMUID], seenTap[l.TapName] = true, true, true, true
	}
	if repo.count() != n {
		t.Fatalf("lease rows = %d, want %d", repo.count(), n)
	}
}

func TestFleetNetAllocatorRelease(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	host := uuid.New()
	alloc := NewFleetNetAllocator(repo, host, 0, netTestUIDBase, netTestUIDSpan)
	owner := uuid.New()
	if _, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, owner); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := alloc.Release(context.Background(), domain.NetLeaseOwnerReplica, owner); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Idempotent: teardown may run twice and must not start failing.
	if err := alloc.Release(context.Background(), domain.NetLeaseOwnerReplica, owner); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if repo.count() != 0 {
		t.Fatalf("lease rows after release = %d, want 0", repo.count())
	}
	// The freed slot is re-usable — the whole point of releasing.
	lease, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if err != nil || lease.LocalSlot != 1 {
		t.Fatalf("re-Acquire after release = slot %d, %v; want slot 1", lease.LocalSlot, err)
	}
}

// A release with no owner identity must REFUSE rather than guess a target: guessing
// (by index, say) could free a DIFFERENT live VM's addressing, which the next boot
// would then re-use underneath it.
func TestFleetNetAllocatorReleaseRefusesWithoutOwner(t *testing.T) {
	repo := newNetFakeLeaseRepo()
	alloc := NewFleetNetAllocator(repo, uuid.New(), 0, netTestUIDBase, netTestUIDSpan)
	if err := alloc.Release(context.Background(), domain.NetLeaseOwnerReplica, uuid.Nil); !errors.Is(err, domain.ErrNetCoordinateOutOfRange) {
		t.Fatalf("Release without an owner id = %v, want ErrNetCoordinateOutOfRange", err)
	}
	if err := alloc.Release(context.Background(), "", uuid.New()); !errors.Is(err, domain.ErrNetCoordinateOutOfRange) {
		t.Fatalf("Release without an owner kind = %v, want ErrNetCoordinateOutOfRange", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Boot-time reconcile — the decision table
// ─────────────────────────────────────────────────────────────────────

type reconcileFixture struct {
	leases    *netFakeLeaseRepo
	hosts     *netFakeHostRepo
	replicas  *netFakeReplicaRepo
	workloads *netFakeWorkloadRepo
	booter    *netRecordingBooter
	reclaimer *netRecordingReclaimer
	host      uuid.UUID
	uc        *FleetNetLeaseReconciler
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	return newReconcileFixtureOn(t, 0)
}

// newReconcileFixtureOn builds the fixture for a host on a given ordinal. A
// non-zero ordinal is the SECOND fleet host — the case a single-host fleet can
// never exercise.
func newReconcileFixtureOn(t *testing.T, ord int) *reconcileFixture {
	t.Helper()
	hostID := uuid.New()
	f := &reconcileFixture{
		leases:    newNetFakeLeaseRepo(),
		hosts:     &netFakeHostRepo{host: &domain.Host{ID: hostID, NetOrdinal: &ord}},
		replicas:  newNetFakeReplicaRepo(),
		workloads: newNetFakeWorkloadRepo(),
		booter:    &netRecordingBooter{},
		reclaimer: &netRecordingReclaimer{},
		host:      hostID,
	}
	f.uc = NewFleetNetLeaseReconciler(f.leases, f.hosts, f.replicas, f.workloads,
		f.booter, f.reclaimer, hostID, netTestUIDBase)
	return f
}

// replicaAt builds a replica whose recorded addressing matches a lease.
// netPID is the pointer-int helper the replica/workload rows need for their
// recorded VMM pid.
func netPID(v int) *int { return &v }

func replicaAt(id uuid.UUID, hostID uuid.UUID, lease domain.NetLease, state domain.ReplicaState, pid *int) domain.Replica {
	h := hostID
	return domain.Replica{
		ID: id, HostID: &h, State: state, PID: pid,
		GuestIP: lease.GuestIP, TapName: lease.TapName, NetIndex: lease.NetIndex,
		SocketPath: "/srv/fc/" + id.String() + ".sock", RootfsPath: "/srv/work/" + id.String() + "/rootfs.ext4",
	}
}

func TestFleetNetLeaseReconcileDecisionTable(t *testing.T) {
	// A pid that is alive for the whole test, and one that is not.
	alive := 4242
	dead := 4243
	origAlive := processAlive
	processAlive = func(pid int) bool { return pid == alive }
	defer func() { processAlive = origAlive }()

	tests := []struct {
		name string
		// setup returns nothing; it seeds the fixture.
		setup       func(t *testing.T, f *reconcileFixture)
		wantErr     error
		wantAdopted int
		wantTorn    int
		wantReclaim int
		wantLeft    int
		// wantLeaseHeld asserts the lease survived (fail-closed must NOT delete it).
		wantLeaseHeld bool
	}{
		{
			name: "owner row is gone: reclaim",
			setup: func(t *testing.T, f *reconcileFixture) {
				f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, uuid.New(), time.Now().Add(-time.Hour))
			},
			wantReclaim: 1,
		},
		{
			name: "replica is dead but its VMM was never stopped: tear down, then reclaim",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateDead, netPID(alive)))
			},
			wantTorn: 1, wantReclaim: 1,
		},
		{
			name: "workload exited: tear down, then reclaim",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerWorkload, id, time.Now().Add(-time.Hour))
				f.workloads.put(domain.ImageWorkload{
					ID: id, State: domain.ImageWorkloadStateExited, PID: netPID(alive),
					GuestIP: lease.GuestIP, TapName: lease.TapName, NetIndex: lease.NetIndex,
				})
			},
			wantTorn: 1, wantReclaim: 1,
		},
		{
			name: "occupying but its recorded pid is gone: tear down the residue, then reclaim",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(dead)))
			},
			wantTorn: 1, wantReclaim: 1,
		},
		{
			name: "live VM whose addresses match its lease: adopt",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(alive)))
			},
			wantAdopted: 1, wantLeaseHeld: true,
		},
		{
			name: "live VM whose addresses DISAGREE with its lease: fail closed",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				r := replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(alive))
				r.GuestIP = "10.201.0.10" // another /30 entirely
				f.replicas.put(r)
			},
			wantErr: domain.ErrNetPlaneUnreconciled, wantLeaseHeld: true,
		},
		{
			name: "live VM whose TAP disagrees with its lease: fail closed",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				r := replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(alive))
				r.TapName = "img7"
				f.replicas.put(r)
			},
			wantErr: domain.ErrNetPlaneUnreconciled, wantLeaseHeld: true,
		},
		{
			name: "boot in flight (no pid yet, inside the grace): leave it alone",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now())
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateBooting, nil))
			},
			wantLeft: 1, wantLeaseHeld: true,
		},
		{
			name: "never recorded a pid and past the grace: reclaim",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id,
					time.Now().Add(-leaseAdoptGrace-time.Minute))
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateBooting, nil))
			},
			wantReclaim: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			tt.setup(t, f)

			report, err := f.uc.Reconcile(context.Background())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Reconcile = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if report.Adopted != tt.wantAdopted {
				t.Errorf("adopted = %d, want %d", report.Adopted, tt.wantAdopted)
			}
			if report.TornDown != tt.wantTorn {
				t.Errorf("torn down = %d, want %d", report.TornDown, tt.wantTorn)
			}
			if report.Reclaimed != tt.wantReclaim {
				t.Errorf("reclaimed = %d, want %d", report.Reclaimed, tt.wantReclaim)
			}
			if report.Left != tt.wantLeft {
				t.Errorf("left = %d, want %d", report.Left, tt.wantLeft)
			}
			if f.booter.count() != tt.wantTorn {
				t.Errorf("teardown calls = %d, want %d", f.booter.count(), tt.wantTorn)
			}
			if f.reclaimer.count() != tt.wantReclaim {
				t.Errorf("artifact reclaims = %d, want %d", f.reclaimer.count(), tt.wantReclaim)
			}
			// ⚠ FAIL-CLOSED MUST NOT DELETE. An unexplained lease is the only record
			// of what a running VM holds; deleting it would free the slot for the next
			// boot, which is precisely the collision the refusal exists to prevent.
			if held := f.leases.count(); (held > 0) != tt.wantLeaseHeld {
				t.Errorf("leases still held = %d, want held=%v", held, tt.wantLeaseHeld)
			}
		})
	}
}

// A teardown target is decommissioned through the OWNER's recorded handle (pid,
// socket, rootfs) — not through the lease — because that handle is what can
// actually stop the VM.
func TestFleetNetLeaseReconcileTearsDownWithTheOwnersHandle(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	f := newReconcileFixture(t)
	id := uuid.New()
	lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
	replica := replicaAt(id, f.host, lease, domain.ReplicaStateDead, netPID(4242))
	f.replicas.put(replica)

	if _, err := f.uc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.booter.decommonned) != 1 {
		t.Fatalf("teardown calls = %d, want 1", len(f.booter.decommonned))
	}
	got := f.booter.decommonned[0]
	if got.OwnerKind != domain.NetLeaseOwnerReplica || got.OwnerID != id {
		t.Errorf("teardown owner = %s/%s, want replica/%s", got.OwnerKind, got.OwnerID, id)
	}
	if got.PID != 4242 || got.SocketPath != replica.SocketPath || got.RootfsPath != replica.RootfsPath {
		t.Errorf("teardown handle = %+v, want the replica's recorded pid/socket/rootfs", got)
	}
}

// A VM that refuses to die must NOT strand its slot: the reclaim continues and the
// lease is released, mirroring the booter's "teardown is never blockable" rule.
func TestFleetNetLeaseReconcileReclaimsEvenWhenTeardownFails(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	f := newReconcileFixture(t)
	f.booter.err = errors.New("vmm will not stop")
	id := uuid.New()
	lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
	f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateDead, netPID(4242)))

	report, err := f.uc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Reclaimed != 1 || f.leases.count() != 0 {
		t.Fatalf("reclaimed=%d leases=%d, want 1 and 0", report.Reclaimed, f.leases.count())
	}
}

// THE FAIL-OPEN NEGATIVE CONTROL. A store error must return an error and reclaim
// NOTHING. The bug this whole change exists to fix was exactly this shape: a
// startup query failed, the failure was logged, and the process continued with an
// empty picture of what was held.
func TestFleetNetLeaseReconcileStoreErrorsReclaimNothing(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	boom := errors.New("connection reset by peer")
	tests := []struct {
		name     string
		sabotage func(f *reconcileFixture)
		// ownerState decides whether the reconcile would otherwise ADOPT (resident)
		// or RECLAIM (dead) this lease — the release path only runs on the latter.
		ownerState domain.ReplicaState
	}{
		{"listing this host's leases fails", func(f *reconcileFixture) { f.leases.listErr = boom }, domain.ReplicaStateResident},
		{"loading this host's row fails", func(f *reconcileFixture) { f.hosts.err = boom }, domain.ReplicaStateResident},
		{"loading a lease's owner fails", func(f *reconcileFixture) { f.replicas.findErr = boom }, domain.ReplicaStateResident},
		{"listing replicas by state fails", func(f *reconcileFixture) { f.replicas.listErr = boom }, domain.ReplicaStateResident},
		{"releasing a reclaimed lease fails", func(f *reconcileFixture) { f.leases.releaseErr = boom }, domain.ReplicaStateDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			id := uuid.New()
			lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
			f.replicas.put(replicaAt(id, f.host, lease, tt.ownerState, netPID(4242)))
			tt.sabotage(f)

			report, err := f.uc.Reconcile(context.Background())
			if err == nil {
				t.Fatal("Reconcile returned nil on a store error — the plane would come up believing nothing is held")
			}
			if report.Reclaimed != 0 {
				t.Errorf("reclaimed = %d on a store error, want 0", report.Reclaimed)
			}
		})
	}
}

// An occupying row with NO lease is a proven collision: two rows once claimed one
// index. It refuses to serve and NAMES the row, because guessing which one the
// running VM is would be guessing about customer data.
func TestFleetNetLeaseReconcileRefusesLeaselessOccupyingRow(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	f := newReconcileFixture(t)
	// The winner holds slot 1's lease.
	winnerID := uuid.New()
	lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, winnerID, time.Now().Add(-time.Hour))
	f.replicas.put(replicaAt(winnerID, f.host, lease, domain.ReplicaStateResident, netPID(4242)))
	// The loser claims the same index with no lease.
	loserID := uuid.New()
	loser := replicaAt(loserID, f.host, lease, domain.ReplicaStateResident, netPID(4242))
	f.replicas.put(loser)

	_, err := f.uc.Reconcile(context.Background())
	if !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("Reconcile = %v, want ErrNetPlaneUnreconciled", err)
	}
	// Both sides of the collision must be named — the holder and the claimant.
	if !strings.Contains(err.Error(), loserID.String()) {
		t.Errorf("error does not name the leaseless row %s: %v", loserID, err)
	}
	if !strings.Contains(err.Error(), winnerID.String()) {
		t.Errorf("error does not name the row that HOLDS the address (%s): %v", winnerID, err)
	}
	if f.leases.count() != 1 {
		t.Errorf("leases = %d, want the winner's lease left intact", f.leases.count())
	}
}

// A lease that does not match its OWN recorded coordinates (a hand-edit, a bad
// backfill, a changed APP_FC_VM_UID_BASE) fences the wrong uid/address — it looks
// like protection while protecting nothing — so the host refuses to boot.
func TestFleetNetLeaseReconcileRefusesSelfInconsistentLease(t *testing.T) {
	f := newReconcileFixture(t)
	id := uuid.New()
	lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
	// Corrupt the recorded uid, as a changed uid base would.
	f.leases.mu.Lock()
	f.leases.leases[0].VMUID = lease.VMUID + 500
	f.leases.mu.Unlock()

	if _, err := f.uc.Reconcile(context.Background()); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("Reconcile = %v, want ErrNetPlaneUnreconciled", err)
	}
	if f.leases.count() != 1 {
		t.Error("a fail-closed reconcile deleted the lease")
	}
}

// No host identity, and no assigned ordinal, are both fatal: neither can be scoped
// or allocated from, and defaulting the ordinal to 0 would alias another host's
// whole addressing block.
func TestFleetNetLeaseReconcileRefusesWithoutHostOrOrdinal(t *testing.T) {
	t.Run("no host identity", func(t *testing.T) {
		f := newReconcileFixture(t)
		uc := NewFleetNetLeaseReconciler(f.leases, f.hosts, f.replicas, f.workloads,
			f.booter, f.reclaimer, uuid.Nil, netTestUIDBase)
		if _, err := uc.Reconcile(context.Background()); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
			t.Fatalf("Reconcile = %v, want ErrNetPlaneUnreconciled", err)
		}
	})
	t.Run("no assigned ordinal", func(t *testing.T) {
		f := newReconcileFixture(t)
		f.hosts.host.NetOrdinal = nil
		if _, err := f.uc.Reconcile(context.Background()); !errors.Is(err, domain.ErrHostNetOrdinalUnset) {
			t.Fatalf("Reconcile = %v, want ErrHostNetOrdinalUnset", err)
		}
	})
	t.Run("no host row at all", func(t *testing.T) {
		f := newReconcileFixture(t)
		f.hosts.host = nil
		if _, err := f.uc.Reconcile(context.Background()); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
			t.Fatalf("Reconcile = %v, want ErrNetPlaneUnreconciled", err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────
// Verify — the per-boot precondition (a refusal that clears itself)
// ─────────────────────────────────────────────────────────────────────

// ⚠ A REFUSAL MUST NOT LATCH. Every condition Verify refuses on must stop refusing
// on the very next pass once its cause is gone. Live, the boot-time verdict was
// frozen into the booter seam and a second fleet host kept refusing every boot for
// 10+ minutes after the offending replica row had been deleted from both tables —
// only `systemctl restart runtime-service` cleared it.
//
// Each case therefore asserts BOTH directions on the same fixture: refused while
// the cause exists (the fence is real), served after `resolve` (the fence is a
// precondition, not a latch).
func TestFleetNetLeaseVerifyIsAPreconditionNotALatch(t *testing.T) {
	alive := 5150
	origAlive := processAlive
	processAlive = func(pid int) bool { return pid == alive }
	defer func() { processAlive = origAlive }()

	tests := []struct {
		name string
		// setup creates the violation; resolve removes its cause.
		setup   func(t *testing.T, f *reconcileFixture)
		resolve func(t *testing.T, f *reconcileFixture)
		wantErr error
	}{
		{
			name: "an occupying row that holds no lease",
			setup: func(t *testing.T, f *reconcileFixture) {
				winner := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, winner, time.Now().Add(-time.Hour))
				f.replicas.put(replicaAt(winner, f.host, lease, domain.ReplicaStateResident, netPID(alive)))
				loser := uuid.New()
				f.replicas.put(replicaAt(loser, f.host, lease, domain.ReplicaStateResident, netPID(alive)))
			},
			// The operator removed the losing row (what actually happened on .245).
			resolve: func(t *testing.T, f *reconcileFixture) {
				f.replicas.byState[domain.ReplicaStateResident] = f.replicas.byState[domain.ReplicaStateResident][:1]
			},
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			name: "a live workload that holds no lease",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				f.workloads.put(domain.ImageWorkload{
					ID: id, State: domain.ImageWorkloadStateRunning, PID: netPID(alive),
					GuestIP: "10.201.0.6", TapName: "img1", NetIndex: 1,
				})
			},
			resolve: func(t *testing.T, f *reconcileFixture) { f.workloads.active = nil },
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			name: "a lease that is not self-consistent",
			setup: func(t *testing.T, f *reconcileFixture) {
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, uuid.New(), time.Now().Add(-time.Hour))
				f.leases.mu.Lock()
				f.leases.leases[0].VMUID = lease.VMUID + 500
				f.leases.mu.Unlock()
			},
			// A hand-edit is corrected, or the lease is released.
			resolve: func(t *testing.T, f *reconcileFixture) {
				f.leases.mu.Lock()
				f.leases.leases = nil
				f.leases.mu.Unlock()
			},
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			name: "a RUNNING VM whose row and lease disagree about its addressing",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				r := replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(alive))
				r.TapName = "img7"
				f.replicas.put(r)
			},
			// The VM was torn down, so nothing is running at the disputed address.
			resolve: func(t *testing.T, f *reconcileFixture) {
				f.replicas.byID = map[uuid.UUID]domain.Replica{}
				f.replicas.byState = map[domain.ReplicaState][]domain.Replica{}
			},
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			name:  "the host has no assigned ordinal",
			setup: func(t *testing.T, f *reconcileFixture) { f.hosts.host.NetOrdinal = nil },
			resolve: func(t *testing.T, f *reconcileFixture) {
				ord := 0
				f.hosts.host.NetOrdinal = &ord
			},
			wantErr: domain.ErrHostNetOrdinalUnset,
		},
		{
			name:    "the lease store cannot be read",
			setup:   func(t *testing.T, f *reconcileFixture) { f.leases.listErr = errors.New("connection reset by peer") },
			resolve: func(t *testing.T, f *reconcileFixture) { f.leases.listErr = nil },
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			tt.setup(t, f)

			if err := f.uc.Verify(context.Background()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify while the cause exists = %v, want %v", err, tt.wantErr)
			}
			tt.resolve(t, f)
			if err := f.uc.Verify(context.Background()); err != nil {
				t.Fatalf("Verify after the cause was resolved = %v, want nil (the refusal LATCHED)", err)
			}
			// Verify decides; it never repairs. Anything it mutated would make the
			// per-boot check a write path on a live host's addressing.
			if f.booter.count() != 0 || f.reclaimer.count() != 0 || len(f.leases.releases) != 0 {
				t.Errorf("Verify mutated the host: teardowns=%d reclaims=%d releases=%d",
					f.booter.count(), f.reclaimer.count(), len(f.leases.releases))
			}
		})
	}
}

// Residue is not a refusal. A lease whose owner is gone, or whose VM is not
// running, holds a SLOT (which the allocator will not reuse until the next
// boot-time reconcile releases it) but nothing is running at that address, so it
// cannot collide with a boot — refusing on it would strand the host on its own
// garbage.
func TestFleetNetLeaseVerifyServesBootsOverReclaimableResidue(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return false }
	defer func() { processAlive = origAlive }()

	f := newReconcileFixture(t)
	// A lease whose owner row is gone.
	f.leases.seed(t, f.host, 1, domain.NetLeaseOwnerReplica, uuid.New(), time.Now().Add(-time.Hour))
	// A lease whose owner exists but whose VMM is dead.
	id := uuid.New()
	lease := f.leases.seed(t, f.host, 2, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
	f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(4243)))

	if err := f.uc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify over reclaimable residue = %v, want nil", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The second fleet host — indices allocated before the plane had a host term
// ─────────────────────────────────────────────────────────────────────

// legacyReplicaAt builds a replica addressed the way the PRE-PLANE allocator did:
// one host-global index, with the TAP named after the index itself (there was no
// host term, so every host handed out low indices).
func legacyReplicaAt(id, hostID uuid.UUID, netIndex int, state domain.ReplicaState, pid *int) domain.Replica {
	h := hostID
	return domain.Replica{
		ID: id, HostID: &h, State: state, PID: pid,
		GuestIP:  fmt.Sprintf("10.201.%d.%d", (netIndex*4)>>8, ((netIndex*4)&0xff)+2),
		TapName:  fmt.Sprintf("img%d", netIndex),
		NetIndex: netIndex,
	}
}

// ⚠ A LEGACY INDEX MUST NOT BRICK A NON-ZERO-ORDINAL HOST. Observed live: an
// ordinary image-boot app on the second fleet host (.245, ordinal 1) carried
// net_index=1 — an index inside ORDINAL 0's block, minted before the plane had a
// host term — with no lease, and the leaseless rule then refused every data-VM
// provision on that host. Such a row can never obtain a lease here (net_index is
// fleet-globally unique and that one belongs to another host's block, so minting it
// would adopt another host's address), so the refusal could never clear.
//
// The fence is narrowed by exactly one fact and no more: whether that row's VM is
// still RUNNING. A running one can still collide on the host-local names its old
// index derived (TAP, jail id, uid), so it is still refused; stale residue holds
// nothing (the jail dir is cleared by prepare(), a stale TAP by createTap).
func TestFleetNetLeaseVerifyOnASecondHostWithALegacyIndex(t *testing.T) {
	alive := 6060
	origAlive := processAlive
	processAlive = func(pid int) bool { return pid == alive }
	defer func() { processAlive = origAlive }()

	const ordinal = 1

	tests := []struct {
		name    string
		setup   func(t *testing.T, f *reconcileFixture)
		wantErr error
	}{
		{
			// The .245 case: the host must be able to provision.
			name: "a stale legacy out-of-block replica does not refuse boots",
			setup: func(t *testing.T, f *reconcileFixture) {
				f.replicas.put(legacyReplicaAt(uuid.New(), f.host, 1, domain.ReplicaStateResident, netPID(4243)))
			},
		},
		{
			name: "a legacy out-of-block replica that never recorded a pid does not refuse boots",
			setup: func(t *testing.T, f *reconcileFixture) {
				f.replicas.put(legacyReplicaAt(uuid.New(), f.host, 1, domain.ReplicaStateDead, nil))
			},
		},
		{
			// The hole the exclusion must NOT open: a live VM whose old index derived
			// tap/jail/uid this host can hand out again.
			name: "a RUNNING legacy out-of-block replica still refuses boots",
			setup: func(t *testing.T, f *reconcileFixture) {
				f.replicas.put(legacyReplicaAt(uuid.New(), f.host, 1, domain.ReplicaStateResident, netPID(alive)))
			},
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			// The fence itself, unchanged: an index INSIDE this host's own block with
			// no lease is the proven-collision signal, live VM or not.
			name: "an in-block index with no lease still refuses boots",
			setup: func(t *testing.T, f *reconcileFixture) {
				r := legacyReplicaAt(uuid.New(), f.host, ordinal*domain.NetSlotStride+5, domain.ReplicaStateResident, netPID(4243))
				f.replicas.put(r)
			},
			wantErr: domain.ErrNetPlaneUnreconciled,
		},
		{
			// A properly leased VM on this host's block is the healthy case, and it
			// must stay healthy alongside the legacy row above.
			name: "a leased in-block replica plus stale legacy residue serves boots",
			setup: func(t *testing.T, f *reconcileFixture) {
				id := uuid.New()
				lease := f.leases.seedAt(t, f.host, ordinal, 3, domain.NetLeaseOwnerReplica, id, time.Now().Add(-time.Hour))
				f.replicas.put(replicaAt(id, f.host, lease, domain.ReplicaStateResident, netPID(alive)))
				f.replicas.put(legacyReplicaAt(uuid.New(), f.host, 1, domain.ReplicaStateResident, netPID(4243)))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReconcileFixtureOn(t, ordinal)
			tt.setup(t, f)

			err := f.uc.Verify(context.Background())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Verify = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify = %v, want nil (this host cannot provision anything)", err)
			}
			// The same rule governs the startup pass, or a restart would brick the host
			// again.
			if _, rerr := f.uc.Reconcile(context.Background()); rerr != nil {
				t.Fatalf("Reconcile = %v, want nil", rerr)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// The fail-loud booter's asymmetry
// ─────────────────────────────────────────────────────────────────────

// An unreconciled plane must refuse every BOOT while still serving TEARDOWN:
// refusing to boot protects data, refusing to tear down only strands a customer's
// running VM with its /30, its lease and its rootfs.
func TestFailLoudImageBooterRefusesBootsButDelegatesTeardown(t *testing.T) {
	real := &netRecordingBooter{}
	shim := FailLoudImageBooter{
		Reason:   fmt.Errorf("%w: reconcile failed", domain.ErrNetPlaneUnreconciled),
		Teardown: real,
	}

	if _, err := shim.BootResident(context.Background(), ImageBootInput{}); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Errorf("BootResident = %v, want ErrNetPlaneUnreconciled", err)
	}
	if _, err := shim.BootTest(context.Background(), ImageBootInput{}); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Errorf("BootTest = %v, want ErrNetPlaneUnreconciled", err)
	}
	owner := uuid.New()
	if err := shim.Decommission(context.Background(), ImageDecommissionInput{
		OwnerKind: domain.NetLeaseOwnerReplica, OwnerID: owner,
	}); err != nil {
		t.Fatalf("Decommission through the shim: %v", err)
	}
	if real.count() != 1 || real.decommonned[0].OwnerID != owner {
		t.Fatalf("teardown was not delegated to the real booter: %+v", real.decommonned)
	}
}

// The zero value keeps its original meaning: off a firecracker host there is
// nothing real to delegate to, so every call — teardown included — is refused with
// ErrImageBootUnavailable.
func TestFailLoudImageBooterZeroValueRefusesEverything(t *testing.T) {
	shim := FailLoudImageBooter{}
	if _, err := shim.BootResident(context.Background(), ImageBootInput{}); !errors.Is(err, domain.ErrImageBootUnavailable) {
		t.Errorf("BootResident = %v, want ErrImageBootUnavailable", err)
	}
	if err := shim.Decommission(context.Background(), ImageDecommissionInput{}); !errors.Is(err, domain.ErrImageBootUnavailable) {
		t.Errorf("Decommission = %v, want ErrImageBootUnavailable", err)
	}
}
