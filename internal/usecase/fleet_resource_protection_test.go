package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// D-202 — protection attaches or the provision fails.
//
// The three faces of the ruling are asserted here: the REFUSAL (nothing is
// created), the ATTACHMENT (the claim's own INSERT carries it), and the WAIVER
// (audited, permanent, and never able to forgive placement). Plus the two host
// rules that keep one host's liveness from greening another host's data.

// protectionUC builds a provisioner with the protection seams under the test's
// control. Deliberately NOT newTestResourceProvisioner: that helper holds the
// gate open, which is the one thing these tests must be able to close.
func protectionUC(t *testing.T, repo *fakeResourceRepo, prov FleetProvisioner, hosts HAPlacementHosts, affinity ProtectionAffinityReader) *FleetResourceProvisioner {
	t.Helper()
	return protectionUCWithReplicas(t, repo, prov, hosts, affinity, newFakeResourceReplicaRepo())
}

func protectionUCWithReplicas(t *testing.T, repo *fakeResourceRepo, prov FleetProvisioner, hosts HAPlacementHosts, affinity ProtectionAffinityReader, replicas *fakeResourceReplicaRepo) *FleetResourceProvisioner {
	t.Helper()
	if affinity == nil {
		affinity = &fakeVolumeAffinity{}
	}
	return NewFleetResourceProvisioner(
		prov, repo, replicas, &fakeSnapshotter{}, &fakeVolumeBinder{}, testEngine(), testEndpointNaming(),
		hosts, 90*time.Second, repo, affinity, testProtectionConfig(),
	)
}

func hostsWithIDs(ids ...uuid.UUID) *fakeLiveHosts {
	hosts := make([]domain.Host, 0, len(ids))
	for _, id := range ids {
		h := liveHost("eu-central", "site-a/breaker-a/switch-1")
		h.ID = id
		hosts = append(hosts, h)
	}
	return &fakeLiveHosts{hosts: hosts}
}

// The accept rule: EVERY eligible host must be beating. A resource's placement is
// not chosen at accept, so a fleet that can only protect some of the hosts the
// scheduler might pick cannot promise to protect this database at all.
func TestEvaluateProtection_AcceptRequiresEveryEligibleHost(t *testing.T) {
	hostA, hostB := uuid.New(), uuid.New()
	fresh := time.Now().UTC()
	stale := fresh.Add(-30 * time.Minute)

	tests := []struct {
		name string
		// seed decides which beats exist and how old they are.
		seed        func(*fakeResourceRepo)
		hosts       HAPlacementHosts
		wantCadence error
		wantOffsite error
	}{
		{
			name: "every host beats and the platform beats — everything attaches",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentCadence, hostA.String(), fresh)
				r.seedBeat(domain.ProtectionComponentCadence, hostB.String(), fresh)
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts: hostsWithIDs(hostA, hostB),
		},
		{
			name: "ONE host with no beat refuses the whole cadence component",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentCadence, hostA.String(), fresh)
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts:       hostsWithIDs(hostA, hostB),
			wantCadence: domain.ErrProtectionCadenceUnavailable,
		},
		{
			name: "a STALE beat is not a beat",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentCadence, hostA.String(), stale)
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts:       hostsWithIDs(hostA),
			wantCadence: domain.ErrProtectionCadenceUnavailable,
		},
		{
			name: "an EMPTY live-host set refuses — nothing could take the snapshots",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts:       &fakeLiveHosts{},
			wantCadence: domain.ErrProtectionCadenceUnavailable,
		},
		{
			name: "an UNREADABLE inventory refuses — an unknowable host set is not a protected one",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts:       &fakeLiveHosts{err: errors.New("control-plane db unreachable")},
			wantCadence: domain.ErrProtectionCadenceUnavailable,
		},
		{
			name: "no inventory wired at all refuses",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, fresh)
			},
			hosts:       nil,
			wantCadence: domain.ErrProtectionCadenceUnavailable,
		},
		{
			name: "the platform offsite row is the ONLY proof of the durability store — absent means refused",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentCadence, hostA.String(), fresh)
			},
			hosts:       hostsWithIDs(hostA),
			wantOffsite: domain.ErrProtectionOffsiteUnproven,
		},
		{
			name: "a STALE offsite row is not proof either",
			seed: func(r *fakeResourceRepo) {
				r.seedBeat(domain.ProtectionComponentCadence, hostA.String(), fresh)
				r.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, stale)
			},
			hosts:       hostsWithIDs(hostA),
			wantOffsite: domain.ErrProtectionOffsiteUnproven,
		},
		{
			name:        "an unreadable fact ledger fails BOTH components — an unreadable fact is not a held fact",
			seed:        func(r *fakeResourceRepo) { r.beatErr = errors.New("control-plane db unreachable") },
			hosts:       hostsWithIDs(hostA),
			wantCadence: domain.ErrProtectionCadenceUnavailable,
			wantOffsite: domain.ErrProtectionOffsiteUnproven,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			tt.seed(repo)
			uc := protectionUC(t, repo, &fakeFleetProvisioner{}, tt.hosts, nil)

			eval := uc.evaluateProtection(context.Background(), uc.acceptScopes(context.Background()), uc.protection.CadenceSeconds)

			assertComponent(t, "cadence", eval.Cadence, tt.wantCadence)
			assertComponent(t, "offsite", eval.Offsite, tt.wantOffsite)

			err := eval.Err()
			if tt.wantCadence == nil && tt.wantOffsite == nil {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrProtectionUnattachable) {
				t.Fatalf("Err() = %v, want it to wrap ErrProtectionUnattachable", err)
			}
			// The refusal must name EVERY failed component in ONE answer: a caller
			// told about one, fixing it, and being refused again learns the gate one
			// component at a time.
			for _, want := range []error{tt.wantCadence, tt.wantOffsite} {
				if want != nil && !errors.Is(err, want) {
					t.Fatalf("Err() = %v, want it to name %v", err, want)
				}
			}
		})
	}
}

func assertComponent(t *testing.T, name string, got ProtectionComponentResult, want error) {
	t.Helper()
	if want == nil {
		if !got.Attached {
			t.Fatalf("%s must attach, got %v", name, got.Err)
		}
		if got.Err != nil {
			t.Fatalf("%s attached but carries err %v", name, got.Err)
		}
		return
	}
	if got.Attached {
		t.Fatalf("%s must NOT attach", name)
	}
	if !errors.Is(got.Err, want) {
		t.Fatalf("%s err = %v, want %v", name, got.Err, want)
	}
}

// The status rule: ONLY the host this resource's own claim-owned volumes are
// pinned to. A resource whose bytes are elsewhere is not protected by a worker
// here, and an unresolvable affinity reports the pessimistic answer rather than
// this process's own identity.
func TestEvaluateProtection_StatusEvaluatesOnlyTheAffinityHost(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	res := &domain.FleetResource{ID: uuid.New()}
	vol := func(host *uuid.UUID) domain.Volume {
		return domain.Volume{ID: uuid.New(), ResourceID: &res.ID, HostAffinity: host}
	}

	tests := []struct {
		name         string
		volumes      []domain.Volume
		volumesErr   error
		beatHosts    []uuid.UUID
		wantAttached bool
	}{
		{
			name:         "the affinity host beats — cadence attaches",
			volumes:      []domain.Volume{vol(&own)},
			beatHosts:    []uuid.UUID{own},
			wantAttached: true,
		},
		{
			name:      "ANOTHER host's beat does not protect these bytes",
			volumes:   []domain.Volume{vol(&own)},
			beatHosts: []uuid.UUID{other},
		},
		{
			name:      "volumes pinned to two different hosts are unprovable",
			volumes:   []domain.Volume{vol(&own), vol(&other)},
			beatHosts: []uuid.UUID{own, other},
		},
		{
			name:      "an UNPINNED volume is unprovable — nothing says where the bytes are",
			volumes:   []domain.Volume{vol(nil)},
			beatHosts: []uuid.UUID{own},
		},
		{
			name:      "a resource that owns no volume has nothing to snapshot",
			volumes:   nil,
			beatHosts: []uuid.UUID{own},
		},
		{
			name:       "an unreadable volume ledger is unprovable",
			volumesErr: errors.New("control-plane db unreachable"),
			beatHosts:  []uuid.UUID{own},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			for _, h := range tt.beatHosts {
				repo.seedBeat(domain.ProtectionComponentCadence, h.String(), time.Now().UTC())
			}
			affinity := &fakeVolumeAffinity{
				byResource: map[uuid.UUID][]domain.Volume{res.ID: tt.volumes},
				err:        tt.volumesErr,
			}
			uc := protectionUC(t, repo, &fakeFleetProvisioner{}, hostsWithIDs(own, other), affinity)

			eval := uc.evaluateProtection(context.Background(), uc.statusScopes(context.Background(), res), uc.protection.CadenceSeconds)
			if eval.Cadence.Attached != tt.wantAttached {
				t.Fatalf("cadence attached = %v, want %v (err %v)", eval.Cadence.Attached, tt.wantAttached, eval.Cadence.Err)
			}
			if !tt.wantAttached && !errors.Is(eval.Cadence.Err, domain.ErrProtectionCadenceUnavailable) {
				t.Fatalf("cadence err = %v, want ErrProtectionCadenceUnavailable", eval.Cadence.Err)
			}
		})
	}
}

// The refusing direction, and the whole point of evaluating BEFORE anything is
// materialized: a refused durable claim leaves no VM and no row.
func TestProvisionDedicated_RefusesWhenProtectionUnattachable(t *testing.T) {
	host := uuid.New()
	repo := newFakeResourceRepo()
	// Cadence attaches; the off-provider durability store is unproven — the live
	// state of the fleet the day D-202 lands.
	repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := protectionUC(t, repo, prov, hostsWithIDs(host), nil)

	_, err := uc.ProvisionDedicated(context.Background(), validDedicatedInput())
	if !errors.Is(err, domain.ErrProtectionUnattachable) {
		t.Fatalf("err = %v, want ErrProtectionUnattachable", err)
	}
	if !errors.Is(err, domain.ErrProtectionOffsiteUnproven) {
		t.Fatalf("the refusal must NAME the failing component: %v", err)
	}
	if errors.Is(err, domain.ErrProtectionCadenceUnavailable) {
		t.Fatalf("the refusal must not name a component that DID attach: %v", err)
	}
	if prov.provisionCalls != 0 {
		t.Fatalf("a refused durable claim must boot nothing (provision calls = %d)", prov.provisionCalls)
	}
	if len(repo.byID) != 0 {
		t.Fatalf("a refused durable claim must persist nothing (%d rows)", len(repo.byID))
	}
}

// The attaching direction: the enrolment and the attach stamp are written in the
// SAME INSERT that creates the claim.
func TestProvisionDedicated_AttachesProtectionOnAccept(t *testing.T) {
	host := uuid.New()
	repo := newFakeResourceRepo()
	now := time.Now().UTC()
	repo.seedBeat(domain.ProtectionComponentCadence, host.String(), now)
	repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, now)
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := protectionUC(t, repo, prov, hostsWithIDs(host), nil)

	out, err := uc.ProvisionDedicated(context.Background(), validDedicatedInput())
	if err != nil {
		t.Fatalf("ProvisionDedicated: %v", err)
	}
	row := repo.byID[uuid.MustParse(out.Handle)]
	if row == nil {
		t.Fatal("the accepted claim was not persisted")
	}
	if row.Durability != domain.DurabilityDurable {
		t.Fatalf("durability = %q, want durable", row.Durability)
	}
	if row.ProtectionCadenceSeconds == nil || *row.ProtectionCadenceSeconds != testProtectionConfig().CadenceSeconds {
		t.Fatalf("cadence enrolment = %v, want %d", row.ProtectionCadenceSeconds, testProtectionConfig().CadenceSeconds)
	}
	if row.ProtectionAttachedAt == nil {
		t.Fatal("protection_attached_at must be stamped when the FULL component set attached")
	}
	if row.ProtectionWaivedBy != "" || row.ProtectionWaiverReason != "" || row.ProtectionWaivedAt != nil {
		t.Fatalf("an unwaived accept must carry no waiver audit: %+v", row)
	}
}

// The waiver: it forgives the REQUIREMENT, never the protection. What can attach
// still attaches, the audit is complete, and attached-at stays nil because the
// full set did not attach.
func TestProvisionDedicated_WaiverProceedsAndRecords(t *testing.T) {
	host := uuid.New()
	repo := newFakeResourceRepo()
	repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := protectionUC(t, repo, prov, hostsWithIDs(host), nil)

	in := validDedicatedInput()
	in.Waiver = &ProtectionWaiver{Actor: "user:ops-1", Reason: "D-205 drill run 42"}
	out, err := uc.ProvisionDedicated(context.Background(), in)
	if err != nil {
		t.Fatalf("a waived provision must proceed: %v", err)
	}
	row := repo.byID[uuid.MustParse(out.Handle)]
	if row.ProtectionWaivedBy != "user:ops-1" || row.ProtectionWaiverReason != "D-205 drill run 42" || row.ProtectionWaivedAt == nil {
		t.Fatalf("the waiver audit must be complete: by=%q reason=%q at=%v", row.ProtectionWaivedBy, row.ProtectionWaiverReason, row.ProtectionWaivedAt)
	}
	if row.ProtectionAttachedAt != nil {
		t.Fatal("attached-at means the FULL component set attached — a waived row must not claim it")
	}
	if row.ProtectionCadenceSeconds == nil {
		t.Fatal("a waiver forgives the requirement, not the protection: cadence WAS attachable and must be attached")
	}
}

// A half waiver is not an audit record, and the 0025 CHECK refuses to hold one.
func TestProvisionDedicated_WaiverIncompleteRefused(t *testing.T) {
	tests := []struct {
		name   string
		waiver *ProtectionWaiver
	}{
		{"an actor with no reason is not attributable to a decision", &ProtectionWaiver{Actor: "user:ops-1"}},
		{"a reason attributable to nobody is not an audit record", &ProtectionWaiver{Reason: "because"}},
		{"neither half", &ProtectionWaiver{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			repo.beatsDefaultFresh = true
			prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
			uc := protectionUC(t, repo, prov, hostsWithIDs(uuid.New()), nil)

			in := validDedicatedInput()
			in.Waiver = tt.waiver
			if _, err := uc.ProvisionDedicated(context.Background(), in); !errors.Is(err, domain.ErrProtectionWaiverIncomplete) {
				t.Fatalf("err = %v, want ErrProtectionWaiverIncomplete", err)
			}
			if prov.provisionCalls != 0 || len(repo.byID) != 0 {
				t.Fatalf("an incomplete waiver must create nothing (provisions=%d rows=%d)", prov.provisionCalls, len(repo.byID))
			}
		})
	}
}

// A waiver can never make an HA claim placeable: a waived placement would sell a
// physically impossible promise (I39/I40).
func TestProvisionDedicated_WaiverNeverForgivesPlacement(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.beatsDefaultFresh = true
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := protectionUC(t, repo, prov, hostsWithIDs(uuid.New()), nil)

	in := validDedicatedInput()
	in.AvailabilityClass = "ha"
	in.Waiver = &ProtectionWaiver{Actor: "user:ops-1", Reason: "please"}
	_, err := uc.ProvisionDedicated(context.Background(), in)
	if !errors.Is(err, domain.ErrHAHostsInsufficient) {
		t.Fatalf("err = %v, want the placement refusal — placement is never waivable", err)
	}
	if prov.provisionCalls != 0 || len(repo.byID) != 0 {
		t.Fatalf("a refused ha claim must create nothing (provisions=%d rows=%d)", prov.provisionCalls, len(repo.byID))
	}
}

// The dedicated tier IS durable. Absence means durable (the same promise, not a
// weaker one); "ephemeral" names a combination the ledger cannot represent.
func TestProvisionDedicated_DurabilityIsStoredAndEnforced(t *testing.T) {
	tests := []struct {
		name       string
		durability string
		wantErr    error
	}{
		{"an absent claim means durable — the tier is durable", "", nil},
		{"an explicit durable claim is accepted", "durable", nil},
		{"an ephemeral dedicated claim is refused, never coerced", "ephemeral", domain.ErrResourceDurabilityInvalid},
		{"garbage is refused", "sort-of-durable", domain.ErrResourceDurabilityInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			repo.beatsDefaultFresh = true
			prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
			uc := protectionUC(t, repo, prov, hostsWithIDs(uuid.New()), nil)

			in := validDedicatedInput()
			in.Durability = tt.durability
			out, err := uc.ProvisionDedicated(context.Background(), in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if prov.provisionCalls != 0 || len(repo.byID) != 0 {
					t.Fatalf("a refused claim must create nothing (provisions=%d rows=%d)", prov.provisionCalls, len(repo.byID))
				}
				return
			}
			if err != nil {
				t.Fatalf("ProvisionDedicated: %v", err)
			}
			if row := repo.byID[uuid.MustParse(out.Handle)]; row.Durability != domain.DurabilityDurable {
				t.Fatalf("stored durability = %q, want durable", row.Durability)
			}
		})
	}
}

// Attach-on-recover: a claim accepted BEFORE the gate existed carries no
// enrolment, and the declarative re-provision is where the fleet brings it under
// protection. It is best-effort in BOTH directions — it never refuses the
// recovery, and it stamps only what it could actually prove.
func TestProvisionDedicated_AttachesProtectionOnRecover(t *testing.T) {
	tests := []struct {
		name string
		// seed decides which beats exist, on the resource's own affinity host.
		seed func(*fakeResourceRepo, uuid.UUID)
		// mutate shapes the pre-existing row.
		mutate     func(*domain.FleetResource)
		saveErr    error
		wantAttach bool
	}{
		{
			name: "a pre-D-202 durable row whose protection CAN attach is enrolled and stamped",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			wantAttach: true,
		},
		{
			name: "protection that cannot attach leaves the row alone and NEVER refuses the recovery",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				// Cadence beats; the off-provider store does not — the live shape of
				// this fleet, and the shape that must not take a database down.
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
			},
		},
		{
			name: "an unreadable fact ledger is not a held fact — and still never refuses",
			seed: func(repo *fakeResourceRepo, _ uuid.UUID) {
				repo.beatErr = errors.New("control-plane db unreachable")
			},
		},
		{
			name: "an ALREADY-attached row is left exactly as it is",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			mutate: func(r *domain.FleetResource) {
				at := time.Now().UTC().Add(-24 * time.Hour)
				r.ProtectionAttachedAt = &at
			},
		},
		{
			name: "a WAIVED row is never silently promoted to attached — the audit is the record",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			mutate: func(r *domain.FleetResource) {
				at := time.Now().UTC()
				r.ProtectionWaivedBy = "user:ops-1"
				r.ProtectionWaiverReason = "drill"
				r.ProtectionWaivedAt = &at
			},
		},
		{
			name: "an EPHEMERAL row has no protection to attach",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			mutate: func(r *domain.FleetResource) { r.Durability = domain.DurabilityEphemeral },
		},
		{
			name: "a failed persist is logged and swallowed — the recovery still happens",
			seed: func(repo *fakeResourceRepo, host uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			saveErr: errors.New("control-plane db unreachable"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := uuid.New()
			repo := newFakeResourceRepo()
			tt.seed(repo, host)

			in := validDedicatedInput()
			appID := uuid.New()
			res := &domain.FleetResource{
				ID: uuid.New(), OwnerOrg: uuid.MustParse(in.OwnerOrg), ClaimKey: in.ClaimKey, Env: in.Env,
				Revision: 1, Class: resourceClassPostgres, Tier: resourceTierDedicated,
				Phase: domain.FleetResourcePhaseReady, AppID: &appID,
				// The pre-D-202 shape: durable, never enrolled, never waived.
				Durability: domain.DurabilityDurable,
			}
			if tt.mutate != nil {
				tt.mutate(res)
			}
			repo.seed(res)
			repo.saveErr = tt.saveErr

			affinity := &fakeVolumeAffinity{byResource: map[uuid.UUID][]domain.Volume{
				res.ID: {{ID: uuid.New(), ResourceID: &res.ID, HostAffinity: &host}},
			}}
			prov := &fakeFleetProvisioner{
				healthOut:    FleetHealthOutput{Healthy: true},
				provisionOut: FleetProvisionOutput{Handle: appID.String()},
			}
			uc := protectionUC(t, repo, prov, hostsWithIDs(host), affinity)

			// ⚠ The regression guard: whatever the protection verdict is, a
			// re-provision of an EXISTING claim never errors and always drives the
			// recovery. A regressed platform must keep serving what it promised.
			out, err := uc.ProvisionDedicated(context.Background(), in)
			if err != nil {
				t.Fatalf("a re-provision of an existing claim must never be refused by the attach attempt: %v", err)
			}
			if out.Handle != res.ID.String() {
				t.Fatalf("handle = %q, want the existing claim %q", out.Handle, res.ID)
			}
			if prov.provisionCalls != 1 {
				t.Fatalf("the recovery re-provision must still run (provision calls = %d)", prov.provisionCalls)
			}

			stored, gerr := repo.GetResourceByHandle(context.Background(), res.ID)
			if gerr != nil {
				t.Fatalf("GetResourceByHandle: %v", gerr)
			}
			if !tt.wantAttach {
				if stored.ProtectionCadenceSeconds != nil {
					t.Fatalf("nothing should have been enrolled, got cadence %v", *stored.ProtectionCadenceSeconds)
				}
				if tt.mutate == nil && stored.ProtectionAttachedAt != nil {
					t.Fatal("an unprovable attach must never be stamped")
				}
				return
			}
			if stored.ProtectionCadenceSeconds == nil || *stored.ProtectionCadenceSeconds != testProtectionConfig().CadenceSeconds {
				t.Fatalf("cadence enrolment = %v, want %d", stored.ProtectionCadenceSeconds, testProtectionConfig().CadenceSeconds)
			}
			if stored.ProtectionAttachedAt == nil {
				t.Fatal("a fully-attached recover must stamp protection_attached_at")
			}
			if stored.ProtectionWaivedBy != "" {
				t.Fatalf("an attach is not a waiver: %+v", stored)
			}
		})
	}
}

// The status read judges the enrolment ON THE ROW, never the fleet's current
// configured default. A cleared or changed config is a fact about the config, and
// re-reading it here would silently re-classify enrolments nobody touched.
func TestStatusOf_ProtectionEvaluatesThePersistedEnrolment(t *testing.T) {
	host := uuid.New()
	repo := newFakeResourceRepo()
	repo.seedBeat(domain.ProtectionComponentCadence, host.String(), time.Now().UTC())
	repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())

	appID := uuid.New()
	persisted := 3600
	res := &domain.FleetResource{
		ID: uuid.New(), OwnerOrg: uuid.New(), ClaimKey: "orders-db", Env: "prod",
		Class: resourceClassPostgres, Tier: resourceTierDedicated,
		Phase: domain.FleetResourcePhaseReady, AppID: &appID,
		Durability: domain.DurabilityDurable, ProtectionCadenceSeconds: &persisted,
	}
	repo.seed(res)

	affinity := &fakeVolumeAffinity{byResource: map[uuid.UUID][]domain.Volume{
		res.ID: {{ID: uuid.New(), ResourceID: &res.ID, HostAffinity: &host}},
	}}
	prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
	replicas := newFakeResourceReplicaRepo()
	replicas.byApp[appID] = []domain.Replica{{
		ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
		GuestIP: "10.10.0.2", Port: 5432,
	}}
	uc := protectionUCWithReplicas(t, repo, prov, hostsWithIDs(host), affinity, replicas)
	uc.pgReady = func(context.Context, string, int) error { return nil }
	// The ACCEPT-time cadence is zeroed: no NEW durable claim could be enrolled on
	// this fleet right now. That must say nothing about a resource already enrolled.
	uc.protection.CadenceSeconds = 0

	status, err := uc.StatusOf(context.Background(), res.ID)
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	for _, token := range []string{conditionProtectionCadenceUnattached, conditionProtectionCadenceStalled} {
		if hasCondition(status.Conditions, token) {
			t.Fatalf("conditions %v must not contain %q: the row IS enrolled and its host IS beating", status.Conditions, token)
		}
	}
}

// Regression ALARMS; it never blocks. Conditions are added, the phase is not
// touched — reported or persisted — and a resource that is serving keeps serving.
func TestStatusOf_ProtectionConditions(t *testing.T) {
	host := uuid.New()
	cadence := 3600

	tests := []struct {
		name string
		// mutate shapes the persisted row.
		mutate func(*domain.FleetResource)
		// seed decides which beats exist.
		seed           func(*fakeResourceRepo, uuid.UUID)
		affinityHost   *uuid.UUID
		wantConditions []string
	}{
		{
			name:   "fully attached on a beating host reports nothing",
			mutate: func(r *domain.FleetResource) { r.ProtectionCadenceSeconds = &cadence },
			seed: func(repo *fakeResourceRepo, h uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, h.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			affinityHost: &host,
		},
		{
			name:   "a pre-D-202 row was never enrolled",
			mutate: func(r *domain.FleetResource) { r.ProtectionCadenceSeconds = nil },
			seed: func(repo *fakeResourceRepo, h uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, h.String(), time.Now().UTC())
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			affinityHost:   &host,
			wantConditions: []string{conditionProtectionCadenceUnattached},
		},
		{
			name:   "an ENROLLED row whose host stopped beating is STALLED, not unattached",
			mutate: func(r *domain.FleetResource) { r.ProtectionCadenceSeconds = &cadence },
			seed: func(repo *fakeResourceRepo, _ uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentOffsite, domain.ProtectionScopePlatform, time.Now().UTC())
			},
			affinityHost:   &host,
			wantConditions: []string{conditionProtectionCadenceStalled},
		},
		{
			name:   "no offsite proof is a platform-wide condition",
			mutate: func(r *domain.FleetResource) { r.ProtectionCadenceSeconds = &cadence },
			seed: func(repo *fakeResourceRepo, h uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, h.String(), time.Now().UTC())
			},
			affinityHost:   &host,
			wantConditions: []string{conditionProtectionOffsiteUnavailable},
		},
		{
			name: "a waived row reports the waiver PERMANENTLY, alongside what is still missing",
			mutate: func(r *domain.FleetResource) {
				at := time.Now().UTC()
				r.ProtectionCadenceSeconds = &cadence
				r.ProtectionWaivedBy = "user:ops-1"
				r.ProtectionWaiverReason = "drill"
				r.ProtectionWaivedAt = &at
			},
			seed: func(repo *fakeResourceRepo, h uuid.UUID) {
				repo.seedBeat(domain.ProtectionComponentCadence, h.String(), time.Now().UTC())
			},
			affinityHost:   &host,
			wantConditions: []string{conditionProtectionWaived, conditionProtectionOffsiteUnavailable},
		},
		{
			name: "a tombstone reports none of it — a torn-down resource's protection is history",
			mutate: func(r *domain.FleetResource) {
				r.Phase = domain.FleetResourcePhaseDecommissioned
				r.ProtectionCadenceSeconds = nil
			},
			seed:         func(*fakeResourceRepo, uuid.UUID) {},
			affinityHost: &host,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			tt.seed(repo, host)
			appID := uuid.New()
			res := &domain.FleetResource{
				ID: uuid.New(), OwnerOrg: uuid.New(), ClaimKey: "orders-db", Env: "prod",
				Class: resourceClassPostgres, Tier: resourceTierDedicated,
				Phase: domain.FleetResourcePhaseReady, AppID: &appID,
				Durability: domain.DurabilityDurable,
			}
			tt.mutate(res)
			repo.seed(res)

			affinity := &fakeVolumeAffinity{byResource: map[uuid.UUID][]domain.Volume{
				res.ID: {{ID: uuid.New(), ResourceID: &res.ID, HostAffinity: tt.affinityHost}},
			}}
			// The app is healthy and its engine admits: the resource is SERVING, which
			// is precisely the state a protection regression must not disturb.
			prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
			replicas := newFakeResourceReplicaRepo()
			replicas.byApp[appID] = []domain.Replica{{
				ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
				GuestIP: "10.10.0.2", Port: 5432,
			}}
			uc := protectionUCWithReplicas(t, repo, prov, hostsWithIDs(host), affinity, replicas)
			uc.pgReady = func(context.Context, string, int) error { return nil }

			status, err := uc.StatusOf(context.Background(), res.ID)
			if err != nil {
				t.Fatalf("StatusOf: %v", err)
			}
			for _, want := range tt.wantConditions {
				if !hasCondition(status.Conditions, want) {
					t.Fatalf("conditions %v must contain %q", status.Conditions, want)
				}
			}
			for _, token := range []string{
				conditionProtectionWaived, conditionProtectionCadenceUnattached,
				conditionProtectionCadenceStalled, conditionProtectionOffsiteUnavailable,
			} {
				if hasCondition(status.Conditions, token) && !hasCondition(tt.wantConditions, token) {
					t.Fatalf("conditions %v must NOT contain %q", status.Conditions, token)
				}
			}
			// ⚠ ALARM, NEVER BLOCK. The persisted phase is untouched by any protection
			// condition, and a resource that was serving is still reported as serving.
			stored, _ := repo.GetResourceByHandle(context.Background(), res.ID)
			if res.Phase != domain.FleetResourcePhaseDecommissioned {
				if stored.Phase != domain.FleetResourcePhaseReady {
					t.Fatalf("a protection condition must never rewrite the persisted phase (got %q)", stored.Phase)
				}
				if status.Phase != string(domain.FleetResourcePhaseReady) {
					t.Fatalf("a protection condition must never demote the reported phase (got %q)", status.Phase)
				}
			}
		})
	}
}
