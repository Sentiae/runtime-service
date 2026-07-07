package usecase

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// liveHostLister is the narrow slice of FleetHostRegistry the scheduler needs:
// the live-host candidate set (active + healthy + fresh heartbeat). Declared
// here so the scheduler is unit-testable without a DB or the full registry.
type liveHostLister interface {
	ListLive(ctx context.Context, staleness time.Duration) ([]domain.Host, error)
}

// FleetScheduler is the runtime-fleet CP4 §9#5 placement algorithm: a pure
// decision function that selects one live host for a replica of a FleetApp,
// honoring bin_pack / spread / affinity constraints. It reads the durable
// registry + replica state; it mutates NOTHING (no replicas booted, no host
// allocatable updated) — the #7 reconciler acts on its choice.
type FleetScheduler struct {
	hosts     liveHostLister
	replicas  repository.ReplicaRepository
	apps      repository.FleetAppRepository
	staleness time.Duration
}

// NewFleetScheduler constructs the scheduler. staleness bounds how old a host's
// last heartbeat may be to still count as a live placement candidate.
func NewFleetScheduler(
	hosts liveHostLister,
	replicas repository.ReplicaRepository,
	apps repository.FleetAppRepository,
	staleness time.Duration,
) *FleetScheduler {
	return &FleetScheduler{hosts: hosts, replicas: replicas, apps: apps, staleness: staleness}
}

// PlacementRequest is one replica placement to resolve.
type PlacementRequest struct {
	AppID          uuid.UUID
	NeedVCPU       int
	NeedMemMB      int64
	NeedDiskMB     int64
	Constraint     domain.PlacementConstraint // "" defaults to bin_pack
	AffinityHostID *uuid.UUID                 // affinity: pin to this host
	AffinityLabels map[string]string          // affinity: host.Labels must be a superset
}

// occupyingState reports whether a replica in the given state consumes host
// capacity for scheduling purposes. Draining + dead replicas are releasing
// their footprint and do NOT occupy.
func occupyingState(s domain.ReplicaState) bool {
	switch s {
	case domain.ReplicaStateScheduled, domain.ReplicaStateBooting,
		domain.ReplicaStateResident, domain.ReplicaStatePaused:
		return true
	}
	return false
}

// candidate is a live host that fits the request, with the accounting the
// strategy tie-breaks on.
type candidate struct {
	hostID     uuid.UUID
	freeVCPU   int
	freeMemMB  int64
	appReplica int // occupying replicas of req.AppID already on this host
}

// SelectHost picks the host.ID a new replica should be placed on, or
// domain.ErrNoSchedulableHost when no live host satisfies the request. It is a
// read-only decision: it never mutates a host, replica, or app.
func (s *FleetScheduler) SelectHost(ctx context.Context, req PlacementRequest) (uuid.UUID, error) {
	live, err := s.hosts.ListLive(ctx, s.staleness)
	if err != nil {
		return uuid.Nil, err
	}
	if len(live) == 0 {
		return uuid.Nil, domain.ErrNoSchedulableHost
	}

	// Local app-resource cache so a repeated AppID across hosts/replicas is
	// only looked up once. A missing app contributes zero resources.
	appCache := map[uuid.UUID]*domain.FleetApp{}
	appResources := func(appID uuid.UUID) (vcpu int, memMB int64) {
		app, ok := appCache[appID]
		if !ok {
			app, err = s.apps.FindByID(ctx, appID)
			if err != nil {
				app = nil // treat a missing/errored app as 0 resources; do not fail placement
			}
			appCache[appID] = app
		}
		if app == nil {
			return 0, 0
		}
		return app.ResourcesVCPU, app.ResourcesMemMB
	}

	constraint := req.Constraint
	if constraint == "" {
		constraint = domain.PlacementConstraintBinPack
	}

	candidates := make([]candidate, 0, len(live))
	for i := range live {
		h := live[i]

		// Affinity by host id: only the pinned host is ever a candidate.
		if req.AffinityHostID != nil && h.ID != *req.AffinityHostID {
			continue
		}
		// Affinity by labels: host.Labels must contain every requested k=v.
		if !labelsSuperset(h.Labels, req.AffinityLabels) {
			continue
		}

		placed, lerr := s.replicas.ListByHost(ctx, h.ID)
		if lerr != nil {
			return uuid.Nil, lerr
		}

		// Effective free = capacity − Σ occupying replicas' app resources.
		// Disk has no per-app field yet, so disk occupancy is 0 and disk fit
		// is checked against host capacity only.
		usedVCPU := 0
		var usedMemMB int64
		appReplica := 0
		for j := range placed {
			r := placed[j]
			if !occupyingState(r.State) {
				continue
			}
			if r.AppID == req.AppID {
				appReplica++
			}
			vcpu, memMB := appResources(r.AppID)
			usedVCPU += vcpu
			usedMemMB += memMB
		}
		freeVCPU := h.CapacityVCPU - usedVCPU
		freeMemMB := h.CapacityMemMB - usedMemMB
		freeDiskMB := h.CapacityDiskMB // no per-replica disk accounting yet

		if freeVCPU < req.NeedVCPU || freeMemMB < req.NeedMemMB || freeDiskMB < req.NeedDiskMB {
			continue
		}
		candidates = append(candidates, candidate{
			hostID:     h.ID,
			freeVCPU:   freeVCPU,
			freeMemMB:  freeMemMB,
			appReplica: appReplica,
		})
	}

	if len(candidates) == 0 {
		return uuid.Nil, domain.ErrNoSchedulableHost
	}

	switch constraint {
	case domain.PlacementConstraintSpread:
		// Fewest occupying replicas of the app; tie-break: most freeVCPU,
		// then smallest host.ID string (deterministic).
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.appReplica != b.appReplica {
				return a.appReplica < b.appReplica
			}
			if a.freeVCPU != b.freeVCPU {
				return a.freeVCPU > b.freeVCPU
			}
			return a.hostID.String() < b.hostID.String()
		})
	default:
		// bin_pack (and affinity, already restricted above): best-fit —
		// densest packing = least freeVCPU that still fits; tie-break: least
		// freeMemMB, then smallest host.ID string.
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.freeVCPU != b.freeVCPU {
				return a.freeVCPU < b.freeVCPU
			}
			if a.freeMemMB != b.freeMemMB {
				return a.freeMemMB < b.freeMemMB
			}
			return a.hostID.String() < b.hostID.String()
		})
	}

	return candidates[0].hostID, nil
}

// labelsSuperset reports whether have contains every key=value in want. An
// empty want is trivially satisfied.
func labelsSuperset(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}
