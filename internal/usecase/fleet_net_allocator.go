package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// netLeaseAcquireAttempts bounds the acquire retry loop. Each retry means the
// INSERT lost a race on one of the unique fences, so the bound only has to exceed
// the number of boots that can realistically race on this host — and it must
// exist, because retrying forever under a permanent conflict would hang a boot
// instead of refusing it.
const netLeaseAcquireAttempts = 32

// FleetNetAllocator allocates microVM addressing on THIS host: the lowest free
// host-local slot, turned into a /30, a TAP name, a jail id and a per-VM uid by
// domain.DeriveNetLease, and held by a lease row.
//
// ⚠ There is no mutex and no in-memory used-set here, deliberately. The one it
// replaces (a process-local map seeded at startup from live rows) was fail-open in
// four separate ways: it forgot everything on restart, its seed swallowed its own
// errors, its state filter missed `dead` replicas whose VMM was still running, and
// two processes shared nothing at all. The unique indexes on fleet_net_leases are
// the only correct place for that mutual exclusion, so UsedSlots is a HINT and the
// INSERT is the decision.
type FleetNetAllocator struct {
	leases   repository.NetLeaseRepository
	selfHost uuid.UUID
	// ordinal is this host's /1024 block. NEGATIVE means unassigned, and an
	// unassigned ordinal refuses every allocation: 0 is a real block another host
	// may own, so there is no default to fall back to.
	ordinal int
	uidBase int
	uidSpan int
}

var _ NetLeaseAllocator = (*FleetNetAllocator)(nil)

// NewFleetNetAllocator constructs the allocator for one host. A negative ordinal
// (the caller could not resolve one) is accepted on purpose and yields an
// allocator that refuses every Acquire while still serving Release — teardown of
// VMs recorded before the ordinal was lost must keep working.
func NewFleetNetAllocator(
	leases repository.NetLeaseRepository,
	selfHost uuid.UUID,
	ordinal int,
	uidBase int,
	uidSpan int,
) *FleetNetAllocator {
	return &FleetNetAllocator{
		leases:   leases,
		selfHost: selfHost,
		ordinal:  ordinal,
		uidBase:  uidBase,
		uidSpan:  uidSpan,
	}
}

// Ordinal reports this host's assigned block, or a negative value when it has
// none. Read by the boot wiring to decide whether the plane may serve boots.
func (a *FleetNetAllocator) Ordinal() int { return a.ordinal }

// Acquire returns the lease this owner already holds, or allocates the lowest free
// slot on this host.
//
// Ordering matters: the owner lookup comes FIRST. A boot that is retried after a
// crash between "lease inserted" and "VM started" must re-use its own addresses;
// allocating a second lease would both burn a slot forever and leave two rows
// describing one owner.
func (a *FleetNetAllocator) Acquire(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (domain.NetLease, error) {
	lease, err := a.acquire(ctx, kind, ownerID)
	if err != nil {
		// Every refusal is a boot that did not happen, so it is counted at the ONE
		// seam every return path passes through — a per-branch increment is how a
		// later branch silently stops being counted.
		recordLeaseAcquire(leaseOutcomeRefused)
	}
	return lease, err
}

func (a *FleetNetAllocator) acquire(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (domain.NetLease, error) {
	if a.leases == nil {
		return domain.NetLease{}, fmt.Errorf("%w: no lease store wired", domain.ErrNetPlaneUnreconciled)
	}
	if !kind.IsValid() {
		return domain.NetLease{}, fmt.Errorf("%w: owner kind %q is not a recognized lease owner",
			domain.ErrNetCoordinateOutOfRange, kind)
	}
	if ownerID == uuid.Nil {
		return domain.NetLease{}, fmt.Errorf("%w: a net lease needs a real owner id (%s)",
			domain.ErrNetCoordinateOutOfRange, kind)
	}
	if a.selfHost == uuid.Nil {
		return domain.NetLease{}, fmt.Errorf("%w: this instance has no fleet host identity, so it cannot own an addressing block",
			domain.ErrHostNetOrdinalUnset)
	}
	if a.ordinal < 0 {
		return domain.NetLease{}, fmt.Errorf("%w: host %s", domain.ErrHostNetOrdinalUnset, a.selfHost)
	}

	existing, err := a.leases.FindByOwner(ctx, kind, ownerID)
	if err != nil && !errors.Is(err, domain.ErrNetLeaseNotFound) {
		return domain.NetLease{}, fmt.Errorf("look up existing net lease: %w", err)
	}
	if existing != nil {
		// A lease held on ANOTHER host is not adoptable here: configuring a VM on
		// this host with a /30 allocated out of another host's block would put two
		// machines on one address. Refuse rather than re-allocate — re-allocating
		// would leave the other host's lease held by a VM that no longer exists.
		if existing.HostID != a.selfHost {
			return domain.NetLease{}, fmt.Errorf(
				"%w: %s %s already holds net_index %d on host %s, not on this host %s",
				domain.ErrNetLeaseConflict, kind, ownerID, existing.NetIndex, existing.HostID, a.selfHost)
		}
		recordLeaseAcquire(leaseOutcomeAdopted)
		return *existing, nil
	}

	// Slots already refused by a conflict in THIS call. Without it a retry could
	// re-pick the same slot from a stale UsedSlots read and spin out its attempts.
	tried := make(map[int]bool)
	for attempt := 0; attempt < netLeaseAcquireAttempts; attempt++ {
		used, err := a.leases.UsedSlots(ctx, a.selfHost)
		if err != nil {
			return domain.NetLease{}, fmt.Errorf("list used net-lease slots: %w", err)
		}
		taken := make(map[int]bool, len(used)+len(tried))
		for _, s := range used {
			taken[s] = true
		}
		for s := range tried {
			taken[s] = true
		}

		slot := 0
		for candidate := 1; candidate <= domain.NetMaxSlot; candidate++ {
			if !taken[candidate] {
				slot = candidate
				break
			}
		}
		if slot == 0 {
			return domain.NetLease{}, fmt.Errorf("%w: host %s holds all %d slots",
				domain.ErrNetLeaseExhausted, a.selfHost, domain.NetMaxSlot)
		}

		lease, derr := domain.DeriveNetLease(a.ordinal, slot, a.uidBase)
		if derr != nil {
			return domain.NetLease{}, derr
		}
		// The uid fence is the jail's whole isolation guarantee, so it is checked
		// against the CONFIGURED span rather than assumed from the slot bound: a
		// narrowed APP_FC_VM_UID_SPAN must refuse boots, not silently hand out a uid
		// the jailer path treats as out of range.
		if lease.VMUID >= a.uidBase+a.uidSpan {
			return domain.NetLease{}, fmt.Errorf("%w: derived vm uid %d is outside the per-VM span [%d,%d)",
				domain.ErrNetCoordinateOutOfRange, lease.VMUID, a.uidBase, a.uidBase+a.uidSpan)
		}

		now := time.Now().UTC()
		lease.ID = uuid.New()
		lease.HostID = a.selfHost
		lease.OwnerKind = kind
		lease.OwnerID = ownerID
		lease.CreatedAt = now
		lease.UpdatedAt = now

		aerr := a.leases.Acquire(ctx, &lease)
		if aerr == nil {
			recordLeaseAcquire(leaseOutcomeAllocated)
			return lease, nil
		}
		if !errors.Is(aerr, domain.ErrNetLeaseConflict) {
			return domain.NetLease{}, aerr
		}
		// A conflict is a fence doing its job. It can also mean a concurrent boot of
		// the SAME owner won the (owner_kind, owner_id) fence, in which case the
		// lease to use is that one — so re-check the owner before trying a new slot.
		if held, herr := a.leases.FindByOwner(ctx, kind, ownerID); herr == nil && held.HostID == a.selfHost {
			recordLeaseAcquire(leaseOutcomeAdopted)
			return *held, nil
		}
		tried[slot] = true
		recordLeaseAcquire(leaseOutcomeConflictRetry)
		logger.FromContext(ctx).Warn("fleet net plane: lease conflict, retrying with the next free slot",
			"host_id", a.selfHost, "slot", slot, "net_index", lease.NetIndex,
			"owner_kind", kind, "owner_id", ownerID, "attempt", attempt+1, "err", aerr)
	}
	return domain.NetLease{}, fmt.Errorf("%w: could not claim a free slot on host %s in %d attempts",
		domain.ErrNetLeaseConflict, a.selfHost, netLeaseAcquireAttempts)
}

// Release frees an owner's lease. It is called from teardown, which is never
// blockable, so it does the one thing it can do and reports the error for the
// caller to log rather than swallowing it.
func (a *FleetNetAllocator) Release(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error {
	if a.leases == nil {
		return fmt.Errorf("%w: no lease store wired", domain.ErrNetPlaneUnreconciled)
	}
	if !kind.IsValid() || ownerID == uuid.Nil {
		// Refusing here is not pedantry: a release with no identity cannot be
		// targeted, and guessing (by index, say) could free a DIFFERENT live VM's
		// addresses — which the next boot would then re-use underneath it.
		return fmt.Errorf("%w: cannot release a net lease without an owner identity (kind=%q id=%s)",
			domain.ErrNetCoordinateOutOfRange, kind, ownerID)
	}
	return a.leases.Release(ctx, kind, ownerID)
}
