package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// The app seam must not destroy a durable resource's data.
//
// DecommissionApp drains the replicas, UNLINKS the ext4 backing files and drops
// the app row, while fleet_resources.app_id carries no FK — so before this guard
// a customer's dedicated Postgres could be destroyed, with no recovery point,
// through a verb that never consulted the claim. These tests assert the refusal
// AND that nothing destructive ran: a guard that refuses after the file is gone
// is worthless.
// ─────────────────────────────────────────────────────────────────────

// unlinkingBackend really removes the backing file, so "the file survived" is a
// statement about the filesystem rather than about a mock's call log.
type unlinkingBackend struct{}

func (unlinkingBackend) Ensure(context.Context, VolumeEnsureInput) (VolumeEnsureOutput, error) {
	return VolumeEnsureOutput{}, nil
}
func (unlinkingBackend) Delete(_ context.Context, backingPath string) error {
	return os.Remove(backingPath)
}

// guardHarness is one provisioned, volume-bearing, single-replica app with a
// REAL backing file on disk — the shape of a dedicated data VM.
type guardHarness struct {
	orch     orchHarness
	appID    uuid.UUID
	volumes  *volRepoFake
	backing  string
	volumeID uuid.UUID
}

func newGuardHarness(t *testing.T) guardHarness {
	t.Helper()
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	t.Cleanup(func() { processAlive = origAlive })

	h := newOrchHarness(oneLiveHost())
	handle, _, err := h.orch.ProvisionApp(context.Background(), FleetProvisionInput{
		ComponentID: "resource/" + orgA + "/db", Env: "prod", OwnerOrg: orgA,
		Registry: "reg", Repository: "sentiae/pg", Digest: "sha256:abc",
		VCPU: 2, MemoryMB: 1024, Port: residentPGPort,
	})
	if err != nil {
		t.Fatalf("ProvisionApp: %v", err)
	}
	appID := uuid.MustParse(handle)

	// The volume manager is attached AFTER the provision so the app is created
	// without one — the volume row + backing file are seeded directly, which keeps
	// this test about the teardown guard, not about volume placement.
	backing := filepath.Join(t.TempDir(), "data.ext4")
	if werr := os.WriteFile(backing, []byte("customer data"), 0o600); werr != nil {
		t.Fatalf("seed backing file: %v", werr)
	}
	vol := volWithBacking(appID, backing)
	volumes := newVolRepoFake(vol)
	h.orch.SetVolumeManager(NewFleetVolumeManager(volumes, unlinkingBackend{}, filepath.Dir(backing)))

	return guardHarness{orch: h, appID: appID, volumes: volumes, backing: backing, volumeID: vol.ID}
}

func (g guardHarness) backingExists() bool {
	_, err := os.Stat(g.backing)
	return err == nil
}

func TestDecommissionApp_RefusesWhileALiveResourceClaimsTheApp(t *testing.T) {
	liveClaim := func(appID uuid.UUID) *domain.FleetResource {
		return &domain.FleetResource{
			ID: uuid.New(), OwnerOrg: uuid.MustParse(orgA), ClaimKey: "db", Env: "prod",
			Class: resourceClassPostgres, Tier: resourceTierDedicated,
			Phase: domain.FleetResourcePhaseReady, AppID: &appID,
		}
	}

	tests := []struct {
		name string
		// claim builds the ledger row for this app (nil → the app is claimed by
		// nothing, the ordinary component deploy).
		claim      func(appID uuid.UUID) *domain.FleetResource
		wantRefuse bool
	}{
		{
			name:       "live claim refuses and destroys nothing",
			claim:      liveClaim,
			wantRefuse: true,
		},
		{
			name: "tombstoned claim proceeds",
			claim: func(appID uuid.UUID) *domain.FleetResource {
				r := liveClaim(appID)
				r.Phase = domain.FleetResourcePhaseDecommissioned
				at := time.Now().UTC()
				r.DecommissionedAt = &at
				return r
			},
		},
		{
			name: "teardown already in flight (intent stamped) proceeds",
			claim: func(appID uuid.UUID) *domain.FleetResource {
				r := liveClaim(appID)
				at := time.Now().UTC()
				r.DecommissionedAt = &at // phase still ready — the resource seam is calling down
				return r
			},
		},
		{name: "no claim proceeds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuardHarness(t)
			if tt.claim != nil {
				g.orch.resources.seed(tt.claim(g.appID))
			}

			isApp, err := g.orch.orch.DecommissionApp(context.Background(), g.appID)

			if tt.wantRefuse {
				if !errors.Is(err, domain.ErrAppBacksDurableResource) {
					t.Fatalf("DecommissionApp err = %v, want ErrAppBacksDurableResource", err)
				}
				if !isApp {
					t.Fatalf("isApp = false, want true — the app IS known, it is just protected")
				}
				// Nothing destructive may have run. The backing file is the one that
				// matters: it is the customer's data.
				if !g.backingExists() {
					t.Fatal("the backing FILE was unlinked — the guard ran too late to be worth anything")
				}
				if g.volumes.count() != 1 {
					t.Fatalf("volume rows = %d, want 1 (none deleted)", g.volumes.count())
				}
				if !g.orch.apps.has(g.appID) {
					t.Fatal("app row deleted")
				}
				if got := g.orch.replicas.countState(domain.ReplicaStateResident); got != 1 {
					t.Fatalf("resident replicas = %d, want 1 (none drained)", got)
				}
				return
			}

			if err != nil || !isApp {
				t.Fatalf("DecommissionApp: isApp=%v err=%v, want true,nil", isApp, err)
			}
			// The unguarded path really is destructive — which is what makes the
			// assertions above meaningful.
			if g.backingExists() {
				t.Fatal("backing file survived a legitimate teardown")
			}
			if g.orch.apps.has(g.appID) {
				t.Fatal("app row survived a legitimate teardown")
			}
			if got := g.orch.replicas.count(); got != 0 {
				t.Fatalf("replicas after teardown = %d, want 0", got)
			}
		})
	}
}

// TestDecommissionApp_FailsClosedWhenTheLedgerCannotBeRead — "I could not check"
// must never mean "no claim exists".
func TestDecommissionApp_FailsClosedWhenTheLedgerCannotBeRead(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	// No ledger wired at all: the app cannot be SHOWN free of a claim.
	blind := NewFleetOrchestrator(h.apps, h.replicas, nil, nil, nil)
	app := testFleetApp(1)
	if err := h.apps.Create(context.Background(), app); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	isApp, err := blind.DecommissionApp(context.Background(), app.ID)
	if !errors.Is(err, domain.ErrAppBacksDurableResource) {
		t.Fatalf("DecommissionApp err = %v, want ErrAppBacksDurableResource (fail closed)", err)
	}
	if !isApp {
		t.Fatal("isApp = false, want true")
	}
	if !h.apps.has(app.ID) {
		t.Fatal("app row deleted on a refusal")
	}
}

// TestDecommissionDedicated_LegitimatePathStillTearsDownEndToEnd is the wrinkle
// this change had to get right: the resource's own teardown reaches
// DecommissionApp through the SAME function the guard protects, so it must pass
// — and it must pass because the resource marked its teardown intent first, not
// because a caller was allowed to say "trust me".
func TestDecommissionDedicated_LegitimatePathStillTearsDownEndToEnd(t *testing.T) {
	g := newGuardHarness(t)
	ctx := context.Background()

	appID := g.appID
	res := &domain.FleetResource{
		ID: uuid.New(), OwnerOrg: uuid.MustParse(orgA), ClaimKey: "db", Env: "prod",
		Class: resourceClassPostgres, Tier: resourceTierDedicated,
		Phase: domain.FleetResourcePhaseReady, AppID: &appID,
	}
	g.orch.resources.seed(res)

	snap := &fakeSnapshotter{}
	uc := NewFleetResourceProvisioner(
		orchProvisioner{orch: g.orch.orch}, g.orch.resources, g.orch.replicas, snap, testEngine(), testEndpointNaming(), nil, 0)

	final, err := uc.DecommissionDedicated(ctx, res.ID, true)
	if err != nil {
		t.Fatalf("the legitimate resource teardown must pass the app-seam guard: %v", err)
	}
	if final == nil {
		t.Fatal("teardown must report the final recovery point")
	}
	if snap.calls != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (snapshot-first)", snap.calls)
	}
	// It really tore down: replicas drained, backing file reclaimed, app row gone.
	if g.backingExists() {
		t.Fatal("backing file survived — the teardown did not reach DeleteAppVolumes")
	}
	if g.orch.apps.has(appID) {
		t.Fatal("app row survived the resource teardown")
	}
	if got := g.orch.replicas.count(); got != 0 {
		t.Fatalf("replicas after teardown = %d, want 0", got)
	}
	// And the claim is a tombstone.
	tomb, gerr := g.orch.resources.GetResourceByHandle(ctx, res.ID)
	if gerr != nil {
		t.Fatalf("reload resource: %v", gerr)
	}
	if tomb.Phase != domain.FleetResourcePhaseDecommissioned || tomb.DecommissionedAt == nil {
		t.Fatalf("resource not tombstoned: phase=%q at=%v", tomb.Phase, tomb.DecommissionedAt)
	}
}

// TestDecommissionDedicated_FailedTeardownLeavesTheAppGuarded — a teardown that
// dies on the way down must not leave a live database sitting outside the guard.
func TestDecommissionDedicated_FailedTeardownLeavesTheAppGuarded(t *testing.T) {
	ctx := context.Background()
	repo := newFakeResourceRepo()
	appID := uuid.New()
	res := &domain.FleetResource{
		ID: uuid.New(), OwnerOrg: uuid.MustParse(orgA), ClaimKey: "db", Env: "prod",
		Class: resourceClassPostgres, Tier: resourceTierDedicated,
		Phase: domain.FleetResourcePhaseReady, AppID: &appID,
	}
	repo.seed(res)

	boom := errors.New("host unreachable")
	uc := NewFleetResourceProvisioner(
		&fakeFleetProvisioner{decommissionErr: boom}, repo, nil, &fakeSnapshotter{}, testEngine(), testEndpointNaming(), nil, 0)

	if _, err := uc.DecommissionDedicated(ctx, res.ID, true); !errors.Is(err, boom) {
		t.Fatalf("DecommissionDedicated err = %v, want the teardown failure", err)
	}
	if _, ferr := repo.FindLiveResourceByApp(ctx, appID); ferr != nil {
		t.Fatalf("the claim must be LIVE again after a failed teardown (so the app seam still refuses), got %v", ferr)
	}
}
