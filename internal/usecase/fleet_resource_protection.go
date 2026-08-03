package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// D-202 — protection attaches or the provision fails.
//
// ONE COMPUTATION, TWO CONSUMERS. Whether a durable resource's protection can
// attach, and whether it is attached now, is computed HERE and nowhere else. The
// accept gate (provisionDedicated) and the customer-visible status (StatusOf)
// both read this same evaluation, which is what makes the gate and the report
// incapable of disagreeing: the moment they were two computations, one of them
// would start telling a nicer story.
//
// ⚠ FACTS, NEVER CONFIGURATION. Attachment is proven by a heartbeat ROW written
// by the worker into the ledger this process reads — never by an env var saying a
// worker exists. The empirical case is in the D-202 spec: the D-200 mirror was
// found ENABLED by config while draining a completely different database. A
// config-name miss must produce a WORSE promise; a boolean does the opposite.
//
// The two attachable components, and what each one's absence means:
//
//	cadence  per-host  — the snapshot-cadence worker on the host that holds (or
//	                     could hold) this resource's volumes is provably passing.
//	                     Absent ⇒ nothing takes scheduled recovery points there.
//	offsite  platform  — every artifact the resource produces reaches the
//	                     off-provider durability store of record (R2, D-213).
//	                     D-212 owns the sole writer; D-202 ships NONE, so today
//	                     nothing beats it and every non-waived durable provision
//	                     refuses naming it. That refusal is the truth of the fleet
//	                     and it opens with zero code change the day D-212 beats.
//
// Placement (resolveAvailability) is a THIRD component, already built in exactly
// this shape and never edited here — and never waivable: a waived placement would
// sell physically-impossible HA (I39/I40).
// ─────────────────────────────────────────────────────────────────────

// Condition tokens this file's evaluation contributes to ResourceStatus. Stable,
// machine-readable, and deliberately four rather than one: "unattached" (this
// resource was never enrolled) and "stalled" (it IS enrolled and the worker that
// serves it has stopped) call for different actions, and a single token would
// make the second look like the first forever.
const (
	// conditionProtectionOffsiteUnavailable — the off-provider durability store of
	// record is not provably receiving artifacts: the platform-wide `offsite`
	// heartbeat is absent, unreadable or stale. Every recovery point this fleet
	// takes is accumulating where losing the provider loses it.
	conditionProtectionOffsiteUnavailable = "protection-offsite-unavailable"
	// conditionProtectionCadenceUnattached — a durable resource with NO cadence
	// enrolment on its row (pre-D-202, or waived-and-unattachable). Its data is
	// only ever captured by a manual RPC or by teardown.
	conditionProtectionCadenceUnattached = "protection-cadence-unattached"
	// conditionProtectionCadenceStalled — the resource IS enrolled, but the cadence
	// worker on the host its volumes live on has no fresh heartbeat (or its host
	// cannot be resolved at all). The enrolment says snapshots should be happening;
	// this says they are not provably happening.
	conditionProtectionCadenceStalled = "protection-cadence-stalled"
	// conditionProtectionWaived — this resource was provisioned under a D-202
	// audited waiver. PERMANENT and always visible: a silent waiver is a label
	// nobody reads, and the whole justification for allowing an override is that it
	// stays on the record.
	conditionProtectionWaived = "protection-waived"
)

// ProtectionFactsReader is the narrow ledger slice the protection evaluation
// reads: worker liveness facts, and nothing else. Deliberately read-only and
// deliberately not the whole repository — an evaluation must not be able to write
// the fact it is judging.
type ProtectionFactsReader interface {
	// GetProtectionHeartbeat returns a component's newest beat in THIS ledger, or
	// domain.ErrProtectionHeartbeatNotFound when it has never beaten. scope is the
	// fleet host UUID for `cadence` and domain.ProtectionScopePlatform for
	// `offsite`.
	GetProtectionHeartbeat(ctx context.Context, component, scope string) (*domain.ProtectionHeartbeat, error)
}

// ProtectionAffinityReader resolves which host a resource's protection must run
// on: the host its CLAIM-OWNED volumes (D-203) are pinned to. It is the resource's
// own bytes that answer the question — never a guest IP, a PID, or the host this
// process happens to be.
type ProtectionAffinityReader interface {
	ListByResource(ctx context.Context, resourceID uuid.UUID) ([]domain.Volume, error)
}

// ProtectionConfig is the tunable NUMBERS of the gate — never its existence.
// Durations only, by construction: there is no key here that can be set to false
// (D-202 bans a config path to acceptance), and a cadence of <= 0 makes the
// cadence component UNATTACHABLE, which is the fail-safe direction of a config
// miss.
type ProtectionConfig struct {
	// CadenceSeconds is the snapshot cadence stamped onto every durable resource
	// at accept.
	CadenceSeconds int
	// CadenceStaleness / OffsiteStaleness bound how old a worker's heartbeat may be
	// while still counting as "provably running".
	CadenceStaleness time.Duration
	OffsiteStaleness time.Duration
}

// ProtectionWaiver is the D-202 per-resource audited override, carried on the
// provision call as typed data. Wire-agnostic on purpose: the actor is derived
// server-side from the authenticated principal (J6), never taken from a
// caller-supplied label, and this struct is what the use case is handed either
// way.
type ProtectionWaiver struct {
	// Actor is the principal the waiver is attributed to. Non-empty.
	Actor string
	// Reason is why a durable database was accepted unprotected. Non-empty.
	Reason string
}

// ProtectionComponentResult is one component's verdict.
type ProtectionComponentResult struct {
	// Component is the 0025 vocabulary name (`cadence` / `offsite`).
	Component string
	// Scope is what the verdict is ABOUT: a fleet host UUID for cadence, "" for
	// the platform-wide offsite component. Empty on cadence when the host set
	// itself could not be resolved.
	Scope string
	// Attached reports whether the component can attach right now.
	Attached bool
	// Err is why it cannot, wrapping the component's domain sentinel. nil iff
	// Attached.
	Err error
}

// ProtectionEvaluation is the structured verdict of ONE evaluation over live
// facts. Both consumers read this same value: the accept gate turns Err() into a
// refusal, the status path turns the per-component results into conditions.
type ProtectionEvaluation struct {
	Cadence ProtectionComponentResult
	Offsite ProtectionComponentResult
}

// Attached reports whether EVERY component attached.
func (e ProtectionEvaluation) Attached() bool { return e.Cadence.Attached && e.Offsite.Attached }

// Err is the refusal, or nil when everything attaches.
//
// It joins EVERY failed component rather than short-circuiting: a caller told
// only about the first unattachable component fixes it and is refused again, so
// one refusal must name every part that could not attach.
func (e ProtectionEvaluation) Err() error {
	var failures []error
	for _, r := range []ProtectionComponentResult{e.Cadence, e.Offsite} {
		if !r.Attached && r.Err != nil {
			failures = append(failures, r.Err)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", domain.ErrProtectionUnattachable, errors.Join(failures...))
}

// cadenceScopes is the set of hosts whose cadence workers must be live for the
// cadence component to attach, or the reason that set could not be established.
//
// A struct rather than ([]string, error) because "the set is unknowable" and "the
// set is empty" are both refusals and must both travel: an evaluation that
// silently saw zero hosts would attach a cadence nobody runs.
type cadenceScopes struct {
	hosts []string
	err   error
}

// evaluateProtection answers, from live facts only, whether each attachable
// protection component can attach RIGHT NOW for the given cadence scope set.
//
// It never short-circuits and it never returns an error of its own: the caller
// gets the whole structured verdict and decides what to do with it.
func (uc *FleetResourceProvisioner) evaluateProtection(ctx context.Context, scopes cadenceScopes) ProtectionEvaluation {
	return ProtectionEvaluation{
		Cadence: uc.evaluateCadence(ctx, scopes),
		Offsite: uc.evaluateOffsite(ctx),
	}
}

// evaluateCadence requires a fresh `cadence` beat from EVERY host in the scope
// set. One missing beat fails the component and NAMES that host: a set-wide
// verdict that hid which host was dark would be unactionable, and accepting on
// "some host beats" is the cross-host false positive migration 0025's scope-shape
// CHECK exists to forbid at the storage layer.
func (uc *FleetResourceProvisioner) evaluateCadence(ctx context.Context, scopes cadenceScopes) ProtectionComponentResult {
	res := ProtectionComponentResult{Component: domain.ProtectionComponentCadence}
	if uc.protection.CadenceSeconds <= 0 {
		res.Err = fmt.Errorf("%w: no snapshot cadence is configured, so there is no enrolment to attach", domain.ErrProtectionCadenceUnavailable)
		return res
	}
	if uc.facts == nil {
		res.Err = fmt.Errorf("%w: no protection fact ledger is wired, so the cadence worker cannot be proven to run", domain.ErrProtectionCadenceUnavailable)
		return res
	}
	if scopes.err != nil {
		res.Err = fmt.Errorf("%w: %w", domain.ErrProtectionCadenceUnavailable, scopes.err)
		return res
	}
	if len(scopes.hosts) == 0 {
		// Belt-and-braces: every resolver above already refuses an empty set. An
		// empty set reaching here would attach a cadence vacuously — the exact
		// fail-open shape this component exists to close.
		res.Err = fmt.Errorf("%w: no host could hold this resource, so no cadence worker can serve it", domain.ErrProtectionCadenceUnavailable)
		return res
	}
	now := uc.now()
	for _, host := range scopes.hosts {
		beat, err := uc.facts.GetProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, host)
		res.Scope = host
		switch {
		case errors.Is(err, domain.ErrProtectionHeartbeatNotFound):
			res.Err = fmt.Errorf("%w: host %s has never recorded a snapshot-cadence pass", domain.ErrProtectionCadenceUnavailable, host)
			return res
		case err != nil:
			// An unreadable fact is not a held fact — the same stance
			// resolveAvailability takes on an unreadable host inventory.
			logger.FromContext(ctx).Error("fleet resource: cadence heartbeat unreadable, treating the component as unattached",
				"host_id", host, "err", err)
			res.Err = fmt.Errorf("%w: the snapshot-cadence fact for host %s could not be read", domain.ErrProtectionCadenceUnavailable, host)
			return res
		case !beat.IsFreshAt(now, uc.protection.CadenceStaleness):
			res.Err = fmt.Errorf("%w: host %s last recorded a snapshot-cadence pass at %s, older than the %s staleness window",
				domain.ErrProtectionCadenceUnavailable, host, beat.BeatenAt.UTC().Format(time.RFC3339), uc.protection.CadenceStaleness)
			return res
		}
	}
	res.Scope = ""
	if len(scopes.hosts) == 1 {
		res.Scope = scopes.hosts[0]
	}
	res.Attached = true
	return res
}

// evaluateOffsite requires the fresh PLATFORM row. There is exactly one, and D-212
// is its only writer (J1) — a completed legacy drain does not change this, because
// receipts prove where OLD artifacts went, not where the next byte goes.
func (uc *FleetResourceProvisioner) evaluateOffsite(ctx context.Context) ProtectionComponentResult {
	res := ProtectionComponentResult{
		Component: domain.ProtectionComponentOffsite,
		Scope:     domain.ProtectionScopePlatform,
	}
	if uc.facts == nil {
		res.Err = fmt.Errorf("%w: no protection fact ledger is wired, so the durability store cannot be proven to receive artifacts", domain.ErrProtectionOffsiteUnproven)
		return res
	}
	beat, err := uc.facts.GetProtectionHeartbeat(ctx, domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform)
	switch {
	case errors.Is(err, domain.ErrProtectionHeartbeatNotFound):
		res.Err = fmt.Errorf("%w: no off-provider durability capture has ever been recorded in this ledger", domain.ErrProtectionOffsiteUnproven)
		return res
	case err != nil:
		logger.FromContext(ctx).Error("fleet resource: offsite heartbeat unreadable, treating the component as unattached", "err", err)
		res.Err = fmt.Errorf("%w: the off-provider durability fact could not be read", domain.ErrProtectionOffsiteUnproven)
		return res
	case !beat.IsFreshAt(uc.now(), uc.protection.OffsiteStaleness):
		res.Err = fmt.Errorf("%w: the last off-provider durability capture was at %s, older than the %s staleness window",
			domain.ErrProtectionOffsiteUnproven, beat.BeatenAt.UTC().Format(time.RFC3339), uc.protection.OffsiteStaleness)
		return res
	}
	res.Attached = true
	return res
}

// acceptScopes is the ACCEPT-time cadence scope set: every host the scheduler
// could place this claim on.
//
// The all-eligible-hosts rule (J3) rather than "the host it will land on": the
// placement is not chosen at accept, and inventing a second placement engine here
// to guess it would be a worse answer than requiring the fleet to be able to
// protect the resource wherever it lands. It reads the SAME live-host view
// resolveAvailability uses, for the same reason that gate does — a candidate set
// the two disagreed about would admit claims placement cannot satisfy.
func (uc *FleetResourceProvisioner) acceptScopes(ctx context.Context) cadenceScopes {
	if uc.hosts == nil {
		return cadenceScopes{err: errors.New("no live-host inventory is wired, so the hosts that would have to protect this resource are unknowable")}
	}
	live, err := uc.hosts.ListLive(ctx, uc.hostStaleness)
	if err != nil {
		logger.FromContext(ctx).Error("fleet resource: live host inventory unreadable, refusing to attach a cadence over an unknown host set", "err", err)
		return cadenceScopes{err: errors.New("the live host inventory could not be read, so the hosts that would have to protect this resource are unknowable")}
	}
	if len(live) == 0 {
		return cadenceScopes{err: errors.New("no host is live, so nothing could take this resource's scheduled snapshots")}
	}
	hosts := make([]string, 0, len(live))
	for i := range live {
		hosts = append(hosts, live[i].ID.String())
	}
	return cadenceScopes{hosts: hosts}
}

// statusScopes is the STATUS-time cadence scope set: the ONE host this resource's
// claim-owned volumes are pinned to (D-203 ownership, `host_affinity`).
//
// Missing, unreadable, unpinned or CONFLICTING affinity all resolve to "the
// protecting host is unprovable", which becomes a condition — never a guess, and
// never this process's own identity. A resource whose bytes are on another host is
// not protected by a worker here, and reporting otherwise is precisely the
// cross-host false positive D-202 exists to prevent.
func (uc *FleetResourceProvisioner) statusScopes(ctx context.Context, res *domain.FleetResource) cadenceScopes {
	if uc.affinity == nil {
		return cadenceScopes{err: errors.New("no volume-affinity reader is wired, so the host that would protect this resource is unknowable")}
	}
	vols, err := uc.affinity.ListByResource(ctx, res.ID)
	if err != nil {
		logger.FromContext(ctx).Error("fleet resource: claim-owned volume listing failed, reporting the protecting host as unprovable",
			"resource_id", res.ID, "err", err)
		return cadenceScopes{err: errors.New("this resource's claim-owned volumes could not be read, so the host that protects them is unknowable")}
	}
	seen := map[uuid.UUID]struct{}{}
	for i := range vols {
		if vols[i].HostAffinity == nil {
			return cadenceScopes{err: fmt.Errorf("volume %s carries no host affinity, so nothing says where this resource's bytes are", vols[i].ID)}
		}
		seen[*vols[i].HostAffinity] = struct{}{}
	}
	switch len(seen) {
	case 0:
		return cadenceScopes{err: errors.New("this resource owns no volume, so there is nothing for a cadence worker to snapshot")}
	case 1:
		for host := range seen {
			return cadenceScopes{hosts: []string{host.String()}}
		}
	}
	return cadenceScopes{err: fmt.Errorf("this resource's volumes are pinned to %d different hosts, so no single cadence worker protects it", len(seen))}
}

// resolveDedicatedDurability resolves the wire durability claim for the DEDICATED
// tier. "" and "durable" both mean durable — the tier IS durable (Aurora-shape,
// not disableable), so an absent claim is not a weaker promise, it is the same
// one. "ephemeral" is REFUSED rather than coerced: it names a combination the
// ledger cannot even represent (0025), and silently upgrading it would hand back
// something other than what was asked for.
func resolveDedicatedDurability(requested string) (domain.Durability, error) {
	switch requested {
	case "", string(domain.DurabilityDurable):
		return domain.DurabilityDurable, nil
	default:
		return "", fmt.Errorf("%w: the dedicated tier is durable, and %q is not", domain.ErrResourceDurabilityInvalid, requested)
	}
}

// normalizeWaiver validates the D-202 override. Half a waiver — an actor with no
// reason, or a reason attributable to nobody — is refused rather than stored:
// the override is only tolerable because it leaves a complete, permanent record,
// and 0025's CHECK refuses to hold an incomplete one anyway.
func normalizeWaiver(w *ProtectionWaiver) (*ProtectionWaiver, error) {
	if w == nil {
		return nil, nil
	}
	if w.Actor == "" || w.Reason == "" {
		return nil, domain.ErrProtectionWaiverIncomplete
	}
	return &ProtectionWaiver{Actor: w.Actor, Reason: w.Reason}, nil
}
