package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// volRepoFake is a minimal stateful VolumeRepository for the manager tests.
type volRepoFake struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.Volume
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
	cp := *v
	f.store[v.ID] = &cp
	return nil
}
func (f *volRepoFake) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Volume
	for _, v := range f.store {
		if v.AppID == appID {
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

func volWithBacking(appID uuid.UUID, backing string) *domain.Volume {
	return &domain.Volume{
		ID:          uuid.New(),
		AppID:       appID,
		SizeMB:      64,
		MountPath:   "/data",
		BackingPath: backing,
		Status:      domain.VolumeStatusAvailable,
	}
}

func TestDeleteAppVolumes_DeletesBackingFilesAndRows(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake(
		volWithBacking(appID, "/vol/a.ext4"),
		volWithBacking(appID, "/vol/b.ext4"),
	)
	backend := &recordingBackend{}
	m := NewFleetVolumeManager(repo, backend, "/vol")

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
		volWithBacking(appID, ""),           // never materialized — no backend call
		volWithBacking(appID, "/vol/c.ext4"),
	)
	backend := &recordingBackend{}
	m := NewFleetVolumeManager(repo, backend, "/vol")

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
	m := NewFleetVolumeManager(repo, backend, "/vol")

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
			m := NewFleetVolumeManager(repo, &recordingBackend{}, "/vol")

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
