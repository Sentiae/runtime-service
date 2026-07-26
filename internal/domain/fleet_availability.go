package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// AvailabilityClass is whether a resource has a second, synchronously-replicating
// member — a THIRD axis, independent of Tier (isolation: shared/dedicated) and of
// durability (retention). It is its own column (migration 0022) so the thing a
// customer is sold is RECORDED rather than inferred from a column that means
// something else.
type AvailabilityClass string

const (
	// AvailabilityClassSingle — one member. The tier every resource has today.
	AvailabilityClassSingle AvailabilityClass = "single"
	// AvailabilityClassHA — `standard-ha`: a primary plus a synchronous standby in
	// a DIFFERENT failure domain and the SAME region.
	//
	// ⚠ On a resource row this value means CLAIMED, never HELD. The evidence that
	// the promise is held is a streaming standby member, and that machinery is
	// slices 2-3. Nothing may read this constant as proof of protection.
	AvailabilityClassHA AvailabilityClass = "ha"
)

// IsValid reports whether the class is one the fleet recognizes. Matches the
// migration 0022 CHECK exactly.
func (c AvailabilityClass) IsValid() bool {
	switch c {
	case AvailabilityClassSingle, AvailabilityClassHA:
		return true
	}
	return false
}

// SyncDegradePolicy is what an `ha` resource does when its synchronous standby is
// gone.
type SyncDegradePolicy string

const (
	// SyncDegradePolicyFailClosed — the primary cannot acknowledge a commit it did
	// not replicate. The durability promise and the zombie-primary fence are the
	// same mechanism (design §3.4 fence 1): a partitioned primary physically
	// cannot tell a client a write succeeded. This is the default and the only
	// value the tier is designed around.
	SyncDegradePolicyFailClosed SyncDegradePolicy = "fail_closed"
	// SyncDegradePolicyFailOpen — trade RPO for write availability. It exists so
	// the escape hatch is an explicit, per-resource, auditable ROW rather than an
	// operator's ad-hoc edit to a config file nobody diffs.
	SyncDegradePolicyFailOpen SyncDegradePolicy = "fail_open"
)

// IsValid reports whether the policy is one the fleet recognizes. Matches the
// migration 0022 CHECK exactly.
func (p SyncDegradePolicy) IsValid() bool {
	switch p {
	case SyncDegradePolicyFailClosed, SyncDegradePolicyFailOpen:
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// FROZEN WIRE CONTRACTS BETWEEN IMAGE GENERATIONS (D-196 amendment 4)
// ─────────────────────────────────────────────────────────────────────────────

// StandbyApplicationName is the standby's `application_name`, and therefore the
// exact string that appears in the primary's `synchronous_standby_names`:
//
//	std-<resource_id>-<generation>
//
// ⚠ FROZEN. This string IS the synchronous-quorum fence, not a label for one. The
// primary blocks every commit until a standby matching this name acknowledges it,
// and the name is carried in each standby's `primary_conninfo` inside a published
// engine image. Changing the syntax later orphans every standby that is already
// streaming — mid-flight, silently, with the primary still reporting a healthy
// quorum it is no longer getting — so it is exactly as frozen as a customer's
// hostname.
//
// It is GENERATION-SCOPED for one reason: a revived member from generation N must
// NOT be able to satisfy generation N+1's quorum. Without the generation in the
// name, an old standby coming back would let the new primary ack commits against
// data on a dead timeline (design §8 row 6).
func StandbyApplicationName(resourceID uuid.UUID, generation int) string {
	return fmt.Sprintf("std-%s-%d", resourceID, generation)
}

// ReplicationCertCommonName is the identity a per-resource replication certificate
// is issued under:
//
//	CN=resource:<resource_id>
//
// ⚠ FROZEN. The standby pins it with `sslmode=verify-full`, so the primary and
// every standby image must derive it identically or replication stops trusting a
// peer it should trust — and, worse, a scheme change could make a certificate
// issued for ANOTHER resource verify. Resource-scoped rather than host-scoped
// deliberately: the authorization question is "may this peer stream THIS
// database", which a host identity cannot answer.
func ReplicationCertCommonName(resourceID uuid.UUID) string {
	return "resource:" + resourceID.String()
}

// FailoverCause is the taxonomy of failover_events.cause. It is UNBACKFILLABLE:
// nothing about a finished row reveals which of the three it was, and the three
// have materially different durations (a switchover has no detection delay at
// all), so a population that cannot separate them reports an RTO nobody
// experienced.
type FailoverCause string

const (
	// FailoverCauseReal — an unplanned failure the detector observed.
	FailoverCauseReal FailoverCause = "real"
	// FailoverCauseDrill — a deliberate exercise of the path. The trust surface's
	// numbers come from these, which is why they must be distinguishable from the
	// friendlier switchover population.
	FailoverCauseDrill FailoverCause = "drill"
	// FailoverCauseSwitchover — a PLANNED move (cordon/drain): quiesce, wait for
	// full catch-up, promote.
	FailoverCauseSwitchover FailoverCause = "switchover"
)

// IsValid reports whether the cause is one the fleet recognizes. Matches the
// migration 0022 CHECK exactly.
func (c FailoverCause) IsValid() bool {
	switch c {
	case FailoverCauseReal, FailoverCauseDrill, FailoverCauseSwitchover:
		return true
	}
	return false
}

// The witness vocabulary recorded on failover_events.witnesses (design §3.5).
// Promotion is permitted only on W1 AND (W2 OR W3); storing which witnesses fired
// is what makes that rule auditable after the fact, and what distinguishes a
// legitimate promotion from one taken on a partition alone — the forbidden trigger.
// Must match the migration 0022 witness CHECK exactly.
const (
	// FailoverWitnessLeaseExpired (W1) — the primary's host stopped renewing the
	// lease for longer than its TTL. Alone it is exactly the forbidden trigger: it
	// is DEFINED by the partition.
	FailoverWitnessLeaseExpired = "w1_lease_expired"
	// FailoverWitnessStandbyReplicationLost (W2) — the standby's WAL receiver lost
	// its connection AND the standby cannot TCP-connect to the primary's guest IP.
	FailoverWitnessStandbyReplicationLost = "w2_standby_replication_lost"
	// FailoverWitnessGateBackendUnreachable (W3) — the co-resident db-gate cannot
	// open a backend connection, or is gone.
	FailoverWitnessGateBackendUnreachable = "w3_gate_backend_unreachable"
)

// ─────────────────────────────────────────────────────────────────────────────
// The placement invariant (design §5.1, D-196 amendment 2)
// ─────────────────────────────────────────────────────────────────────────────

// RequireHAPlacement reports whether the fleet can currently satisfy `standard-ha`'s
// placement invariant over the given LIVE hosts:
//
//	two hosts with DIFFERENT failure_domain and the SAME region — or the
//	resource is not provisioned.
//
// A refusal, never a preference. The same-region half is not decoration: D-190
// bakes the region into the customer's permanent hostname, so a standby in another
// region would serve a name that names the wrong place, and no later feature can
// rename it.
//
// It returns a specific sentinel naming the UNMET condition, because the operator
// action differs completely: buy a machine, attest a domain, move a machine, or
// place them in one region.
//
// ⚠ It decides nothing about WHICH hosts get used — placement itself is a later
// slice. This function answers only "is the invariant satisfiable at all", which
// with one physical machine is `no`, and that refusal is the deliverable.
func RequireHAPlacement(hosts []Host) error {
	// Only hosts that can be reasoned about at all. An unattested domain is
	// excluded rather than treated as unique: the whole failure mode this guards is
	// two members landing in one chassis while every dashboard says `ha`.
	candidates := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		if !h.HasAttestedFailureDomain() {
			continue
		}
		if h.Region == "" {
			continue
		}
		candidates = append(candidates, h)
	}

	if len(candidates) < 2 {
		// Distinguish "not enough machines" from "enough machines, unstated
		// domains": the first needs hardware, the second needs one line of config,
		// and answering the second with the first sends an operator shopping.
		if len(hosts) >= 2 {
			return ErrHAFailureDomainUnattested
		}
		return ErrHAHostsInsufficient
	}

	// Same region AND different domain, together — checked per region so a pair
	// that is split across regions can never satisfy it.
	domainsByRegion := make(map[string]map[string]struct{}, len(candidates))
	for _, h := range candidates {
		if domainsByRegion[h.Region] == nil {
			domainsByRegion[h.Region] = make(map[string]struct{}, 2)
		}
		domainsByRegion[h.Region][h.FailureDomain] = struct{}{}
	}
	allDomains := make(map[string]struct{}, len(candidates))
	for _, h := range candidates {
		allDomains[h.FailureDomain] = struct{}{}
	}
	for _, domains := range domainsByRegion {
		if len(domains) >= 2 {
			return nil
		}
	}
	if len(allDomains) >= 2 {
		// Distinct domains exist, but never two inside one region.
		return ErrHARegionSplit
	}
	return ErrHAFailureDomainShared
}
