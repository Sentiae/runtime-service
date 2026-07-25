package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// stubHostRepo is the narrowest thing that satisfies repository.HostRepository:
// RegisterHost only ever calls FindByID + Update/Create.
type stubHostRepo struct {
	existing *domain.Host
	updated  *domain.Host
	created  *domain.Host
}

func (r *stubHostRepo) Create(_ context.Context, h *domain.Host) error {
	cp := *h
	r.created = &cp
	return nil
}

func (r *stubHostRepo) Update(_ context.Context, h *domain.Host) error {
	cp := *h
	r.updated = &cp
	return nil
}

func (r *stubHostRepo) FindByID(_ context.Context, _ uuid.UUID) (*domain.Host, error) {
	if r.existing == nil {
		return nil, domain.ErrFleetHostNotFound
	}
	cp := *r.existing
	return &cp, nil
}

func (r *stubHostRepo) List(_ context.Context) ([]domain.Host, error) { return nil, nil }
func (r *stubHostRepo) ListActive(_ context.Context) ([]domain.Host, error) {
	return nil, nil
}
func (r *stubHostRepo) ListByStatus(_ context.Context, _ domain.HostStatus) ([]domain.Host, error) {
	return nil, nil
}
func (r *stubHostRepo) Delete(_ context.Context, _ uuid.UUID) error { return nil }

// A re-register must never leave a host advertising allocatable capacity above
// the capacity it just measured. The live fleet host reproduced this exactly:
// seeded at 51200MB disk from a hardcoded config default, then measured at
// 17542MB, and the un-clamped allocatable kept reporting 51200 because only
// Create ever seeded it.
func TestRegisterHost_ClampsAllocatableToRefreshedCapacity(t *testing.T) {
	hostID := uuid.New()

	tests := []struct {
		name                                    string
		seededVCPU, seededMem, seededDisk       int
		measuredVCPU, measuredMem, measuredDisk int
		wantVCPU, wantMem, wantDisk             int
	}{
		{
			name:       "capacity shrank — allocatable is clamped down to it",
			seededVCPU: 4, seededMem: 2048, seededDisk: 51200,
			measuredVCPU: 4, measuredMem: 7941, measuredDisk: 17542,
			// mem stays 2048: it is BELOW the measurement, so it is honest
			// accounting and must not be inflated to the new capacity.
			wantVCPU: 4, wantMem: 2048, wantDisk: 17542,
		},
		{
			name:       "capacity grew — allocatable accounting is left alone, never inflated",
			seededVCPU: 2, seededMem: 1024, seededDisk: 8000,
			measuredVCPU: 8, measuredMem: 16000, measuredDisk: 40000,
			wantVCPU: 2, wantMem: 1024, wantDisk: 8000,
		},
		{
			name:       "already consistent — nothing moves",
			seededVCPU: 4, seededMem: 7941, seededDisk: 17542,
			measuredVCPU: 4, measuredMem: 7941, measuredDisk: 17542,
			wantVCPU: 4, wantMem: 7941, wantDisk: 17542,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &stubHostRepo{existing: &domain.Host{
				ID:                hostID,
				CapacityVCPU:      tt.seededVCPU,
				CapacityMemMB:     int64(tt.seededMem),
				CapacityDiskMB:    int64(tt.seededDisk),
				AllocatableVCPU:   tt.seededVCPU,
				AllocatableMemMB:  int64(tt.seededMem),
				AllocatableDiskMB: int64(tt.seededDisk),
			}}

			got, err := NewFleetHostRegistry(repo, newNetFakeLeaseRepo()).RegisterHost(context.Background(), domain.Host{
				ID:             hostID,
				CapacityVCPU:   tt.measuredVCPU,
				CapacityMemMB:  int64(tt.measuredMem),
				CapacityDiskMB: int64(tt.measuredDisk),
			})
			if err != nil {
				t.Fatalf("RegisterHost: %v", err)
			}
			if repo.updated == nil {
				t.Fatal("expected the existing host to be updated, not created")
			}

			// Assert on the row that was PERSISTED, not only the return value:
			// the reported lie lives in the database column.
			if repo.updated.AllocatableVCPU != tt.wantVCPU {
				t.Errorf("persisted allocatable vcpu = %d, want %d", repo.updated.AllocatableVCPU, tt.wantVCPU)
			}
			if repo.updated.AllocatableMemMB != int64(tt.wantMem) {
				t.Errorf("persisted allocatable mem = %dMB, want %dMB", repo.updated.AllocatableMemMB, tt.wantMem)
			}
			if repo.updated.AllocatableDiskMB != int64(tt.wantDisk) {
				t.Errorf("persisted allocatable disk = %dMB, want %dMB", repo.updated.AllocatableDiskMB, tt.wantDisk)
			}
			if got.AllocatableDiskMB != int64(tt.wantDisk) {
				t.Errorf("returned allocatable disk = %dMB, want %dMB", got.AllocatableDiskMB, tt.wantDisk)
			}

			// Allocatable must never exceed capacity, in any direction the
			// measurement moves — the invariant, stated independently of the
			// per-case expectations above.
			if repo.updated.AllocatableDiskMB > repo.updated.CapacityDiskMB ||
				repo.updated.AllocatableMemMB > repo.updated.CapacityMemMB ||
				repo.updated.AllocatableVCPU > repo.updated.CapacityVCPU {
				t.Errorf("allocatable exceeds capacity: alloc=%dvcpu/%dMB/%dMB cap=%dvcpu/%dMB/%dMB",
					repo.updated.AllocatableVCPU, repo.updated.AllocatableMemMB, repo.updated.AllocatableDiskMB,
					repo.updated.CapacityVCPU, repo.updated.CapacityMemMB, repo.updated.CapacityDiskMB)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Net ordinal assignment (SentiaeDB Phase 0 — the microVM addressing plane)
// ─────────────────────────────────────────────────────────────────────

// Registration is where a host gets its addressing BLOCK. Without one it can
// allocate no /30, uid or jail id at all, so this is the seam that decides whether
// a host can boot anything — and it must be idempotent, because every restart
// re-registers.
func TestRegisterHostAssignsAndKeepsItsNetOrdinal(t *testing.T) {
	leases := newNetFakeLeaseRepo()
	first := uuid.New()
	second := uuid.New()

	reg := NewFleetHostRegistry(&stubHostRepo{}, leases)
	got, err := reg.RegisterHost(context.Background(), domain.Host{ID: first})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	if got.NetOrdinal == nil || *got.NetOrdinal != 0 {
		t.Fatalf("first host ordinal = %v, want 0 (the existing host keeps the block its live VMs are addressed out of)", got.NetOrdinal)
	}

	// A SECOND host must get a different block; sharing one would alias every
	// address, uid and chroot on both machines.
	second2, err := NewFleetHostRegistry(&stubHostRepo{}, leases).
		RegisterHost(context.Background(), domain.Host{ID: second})
	if err != nil {
		t.Fatalf("RegisterHost (second host): %v", err)
	}
	if second2.NetOrdinal == nil || *second2.NetOrdinal != 1 {
		t.Fatalf("second host ordinal = %v, want 1", second2.NetOrdinal)
	}

	// Re-registering must return the SAME ordinal: moving it would re-point a block
	// whose leases (and whose running VMs) are already addressed out of it.
	again, err := NewFleetHostRegistry(&stubHostRepo{existing: &domain.Host{ID: first}}, leases).
		RegisterHost(context.Background(), domain.Host{ID: first})
	if err != nil {
		t.Fatalf("re-RegisterHost: %v", err)
	}
	if again.NetOrdinal == nil || *again.NetOrdinal != 0 {
		t.Fatalf("re-registered ordinal = %v, want a stable 0", again.NetOrdinal)
	}
}

// A fleet with no free block must REFUSE the registration. Admitting a host that
// would have to share another host's addressing block is worse than not admitting
// it: it would be a machine whose every VM aliases another machine's.
func TestRegisterHostRefusedWhenOrdinalsAreExhausted(t *testing.T) {
	leases := newNetFakeLeaseRepo()
	leases.ordinalErr = domain.ErrNetOrdinalExhausted

	_, err := NewFleetHostRegistry(&stubHostRepo{}, leases).
		RegisterHost(context.Background(), domain.Host{ID: uuid.New()})
	if !errors.Is(err, domain.ErrNetOrdinalExhausted) {
		t.Fatalf("RegisterHost with no free ordinal = %v, want ErrNetOrdinalExhausted", err)
	}
}
