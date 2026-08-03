package usecase

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// volRepoFake is a minimal stateful VolumeRepository for the manager tests.
type volRepoFake struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.Volume
	// hostBinds counts BindHostAffinity calls, and createErr / creates let a test
	// drive the create-compensation matrix. Side effects are counted, not just
	// return values: "returned the sentinel" and "wrote nothing" are different
	// claims and only the second is the fence.
	hostBinds int
	creates   int
	createErr error
	updates   int
	// listErr fails the AUTHORITATIVE re-read the create-compensation saga makes.
	listErr error
	// onCreate runs inside a failing Create, so a test can install the committed
	// "winner" the compensation logic will then re-read.
	onCreate func(attempt *domain.Volume)
	// beforeHostBind runs inside BindHostAffinity, so a test can make another host
	// win the CAS in the window this one is adopting.
	beforeHostBind func()
}

// put installs a row directly (the committed winner of a race).
func (f *volRepoFake) put(v *domain.Volume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	cp := *v
	f.store[v.ID] = &cp
}

func newVolRepoFake(vols ...*domain.Volume) *volRepoFake {
	r := &volRepoFake{store: map[uuid.UUID]*domain.Volume{}}
	for _, v := range vols {
		cp := *v
		r.store[v.ID] = &cp
	}
	return r
}
func (f *volRepoFake) Create(_ context.Context, v *domain.Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createErr != nil {
		if f.onCreate != nil {
			f.mu.Unlock()
			f.onCreate(v)
			f.mu.Lock()
		}
		return f.createErr
	}
	cp := *v
	f.store[v.ID] = &cp
	return nil
}
func (f *volRepoFake) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.Volume
	for _, v := range f.store {
		if v.AppID != nil && *v.AppID == appID {
			out = append(out, *v)
		}
	}
	return out, nil
}
func (f *volRepoFake) FindByID(_ context.Context, id uuid.UUID) (*domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.store[id]
	if !ok {
		return nil, domain.ErrVolumeNotFound
	}
	cp := *v
	return &cp, nil
}
func (f *volRepoFake) Update(_ context.Context, v *domain.Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	cp := *v
	f.store[v.ID] = &cp
	return nil
}
func (f *volRepoFake) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, id)
	return nil
}

// ListByResource mirrors the postgres filter: the claim's OWNERSHIP stamp
// (resource_id), never the current app attachment.
func (f *volRepoFake) ListByResource(_ context.Context, resourceID uuid.UUID) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Volume
	for _, v := range f.store {
		if v.ResourceID != nil && *v.ResourceID == resourceID {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (f *volRepoFake) ListByHost(_ context.Context, hostID uuid.UUID) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Volume
	for _, v := range f.store {
		if v.HostAffinity != nil && *v.HostAffinity == hostID {
			out = append(out, *v)
		}
	}
	return out, nil
}
func (f *volRepoFake) BindVolumesToResource(_ context.Context, appID, resourceID uuid.UUID) (repository.VolumeBindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var mine []*domain.Volume
	for _, v := range f.store {
		if v.AppID != nil && *v.AppID == appID {
			mine = append(mine, v)
		}
	}
	if len(mine) == 0 {
		return repository.VolumeBindResult{Outcome: repository.VolumeBindNoVolumes}, nil
	}
	for _, v := range mine {
		if v.ResourceID != nil && *v.ResourceID != resourceID {
			return repository.VolumeBindResult{
				Outcome:          repository.VolumeBindConflict,
				ConflictVolumeID: v.ID,
				ConflictOwner:    *v.ResourceID,
			}, nil
		}
	}
	stamped := 0
	for _, v := range mine {
		if v.ResourceID == nil {
			res := resourceID
			v.ResourceID = &res
			v.UpdatedAt = time.Now().UTC()
			stamped++
		}
	}
	if stamped > 0 {
		return repository.VolumeBindResult{Outcome: repository.VolumeBindBound}, nil
	}
	return repository.VolumeBindResult{Outcome: repository.VolumeBindAlreadyBound}, nil
}

// BindHostAffinity mirrors the postgres CAS: compare and set under one lock, and
// report which of the three states it found. The mutex stands in for the row
// lock, so the concurrent-adopt test exercises a real serialization point rather
// than a check-then-act it would always win.
func (f *volRepoFake) BindHostAffinity(_ context.Context, volumeID, hostID uuid.UUID) (repository.VolumeHostBindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostBinds++
	if f.beforeHostBind != nil {
		f.beforeHostBind()
	}
	v, ok := f.store[volumeID]
	if !ok {
		return repository.VolumeHostBindResult{}, domain.ErrVolumeNotFound
	}
	if v.HostAffinity != nil {
		if *v.HostAffinity == hostID {
			return repository.VolumeHostBindResult{Outcome: repository.VolumeHostBindAlreadyBound}, nil
		}
		return repository.VolumeHostBindResult{
			Outcome:    repository.VolumeHostBindConflict,
			ActualHost: *v.HostAffinity,
		}, nil
	}
	h := hostID
	v.HostAffinity = &h
	v.UpdatedAt = time.Now().UTC()
	return repository.VolumeHostBindResult{Outcome: repository.VolumeHostBindBound}, nil
}

func (f *volRepoFake) HasUnstampedVolumes(_ context.Context, appID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.store {
		if v.AppID != nil && *v.AppID == appID && v.ResourceID == nil {
			return true, nil
		}
	}
	return false, nil
}
func (f *volRepoFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.store)
}

// recordingBackend records the backing paths passed to Delete and can fail a
// chosen path to prove the loop keeps going.
type recordingBackend struct {
	mu      sync.Mutex
	deleted []string
	failOn  string
}

func (b *recordingBackend) Ensure(context.Context, VolumeEnsureInput) (VolumeEnsureOutput, error) {
	return VolumeEnsureOutput{}, nil
}
func (b *recordingBackend) Delete(_ context.Context, backingPath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, backingPath)
	if backingPath == b.failOn {
		return errors.New("boom")
	}
	return nil
}

// modeBackend records the ensure MODE per volume so the intent EnsureAppVolumes
// declares can be asserted at the call site. It fabricates the same path shape
// the real backend uses.
type modeBackend struct {
	modes    []VolumeEnsureMode
	failWith error
}

func (b *modeBackend) Ensure(_ context.Context, in VolumeEnsureInput) (VolumeEnsureOutput, error) {
	b.modes = append(b.modes, in.Mode)
	if b.failWith != nil {
		return VolumeEnsureOutput{}, b.failWith
	}
	return VolumeEnsureOutput{BackingPath: in.Dir + "/" + in.VolumeID.String() + ".ext4"}, nil
}
func (b *modeBackend) Delete(context.Context, string) error { return nil }

// recordingModeBackend is modeBackend plus a Created flag and a delete log — the
// two facts the create-compensation matrix is decided by.
type recordingModeBackend struct {
	created bool
	modes   []VolumeEnsureMode
	deleted []string
}

func (b *recordingModeBackend) Ensure(_ context.Context, in VolumeEnsureInput) (VolumeEnsureOutput, error) {
	b.modes = append(b.modes, in.Mode)
	return VolumeEnsureOutput{
		BackingPath: in.Dir + "/" + in.VolumeID.String() + ".ext4",
		Created:     b.created && in.Mode == VolumeEnsureCreate,
	}, nil
}

func (b *recordingModeBackend) Delete(_ context.Context, backingPath string) error {
	b.deleted = append(b.deleted, backingPath)
	return nil
}

// TestEnsureAppVolumes_DeclaresIntentPerCallSite is the ledger half of the
// data-loss fix: the backend cannot see fleet_volumes, so the manager must say
// which of the two meanings an absent backing file has. A volume the ledger
// already records is ADOPT (its file is customer data — refuse if gone); a
// volume the ledger has never seen is CREATE (nothing can be lost).
func TestEnsureAppVolumes_DeclaresIntentPerCallSite(t *testing.T) {
	appID := uuid.New()
	tests := []struct {
		name     string
		seeded   *domain.Volume
		wantMode VolumeEnsureMode
	}{
		{"no ledger row for (app, mount) → first provision creates", nil, VolumeEnsureCreate},
		{"ledger already records the volume → attach may only adopt", volAt(appID, "/vol"), VolumeEnsureAdopt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newVolRepoFake()
			if tt.seeded != nil {
				repo = newVolRepoFake(tt.seeded)
			}
			backend := &modeBackend{}
			m := newTestVolumeManager(t, repo, backend, "/vol", nil)

			if _, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}}); err != nil {
				t.Fatalf("EnsureAppVolumes: %v", err)
			}
			if len(backend.modes) != 1 || backend.modes[0] != tt.wantMode {
				t.Fatalf("ensure modes = %v, want [%s]", backend.modes, tt.wantMode)
			}
		})
	}
}

// TestEnsureAppVolumes_MissingBackingFileRefusesProvision proves the refusal is
// carried, not swallowed: a provision whose recorded volume has no file on the
// host must fail with the sentinel the boundary reads, never continue to boot a
// replica over an empty disk.
func TestEnsureAppVolumes_MissingBackingFileRefusesProvision(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(volWithBacking(appID, "/vol/x.ext4"))
	m := newTestVolumeManager(t, repo, &modeBackend{failWith: domain.ErrVolumeBackingFileMissing}, "/vol", nil)

	_, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
	if !errors.Is(err, domain.ErrVolumeBackingFileMissing) {
		t.Fatalf("got %v, want ErrVolumeBackingFileMissing", err)
	}
}

// volAt seeds a volume whose recorded BackingPath is the one the backend derives
// for it (dir + volume id + ".ext4"). EnsureAppVolumes now requires the adopted
// path to EQUAL the recorded one — a divergence means this host's volume
// directory is not the row's — so any test that goes through the adopt path has
// to seed a self-consistent row, exactly as production writes it.
func volAt(appID uuid.UUID, dir string) *domain.Volume {
	id := uuid.New()
	v := volWithBacking(appID, filepath.Join(dir, id.String()+".ext4"))
	v.ID = id
	v.BackingPath = filepath.Join(dir, id.String()+".ext4")
	return v
}

// volWithBacking seeds a volume that lives on THIS host. Every host-fenced verb
// requires a positive affinity, so an unstamped seed would exercise the refusal
// path rather than the behavior each test is about (the nil/foreign cases have
// their own tests).
func volWithBacking(appID uuid.UUID, backing string) *domain.Volume {
	return &domain.Volume{
		ID:           uuid.New(),
		AppID:        &appID,
		SizeMB:       64,
		MountPath:    "/data",
		BackingPath:  backing,
		HostAffinity: hostPtr(testSelfHost),
		Status:       domain.VolumeStatusAvailable,
	}
}

func TestDeleteAppVolumes_DeletesBackingFilesAndRows(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(
		volWithBacking(appID, "/vol/a.ext4"),
		volWithBacking(appID, "/vol/b.ext4"),
	)
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	if err := m.DeleteAppVolumes(context.Background(), appID); err != nil {
		t.Fatalf("DeleteAppVolumes: %v", err)
	}
	if len(backend.deleted) != 2 {
		t.Fatalf("backend.Delete calls = %d, want 2 (%v)", len(backend.deleted), backend.deleted)
	}
	if repo.count() != 0 {
		t.Fatalf("volume rows remaining = %d, want 0", repo.count())
	}
}

func TestDeleteAppVolumes_SkipsEmptyBackingPath(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(
		volWithBacking(appID, ""), // never materialized — no backend call
		volWithBacking(appID, "/vol/c.ext4"),
	)
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	if err := m.DeleteAppVolumes(context.Background(), appID); err != nil {
		t.Fatalf("DeleteAppVolumes: %v", err)
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != "/vol/c.ext4" {
		t.Fatalf("backend.Delete = %v, want only [/vol/c.ext4]", backend.deleted)
	}
	if repo.count() != 0 {
		t.Fatalf("volume rows remaining = %d, want 0", repo.count())
	}
}

func TestDeleteAppVolumes_ContinuesAfterDeleteErrorAndReturnsFirst(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(
		volWithBacking(appID, "/vol/a.ext4"),
		volWithBacking(appID, "/vol/b.ext4"),
	)
	backend := &recordingBackend{failOn: "/vol/a.ext4"}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	err := m.DeleteAppVolumes(context.Background(), appID)
	if err == nil {
		t.Fatalf("want error from failed backing delete, got nil")
	}
	// Both backing files attempted despite the first failing.
	if len(backend.deleted) != 2 {
		t.Fatalf("backend.Delete calls = %d, want 2 (%v)", len(backend.deleted), backend.deleted)
	}
}

// DetachFrom must not revive a volume the D-184 restore owns: the restore
// drains the replica itself, so a detach that flipped `restoring` back to
// `available` would tear down its own boot stand-off.
func TestDetachFrom_PreservesTerminalAndRestoringStatuses(t *testing.T) {
	tests := []struct {
		name string
		from domain.VolumeStatus
		want domain.VolumeStatus
	}{
		{"attached is released", domain.VolumeStatusAttached, domain.VolumeStatusAvailable},
		{"degraded stays degraded", domain.VolumeStatusDegraded, domain.VolumeStatusDegraded},
		{"restoring stays restoring", domain.VolumeStatusRestoring, domain.VolumeStatusRestoring},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appID := uuid.New()
			vol := volWithBacking(appID, "/vol/a.ext4")
			vol.Status = tt.from
			replicaID := uuid.New()
			vol.AttachedReplica = &replicaID
			repo := newVolRepoFake(vol)
			m := newTestVolumeManager(t, repo, &recordingBackend{}, "/vol", nil)

			if err := m.DetachFrom(context.Background(), appID); err != nil {
				t.Fatalf("DetachFrom: %v", err)
			}
			got, _ := repo.FindByID(context.Background(), vol.ID)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q", got.Status, tt.want)
			}
			if got.AttachedReplica != nil {
				t.Fatal("detach must always clear the attachment")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// D-203 — claim ownership: the deletion guard and the write-once bind.
// ─────────────────────────────────────────────────────────────────────

// volClaimLedger answers the ONE ledger read the deletion guard makes. The
// embedded interface is nil on purpose: any other call would be a seam the guard
// is not supposed to use, and a panic says so louder than a zero value.
type volClaimLedger struct {
	repository.FleetResourceRepository
	res *domain.FleetResource
	err error
}

func (l volClaimLedger) GetResourceByHandle(context.Context, uuid.UUID) (*domain.FleetResource, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.res, nil
}

func ownedVolume(appID, resourceID uuid.UUID, backing string) *domain.Volume {
	v := volWithBacking(appID, backing)
	res := resourceID
	v.ResourceID = &res
	return v
}

func liveClaim(id uuid.UUID) *domain.FleetResource {
	return &domain.FleetResource{ID: id, Class: resourceClassPostgres, Tier: resourceTierDedicated}
}

func retiredClaim(id uuid.UUID) *domain.FleetResource {
	res := liveClaim(id)
	at := time.Now().UTC()
	res.DecommissionedAt = &at
	return res
}

// The refusing direction: a volume a LIVE claim owns must survive an app-level
// teardown, and it must survive it UNTOUCHED — the guard runs before the first
// unlink, so neither the backing file nor the row may be gone.
func TestDeleteAppVolumes_RefusesVolumeOwnedByLiveClaim(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(ownedVolume(appID, resID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", volClaimLedger{res: liveClaim(resID)})

	err := m.DeleteAppVolumes(context.Background(), appID)
	if !errors.Is(err, domain.ErrVolumeOwnedByLiveResource) {
		t.Fatalf("got %v, want ErrVolumeOwnedByLiveResource", err)
	}
	if len(backend.deleted) != 0 {
		t.Fatalf("backing files touched = %v, want none", backend.deleted)
	}
	if repo.count() != 1 {
		t.Fatalf("volume rows = %d, want the row untouched", repo.count())
	}
}

// The passing direction: the resource's own snapshot-first teardown tombstones
// the claim BEFORE it calls down, and that stamp is what lets this through.
func TestDeleteAppVolumes_ProceedsWhenClaimTombstoned(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(ownedVolume(appID, resID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", volClaimLedger{res: retiredClaim(resID)})

	if err := m.DeleteAppVolumes(context.Background(), appID); err != nil {
		t.Fatalf("DeleteAppVolumes: %v", err)
	}
	if len(backend.deleted) != 1 {
		t.Fatalf("backing files deleted = %v, want 1", backend.deleted)
	}
	if repo.count() != 0 {
		t.Fatalf("volume rows remaining = %d, want 0", repo.count())
	}
}

func TestDeleteAppVolumes_FailsClosedWhenLedgerUnwired(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(ownedVolume(appID, resID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	err := m.DeleteAppVolumes(context.Background(), appID)
	if !errors.Is(err, domain.ErrVolumeOwnedByLiveResource) {
		t.Fatalf("got %v, want ErrVolumeOwnedByLiveResource", err)
	}
	if len(backend.deleted) != 0 || repo.count() != 1 {
		t.Fatalf("nothing may be reclaimed when the claim cannot be checked (deleted=%v rows=%d)", backend.deleted, repo.count())
	}
}

func TestDeleteAppVolumes_FailsClosedWhenLedgerUnreadable(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(ownedVolume(appID, resID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", volClaimLedger{err: errors.New("ledger down")})

	if err := m.DeleteAppVolumes(context.Background(), appID); err == nil {
		t.Fatal("want a refusal when the claim ledger cannot be read")
	}
	if len(backend.deleted) != 0 || repo.count() != 1 {
		t.Fatalf("nothing may be reclaimed on an unreadable ledger (deleted=%v rows=%d)", backend.deleted, repo.count())
	}
}

// The owner row is GONE while the volume still names it: since the 0024 FK that
// state cannot arise except by manual surgery, so it is a corruption signal, not
// a licence to reclaim. Refuse, and leave both the file and the row untouched.
func TestDeleteAppVolumes_RefusesWhenOwnerRowGone(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(ownedVolume(appID, resID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", volClaimLedger{err: domain.ErrResourceNotFound})

	err := m.DeleteAppVolumes(context.Background(), appID)
	if !errors.Is(err, domain.ErrVolumeOwnedByLiveResource) {
		t.Fatalf("got %v, want ErrVolumeOwnedByLiveResource", err)
	}
	if len(backend.deleted) != 0 {
		t.Fatalf("backing files touched = %v, want none", backend.deleted)
	}
	if repo.count() != 1 {
		t.Fatalf("volume rows = %d, want the row untouched", repo.count())
	}
}

// The plain stateful-app path must not need the ledger at all: an unowned volume
// is deleted even with no claim repository wired.
func TestDeleteAppVolumes_UnownedVolumeDeletes(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(volWithBacking(appID, "/vol/a.ext4"))
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	if err := m.DeleteAppVolumes(context.Background(), appID); err != nil {
		t.Fatalf("DeleteAppVolumes: %v", err)
	}
	if len(backend.deleted) != 1 || repo.count() != 0 {
		t.Fatalf("unowned volume must be reclaimed (deleted=%v rows=%d)", backend.deleted, repo.count())
	}
}

func TestBindToResource_StampsUnownedVolumes(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	a := volWithBacking(appID, "/vol/a.ext4")
	b := volWithBacking(appID, "/vol/b.ext4")
	b.MountPath = "/data2"
	repo := newVolRepoFake(a, b)
	m := newTestVolumeManager(t, repo, &recordingBackend{}, "/vol", nil)

	if err := m.BindToResource(context.Background(), appID, resID); err != nil {
		t.Fatalf("BindToResource: %v", err)
	}
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		got, _ := repo.FindByID(context.Background(), id)
		if got.ResourceID == nil || *got.ResourceID != resID {
			t.Fatalf("volume %s resource_id = %v, want %s", id, got.ResourceID, resID)
		}
	}
}

func TestBindToResource_IdempotentForSameResource(t *testing.T) {
	appID, resID := uuid.New(), uuid.New()
	repo := newVolRepoFake(volWithBacking(appID, "/vol/a.ext4"))
	m := newTestVolumeManager(t, repo, &recordingBackend{}, "/vol", nil)

	if err := m.BindToResource(context.Background(), appID, resID); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := m.BindToResource(context.Background(), appID, resID); err != nil {
		t.Fatalf("second bind must be a no-op, got %v", err)
	}
}

// Write-once: re-parenting a customer's bytes onto a different claim is refused,
// and the recorded owner is left exactly as it was.
func TestBindToResource_RefusesForeignOwner(t *testing.T) {
	appID, resA, resB := uuid.New(), uuid.New(), uuid.New()
	vol := ownedVolume(appID, resA, "/vol/a.ext4")
	repo := newVolRepoFake(vol)
	m := newTestVolumeManager(t, repo, &recordingBackend{}, "/vol", nil)

	err := m.BindToResource(context.Background(), appID, resB)
	if !errors.Is(err, domain.ErrVolumeClaimConflict) {
		t.Fatalf("got %v, want ErrVolumeClaimConflict", err)
	}
	got, _ := repo.FindByID(context.Background(), vol.ID)
	if got.ResourceID == nil || *got.ResourceID != resA {
		t.Fatalf("owner = %v, want it unchanged at %s", got.ResourceID, resA)
	}
}

// AppID is a pointer now (D-203): a created volume must still carry the app it
// is attached to, not a nil parent the owner-present CHECK would reject.
func TestEnsureAppVolumes_SetsAppAttachment(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake()
	m := newTestVolumeManager(t, repo, &modeBackend{}, "/vol", nil)

	vols, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
	if err != nil {
		t.Fatalf("EnsureAppVolumes: %v", err)
	}
	if len(vols) != 1 || vols[0].AppID == nil || *vols[0].AppID != appID {
		t.Fatalf("created volume app attachment = %v, want %s", vols, appID)
	}
}
