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

// Enforcement tiers reported per policy (D-140). The fleet compiles iptables
// ACCEPTs from live guest IPs, which is a real enforcement point — so an
// installed rule is "enforced". A rule we did NOT install is never reported as
// enforced; the plan is told the truth.
const (
	EnforcementEnforced = "enforced"
	EnforcementAdvisory = "advisory"
)

// FleetNetworkFabric is the P21 NetworkFabric provider for the sentiae_fleet
// target class (CP4.5 §9 #5, D-164). Scope: ONE fleet, ONE org trust domain,
// iptables, no overlay agent, no CA. Cross-cloud (Nebula) is §9 #18 and is not
// this type.
//
// The network is a per-(system_id, env) POLICY SCOPE. system_id is an opaque key
// (the catalog Product ID): stored and compared, never dereferenced.
type FleetNetworkFabric struct {
	networks repository.FleetNetworkRepository
	policies repository.FleetNetworkPolicyRepository
	apps     repository.FleetAppRepository
	replicas repository.ReplicaRepository
	enforcer NetworkEnforcer
}

// NewFleetNetworkFabric constructs the fabric.
func NewFleetNetworkFabric(
	networks repository.FleetNetworkRepository,
	policies repository.FleetNetworkPolicyRepository,
	apps repository.FleetAppRepository,
	replicas repository.ReplicaRepository,
	enforcer NetworkEnforcer,
) *FleetNetworkFabric {
	return &FleetNetworkFabric{
		networks: networks,
		policies: policies,
		apps:     apps,
		replicas: replicas,
		enforcer: enforcer,
	}
}

// EnsureNetworkInput is the wire-agnostic EnsureNetwork request.
type EnsureNetworkInput struct {
	SystemID string
	Env      string
	OwnerOrg string
}

// EnsureNetworkOutput carries the NetworkHandle (the fleet_networks row id).
type EnsureNetworkOutput struct {
	Handle uuid.UUID
}

// PolicySpecInput is one requested policy (wire-agnostic).
type PolicySpecInput struct {
	FromComponentID   string
	ToComponentID     string
	Protocol          string
	Port              int
	DerivedFromEdgeID string
}

// ApplyPoliciesInput is the COMPLETE desired policy set for a network. It
// replaces, never merges. An empty Policies list is VALID and means "revoke
// everything" — the empty set is the most restrictive set, not a no-op.
type ApplyPoliciesInput struct {
	Handle   uuid.UUID
	Policies []PolicySpecInput
}

// PolicyEnforcementReport is the per-policy EnforcementReport (D-140).
type PolicyEnforcementReport struct {
	FromComponentID string
	ToComponentID   string
	Port            int
	Tier            string
	Detail          string
}

// ApplyPoliciesOutput carries the aggregate (weakest) tier plus the per-policy
// reports.
type ApplyPoliciesOutput struct {
	Aggregate string
	Policies  []PolicyEnforcementReport
}

// EnsureNetwork creates (or returns) the policy scope for a system×env. It is
// idempotent: a second call with the same key returns the same handle.
func (uc *FleetNetworkFabric) EnsureNetwork(ctx context.Context, in EnsureNetworkInput) (EnsureNetworkOutput, error) {
	if in.SystemID == "" || in.Env == "" {
		return EnsureNetworkOutput{}, domain.ErrInvalidNetworkPolicy
	}
	// A network is a net-new surface with no legacy caller, so an unattested
	// tenant is refused outright (unlike Provision, which still tolerates "" for
	// CP3 test boots). New surface ⇒ strict from birth.
	if in.OwnerOrg == "" {
		return EnsureNetworkOutput{}, domain.ErrNetworkOwnerOrgRequired
	}
	// Prove the host can enforce BEFORE recording a scope that implies it can.
	if err := uc.enforcer.AssertPosture(ctx); err != nil {
		return EnsureNetworkOutput{}, err
	}

	existing, err := uc.networks.FindBySystemEnv(ctx, in.SystemID, in.Env)
	switch {
	case err == nil:
		if existing.OwnerOrg != in.OwnerOrg {
			// The scope key is opaque and org-anchored: letting a second org adopt an
			// existing (system_id, env) would hand it the first org's policy scope.
			return EnsureNetworkOutput{}, domain.ErrNetworkOwnerOrgRequired
		}
		if existing.Status != domain.FleetNetworkActive {
			// REVIVE (D-179 §807, #fleet-network-revive-after-teardown): a re-Ensure of
			// a tombstoned scope reuses the SAME row rather than tombstone-refusing (the
			// old KNOWN GAP) or minting a second row (which uq_fleet_networks_system_env
			// forbids). The org-anchor guard above already blocked cross-org adoption, so
			// this is the original owner re-opening its own scope. The revived scope comes
			// back with ZERO reach: flip status Active, then install the EMPTY chain
			// (replace policies with the empty set, then re-sync) so nothing is reachable
			// until ApplyPolicies says otherwise (default-deny I23). The pre-teardown
			// policy rows are deliberately NOT re-animated — re-opening reach that was
			// explicitly torn down would be the fail-open reading.
			if merr := uc.networks.MarkActive(ctx, existing.ID); merr != nil {
				return EnsureNetworkOutput{}, fmt.Errorf("revive fleet network: %w", merr)
			}
			existing.Status = domain.FleetNetworkActive
			if rerr := uc.policies.ReplaceForNetwork(ctx, existing.ID, nil); rerr != nil {
				return EnsureNetworkOutput{}, fmt.Errorf("clear policies on revive: %w", rerr)
			}
			if serr := uc.syncNetwork(ctx, existing); serr != nil {
				return EnsureNetworkOutput{}, serr
			}
			return EnsureNetworkOutput{Handle: existing.ID}, nil
		}
		if serr := uc.syncNetwork(ctx, existing); serr != nil {
			return EnsureNetworkOutput{}, serr
		}
		return EnsureNetworkOutput{Handle: existing.ID}, nil
	case errors.Is(err, domain.ErrFleetNetworkNotFound):
		now := time.Now().UTC()
		n := &domain.FleetNetwork{
			ID:        uuid.New(),
			SystemID:  in.SystemID,
			Env:       in.Env,
			OwnerOrg:  in.OwnerOrg,
			Status:    domain.FleetNetworkActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if cerr := uc.networks.Create(ctx, n); cerr != nil {
			return EnsureNetworkOutput{}, fmt.Errorf("create fleet network: %w", cerr)
		}
		// A brand-new scope has no policies, so this installs an EMPTY system chain:
		// zero reach until ApplyPolicies says otherwise.
		if serr := uc.syncNetwork(ctx, n); serr != nil {
			return EnsureNetworkOutput{}, serr
		}
		return EnsureNetworkOutput{Handle: n.ID}, nil
	default:
		return EnsureNetworkOutput{}, fmt.Errorf("lookup fleet network: %w", err)
	}
}

// ApplyPolicies replaces a network's complete policy set and re-realizes it.
// The batch is all-or-nothing: ONE invalid policy rejects the whole set and
// applies nothing. A half-applied policy set is a lie about enforcement.
func (uc *FleetNetworkFabric) ApplyPolicies(ctx context.Context, in ApplyPoliciesInput) (ApplyPoliciesOutput, error) {
	if err := uc.enforcer.AssertPosture(ctx); err != nil {
		return ApplyPoliciesOutput{}, err
	}
	n, err := uc.networks.FindByID(ctx, in.Handle)
	if err != nil {
		return ApplyPoliciesOutput{}, err
	}
	if n.Status != domain.FleetNetworkActive {
		return ApplyPoliciesOutput{}, domain.ErrFleetNetworkNotFound
	}

	now := time.Now().UTC()
	compiled := make([]domain.FleetNetworkPolicy, 0, len(in.Policies))
	for _, spec := range in.Policies {
		p := domain.FleetNetworkPolicy{
			ID:                uuid.New(),
			NetworkID:         n.ID,
			FromComponentID:   spec.FromComponentID,
			ToComponentID:     spec.ToComponentID,
			Protocol:          spec.Protocol,
			Port:              spec.Port,
			DerivedFromEdgeID: spec.DerivedFromEdgeID,
			CreatedAt:         now,
		}
		// Validate BEFORE any write: reject, never widen, never default.
		if verr := p.Validate(); verr != nil {
			return ApplyPoliciesOutput{}, fmt.Errorf("policy %s->%s:%d: %w",
				spec.FromComponentID, spec.ToComponentID, spec.Port, verr)
		}
		compiled = append(compiled, p)
	}

	if err := uc.policies.ReplaceForNetwork(ctx, n.ID, compiled); err != nil {
		return ApplyPoliciesOutput{}, fmt.Errorf("replace policies: %w", err)
	}

	rules, reports, err := uc.resolve(ctx, n, compiled)
	if err != nil {
		return ApplyPoliciesOutput{}, err
	}
	if err := uc.enforcer.SyncSystem(ctx, n.ID, rules); err != nil {
		return ApplyPoliciesOutput{}, fmt.Errorf("sync system chain: %w", err)
	}
	return ApplyPoliciesOutput{Aggregate: aggregateTier(reports), Policies: reports}, nil
}

// Deprovision tears the scope down: the chain is removed and the row is
// TOMBSTONED, never deleted (SD3).
func (uc *FleetNetworkFabric) Deprovision(ctx context.Context, handle uuid.UUID) error {
	n, err := uc.networks.FindByID(ctx, handle)
	if err != nil {
		return err
	}
	if err := uc.enforcer.DropSystem(ctx, n.ID); err != nil {
		return fmt.Errorf("drop system chain: %w", err)
	}
	if err := uc.policies.ReplaceForNetwork(ctx, n.ID, nil); err != nil {
		return fmt.Errorf("clear policies: %w", err)
	}
	return uc.networks.MarkDeprovisioned(ctx, n.ID)
}

// RequireNetwork is the provision-time membership gate: a workload carrying a
// system_id may only boot when the host can enforce AND an ACTIVE network exists
// for (system_id, env). There is deliberately no auto-create-on-first-provision —
// that convenience is the permissive branch.
func (uc *FleetNetworkFabric) RequireNetwork(ctx context.Context, systemID, env string) error {
	if systemID == "" {
		return nil // no membership claimed → the cross-tenant DROP governs → nothing to gate
	}
	if err := uc.enforcer.AssertPosture(ctx); err != nil {
		return err
	}
	n, err := uc.networks.FindBySystemEnv(ctx, systemID, env)
	if err != nil {
		return err
	}
	if n.Status != domain.FleetNetworkActive {
		return domain.ErrFleetNetworkNotFound
	}
	return nil
}

// SyncForApp re-realizes the network an app belongs to. Called from the reconcile
// tick: a replica's guest IP is allocated per BOOT, so resolution must be
// reconciler-driven — that is what makes per-boot IPs safe and what lets a reboot
// self-heal with no delivery call.
func (uc *FleetNetworkFabric) SyncForApp(ctx context.Context, systemID, env string) error {
	if systemID == "" {
		return nil
	}
	n, err := uc.networks.FindBySystemEnv(ctx, systemID, env)
	if err != nil {
		if errors.Is(err, domain.ErrFleetNetworkNotFound) {
			return nil
		}
		return err
	}
	if n.Status != domain.FleetNetworkActive {
		return nil
	}
	return uc.syncNetwork(ctx, n)
}

// RestoreAll rebuilds every active network's chain from the DB. Called at DI init
// after InstallSkeleton, BEFORE anything serves: the system chains are ours and
// the kernel does not remember them across a host reboot.
//
// If this fails the caller flips the host to the fail-loud enforcer. Replicas
// already resident keep running, but their chains are absent, so their peers are
// unreachable — broken-CLOSED. A restart degrades to isolation, never to reach.
func (uc *FleetNetworkFabric) RestoreAll(ctx context.Context) error {
	ns, err := uc.networks.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active networks: %w", err)
	}
	for i := range ns {
		if err := uc.syncNetwork(ctx, &ns[i]); err != nil {
			return fmt.Errorf("restore network %s: %w", ns[i].ID, err)
		}
	}
	return nil
}

// ValidateEgressAllow rejects an egress allowlist entry that names the fleet's
// own subnet or a guest address. Egress is for EXTERNAL destinations; inter-VM
// reach is governed by network policies alone.
//
// This is the explicit half of a two-layer guard. The structural half is the
// chain topology (SNT-XVM is terminal for inter-VM flows and is evaluated before
// SNT-EGRESS), which holds even if this validation is bypassed. Neither layer is
// load-bearing alone — that is the point.
func ValidateEgressAllow(allow []string) error {
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if egressEntryOverlapsFleet(entry) {
			return fmt.Errorf("%w: %q", domain.ErrNetworkPolicyEgressOverlap, entry)
		}
	}
	return nil
}

// egressEntryOverlapsFleet reports whether an allowlist entry names fleet-internal
// address space: a bare guest IP, the fleet subnet itself, or a slice of it. A
// supernet is allowed — see domain.CIDRWithinFleetSubnet for why that is safe and
// deliberate.
func egressEntryOverlapsFleet(entry string) bool {
	if domain.InFleetSubnet(entry) {
		return true
	}
	return domain.CIDRWithinFleetSubnet(entry)
}

// syncNetwork resolves a network's desired policies against live replicas and
// pushes the result to the enforcer.
func (uc *FleetNetworkFabric) syncNetwork(ctx context.Context, n *domain.FleetNetwork) error {
	ps, err := uc.policies.ListForNetwork(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("list policies: %w", err)
	}
	rules, _, err := uc.resolve(ctx, n, ps)
	if err != nil {
		return err
	}
	if err := uc.enforcer.SyncSystem(ctx, n.ID, rules); err != nil {
		return fmt.Errorf("sync system chain: %w", err)
	}
	return nil
}

// resolve compiles desired policies into host rules by substituting the live
// guest IPs of each endpoint's replicas.
//
// A policy whose endpoint has NO live replica emits ZERO rules and reports
// `advisory`: the reach is denied and the report says so. There is no wildcard
// substitute, no 0.0.0.0/0, no subnet fallback — an unresolvable endpoint is a
// rule we do not install, and we never claim enforcement for a rule we did not
// install.
func (uc *FleetNetworkFabric) resolve(
	ctx context.Context,
	n *domain.FleetNetwork,
	ps []domain.FleetNetworkPolicy,
) ([]ResolvedRule, []PolicyEnforcementReport, error) {
	// The peer set is scoped by the org that OWNS the network row being resolved,
	// read from that persisted row — never from the ambient tenant context. system_id
	// is an opaque key no producer proves unique per org, so the org is what makes
	// two apps peers; taking it from ctx would let a system context (which carries no
	// tenant) or a forged org header widen the peer set to another tenant's workloads,
	// which is the whole failure mode. n.OwnerOrg is anchored at EnsureNetwork (a
	// second org cannot adopt an existing scope), so it is the app's own org for every
	// legitimately-provisioned member; an app carrying this system_id under a
	// DIFFERENT org is exactly what must not resolve, and now does not.
	apps, err := uc.apps.ListBySystemEnv(ctx, n.SystemID, n.Env, n.OwnerOrg)
	if err != nil {
		return nil, nil, fmt.Errorf("list system apps: %w", err)
	}
	byComponent := make(map[string]*domain.FleetApp, len(apps))
	for i := range apps {
		byComponent[apps[i].ComponentID] = &apps[i]
	}

	liveIPs := make(map[string][]string, len(apps))
	for componentID, app := range byComponent {
		reps, rerr := uc.replicas.ListByApp(ctx, app.ID)
		if rerr != nil {
			return nil, nil, fmt.Errorf("list replicas for %s: %w", componentID, rerr)
		}
		for i := range reps {
			if reps[i].State != domain.ReplicaStateResident {
				continue
			}
			// Defense in depth behind a poisoned or stale guest_ip column: an address
			// outside the fleet subnet is a bug, and a bug must not become a rule.
			if !domain.InFleetSubnet(reps[i].GuestIP) {
				if reps[i].GuestIP != "" {
					logger.FromContext(ctx).Error("fleet network: replica guest ip outside fleet subnet, skipping",
						"replica_id", reps[i].ID, "app_id", app.ID, "guest_ip", reps[i].GuestIP)
				}
				continue
			}
			liveIPs[componentID] = append(liveIPs[componentID], reps[i].GuestIP)
		}
	}

	rules := make([]ResolvedRule, 0, len(ps))
	reports := make([]PolicyEnforcementReport, 0, len(ps))
	for _, p := range ps {
		report := PolicyEnforcementReport{
			FromComponentID: p.FromComponentID,
			ToComponentID:   p.ToComponentID,
			Port:            p.Port,
			Tier:            EnforcementEnforced,
		}
		srcIPs := liveIPs[p.FromComponentID]
		dstIPs := liveIPs[p.ToComponentID]
		if len(srcIPs) == 0 || len(dstIPs) == 0 {
			report.Tier = EnforcementAdvisory
			report.Detail = fmt.Sprintf("no live replica for %s; no rule installed",
				unresolvedComponent(p, len(srcIPs), len(dstIPs)))
			reports = append(reports, report)
			continue
		}
		for _, src := range srcIPs {
			for _, dst := range dstIPs {
				rules = append(rules, ResolvedRule{
					SrcIP:    src,
					DstIP:    dst,
					Protocol: p.Protocol,
					Port:     p.Port,
				})
			}
		}
		reports = append(reports, report)
	}
	return rules, reports, nil
}

// unresolvedComponent names the endpoint that could not be resolved (the source
// when both are missing — the report is a diagnosis, not an inventory).
func unresolvedComponent(p domain.FleetNetworkPolicy, nSrc, nDst int) string {
	if nSrc == 0 {
		return p.FromComponentID
	}
	_ = nDst
	return p.ToComponentID
}

// aggregateTier returns the WEAKEST tier across the reports — the honest summary.
// An empty policy set is fully enforced: zero policies means zero reach, which is
// exactly what the chain delivers.
func aggregateTier(reports []PolicyEnforcementReport) string {
	for _, r := range reports {
		if r.Tier != EnforcementEnforced {
			return r.Tier
		}
	}
	return EnforcementEnforced
}
