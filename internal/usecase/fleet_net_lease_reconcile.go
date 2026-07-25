package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// leaseAdoptGrace is how long a lease with no recorded pid is left alone.
//
// It is the window a boot legitimately occupies between "lease inserted" and
// "pid recorded": the lease is written BEFORE the TAP and the VM exist, so a
// lease with no pid is either a boot in flight or the residue of one that died.
// Ten minutes is far longer than any boot (materialize + start is seconds to a
// couple of minutes) and costs nothing but one slot, whereas reclaiming too early
// would hand a booting VM's address, uid and chroot to a second tenant.
const leaseAdoptGrace = 10 * time.Minute

// NetLeaseReclaimer releases the HOST-SIDE artifacts a lease names: the TAP
// device and the jailer chroot keyed by its local slot. It is a separate, narrow
// port from ImageBooter on purpose — reclaiming a lease whose owner ROW is gone
// has no VM handle to decommission, only the lease's own recorded coordinates.
type NetLeaseReclaimer interface {
	// ReclaimLeaseArtifacts removes the TAP device and jail directory the lease
	// names. Best-effort by contract (it runs on cleanup paths), so it reports
	// what it could not do rather than refusing to continue.
	ReclaimLeaseArtifacts(ctx context.Context, lease domain.NetLease) error
}

// NetLeaseReconcileReport is what one boot-time reconcile did. It is returned (not
// just logged) so the wiring can publish it on the ops posture surface: "which
// addresses does this host believe are held" is the question an operator asks
// after a restart.
type NetLeaseReconcileReport struct {
	// HostOrdinal is the /1024 block this host allocates from.
	HostOrdinal int
	// Leases is how many leases this host held when the reconcile started.
	Leases int
	// Adopted counts live VMs whose lease matched their owner row exactly.
	Adopted int
	// TornDown counts leases whose VM was stopped by this reconcile.
	TornDown int
	// Reclaimed counts leases deleted (their slot is free again).
	Reclaimed int
	// Left counts leases deliberately untouched (a boot still inside its grace).
	Left int
}

// FleetNetLeaseReconciler reconciles this host's addressing leases against the
// reality of the host at startup — the ONE moment the two can be out of step,
// because kernel devices, jail directories and VMM processes all survive a
// process restart while the process's own memory does not.
//
// ⚠ It is HOST-SCOPED and it fails CLOSED. It only ever considers leases whose
// host_id is this instance's, and any condition it cannot explain returns an
// error, which the wiring turns into "every boot on this host is refused". That
// asymmetry is deliberate: refusing to boot costs availability, whereas guessing
// about a live VM's addressing costs a customer's data.
type FleetNetLeaseReconciler struct {
	leases    repository.NetLeaseRepository
	hosts     repository.HostRepository
	replicas  repository.ReplicaRepository
	workloads repository.ImageWorkloadRepository
	booter    ImageBooter
	reclaimer NetLeaseReclaimer
	selfHost  uuid.UUID
	uidBase   int
}

// NewFleetNetLeaseReconciler constructs the boot-time reconcile.
func NewFleetNetLeaseReconciler(
	leases repository.NetLeaseRepository,
	hosts repository.HostRepository,
	replicas repository.ReplicaRepository,
	workloads repository.ImageWorkloadRepository,
	booter ImageBooter,
	reclaimer NetLeaseReclaimer,
	selfHost uuid.UUID,
	uidBase int,
) *FleetNetLeaseReconciler {
	return &FleetNetLeaseReconciler{
		leases:    leases,
		hosts:     hosts,
		replicas:  replicas,
		workloads: workloads,
		booter:    booter,
		reclaimer: reclaimer,
		selfHost:  selfHost,
		uidBase:   uidBase,
	}
}

// Reconcile brings this host's lease set in line with reality and returns the
// host ordinal boots may allocate from.
//
// The decision table, per lease held by THIS host:
//
//	owner row missing                          → RECLAIM
//	owner terminal (replica dead / workload
//	  exited|failed)                           → TEAR DOWN, then RECLAIM
//	owner occupying, recorded pid not alive     → TEAR DOWN, then RECLAIM
//	owner occupying, pid alive, addresses match → ADOPT
//	owner occupying, pid alive, addresses differ → FAIL CLOSED
//	owner occupying, no pid, inside grace       → LEAVE
//	owner occupying, no pid, past grace         → RECLAIM
//
// The `dead` row above is the fix for a live fail-open hole: RefreshHealth marks a
// replica dead WITHOUT stopping its VMM, and the old in-memory allocator's seed
// did not treat `dead` as occupying — so a still-running VM's index was handed to
// the next boot. Tearing the VM down BEFORE releasing the slot is what closes it.
//
// Deliberately NOT included: sweeping img<N> devices that no lease names. createTap
// already deletes a stale device before creating one, and a blind device sweep
// could race another process's boot — deleting a TAP out from under a VM that is
// starting.
func (uc *FleetNetLeaseReconciler) Reconcile(ctx context.Context) (NetLeaseReconcileReport, error) {
	var report NetLeaseReconcileReport
	log := logger.FromContext(ctx)

	if uc.leases == nil || uc.hosts == nil || uc.replicas == nil || uc.workloads == nil {
		return report, fmt.Errorf("%w: the reconcile needs the lease, host, replica and workload stores to decide anything",
			domain.ErrNetPlaneUnreconciled)
	}
	// An instance with no host identity cannot scope the reconcile, and an
	// unscoped reconcile would judge — and tear down — another host's VMs. This is
	// an ERROR rather than a skipped no-op: the fleet host path guarantees an
	// identity (APP_FLEET_HOST_ID is fatal there), so its absence is a real
	// misconfiguration, not a benign off-host case.
	if uc.selfHost == uuid.Nil {
		return report, fmt.Errorf("%w: this instance has no fleet host identity to scope by",
			domain.ErrNetPlaneUnreconciled)
	}

	ordinal, err := uc.resolveOrdinal(ctx)
	if err != nil {
		return report, err
	}
	report.HostOrdinal = ordinal

	held, err := uc.leases.ListByHost(ctx, uc.selfHost)
	if err != nil {
		return report, fmt.Errorf("%w: list leases held on host %s: %v",
			domain.ErrNetPlaneUnreconciled, uc.selfHost, err)
	}
	report.Leases = len(held)

	leasedOwners := make(map[string]bool, len(held))
	// net_index → the lease that holds it, so a collision can be reported as a PAIR
	// (which row holds the address, and which row also claims it). One side alone
	// tells an operator nothing actionable.
	holderOfIndex := make(map[int]domain.NetLease, len(held))
	for i := range held {
		lease := held[i]
		leasedOwners[ownerKey(lease.OwnerKind, lease.OwnerID)] = true
		holderOfIndex[lease.NetIndex] = lease

		if verr := uc.verifyLease(lease); verr != nil {
			return report, verr
		}
		if err := uc.reconcileLease(ctx, lease, &report); err != nil {
			return report, err
		}
	}

	// The collision signal. An owner row that OCCUPIES an index but holds no lease
	// means two rows once claimed one index and this one lost the backfill — i.e.
	// the host may already be running two VMs on one address/uid/chroot. That is
	// not repairable by allocating it a fresh lease (which address is the VM
	// actually configured with?), so the host refuses to boot anything until an
	// operator resolves it.
	if err := uc.assertNoLeaselessOwner(ctx, ordinal, leasedOwners, holderOfIndex); err != nil {
		return report, err
	}

	log.Info("fleet net plane: reconciled",
		"host_id", uc.selfHost, "host_ordinal", ordinal, "leases", report.Leases,
		"adopted", report.Adopted, "torn_down", report.TornDown,
		"reclaimed", report.Reclaimed, "left", report.Left)
	return report, nil
}

// resolveOrdinal reads this host's assigned block. A missing host row or a NULL
// ordinal is fatal: without a block there is no address space to allocate from,
// and defaulting to 0 would alias whichever host legitimately owns it.
func (uc *FleetNetLeaseReconciler) resolveOrdinal(ctx context.Context) (int, error) {
	host, err := uc.hosts.FindByID(ctx, uc.selfHost)
	if err != nil {
		return 0, fmt.Errorf("%w: load host row %s: %v", domain.ErrNetPlaneUnreconciled, uc.selfHost, err)
	}
	if host.NetOrdinal == nil {
		return 0, fmt.Errorf("%w: host %s has no assigned net ordinal",
			domain.ErrHostNetOrdinalUnset, uc.selfHost)
	}
	ord := *host.NetOrdinal
	if ord < 0 || ord > domain.NetMaxOrdinal {
		return 0, fmt.Errorf("%w: host %s carries ordinal %d",
			domain.ErrNetCoordinateOutOfRange, uc.selfHost, ord)
	}
	return ord, nil
}

// verifyLease re-derives a lease's coordinates from its own recorded
// (host_ordinal, local_slot) and refuses any lease that does not match itself.
//
// This catches a class of problem no fence can: a row written by something that
// does not share this plane's arithmetic (a hand-edit, a bad backfill, a changed
// APP_FC_VM_UID_BASE). Such a lease fences the WRONG uid/address — it looks like
// protection while protecting nothing — so the host refuses to boot rather than
// trusting it.
//
// It checks the lease against ITS OWN recorded ordinal, not the host's current
// one. A lease from an older block is not an error: its coordinates are
// self-consistent and net_index is globally unique, so it cannot collide with the
// current block — and re-deriving it from today's ordinal would "correct" the
// addressing of a VM that is running on the old one.
func (uc *FleetNetLeaseReconciler) verifyLease(lease domain.NetLease) error {
	derived, err := domain.DeriveNetLease(lease.HostOrdinal, lease.LocalSlot, uc.uidBase)
	if err != nil {
		return fmt.Errorf("%w: lease %s (owner %s/%s) has coordinates outside the plane: %v",
			domain.ErrNetPlaneUnreconciled, lease.ID, lease.OwnerKind, lease.OwnerID, err)
	}
	if derived.NetIndex != lease.NetIndex ||
		derived.HostIP != lease.HostIP ||
		derived.GuestIP != lease.GuestIP ||
		derived.TapName != lease.TapName ||
		derived.VMUID != lease.VMUID {
		return fmt.Errorf("%w: lease %s (owner %s/%s) is not self-consistent — recorded {index=%d host_ip=%s guest_ip=%s tap=%s uid=%d} but ordinal %d slot %d derives {index=%d host_ip=%s guest_ip=%s tap=%s uid=%d}",
			domain.ErrNetPlaneUnreconciled, lease.ID, lease.OwnerKind, lease.OwnerID,
			lease.NetIndex, lease.HostIP, lease.GuestIP, lease.TapName, lease.VMUID,
			lease.HostOrdinal, lease.LocalSlot,
			derived.NetIndex, derived.HostIP, derived.GuestIP, derived.TapName, derived.VMUID)
	}
	return nil
}

// reconcileLease applies the decision table to one lease.
func (uc *FleetNetLeaseReconciler) reconcileLease(ctx context.Context, lease domain.NetLease, report *NetLeaseReconcileReport) error {
	log := logger.FromContext(ctx)

	owner, err := uc.loadOwner(ctx, lease)
	if err != nil {
		return err
	}
	if owner == nil {
		log.Warn("fleet net plane: reclaiming a lease whose owner row is gone",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"net_index", lease.NetIndex, "tap_name", lease.TapName)
		return uc.reclaim(ctx, lease, report)
	}

	switch {
	case owner.terminal:
		// The row says the workload is over but nothing proves the VMM stopped —
		// RefreshHealth marks a replica dead without touching it. Tear it down FIRST;
		// releasing the slot while its VM still runs is the cross-tenant hole.
		log.Warn("fleet net plane: owner is terminal, tearing its VM down before releasing the slot",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"state", owner.state, "pid", owner.pid, "net_index", lease.NetIndex)
		return uc.tearDownAndReclaim(ctx, lease, owner, report)

	case owner.pid > 0 && !processAlive(owner.pid):
		log.Warn("fleet net plane: owner's recorded VMM pid is gone, tearing down the residue and releasing the slot",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"pid", owner.pid, "net_index", lease.NetIndex)
		return uc.tearDownAndReclaim(ctx, lease, owner, report)

	case owner.pid > 0:
		// A LIVE VM. Adoption is the only safe outcome, and only when the row the
		// fleet will operate it through names exactly the addressing the lease holds.
		if !lease.MatchesAddresses(owner.guestIP, owner.tapName) {
			return fmt.Errorf("%w: live %s %s (pid %d) is recorded at guest_ip=%q tap=%q but its lease %s holds guest_ip=%q tap=%q (net_index %d) — one of the two records is wrong about a RUNNING VM, and this host will not hand out addresses until that is resolved",
				domain.ErrNetPlaneUnreconciled, lease.OwnerKind, lease.OwnerID, owner.pid,
				lease.ID, owner.guestIP, owner.tapName, lease.GuestIP, lease.TapName, lease.NetIndex)
		}
		report.Adopted++
		log.Info("fleet net plane: adopted a live VM's lease",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"pid", owner.pid, "net_index", lease.NetIndex, "guest_ip", lease.GuestIP, "tap_name", lease.TapName)
		return nil

	case time.Since(lease.CreatedAt) < leaseAdoptGrace:
		// A boot in flight: the lease exists, the pid does not yet. Leave it.
		report.Left++
		log.Info("fleet net plane: leaving a lease with no pid inside its boot grace",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"net_index", lease.NetIndex, "age", time.Since(lease.CreatedAt).String())
		return nil

	default:
		log.Warn("fleet net plane: reclaiming a lease that never recorded a pid",
			"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
			"state", owner.state, "net_index", lease.NetIndex, "age", time.Since(lease.CreatedAt).String())
		return uc.reclaim(ctx, lease, report)
	}
}

// leaseOwner is the owner row's addressing-relevant facts, flattened so the
// decision table does not branch on which table it came from.
type leaseOwner struct {
	state    string
	terminal bool
	pid      int
	guestIP  string
	tapName  string
	handle   ImageDecommissionInput
}

// loadOwner resolves a lease's owner row, or nil when it no longer exists. A
// lookup ERROR is fatal (never "assume gone"): reclaiming on a DB blip would tear
// down a live customer VM.
func (uc *FleetNetLeaseReconciler) loadOwner(ctx context.Context, lease domain.NetLease) (*leaseOwner, error) {
	switch lease.OwnerKind {
	case domain.NetLeaseOwnerReplica:
		replica, err := uc.replicas.FindByID(ctx, lease.OwnerID)
		if errors.Is(err, domain.ErrReplicaNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: load replica %s for lease %s: %v",
				domain.ErrNetPlaneUnreconciled, lease.OwnerID, lease.ID, err)
		}
		pid := 0
		if replica.PID != nil {
			pid = *replica.PID
		}
		return &leaseOwner{
			state:    string(replica.State),
			terminal: replica.State == domain.ReplicaStateDead,
			pid:      pid,
			guestIP:  replica.GuestIP,
			tapName:  replica.TapName,
			handle:   replicaDecommissionInput(replica),
		}, nil

	case domain.NetLeaseOwnerWorkload:
		workload, err := uc.workloads.FindByID(ctx, lease.OwnerID)
		if errors.Is(err, domain.ErrWorkloadNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: load workload %s for lease %s: %v",
				domain.ErrNetPlaneUnreconciled, lease.OwnerID, lease.ID, err)
		}
		pid := 0
		if workload.PID != nil {
			pid = *workload.PID
		}
		terminal := workload.State == domain.ImageWorkloadStateExited ||
			workload.State == domain.ImageWorkloadStateFailed
		return &leaseOwner{
			state:    string(workload.State),
			terminal: terminal,
			pid:      pid,
			guestIP:  workload.GuestIP,
			tapName:  workload.TapName,
			handle:   decommissionInput(workload),
		}, nil

	default:
		// The DDL CHECK makes this unreachable through any write path this service
		// owns, which is exactly why it must not be ignored if it ever appears.
		return nil, fmt.Errorf("%w: lease %s has unrecognized owner kind %q",
			domain.ErrNetPlaneUnreconciled, lease.ID, lease.OwnerKind)
	}
}

// tearDownAndReclaim stops the owner's VM, then frees its addressing.
//
// The teardown result is LOGGED, not propagated: a VM that refuses to die must not
// leave its slot held forever, and the artifact reclaim below removes the TAP and
// the chroot regardless. This mirrors the booter's own "teardown is never
// blockable" contract.
func (uc *FleetNetLeaseReconciler) tearDownAndReclaim(ctx context.Context, lease domain.NetLease, owner *leaseOwner, report *NetLeaseReconcileReport) error {
	if uc.booter != nil {
		if err := uc.booter.Decommission(ctx, owner.handle); err != nil && !errors.Is(err, domain.ErrImageBootUnavailable) {
			logger.FromContext(ctx).Error("fleet net plane: teardown of a reclaimed lease's VM failed, continuing with the reclaim",
				"lease_id", lease.ID, "owner_kind", lease.OwnerKind, "owner_id", lease.OwnerID,
				"pid", owner.pid, "err", err)
		}
	}
	report.TornDown++
	return uc.reclaim(ctx, lease, report)
}

// reclaim removes the host artifacts the lease names and then deletes the lease.
//
// ORDER IS THE POINT: artifacts first, lease last. The lease is what keeps the
// slot from being re-allocated, so deleting it before the TAP and chroot are gone
// would let the next boot create a device that already exists and a jail whose
// directory is not clean.
func (uc *FleetNetLeaseReconciler) reclaim(ctx context.Context, lease domain.NetLease, report *NetLeaseReconcileReport) error {
	if uc.reclaimer != nil {
		if err := uc.reclaimer.ReclaimLeaseArtifacts(ctx, lease); err != nil {
			logger.FromContext(ctx).Warn("fleet net plane: reclaiming host artifacts",
				"lease_id", lease.ID, "tap_name", lease.TapName, "local_slot", lease.LocalSlot, "err", err)
		}
	}
	if err := uc.leases.Release(ctx, lease.OwnerKind, lease.OwnerID); err != nil {
		// A lease that cannot be released is a slot that must stay held. Failing the
		// whole reconcile is right: the alternative is a host that believes a slot is
		// free while the row that fences it still exists.
		return fmt.Errorf("%w: release lease %s (owner %s/%s): %v",
			domain.ErrNetPlaneUnreconciled, lease.ID, lease.OwnerKind, lease.OwnerID, err)
	}
	report.Reclaimed++
	return nil
}

// assertNoLeaselessOwner refuses to serve when an owner row that OCCUPIES an index
// holds no lease.
//
// This is the proven-collision signal. The backfill gave the OLDEST claimant of a
// duplicated index the lease and left the loser leaseless on purpose, because
// there is no safe way to guess which of two rows describes the VM that is
// actually running. Both rows are named in the error so an operator can look at
// the host and decide.
func (uc *FleetNetLeaseReconciler) assertNoLeaselessOwner(
	ctx context.Context,
	ordinal int,
	leased map[string]bool,
	holderOfIndex map[int]domain.NetLease,
) error {
	// describe names the offending row AND, when the index it claims is held by a
	// lease, the row that holds it — the pair is the collision.
	describe := func(kind, id, state string, netIndex int, guestIP, tap string) string {
		out := fmt.Sprintf("%s %s (state=%s net_index=%d guest_ip=%s tap=%s) holds NO lease",
			kind, id, state, netIndex, guestIP, tap)
		if holder, ok := holderOfIndex[netIndex]; ok {
			out += fmt.Sprintf(", while net_index %d is leased to %s %s (lease %s, guest_ip=%s tap=%s)",
				netIndex, holder.OwnerKind, holder.OwnerID, holder.ID, holder.GuestIP, holder.TapName)
		}
		return out
	}

	occupying := []domain.ReplicaState{
		domain.ReplicaStateBooting,
		domain.ReplicaStateResident,
		domain.ReplicaStatePaused,
		domain.ReplicaStateDraining,
		domain.ReplicaStateDead,
	}
	var offenders []string
	for _, state := range occupying {
		replicas, err := uc.replicas.ListByState(ctx, state)
		if err != nil {
			return fmt.Errorf("%w: list %s replicas: %v", domain.ErrNetPlaneUnreconciled, state, err)
		}
		for i := range replicas {
			r := replicas[i]
			// Host-scoped: a replica placed on another host is that host's to reconcile.
			if r.HostID == nil || *r.HostID != uc.selfHost || r.NetIndex <= 0 {
				continue
			}
			if leased[ownerKey(domain.NetLeaseOwnerReplica, r.ID)] {
				continue
			}
			offenders = append(offenders, describe("replica", r.ID.String(), string(r.State), r.NetIndex, r.GuestIP, r.TapName))
		}
	}

	// image_workloads carry no host id — the CP3 boot path is single-host by
	// construction and its VMs live on the ordinal-0 host. Judging them from any
	// other host would blame this host for another's rows.
	if ordinal == 0 {
		active, err := uc.workloads.FindActive(ctx)
		if err != nil {
			return fmt.Errorf("%w: list active image workloads: %v", domain.ErrNetPlaneUnreconciled, err)
		}
		for i := range active {
			w := active[i]
			if w.NetIndex <= 0 || leased[ownerKey(domain.NetLeaseOwnerWorkload, w.ID)] {
				continue
			}
			offenders = append(offenders, describe("workload", w.ID.String(), string(w.State), w.NetIndex, w.GuestIP, w.TapName))
		}
	}

	if len(offenders) > 0 {
		return fmt.Errorf("%w: %d occupying row(s) on host %s claim an index with NO lease, which can only mean two rows claimed one address/uid/chroot: %s",
			domain.ErrNetPlaneUnreconciled, len(offenders), uc.selfHost, strings.Join(offenders, "; "))
	}
	return nil
}

// ownerKey is the (kind, id) composite the owner fence is built on.
func ownerKey(kind domain.NetLeaseOwnerKind, id uuid.UUID) string {
	return string(kind) + ":" + id.String()
}
