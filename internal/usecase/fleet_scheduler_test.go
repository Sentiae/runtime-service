//go:build unit

package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeLiveHostLister returns a fixed live-host set (the staleness arg is
// irrelevant — freshness is already resolved by the registry it stands in for).
type fakeLiveHostLister struct {
	hosts []domain.Host
	err   error
}

func (f *fakeLiveHostLister) ListLive(_ context.Context, _ time.Duration) ([]domain.Host, error) {
	return f.hosts, f.err
}

// fakeReplicaRepo serves ListByHost from an in-memory host→replicas map. Only
// the methods the scheduler calls are meaningful; the rest satisfy the
// interface and are unused.
type fakeReplicaRepo struct {
	byHost map[uuid.UUID][]domain.Replica
}

func (f *fakeReplicaRepo) ListByHost(_ context.Context, hostID uuid.UUID) ([]domain.Replica, error) {
	return f.byHost[hostID], nil
}
func (f *fakeReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (f *fakeReplicaRepo) Update(context.Context, *domain.Replica) error { return nil }
func (f *fakeReplicaRepo) FindByID(context.Context, uuid.UUID) (*domain.Replica, error) {
	return nil, domain.ErrReplicaNotFound
}
func (f *fakeReplicaRepo) ListByApp(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *fakeReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (f *fakeReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }

// fakeAppRepo serves FindByID from an in-memory app map; a missing id returns
// the not-found sentinel (the scheduler must treat that as 0 resources).
type fakeAppRepo struct {
	apps map[uuid.UUID]*domain.FleetApp
}

func (f *fakeAppRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	if a, ok := f.apps[id]; ok {
		return a, nil
	}
	return nil, domain.ErrFleetAppNotFound
}
func (f *fakeAppRepo) Create(context.Context, *domain.FleetApp) error { return nil }
func (f *fakeAppRepo) Update(context.Context, *domain.FleetApp) error { return nil }
func (f *fakeAppRepo) FindByComponentEnv(context.Context, string, string) (*domain.FleetApp, error) {
	return nil, domain.ErrFleetAppNotFound
}
func (f *fakeAppRepo) List(context.Context) ([]domain.FleetApp, error) {
	out := make([]domain.FleetApp, 0, len(f.apps))
	for _, a := range f.apps {
		out = append(out, *a)
	}
	return out, nil
}
func (f *fakeAppRepo) Delete(context.Context, uuid.UUID) error { return nil }

// host builds a live host with capacity + optional labels.
func host(id uuid.UUID, vcpu int, memMB, diskMB int64, labels map[string]string) domain.Host {
	return domain.Host{
		ID:             id,
		CapacityVCPU:   vcpu,
		CapacityMemMB:  memMB,
		CapacityDiskMB: diskMB,
		Labels:         labels,
	}
}

// replica builds an occupying/non-occupying replica of appID in a given state.
func replica(appID uuid.UUID, state domain.ReplicaState) domain.Replica {
	return domain.Replica{ID: uuid.New(), AppID: appID, State: state}
}

func TestFleetScheduler_SelectHost(t *testing.T) {
	// Stable, ordered ids so host.ID string tie-breaks are predictable.
	hostA := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	hostB := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	appID := uuid.MustParse("00000000-0000-0000-0000-000000000a01")
	otherApp := uuid.MustParse("00000000-0000-0000-0000-000000000a02")

	// A 2-vcpu / 1024-MB app used for occupancy accounting.
	app2 := &domain.FleetApp{ID: appID, ResourcesVCPU: 2, ResourcesMemMB: 1024}
	appsWith2 := map[uuid.UUID]*domain.FleetApp{appID: app2}

	tests := []struct {
		name       string
		hosts      []domain.Host
		byHost     map[uuid.UUID][]domain.Replica
		apps       map[uuid.UUID]*domain.FleetApp
		req        PlacementRequest
		wantHost   uuid.UUID
		wantErrIs  error
	}{
		{
			name: "bin_pack picks densest fitting host",
			// hostA: 8 vcpu free, hostB: 4 vcpu free — both fit a 2-vcpu need;
			// best-fit chooses hostB (less free).
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, nil),
				host(hostB, 4, 8192, 10240, nil),
			},
			byHost:   map[uuid.UUID][]domain.Replica{},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512, Constraint: domain.PlacementConstraintBinPack},
			wantHost: hostB,
		},
		{
			name: "bin_pack excludes a host that does not fit",
			// hostB has only 1 vcpu — too small; hostA is the only candidate.
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, nil),
				host(hostB, 1, 8192, 10240, nil),
			},
			byHost:   map[uuid.UUID][]domain.Replica{},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512},
			wantHost: hostA,
		},
		{
			name: "spread picks host with fewest app replicas even with less free",
			// hostA: 8 vcpu free but already runs 1 occupying replica of the app.
			// hostB: 4 vcpu free, zero replicas of the app → spread picks hostB.
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, nil),
				host(hostB, 4, 8192, 10240, nil),
			},
			byHost: map[uuid.UUID][]domain.Replica{
				hostA: {replica(appID, domain.ReplicaStateResident)},
			},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512, Constraint: domain.PlacementConstraintSpread},
			wantHost: hostB,
		},
		{
			name: "affinity by host id pins to that host",
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, nil),
				host(hostB, 8, 8192, 10240, nil),
			},
			byHost:   map[uuid.UUID][]domain.Replica{},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512, Constraint: domain.PlacementConstraintAffinity, AffinityHostID: &hostB},
			wantHost: hostB,
		},
		{
			name: "affinity by host id that cannot fit yields no host",
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, nil),
				host(hostB, 1, 8192, 10240, nil),
			},
			byHost:    map[uuid.UUID][]domain.Replica{},
			apps:      appsWith2,
			req:       PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512, Constraint: domain.PlacementConstraintAffinity, AffinityHostID: &hostB},
			wantErrIs: domain.ErrNoSchedulableHost,
		},
		{
			name: "affinity by labels excludes non-matching hosts",
			// Only hostB carries zone=z1 → it is the sole candidate.
			hosts: []domain.Host{
				host(hostA, 8, 8192, 10240, map[string]string{"zone": "z0"}),
				host(hostB, 8, 8192, 10240, map[string]string{"zone": "z1", "gpu": "true"}),
			},
			byHost:   map[uuid.UUID][]domain.Replica{},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512, Constraint: domain.PlacementConstraintAffinity, AffinityLabels: map[string]string{"zone": "z1"}},
			wantHost: hostB,
		},
		{
			name:      "no live hosts yields no schedulable host",
			hosts:     []domain.Host{},
			byHost:    map[uuid.UUID][]domain.Replica{},
			apps:      appsWith2,
			req:       PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512},
			wantErrIs: domain.ErrNoSchedulableHost,
		},
		{
			name: "no fitting host yields no schedulable host",
			hosts: []domain.Host{
				host(hostA, 1, 256, 1024, nil),
			},
			byHost:    map[uuid.UUID][]domain.Replica{},
			apps:      appsWith2,
			req:       PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512},
			wantErrIs: domain.ErrNoSchedulableHost,
		},
		{
			name: "occupying replica reduces free; draining and dead do not",
			// hostA: capacity 4 vcpu, one RESIDENT replica of a 2-vcpu app →
			//        free 2 vcpu, still fits a 2-vcpu need.
			// hostB: capacity 4 vcpu, one DRAINING + one DEAD replica of the same
			//        2-vcpu app → they do NOT occupy → free 4 vcpu.
			// best-fit prefers the denser hostA (free 2 < free 4).
			hosts: []domain.Host{
				host(hostA, 4, 8192, 10240, nil),
				host(hostB, 4, 8192, 10240, nil),
			},
			byHost: map[uuid.UUID][]domain.Replica{
				hostA: {replica(appID, domain.ReplicaStateResident)},
				hostB: {replica(appID, domain.ReplicaStateDraining), replica(appID, domain.ReplicaStateDead)},
			},
			apps:     appsWith2,
			req:      PlacementRequest{AppID: appID, NeedVCPU: 2, NeedMemMB: 512},
			wantHost: hostA,
		},
		{
			name: "occupancy sums across distinct apps on a host",
			// hostA runs a resident replica of appID (2 vcpu) AND otherApp (3 vcpu)
			// → used 5 of 8 → free 3, does NOT fit a 4-vcpu need.
			// hostB is empty (8 free) → the only candidate.
			hosts: []domain.Host{
				host(hostA, 8, 16384, 10240, nil),
				host(hostB, 8, 16384, 10240, nil),
			},
			byHost: map[uuid.UUID][]domain.Replica{
				hostA: {replica(appID, domain.ReplicaStateResident), replica(otherApp, domain.ReplicaStateResident)},
			},
			apps: map[uuid.UUID]*domain.FleetApp{
				appID:    app2,
				otherApp: {ID: otherApp, ResourcesVCPU: 3, ResourcesMemMB: 2048},
			},
			req:      PlacementRequest{AppID: appID, NeedVCPU: 4, NeedMemMB: 512},
			wantHost: hostB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched := NewFleetScheduler(
				&fakeLiveHostLister{hosts: tt.hosts},
				&fakeReplicaRepo{byHost: tt.byHost},
				&fakeAppRepo{apps: tt.apps},
				90*time.Second,
			)
			got, err := sched.SelectHost(context.Background(), tt.req)
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("error: got %v, want Is(%v)", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantHost {
				t.Fatalf("host: got %s, want %s", got, tt.wantHost)
			}
		})
	}
}
