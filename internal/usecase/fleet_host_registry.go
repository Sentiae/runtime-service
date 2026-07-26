package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetHostRegistry owns the durable fleet host inventory (runtime-fleet CP4
// §9#4): host registration, heartbeat, and the live-host query the scheduler
// (§9#5) consumes. It holds no placement, scheduling, or reconciliation logic.
type FleetHostRegistry struct {
	repo repository.HostRepository
	// leases assigns each host its net_ordinal — the /1024 block of the microVM
	// addressing plane it may allocate /30s, uids and jail ids from. Registration
	// is the right moment for it: it is the one place a host's existence is
	// established, and a host that never got an ordinal cannot boot anything.
	leases repository.NetLeaseRepository
}

// NewFleetHostRegistry constructs the registry use case.
//
// leases may be nil only in tests; a nil store means RegisterHost assigns no
// ordinal, and a host with no ordinal allocates no addresses at all (the
// addressing plane refuses every boot on it) — so the omission fails closed
// rather than silently defaulting a host into ordinal 0's block.
func NewFleetHostRegistry(repo repository.HostRepository, leases repository.NetLeaseRepository) *FleetHostRegistry {
	return &FleetHostRegistry{repo: repo, leases: leases}
}

// RegisterHost upserts a host by id, then makes sure it has a net ordinal. A new
// host is created healthy/active with allocatable seeded from capacity;
// re-registering an existing host refreshes its spec
// (region/labels/capacity/endpoint) and marks it healthy without clobbering the
// live allocatable accounting a heartbeat maintains — and, critically, without
// resurrecting its own schedulability: an operator-set cordoned/draining/
// decommissioned status SURVIVES re-registration (see below).
//
// The ordinal assignment is a REFUSAL point: if the fleet has no free ordinal the
// registration fails, because admitting a host that would have to share another
// host's addressing block is worse than not admitting it.
//
// The placement FACTS are a second refusal point (SentiaeDB standard-ha slice 0,
// D-196): a host must state its failure domain and its region, and there is no
// default for either. A host admitted without them can never be corrected —
// nothing but a human knows which building and which breaker it is on — and a
// missing value would satisfy the "different failure domain, same region"
// invariant vacuously, which is the exact fail-open that makes an HA pair inside
// one chassis look healthy.
func (uc *FleetHostRegistry) RegisterHost(ctx context.Context, host domain.Host) (domain.Host, error) {
	if host.Region == "" {
		return domain.Host{}, domain.ErrHostRegionRequired
	}
	// Parsed, not merely non-empty: an unparseable domain compares unequal to
	// every other value and would therefore pass an anti-affinity filter by
	// accident. The sentinel migration 0022 backfilled onto pre-existing rows is
	// deliberately unparseable for the same reason, so re-registering such a host
	// WITHOUT a configured domain leaves the row honest rather than promoting a
	// placeholder to a fact.
	if _, err := domain.ParseFailureDomain(host.FailureDomain); err != nil {
		return domain.Host{}, err
	}
	now := time.Now().UTC()
	if host.ID == uuid.Nil {
		host.ID = uuid.New()
	}
	// labels is JSONB NOT NULL DEFAULT '{}' — GORM serializes a nil map to
	// SQL NULL (the DB default never applies on an explicit insert), so
	// normalize before any write.
	if host.Labels == nil {
		host.Labels = map[string]string{}
	}

	existing, err := uc.repo.FindByID(ctx, host.ID)
	if err != nil && !errors.Is(err, domain.ErrFleetHostNotFound) {
		return domain.Host{}, fmt.Errorf("load host: %w", err)
	}

	if existing != nil {
		existing.Region = host.Region
		// Re-registering with a stated domain is how the migration-0022 'unattested'
		// sentinel gets corrected — the only path that ever does. It is refreshed
		// like the rest of the spec because the machine may genuinely have moved
		// (a rack, a breaker, a switch), and a stale domain is worse than none: it
		// would be trusted.
		existing.FailureDomain = host.FailureDomain
		existing.Labels = host.Labels
		existing.CapacityVCPU = host.CapacityVCPU
		existing.CapacityMemMB = host.CapacityMemMB
		existing.CapacityDiskMB = host.CapacityDiskMB
		// Allocatable is heartbeat-owned accounting, so a re-register must not
		// overwrite it — but it can never legitimately EXCEED the capacity we
		// just refreshed. Clamping is what keeps a shrinking measurement honest:
		// this host was seeded at 51200MB disk / 2048MB mem from a hardcoded
		// config default, and once capacity became measured (17542/7941) the
		// un-clamped allocatable kept advertising the old numbers indefinitely,
		// because only Create ever seeded it. Reported capacity that outlives
		// the measurement is the same class of lie as never measuring at all.
		if existing.AllocatableVCPU > existing.CapacityVCPU {
			existing.AllocatableVCPU = existing.CapacityVCPU
		}
		if existing.AllocatableMemMB > existing.CapacityMemMB {
			existing.AllocatableMemMB = existing.CapacityMemMB
		}
		if existing.AllocatableDiskMB > existing.CapacityDiskMB {
			existing.AllocatableDiskMB = existing.CapacityDiskMB
		}
		existing.Endpoint = host.Endpoint
		// Health is OBSERVED and orthogonal to schedulability: this registration is
		// itself the evidence that the host's process is up, so refreshing it is
		// honest.
		existing.Health = domain.HostHealthHealthy
		// Schedulability is OPERATOR-owned, and a re-registration must never widen
		// it. This used to set active unconditionally, and runtime-service
		// self-registers on every boot — so a cordoned host silently UN-CORDONED
		// itself by restarting. Cordon is the precondition of every safe
		// host-lifecycle operation (drain, re-image), and a cordon that lapses on
		// restart is worse than no cordon at all: the operator believes work is being
		// kept off a machine the scheduler is placing on.
		//
		// draining is preserved for the same reason plus a stronger one: a drain is an
		// in-flight operation moving work OFF this host, so resurrecting it mid-drain
		// would have the scheduler fill the machine that is being emptied — the drain
		// would never converge. decommissioned is terminal for the same class of
		// reason. In all three cases only a human can put the host back in the
		// candidate set, because only a human knows the operation finished.
		//
		// The one row this may promote to active is one whose status is not a value
		// the fleet recognizes (a row written before the column existed, or corrupted
		// by hand): leaving that unschedulable forever would be an un-fixable host
		// rather than a preserved decision.
		if existing.Status == domain.HostStatusActive || !existing.Status.IsValid() {
			existing.Status = domain.HostStatusActive
		}
		existing.LastHeartbeat = &now
		existing.UpdatedAt = now
		if err := uc.repo.Update(ctx, existing); err != nil {
			return domain.Host{}, fmt.Errorf("update host: %w", err)
		}
		ord, err := uc.ensureNetOrdinal(ctx, existing.ID)
		if err != nil {
			return domain.Host{}, err
		}
		existing.NetOrdinal = ord
		return *existing, nil
	}

	host.AllocatableVCPU = host.CapacityVCPU
	host.AllocatableMemMB = host.CapacityMemMB
	host.AllocatableDiskMB = host.CapacityDiskMB
	host.Health = domain.HostHealthHealthy
	host.Status = domain.HostStatusActive
	host.LastHeartbeat = &now
	host.CreatedAt = now
	host.UpdatedAt = now
	if err := uc.repo.Create(ctx, &host); err != nil {
		return domain.Host{}, fmt.Errorf("create host: %w", err)
	}
	// AFTER the insert, never before: the ordinal is assigned by an UPDATE against
	// the host row, and the UNIQUE index on net_ordinal is what serializes racing
	// hosts. Assigning it as part of the insert payload would make the row's
	// creation fail on someone else's ordinal collision.
	ord, err := uc.ensureNetOrdinal(ctx, host.ID)
	if err != nil {
		return domain.Host{}, err
	}
	host.NetOrdinal = ord
	return host, nil
}

// ensureNetOrdinal assigns (or re-reads) this host's addressing block. A nil lease
// store leaves the host without one, which the addressing plane treats as "boot
// nothing here" — see NewFleetHostRegistry.
func (uc *FleetHostRegistry) ensureNetOrdinal(ctx context.Context, hostID uuid.UUID) (*int, error) {
	if uc.leases == nil {
		return nil, nil
	}
	ord, err := uc.leases.EnsureHostOrdinal(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("assign microVM addressing block to host %s: %w", hostID, err)
	}
	return &ord, nil
}

// Heartbeat refreshes a host's liveness + allocatable capacity. An empty health
// keeps the prior value; a non-empty health must be a recognized HostHealth.
func (uc *FleetHostRegistry) Heartbeat(ctx context.Context, hostID uuid.UUID, allocVCPU int, allocMemMB, allocDiskMB int64, health string) error {
	host, err := uc.repo.FindByID(ctx, hostID)
	if err != nil {
		return err
	}
	if health != "" {
		h := domain.HostHealth(health)
		if !h.IsValid() {
			return domain.ErrInvalidHostHealth
		}
		host.Health = h
	}
	now := time.Now().UTC()
	host.AllocatableVCPU = allocVCPU
	host.AllocatableMemMB = allocMemMB
	host.AllocatableDiskMB = allocDiskMB
	host.LastHeartbeat = &now
	host.UpdatedAt = now
	if err := uc.repo.Update(ctx, host); err != nil {
		return fmt.Errorf("update host heartbeat: %w", err)
	}
	return nil
}

// ListHosts returns every host in the inventory.
func (uc *FleetHostRegistry) ListHosts(ctx context.Context) ([]domain.Host, error) {
	return uc.repo.List(ctx)
}

// ListLive returns hosts that are active, healthy, and have heartbeated within
// staleness of now — the placement candidate set the scheduler (§9#5) consumes.
func (uc *FleetHostRegistry) ListLive(ctx context.Context, staleness time.Duration) ([]domain.Host, error) {
	hosts, err := uc.repo.ListByStatus(ctx, domain.HostStatusActive)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().Add(-staleness)
	live := make([]domain.Host, 0, len(hosts))
	for i := range hosts {
		h := hosts[i]
		if h.Health != domain.HostHealthHealthy {
			continue
		}
		if h.LastHeartbeat == nil || h.LastHeartbeat.Before(cutoff) {
			continue
		}
		live = append(live, h)
	}
	return live, nil
}
