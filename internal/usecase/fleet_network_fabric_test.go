//go:build unit

package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ── fakes ────────────────────────────────────────────────────────────

type fakeNetworkRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.FleetNetwork
}

func newFakeNetworkRepo() *fakeNetworkRepo {
	return &fakeNetworkRepo{store: map[uuid.UUID]*domain.FleetNetwork{}}
}

func (r *fakeNetworkRepo) Create(_ context.Context, n *domain.FleetNetwork) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *n
	r.store[n.ID] = &cp
	return nil
}

func (r *fakeNetworkRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetNetwork, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.store[id]
	if !ok {
		return nil, domain.ErrFleetNetworkNotFound
	}
	cp := *n
	return &cp, nil
}

func (r *fakeNetworkRepo) FindBySystemEnv(_ context.Context, systemID, env string) (*domain.FleetNetwork, error) {
	if systemID == "" {
		return nil, domain.ErrFleetNetworkNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.store {
		if n.SystemID == systemID && n.Env == env {
			cp := *n
			return &cp, nil
		}
	}
	return nil, domain.ErrFleetNetworkNotFound
}

func (r *fakeNetworkRepo) ListActive(_ context.Context) ([]domain.FleetNetwork, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.FleetNetwork
	for _, n := range r.store {
		if n.Status == domain.FleetNetworkActive {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (r *fakeNetworkRepo) MarkDeprovisioned(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.store[id]
	if !ok {
		return domain.ErrFleetNetworkNotFound
	}
	n.Status = domain.FleetNetworkDeprovisioned
	return nil
}

func (r *fakeNetworkRepo) MarkActive(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.store[id]
	if !ok {
		return domain.ErrFleetNetworkNotFound
	}
	n.Status = domain.FleetNetworkActive
	return nil
}

type fakePolicyRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID][]domain.FleetNetworkPolicy
}

func newFakePolicyRepo() *fakePolicyRepo {
	return &fakePolicyRepo{store: map[uuid.UUID][]domain.FleetNetworkPolicy{}}
}

func (r *fakePolicyRepo) ReplaceForNetwork(_ context.Context, networkID uuid.UUID, ps []domain.FleetNetworkPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store[networkID] = append([]domain.FleetNetworkPolicy{}, ps...)
	return nil
}

func (r *fakePolicyRepo) ListForNetwork(_ context.Context, networkID uuid.UUID) ([]domain.FleetNetworkPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.FleetNetworkPolicy{}, r.store[networkID]...), nil
}

type fakeNetAppRepo struct {
	apps []domain.FleetApp
}

func (r *fakeNetAppRepo) Create(context.Context, *domain.FleetApp) error { return nil }
func (r *fakeNetAppRepo) Update(context.Context, *domain.FleetApp) error { return nil }
func (r *fakeNetAppRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	for i := range r.apps {
		if r.apps[i].ID == id {
			return &r.apps[i], nil
		}
	}
	return nil, domain.ErrFleetAppNotFound
}
func (r *fakeNetAppRepo) FindByComponentEnv(_ context.Context, componentID, env, ownerOrg string) (*domain.FleetApp, error) {
	for i := range r.apps {
		// owner_org is part of the identity, not a filter — matching without it here
		// would let the fake serve a foreign org's app and hide the very defect the
		// org-scoped key exists to prevent.
		if r.apps[i].ComponentID == componentID && r.apps[i].Env == env && r.apps[i].OwnerOrg == ownerOrg {
			return &r.apps[i], nil
		}
	}
	return nil, domain.ErrFleetAppNotFound
}
func (r *fakeNetAppRepo) List(context.Context) ([]domain.FleetApp, error) { return r.apps, nil }
func (r *fakeNetAppRepo) ListBySystemEnv(_ context.Context, systemID, env string) ([]domain.FleetApp, error) {
	if systemID == "" {
		return nil, nil
	}
	var out []domain.FleetApp
	for i := range r.apps {
		if r.apps[i].SystemID == systemID && r.apps[i].Env == env {
			out = append(out, r.apps[i])
		}
	}
	return out, nil
}
func (r *fakeNetAppRepo) Delete(context.Context, uuid.UUID) error { return nil }

type fakeNetReplicaRepo struct {
	byApp map[uuid.UUID][]domain.Replica
}

func (r *fakeNetReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (r *fakeNetReplicaRepo) Update(context.Context, *domain.Replica) error { return nil }
func (r *fakeNetReplicaRepo) FindByID(context.Context, uuid.UUID) (*domain.Replica, error) {
	return nil, domain.ErrReplicaNotFound
}
func (r *fakeNetReplicaRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Replica, error) {
	return r.byApp[appID], nil
}
func (r *fakeNetReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (r *fakeNetReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (r *fakeNetReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }

// fakeEnforcer records what was pushed to the host.
type fakeEnforcer struct {
	mu       sync.Mutex
	synced   map[uuid.UUID][]ResolvedRule
	dropped  []uuid.UUID
	syncs    int
	posture  error
	syncFail error
}

func newFakeEnforcer() *fakeEnforcer {
	return &fakeEnforcer{synced: map[uuid.UUID][]ResolvedRule{}}
}

func (e *fakeEnforcer) InstallSkeleton(context.Context) error { return nil }
func (e *fakeEnforcer) AssertPosture(context.Context) error   { return e.posture }
func (e *fakeEnforcer) SyncSystem(_ context.Context, id uuid.UUID, rules []ResolvedRule) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.syncFail != nil {
		return e.syncFail
	}
	e.syncs++
	e.synced[id] = append([]ResolvedRule{}, rules...)
	return nil
}
func (e *fakeEnforcer) DropSystem(_ context.Context, id uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dropped = append(e.dropped, id)
	return nil
}

// ── harness ──────────────────────────────────────────────────────────

type fabricHarness struct {
	fabric   *FleetNetworkFabric
	networks *fakeNetworkRepo
	policies *fakePolicyRepo
	apps     *fakeNetAppRepo
	replicas *fakeNetReplicaRepo
	enforcer *fakeEnforcer
}

func newFabricHarness() *fabricHarness {
	h := &fabricHarness{
		networks: newFakeNetworkRepo(),
		policies: newFakePolicyRepo(),
		apps:     &fakeNetAppRepo{},
		replicas: &fakeNetReplicaRepo{byApp: map[uuid.UUID][]domain.Replica{}},
		enforcer: newFakeEnforcer(),
	}
	h.fabric = NewFleetNetworkFabric(h.networks, h.policies, h.apps, h.replicas, h.enforcer)
	return h
}

// addApp registers a component in a system with N resident replicas at the given
// guest IPs.
func (h *fabricHarness) addApp(componentID, systemID, env string, guestIPs ...string) uuid.UUID {
	id := uuid.New()
	h.apps.apps = append(h.apps.apps, domain.FleetApp{
		ID: id, ComponentID: componentID, SystemID: systemID, Env: env,
	})
	for _, ip := range guestIPs {
		h.replicas.byApp[id] = append(h.replicas.byApp[id], domain.Replica{
			ID: uuid.New(), AppID: id, State: domain.ReplicaStateResident, GuestIP: ip,
		})
	}
	return id
}

func (h *fabricHarness) ensure(t *testing.T, systemID, env, org string) uuid.UUID {
	t.Helper()
	out, err := h.fabric.EnsureNetwork(context.Background(), EnsureNetworkInput{
		SystemID: systemID, Env: env, OwnerOrg: org,
	})
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	return out.Handle
}

// ensureQuiet is ensure for table-driven setups that have no *testing.T.
func (h *fabricHarness) ensureQuiet(systemID, env, org string) uuid.UUID {
	out, _ := h.fabric.EnsureNetwork(context.Background(), EnsureNetworkInput{
		SystemID: systemID, Env: env, OwnerOrg: org,
	})
	return out.Handle
}

const testOrg = "11111111-1111-1111-1111-111111111111"

// ── EnsureNetwork ────────────────────────────────────────────────────

func TestEnsureNetwork_IsIdempotent(t *testing.T) {
	h := newFabricHarness()
	first := h.ensure(t, "prod-sys", "prod", testOrg)
	second := h.ensure(t, "prod-sys", "prod", testOrg)
	if first != second {
		t.Fatalf("handles differ: %s vs %s", first, second)
	}
	if got := len(h.networks.store); got != 1 {
		t.Fatalf("%d network rows, want 1", got)
	}
}

func TestEnsureNetwork_RejectsUnattestedOrg(t *testing.T) {
	h := newFabricHarness()
	_, err := h.fabric.EnsureNetwork(context.Background(), EnsureNetworkInput{
		SystemID: "s", Env: "prod", OwnerOrg: "",
	})
	if !errors.Is(err, domain.ErrNetworkOwnerOrgRequired) {
		t.Fatalf("EnsureNetwork = %v, want ErrNetworkOwnerOrgRequired", err)
	}
	if len(h.networks.store) != 0 {
		t.Fatal("an org-less network was persisted")
	}
}

func TestEnsureNetwork_RejectsForeignOrgAdoptingAnExistingScope(t *testing.T) {
	h := newFabricHarness()
	h.ensure(t, "shared-key", "prod", testOrg)
	_, err := h.fabric.EnsureNetwork(context.Background(), EnsureNetworkInput{
		SystemID: "shared-key", Env: "prod", OwnerOrg: "22222222-2222-2222-2222-222222222222",
	})
	if err == nil {
		t.Fatal("a second org adopted an existing (system_id, env) scope")
	}
}

func TestEnsureNetwork_RefusesWhenPostureCannotBeProven(t *testing.T) {
	h := newFabricHarness()
	h.enforcer.posture = domain.ErrNetworkEnforcerUnavailable
	_, err := h.fabric.EnsureNetwork(context.Background(), EnsureNetworkInput{
		SystemID: "s", Env: "prod", OwnerOrg: testOrg,
	})
	if !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("EnsureNetwork = %v, want ErrNetworkEnforcerUnavailable", err)
	}
	if len(h.networks.store) != 0 {
		t.Fatal("a network was recorded on a host that cannot enforce it")
	}
}

func TestEnsureNetwork_NewScopeInstallsAnEmptyChain(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	if got := h.enforcer.synced[handle]; len(got) != 0 {
		t.Fatalf("a new scope installed %d rules, want 0 — zero policies means zero reach", len(got))
	}
	if h.enforcer.syncs == 0 {
		t.Fatal("the chain was never installed; it must exist and deny")
	}
}

// ── ApplyPolicies ────────────────────────────────────────────────────

func TestApplyPolicies_CompilesLiveIPs(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod", "10.201.0.10")

	out, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	if out.Aggregate != EnforcementEnforced {
		t.Fatalf("aggregate = %q, want %q", out.Aggregate, EnforcementEnforced)
	}
	rules := h.enforcer.synced[handle]
	if len(rules) != 1 {
		t.Fatalf("%d rules, want 1: %+v", len(rules), rules)
	}
	want := ResolvedRule{SrcIP: "10.201.0.6", DstIP: "10.201.0.10", Protocol: "tcp", Port: 5432}
	if rules[0] != want {
		t.Fatalf("rule = %+v, want %+v", rules[0], want)
	}
}

func TestApplyPolicies_OneInvalidPolicyRejectsTheWholeBatch(t *testing.T) {
	tests := []struct {
		name    string
		bad     PolicySpecInput
		wantErr error
	}{
		{
			name:    "port zero",
			bad:     PolicySpecInput{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 0},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
		{
			name:    "empty protocol",
			bad:     PolicySpecInput{FromComponentID: "api", ToComponentID: "db", Protocol: "", Port: 5432},
			wantErr: domain.ErrUnsupportedPolicyProtocol,
		},
		{
			name:    "empty component",
			bad:     PolicySpecInput{FromComponentID: "", ToComponentID: "db", Protocol: "tcp", Port: 5432},
			wantErr: domain.ErrInvalidNetworkPolicy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFabricHarness()
			handle := h.ensure(t, "s", "prod", testOrg)
			h.addApp("api", "s", "prod", "10.201.0.6")
			h.addApp("db", "s", "prod", "10.201.0.10")
			syncsBefore := h.enforcer.syncs

			_, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
				Handle: handle,
				Policies: []PolicySpecInput{
					{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432},
					tt.bad,
					{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 6379},
				},
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ApplyPolicies = %v, want %v", err, tt.wantErr)
			}
			if len(h.policies.store[handle]) != 0 {
				t.Fatalf("policies were persisted from a rejected batch: %+v", h.policies.store[handle])
			}
			if h.enforcer.syncs != syncsBefore {
				t.Fatal("a rejected batch reached the enforcer — all-or-nothing was violated")
			}
		})
	}
}

func TestApplyPolicies_EmptySetRevokesEverything(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod", "10.201.0.10")
	ctx := context.Background()

	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	}); err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	// The arch edge is deleted and the complete (now empty) set re-applied.
	out, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{Handle: handle})
	if err != nil {
		t.Fatalf("ApplyPolicies empty: %v", err)
	}
	if len(h.enforcer.synced[handle]) != 0 {
		t.Fatalf("rules survived the revoke: %+v", h.enforcer.synced[handle])
	}
	if out.Aggregate != EnforcementEnforced {
		t.Fatalf("aggregate = %q, want %q — zero policies IS fully enforced", out.Aggregate, EnforcementEnforced)
	}
}

// The guard-5 regression: an endpoint with no live replica must produce NO rule
// and an honest report, never a wildcard substitute.
func TestApplyPolicies_NoLiveReplicaEmitsNoRuleAndReportsAdvisory(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod") // no replicas

	out, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	if got := h.enforcer.synced[handle]; len(got) != 0 {
		t.Fatalf("an unresolvable policy produced %d rules, want 0: %+v", len(got), got)
	}
	if out.Aggregate != EnforcementAdvisory {
		t.Fatalf("aggregate = %q, want %q", out.Aggregate, EnforcementAdvisory)
	}
	if len(out.Policies) != 1 || out.Policies[0].Tier != EnforcementAdvisory {
		t.Fatalf("reports = %+v, want one advisory", out.Policies)
	}
	if !strings.Contains(out.Policies[0].Detail, "db") {
		t.Fatalf("detail %q does not name the unresolved component", out.Policies[0].Detail)
	}
}

func TestApplyPolicies_NonResidentReplicaIsNotLive(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	dbID := h.addApp("db", "s", "prod")
	h.replicas.byApp[dbID] = []domain.Replica{
		{ID: uuid.New(), AppID: dbID, State: domain.ReplicaStateBooting, GuestIP: "10.201.0.10"},
		{ID: uuid.New(), AppID: dbID, State: domain.ReplicaStateDead, GuestIP: "10.201.0.14"},
	}
	if _, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	}); err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	if got := h.enforcer.synced[handle]; len(got) != 0 {
		t.Fatalf("a booting/dead replica was compiled into a rule: %+v", got)
	}
}

// Defense in depth against a poisoned guest_ip column.
func TestApplyPolicies_SkipsReplicaOutsideTheFleetSubnet(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod", "10.0.10.20") // poisoned

	out, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	if got := h.enforcer.synced[handle]; len(got) != 0 {
		t.Fatalf("an address outside the fleet subnet became a rule: %+v", got)
	}
	if out.Aggregate != EnforcementAdvisory {
		t.Fatalf("aggregate = %q, want %q", out.Aggregate, EnforcementAdvisory)
	}
}

func TestApplyPolicies_TwoSystemsGetDisjointChains(t *testing.T) {
	h := newFabricHarness()
	sHandle := h.ensure(t, "S", "prod", testOrg)
	tHandle := h.ensure(t, "T", "prod", testOrg)
	h.addApp("api", "S", "prod", "10.201.0.6")
	h.addApp("db", "S", "prod", "10.201.0.10")
	h.addApp("api", "T", "prod", "10.201.0.18")
	h.addApp("db", "T", "prod", "10.201.0.22")

	spec := []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}}
	ctx := context.Background()
	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{Handle: sHandle, Policies: spec}); err != nil {
		t.Fatalf("apply S: %v", err)
	}
	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{Handle: tHandle, Policies: spec}); err != nil {
		t.Fatalf("apply T: %v", err)
	}
	for _, r := range h.enforcer.synced[sHandle] {
		if r.SrcIP == "10.201.0.18" || r.DstIP == "10.201.0.22" {
			t.Fatalf("system S's chain contains system T's addresses: %+v", r)
		}
	}
	if len(h.enforcer.synced[sHandle]) != 1 || len(h.enforcer.synced[tHandle]) != 1 {
		t.Fatalf("want one rule per system, got S=%d T=%d",
			len(h.enforcer.synced[sHandle]), len(h.enforcer.synced[tHandle]))
	}
}

func TestApplyPolicies_RefusesWhenPostureCannotBeProven(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.enforcer.posture = domain.ErrNetworkPostureUnproven
	_, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if !errors.Is(err, domain.ErrNetworkPostureUnproven) {
		t.Fatalf("ApplyPolicies = %v, want ErrNetworkPostureUnproven", err)
	}
}

func TestApplyPolicies_RefusesADeprovisionedNetwork(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	if err := h.fabric.Deprovision(context.Background(), handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	_, err := h.fabric.ApplyPolicies(context.Background(), ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if !errors.Is(err, domain.ErrFleetNetworkNotFound) {
		t.Fatalf("ApplyPolicies = %v, want ErrFleetNetworkNotFound for a tombstoned scope", err)
	}
}

// ── reboot / reconcile ───────────────────────────────────────────────

// A replica reboots onto a new /30. The next reconcile-driven resolve must emit
// the NEW address and not the old one — with no ApplyPolicies call.
func TestSyncForApp_PicksUpARebootedReplicaIP(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	dbID := h.addApp("db", "s", "prod", "10.201.0.10")
	ctx := context.Background()
	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	}); err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}

	// The replica reboots onto a new index.
	h.replicas.byApp[dbID] = []domain.Replica{
		{ID: uuid.New(), AppID: dbID, State: domain.ReplicaStateResident, GuestIP: "10.201.0.14"},
	}
	if err := h.fabric.SyncForApp(ctx, "s", "prod"); err != nil {
		t.Fatalf("SyncForApp: %v", err)
	}
	rules := h.enforcer.synced[handle]
	if len(rules) != 1 || rules[0].DstIP != "10.201.0.14" {
		t.Fatalf("rules = %+v, want the rebooted IP 10.201.0.14", rules)
	}
}

func TestSyncForApp_UnscopedAppIsANoOp(t *testing.T) {
	h := newFabricHarness()
	if err := h.fabric.SyncForApp(context.Background(), "", "prod"); err != nil {
		t.Fatalf("SyncForApp = %v, want nil for an unscoped app", err)
	}
	if h.enforcer.syncs != 0 {
		t.Fatal("an unscoped app touched the enforcer")
	}
}

func TestRestoreAll_RebuildsEveryActiveNetwork(t *testing.T) {
	h := newFabricHarness()
	sHandle := h.ensure(t, "S", "prod", testOrg)
	tHandle := h.ensure(t, "T", "prod", testOrg)
	h.addApp("api", "S", "prod", "10.201.0.6")
	h.addApp("db", "S", "prod", "10.201.0.10")
	ctx := context.Background()
	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{
		Handle:   sHandle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	}); err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}

	// Simulate a restart: the host forgot everything.
	h.enforcer.synced = map[uuid.UUID][]ResolvedRule{}
	if err := h.fabric.RestoreAll(ctx); err != nil {
		t.Fatalf("RestoreAll: %v", err)
	}
	if len(h.enforcer.synced[sHandle]) != 1 {
		t.Fatalf("system S was not restored: %+v", h.enforcer.synced[sHandle])
	}
	if _, ok := h.enforcer.synced[tHandle]; !ok {
		t.Fatal("system T's (empty) chain was not restored; it must exist and deny")
	}
}

func TestRestoreAll_FailsLoudWhenTheEnforcerCannot(t *testing.T) {
	h := newFabricHarness()
	h.ensure(t, "S", "prod", testOrg)
	h.enforcer.syncFail = errors.New("iptables exploded")
	if err := h.fabric.RestoreAll(context.Background()); err == nil {
		t.Fatal("RestoreAll swallowed an enforcer failure — the caller must flip the host fail-loud")
	}
}

// ── Deprovision ──────────────────────────────────────────────────────

func TestDeprovision_DropsTheChainAndTombstonesTheRow(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	if err := h.fabric.Deprovision(context.Background(), handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(h.enforcer.dropped) != 1 || h.enforcer.dropped[0] != handle {
		t.Fatalf("dropped = %v, want [%s]", h.enforcer.dropped, handle)
	}
	n, err := h.networks.FindByID(context.Background(), handle)
	if err != nil {
		t.Fatalf("the row was DELETED, not tombstoned: %v", err)
	}
	if n.Status != domain.FleetNetworkDeprovisioned {
		t.Fatalf("status = %q, want %q", n.Status, domain.FleetNetworkDeprovisioned)
	}
}

// ── revive after teardown (O6, D-179 §807) ───────────────────────────

// The keystone O6 path: Ensure → Deprovision → Ensure must reuse the SAME row,
// come back Active, and come back with an EMPTY chain (default-deny I23) — never
// re-animating the pre-teardown policy rows.
func TestEnsureNetwork_RevivesATombstonedScopeOnTheSameHandleWithAnEmptyChain(t *testing.T) {
	h := newFabricHarness()
	ctx := context.Background()
	handle := h.ensure(t, "s", "prod", testOrg)
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod", "10.201.0.10")

	// Give the live scope real reach so we can prove the revive does NOT restore it.
	if _, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	}); err != nil {
		t.Fatalf("ApplyPolicies: %v", err)
	}
	if len(h.enforcer.synced[handle]) != 1 {
		t.Fatalf("pre-teardown chain = %+v, want one rule", h.enforcer.synced[handle])
	}

	if err := h.fabric.Deprovision(ctx, handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	revived, err := h.fabric.EnsureNetwork(ctx, EnsureNetworkInput{SystemID: "s", Env: "prod", OwnerOrg: testOrg})
	if err != nil {
		t.Fatalf("EnsureNetwork after Deprovision = %v, want revive", err)
	}
	if revived.Handle != handle {
		t.Fatalf("revive minted a new handle %s, want the reused row %s", revived.Handle, handle)
	}
	// One row only — uq_fleet_networks_system_env was never at risk.
	if got := len(h.networks.store); got != 1 {
		t.Fatalf("%d network rows after revive, want 1 (the row is reused)", got)
	}
	n, err := h.networks.FindByID(ctx, handle)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if n.Status != domain.FleetNetworkActive {
		t.Fatalf("status = %q, want %q", n.Status, domain.FleetNetworkActive)
	}
	// Empty chain: the pre-teardown policy rows are gone and zero rules are installed.
	if ps := h.policies.store[handle]; len(ps) != 0 {
		t.Fatalf("revived scope has %d policy rows, want 0 — pre-teardown reach was re-animated", len(ps))
	}
	if rules := h.enforcer.synced[handle]; len(rules) != 0 {
		t.Fatalf("revived chain installed %d rules, want 0 (default-deny): %+v", len(rules), rules)
	}
}

// A foreign org must not be able to adopt a tombstoned scope via the revive path —
// the org-anchor guard runs BEFORE the revive.
func TestEnsureNetwork_RevivePathStillRefusesForeignOrgAdoption(t *testing.T) {
	h := newFabricHarness()
	ctx := context.Background()
	handle := h.ensure(t, "s", "prod", testOrg)
	if err := h.fabric.Deprovision(ctx, handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	_, err := h.fabric.EnsureNetwork(ctx, EnsureNetworkInput{
		SystemID: "s", Env: "prod", OwnerOrg: "22222222-2222-2222-2222-222222222222",
	})
	if !errors.Is(err, domain.ErrNetworkOwnerOrgRequired) {
		t.Fatalf("EnsureNetwork = %v, want ErrNetworkOwnerOrgRequired — a foreign org revived another org's scope", err)
	}
	n, ferr := h.networks.FindByID(ctx, handle)
	if ferr != nil {
		t.Fatalf("FindByID: %v", ferr)
	}
	if n.Status != domain.FleetNetworkDeprovisioned {
		t.Fatalf("status = %q, want the scope to stay tombstoned after a refused adoption", n.Status)
	}
}

// The revived scope behaves like a fresh scope: a subsequent ApplyPolicies works.
func TestEnsureNetwork_RevivedScopeAcceptsNewPolicies(t *testing.T) {
	h := newFabricHarness()
	ctx := context.Background()
	handle := h.ensure(t, "s", "prod", testOrg)
	if err := h.fabric.Deprovision(ctx, handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if _, err := h.fabric.EnsureNetwork(ctx, EnsureNetworkInput{SystemID: "s", Env: "prod", OwnerOrg: testOrg}); err != nil {
		t.Fatalf("revive: %v", err)
	}
	h.addApp("api", "s", "prod", "10.201.0.6")
	h.addApp("db", "s", "prod", "10.201.0.10")
	out, err := h.fabric.ApplyPolicies(ctx, ApplyPoliciesInput{
		Handle:   handle,
		Policies: []PolicySpecInput{{FromComponentID: "api", ToComponentID: "db", Protocol: "tcp", Port: 5432}},
	})
	if err != nil {
		t.Fatalf("ApplyPolicies on revived scope = %v, want nil", err)
	}
	if out.Aggregate != EnforcementEnforced || len(h.enforcer.synced[handle]) != 1 {
		t.Fatalf("revived scope did not accept a new policy: aggregate=%q rules=%+v", out.Aggregate, h.enforcer.synced[handle])
	}
}

// ── RequireNetwork (the provision gate) ──────────────────────────────

func TestRequireNetwork(t *testing.T) {
	tests := []struct {
		name     string
		systemID string
		setup    func(h *fabricHarness)
		wantErr  error
	}{
		{
			name:     "unscoped app needs nothing",
			systemID: "",
			wantErr:  nil,
		},
		{
			name:     "scoped app with an active network",
			systemID: "s",
			setup:    func(h *fabricHarness) { h.ensureQuiet("s", "prod", testOrg) },
			wantErr:  nil,
		},
		{
			name:     "scoped app with no network is refused, never auto-created",
			systemID: "ghost",
			wantErr:  domain.ErrFleetNetworkNotFound,
		},
		{
			name:     "scoped app on a host that cannot enforce is refused",
			systemID: "s",
			setup: func(h *fabricHarness) {
				h.ensureQuiet("s", "prod", testOrg)
				h.enforcer.posture = domain.ErrNetworkEnforcerUnavailable
			},
			wantErr: domain.ErrNetworkEnforcerUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFabricHarness()
			if tt.setup != nil {
				tt.setup(h)
			}
			err := h.fabric.RequireNetwork(context.Background(), tt.systemID, "prod")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("RequireNetwork = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequireNetwork_RefusesADeprovisionedNetwork(t *testing.T) {
	h := newFabricHarness()
	handle := h.ensure(t, "s", "prod", testOrg)
	if err := h.fabric.Deprovision(context.Background(), handle); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if err := h.fabric.RequireNetwork(context.Background(), "s", "prod"); !errors.Is(err, domain.ErrFleetNetworkNotFound) {
		t.Fatalf("RequireNetwork = %v, want ErrFleetNetworkNotFound", err)
	}
}

// ── FailLoudNetworkEnforcer ──────────────────────────────────────────

func TestFailLoudNetworkEnforcer_RefusesEverything(t *testing.T) {
	var e NetworkEnforcer = FailLoudNetworkEnforcer{}
	ctx := context.Background()
	if err := e.InstallSkeleton(ctx); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("InstallSkeleton = %v", err)
	}
	if err := e.AssertPosture(ctx); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("AssertPosture = %v", err)
	}
	if err := e.SyncSystem(ctx, uuid.New(), nil); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("SyncSystem = %v", err)
	}
	if err := e.DropSystem(ctx, uuid.New()); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("DropSystem = %v", err)
	}
}

func TestFailLoudNetworkEnforcer_BlocksEveryFabricCall(t *testing.T) {
	h := &fabricHarness{
		networks: newFakeNetworkRepo(),
		policies: newFakePolicyRepo(),
		apps:     &fakeNetAppRepo{},
		replicas: &fakeNetReplicaRepo{byApp: map[uuid.UUID][]domain.Replica{}},
	}
	fabric := NewFleetNetworkFabric(h.networks, h.policies, h.apps, h.replicas, FailLoudNetworkEnforcer{})
	ctx := context.Background()

	if _, err := fabric.EnsureNetwork(ctx, EnsureNetworkInput{SystemID: "s", Env: "prod", OwnerOrg: testOrg}); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("EnsureNetwork = %v", err)
	}
	if err := fabric.RequireNetwork(ctx, "s", "prod"); !errors.Is(err, domain.ErrNetworkEnforcerUnavailable) {
		t.Fatalf("RequireNetwork = %v", err)
	}
	// Back-compat proof: an unscoped workload is unaffected on the same host.
	if err := fabric.RequireNetwork(ctx, "", "prod"); err != nil {
		t.Fatalf("RequireNetwork for an unscoped app = %v, want nil (pre-#5 behavior preserved)", err)
	}
}

// ── egress_allow (the §7-F seam guard) ───────────────────────────────

func TestValidateEgressAllow(t *testing.T) {
	tests := []struct {
		name    string
		allow   []string
		wantErr error
	}{
		{"external ip accepted", []string{"10.0.10.20"}, nil},
		{"external cidr accepted", []string{"192.168.1.0/24"}, nil},
		{"hostname accepted", []string{"api.stripe.com"}, nil},
		{"empty list accepted", nil, nil},
		{"blank entry ignored", []string{"  "}, nil},
		{"the fleet subnet itself", []string{"10.201.0.0/16"}, domain.ErrNetworkPolicyEgressOverlap},
		{"a fleet guest ip", []string{"10.201.0.10"}, domain.ErrNetworkPolicyEgressOverlap},
		{"a fleet sub-cidr", []string{"10.201.5.0/24"}, domain.ErrNetworkPolicyEgressOverlap},
		// A supernet is ALLOWED: it is a legitimate external destination, and the
		// chain topology (SNT-XVM terminal, evaluated before SNT-EGRESS) is what
		// stops it buying inter-VM reach — not this validator.
		{"a supernet of the fleet is allowed", []string{"10.0.0.0/8"}, nil},
		{"the default route is allowed", []string{"0.0.0.0/0"}, nil},
		{"mixed, one bad", []string{"10.0.10.20", "10.201.0.6"}, domain.ErrNetworkPolicyEgressOverlap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEgressAllow(tt.allow)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateEgressAllow(%v) = %v, want %v", tt.allow, err, tt.wantErr)
			}
		})
	}
}
