package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// ─────────────────────────────────────────────────────────────────────
// #fleet-reconciler-acts-on-foreign-host-replicas — the host-authority fence.
//
// The invariant under test throughout this file:
//
//	a runtime process may perform a host-local side effect for a replica, volume
//	or microVM lease IF AND ONLY IF the durable row names that process's injected
//	selfHost.
//
// Every assertion below is on ATTEMPTED SIDE EFFECTS, not on returned errors.
// "It returned the sentinel" and "it touched nothing on the other host's VM" are
// different claims, and only the second one is the fence — a refusal that had
// already probed a PID, written a row or deleted a file has not protected
// anything.
// ─────────────────────────────────────────────────────────────────────

// ── Replica runtime ──────────────────────────────────────────────────

// foreignAndNilHost is the two-row table every replica-verb fence test runs: a
// row placed on the OTHER fleet host, and an unstamped row. Nil is refused
// exactly like foreign, because nothing about an unstamped row proves which
// machine owns the pid it names.
func foreignAndNilHost() []struct {
	name string
	host *uuid.UUID
} {
	return []struct {
		name string
		host *uuid.UUID
	}{
		{"row placed on the other fleet host", hostPtr(testForeignHost)},
		{"row with no host stamp at all", nil},
	}
}

func TestBootReplica_RefusesForeignRowBeforeEverySideEffect(t *testing.T) {
	for _, tt := range foreignAndNilHost() {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp()
			rep := newTestReplica(app.ID)
			rep.HostID = tt.host
			replicas := newRTReplicaRepo()
			if err := replicas.Create(context.Background(), rep); err != nil {
				t.Fatal(err)
			}
			mat := &countingMaterializer{}
			booter := &recordingBooter{}
			vols := newVolRepoFake(volWithBacking(app.ID, "/vol/data.ext4"))
			uc := newTestReplicaRuntime(t, mat, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")
			uc.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

			err := uc.BootReplica(context.Background(), rep.ID)
			if !errors.Is(err, domain.ErrReplicaHostMismatch) {
				t.Fatalf("BootReplica = %v, want ErrReplicaHostMismatch", err)
			}
			// The state write is the one that looks harmless and is not: persisting
			// `booting` would tell the OWNING host's reconciler that a boot it never
			// started is in flight.
			if replicas.updates != 0 {
				t.Errorf("wrote %d replica updates for a foreign row", replicas.updates)
			}
			if mat.calls != 0 {
				t.Errorf("materialized an image %d times for a foreign row", mat.calls)
			}
			if booter.bootInput != nil {
				t.Error("booted a VM for a foreign row")
			}
			if got, _ := replicas.FindByID(context.Background(), rep.ID); got.State != domain.ReplicaStateScheduled {
				t.Errorf("replica state = %q, want it untouched at %q", got.State, domain.ReplicaStateScheduled)
			}
		})
	}
}

func TestRefreshHealth_RefusesForeignRowWithoutProbing(t *testing.T) {
	for _, tt := range foreignAndNilHost() {
		t.Run(tt.name, func(t *testing.T) {
			replicas := newRTReplicaRepo()
			uc := newTestReplicaRuntime(t, fakeMaterializer{}, &recordingBooter{}, replicas, &rtAppRepo{}, "/tmp/imgwork", "10.0.0.9")

			probedPID, dialed := false, false
			uc.procAlive = func(int) bool { probedPID = true; return true }
			uc.dialGuest = func(string, int) bool { dialed = true; return true }

			pid := 4242
			rep := &domain.Replica{
				ID: uuid.New(), AppID: uuid.New(), HostID: tt.host,
				State: domain.ReplicaStateResident, PID: &pid, GuestIP: "10.201.0.6", Port: 8080,
			}
			healthy, err := uc.RefreshHealth(context.Background(), rep)
			if !errors.Is(err, domain.ErrReplicaHostMismatch) {
				t.Fatalf("RefreshHealth err = %v, want ErrReplicaHostMismatch", err)
			}
			if healthy {
				t.Error("a refused health refresh reported healthy")
			}
			// Probing a pid this host does not own reads an unrelated process, and
			// writing `dead` from that answer makes the OWNING host replace a replica
			// that never stopped serving.
			if probedPID {
				t.Error("probed the process of a replica on another host")
			}
			if dialed {
				t.Error("dialled the guest of a replica on another host")
			}
			if replicas.updates != 0 {
				t.Errorf("wrote %d replica updates for a foreign row", replicas.updates)
			}
		})
	}
}

func TestDecommissionReplica_RefusesForeignRowBeforeAnyTeardown(t *testing.T) {
	for _, tt := range foreignAndNilHost() {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp()
			rep := newTestReplica(app.ID)
			rep.HostID = tt.host
			rep.State = domain.ReplicaStateResident
			pid := 4242
			rep.PID, rep.SocketPath, rep.TapName = &pid, "/run/x.sock", "img7"
			replicas := newRTReplicaRepo()
			if err := replicas.Create(context.Background(), rep); err != nil {
				t.Fatal(err)
			}
			vol := volWithBacking(app.ID, "/vol/data.ext4")
			replicaID := rep.ID
			vol.AttachedReplica = &replicaID
			vol.Status = domain.VolumeStatusAttached
			vols := newVolRepoFake(vol)
			booter := &recordingBooter{}
			uc := newTestReplicaRuntime(t, fakeMaterializer{}, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")
			uc.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

			err := uc.DecommissionReplica(context.Background(), rep.ID)
			if !errors.Is(err, domain.ErrReplicaHostMismatch) {
				t.Fatalf("DecommissionReplica = %v, want ErrReplicaHostMismatch", err)
			}
			if booter.decommN != 0 {
				t.Error("tore down a VM belonging to another host")
			}
			if got, _ := vols.FindByID(context.Background(), vol.ID); got.AttachedReplica == nil {
				t.Error("detached another host's live volume")
			}
			if _, ferr := replicas.FindByID(context.Background(), rep.ID); ferr != nil {
				t.Errorf("deleted another host's replica row: %v", ferr)
			}
		})
	}
}

// TestDecommissionReplica_PreservesEverythingOnUnprovenTermination is the other
// half of the orphan fix. The booter now refuses rather than guessing; this side
// must then keep the replica row and the volume attachment, because they are the
// only handles a retry has — and because a still-running VM is still holding
// that disk.
func TestDecommissionReplica_PreservesEverythingOnUnprovenTermination(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{"termination could not be proven", domain.ErrVMTerminationUnproven},
		{"any other teardown failure", errors.New("tap delete failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp()
			rep := newTestReplica(app.ID)
			rep.State = domain.ReplicaStateResident
			pid := 4242
			rep.PID, rep.SocketPath, rep.TapName = &pid, "/run/x.sock", "img7"
			replicas := newRTReplicaRepo()
			if err := replicas.Create(context.Background(), rep); err != nil {
				t.Fatal(err)
			}
			vol := volWithBacking(app.ID, "/vol/data.ext4")
			replicaID := rep.ID
			vol.AttachedReplica = &replicaID
			vol.Status = domain.VolumeStatusAttached
			vols := newVolRepoFake(vol)

			workDir := t.TempDir()
			staging := filepath.Join(workDir, rep.ID.String())
			if err := mkdirAllForTest(staging); err != nil {
				t.Fatal(err)
			}

			booter := &recordingBooter{decommErr: tt.cause}
			uc := newTestReplicaRuntime(t, fakeMaterializer{}, booter, replicas, &rtAppRepo{app: app}, workDir, "10.0.0.9")
			uc.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

			err := uc.DecommissionReplica(context.Background(), rep.ID)
			if !errors.Is(err, tt.cause) {
				t.Fatalf("DecommissionReplica = %v, want %v", err, tt.cause)
			}
			if got, _ := vols.FindByID(context.Background(), vol.ID); got.AttachedReplica == nil {
				t.Error("volume detached although the VM may still be holding it")
			}
			if _, ferr := replicas.FindByID(context.Background(), rep.ID); ferr != nil {
				t.Errorf("replica row deleted although the VM was not proven gone: %v", ferr)
			}
			if !dirExistsForTest(staging) {
				t.Error("staging directory reclaimed although the VM was not proven gone")
			}
		})
	}
}

// ── Orchestrator ─────────────────────────────────────────────────────

// TestReconcileApp_ForeignRowsCountTowardDesired is the anti-duplicate rule.
// Counting only LOCAL rows is the fail-open direction and it is exactly how a
// second occupant appears: a host that may not touch the first replica would see
// zero, declare a shortfall, and boot a duplicate onto a single-writer volume.
func TestReconcileApp_ForeignRowsCountTowardDesired(t *testing.T) {
	app := testFleetApp(1)
	h := newOrchHarness(t, oneLiveHost(), app)
	foreign := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
		State: domain.ReplicaStateResident, RestartPolicy: domain.RestartPolicyAlways,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.replicas.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	if err := h.converge(t, app.ID); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if got := h.replicas.count(); got != 1 {
		t.Fatalf("replicas = %d, want 1 — the foreign row already occupies the desired slot", got)
	}
}

// TestReconcileApp_ForeignRowsAreNeverActuated: a foreign row is a valid global
// desired-state fact, not work.
func TestReconcileApp_ForeignRowsAreNeverActuated(t *testing.T) {
	states := []domain.ReplicaState{
		domain.ReplicaStateScheduled,
		domain.ReplicaStateResident,
		domain.ReplicaStateDead,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			app := testFleetApp(1)
			h := newOrchHarness(t, oneLiveHost(), app)
			pid := 4242
			foreign := &domain.Replica{
				ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
				State: state, PID: &pid, RestartPolicy: domain.RestartPolicyAlways,
				CreatedAt: time.Now().UTC(),
			}
			if err := h.replicas.Create(context.Background(), foreign); err != nil {
				t.Fatal(err)
			}
			if err := h.converge(t, app.ID); err != nil {
				t.Fatalf("converge: %v", err)
			}
			after, err := h.replicas.FindByID(context.Background(), foreign.ID)
			if err != nil {
				t.Fatalf("the foreign row was deleted: %v", err)
			}
			if after.State != state {
				t.Fatalf("foreign row state = %q, want it untouched at %q", after.State, state)
			}
		})
	}
}

// TestReconcileApp_SurplusDrainsLocalCandidatesOnly: the drain COUNT is global
// (capacity is), but a surplus this host may not tear down is never a reason to
// delete a foreign row — there is no remote teardown verb and inventing one is
// out of scope.
func TestReconcileApp_SurplusDrainsLocalCandidatesOnly(t *testing.T) {
	app := testFleetApp(1)
	h := newOrchHarness(t, oneLiveHost(), app)
	pid := 4242
	local := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
		State: domain.ReplicaStateResident, PID: &pid,
		RestartPolicy: domain.RestartPolicyAlways, CreatedAt: time.Now().UTC(),
	}
	foreign := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
		State: domain.ReplicaStateResident, PID: &pid,
		RestartPolicy: domain.RestartPolicyAlways, CreatedAt: time.Now().UTC().Add(time.Second),
	}
	for _, r := range []*domain.Replica{local, foreign} {
		if err := h.replicas.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
		t.Fatalf("ReconcileApp: %v", err)
	}
	if _, err := h.replicas.FindByID(context.Background(), foreign.ID); err != nil {
		t.Fatalf("the surplus drain deleted a foreign row: %v", err)
	}
	if _, err := h.replicas.FindByID(context.Background(), local.ID); err == nil {
		t.Fatal("the surplus drain kept the LOCAL candidate; it must drain what it owns")
	}
}

// TestActuateScheduledOwned_OnlyBootsRowsStampedWithThisHost is the split the
// whole item introduces: any process may WRITE a placement; only the host it
// names turns it into a running microVM.
func TestActuateScheduledOwned_OnlyBootsRowsStampedWithThisHost(t *testing.T) {
	app := testFleetApp(0)
	h := newOrchHarness(t, oneLiveHost(), app)
	mine := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
		State: domain.ReplicaStateScheduled, CreatedAt: time.Now().UTC(),
	}
	theirs := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
		State: domain.ReplicaStateScheduled, CreatedAt: time.Now().UTC(),
	}
	for _, r := range []*domain.Replica{mine, theirs} {
		if err := h.replicas.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	h.actuate(t)

	got, err := h.replicas.FindByID(context.Background(), mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.ReplicaStateResident {
		t.Fatalf("own scheduled row state = %q, want resident", got.State)
	}
	other, err := h.replicas.FindByID(context.Background(), theirs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != domain.ReplicaStateScheduled {
		t.Fatalf("foreign scheduled row was actuated (state=%q) — it is invisible to this host's ListByHost", other.State)
	}

	// Idempotent: a second pass finds nothing in `scheduled` and boots nothing.
	h.actuate(t)
	again, _ := h.replicas.FindByID(context.Background(), mine.ID)
	if again.UpdatedAt.After(got.UpdatedAt) {
		t.Error("a repeated actuation pass re-booted an already-resident row")
	}
}

// TestActuateScheduledOwned_IgnoresNonScheduledStates: only `scheduled` is work.
// Booting/resident/dead rows are mid-transition or terminal, and re-entering
// them is how a running VM gets a second boot.
func TestActuateScheduledOwned_IgnoresNonScheduledStates(t *testing.T) {
	app := testFleetApp(0)
	h := newOrchHarness(t, oneLiveHost(), app)
	for _, state := range []domain.ReplicaState{
		domain.ReplicaStateBooting, domain.ReplicaStateResident, domain.ReplicaStateDead,
	} {
		r := &domain.Replica{
			ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
			State: state, CreatedAt: time.Now().UTC(),
		}
		if err := h.replicas.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	h.actuate(t)
	for _, state := range []domain.ReplicaState{
		domain.ReplicaStateBooting, domain.ReplicaStateResident, domain.ReplicaStateDead,
	} {
		if got := h.replicas.countState(state); got != 1 {
			t.Fatalf("%s replicas = %d, want 1 (untouched by the actuation pass)", state, got)
		}
	}
}

// TestReconcileApp_ScheduledRowOnADownHostStaysPut. Cross-host stateful mobility
// needs replicated bytes plus a fencing protocol and belongs to DB-HA. Until
// then a row whose owner is gone does NOTHING — it is not deleted, not
// re-placed, and it still counts, because deleting it races the owner returning
// and would leave a rowless VM plus an orphan lease.
func TestReconcileApp_ScheduledRowOnADownHostStaysPut(t *testing.T) {
	app := testFleetApp(1)
	// Only THIS host is live; the foreign host is absent from the candidate set.
	h := newOrchHarness(t, oneLiveHost(), app)
	stranded := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
		State: domain.ReplicaStateScheduled, CreatedAt: time.Now().UTC(),
	}
	if err := h.replicas.Create(context.Background(), stranded); err != nil {
		t.Fatal(err)
	}

	if err := h.converge(t, app.ID); err != nil {
		t.Fatalf("converge: %v", err)
	}
	got, err := h.replicas.FindByID(context.Background(), stranded.ID)
	if err != nil {
		t.Fatalf("the stranded row was deleted: %v", err)
	}
	if got.State != domain.ReplicaStateScheduled || *got.HostID != testForeignHost {
		t.Fatalf("stranded row = state %q host %v, want it unchanged", got.State, got.HostID)
	}
	if n := h.replicas.count(); n != 1 {
		t.Fatalf("replicas = %d, want 1 — the stranded row still counts toward desired", n)
	}
}

// TestHealthApp_ForeignResidentCountsOnlyWhileItsHostIsLive. The fence must not
// become a permanent false red: a resident row on a LIVE peer is the only
// evidence available here and it is honoured; on a stale peer it is a claim
// nobody is refreshing. Either way NOTHING is mutated.
func TestHealthApp_ForeignResidentCountsOnlyWhileItsHostIsLive(t *testing.T) {
	tests := []struct {
		name        string
		hosts       []domain.Host
		wantHealthy bool
		wantState   string
	}{
		{"the peer is live: its recorded resident counts", twoLiveHosts(), true, "running"},
		{"the peer is stale: it does not count, and nothing is written", oneLiveHost(), false, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := testFleetApp(1)
			h := newOrchHarness(t, tt.hosts, app)
			pid := 4242
			foreign := &domain.Replica{
				ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
				State: domain.ReplicaStateResident, PID: &pid,
				GuestIP: "10.201.16.6", Port: 8080, Endpoint: "http://10.201.16.6:8080",
				CreatedAt: time.Now().UTC(),
			}
			if err := h.replicas.Create(context.Background(), foreign); err != nil {
				t.Fatal(err)
			}

			out, known, err := h.orch.HealthApp(context.Background(), app.ID)
			if err != nil || !known {
				t.Fatalf("HealthApp: known=%v err=%v", known, err)
			}
			if out.Healthy != tt.wantHealthy || out.State != tt.wantState {
				t.Fatalf("health = {state:%q healthy:%v}, want {%q %v}", out.State, out.Healthy, tt.wantState, tt.wantHealthy)
			}
			after, _ := h.replicas.FindByID(context.Background(), foreign.ID)
			if after.State != domain.ReplicaStateResident {
				t.Fatalf("HealthApp mutated a foreign row (state=%q) — only its owner may", after.State)
			}
		})
	}
}

// twoLiveHosts is a fleet where BOTH this host and the peer are live.
func twoLiveHosts() []domain.Host {
	hosts := oneLiveHost()
	peer := hosts[0]
	peer.ID = testForeignHost
	return append(hosts, peer)
}

// TestDecommissionApp_WaitsForTheForeignOwnerToDrain: the app-row cascade
// deletes backing FILES, so it must never run while a replica still exists
// anywhere. A foreign remainder is a WAIT (its owner drains it on its next
// tick), not a licence to proceed.
func TestDecommissionApp_WaitsForTheForeignOwnerToDrain(t *testing.T) {
	app := testFleetApp(1)
	h := newOrchHarness(t, oneLiveHost(), app)
	routes := newOrchRouteRepo()
	h.orch.SetIngress(routes, "fleet.sentiae.local", nil)
	vol := volWithBacking(app.ID, "/vol/data.ext4")
	vols := newVolRepoFake(vol)
	h.orch.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

	foreign := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testForeignHost),
		State: domain.ReplicaStateResident, CreatedAt: time.Now().UTC(),
	}
	if err := h.replicas.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	known, err := h.orch.DecommissionApp(context.Background(), app.ID)
	if !known {
		t.Fatal("known = false, want true — the app IS known, its drain is just pending elsewhere")
	}
	if !errors.Is(err, domain.ErrReplicaHostMismatch) {
		t.Fatalf("DecommissionApp = %v, want ErrReplicaHostMismatch", err)
	}
	if vols.count() != 1 {
		t.Errorf("volume rows = %d, want 1 — nothing may be reclaimed while a replica remains", vols.count())
	}
	if !h.apps.has(app.ID) {
		t.Error("app row deleted while a foreign replica was still resident")
	}
	// Desired IS persisted at zero: that fact is global, and it is what makes the
	// owner drain the row on its next pass.
	if a, _ := h.apps.FindByID(context.Background(), app.ID); a.DesiredReplicas != 0 {
		t.Errorf("desired_replicas = %d, want 0 (the drain instruction is global)", a.DesiredReplicas)
	}

	// The owner drains it; a caller retry then finalizes.
	if derr := h.replicas.Delete(context.Background(), foreign.ID); derr != nil {
		t.Fatal(derr)
	}
	known, err = h.orch.DecommissionApp(context.Background(), app.ID)
	if !known || err != nil {
		t.Fatalf("retry after the owner drained: known=%v err=%v", known, err)
	}
	if h.apps.has(app.ID) {
		t.Error("app row survived the finalizing retry")
	}
}

// TestReconcileApp_StatefulPlacementFollowsTheData. New volume bytes are created
// with this host's affinity (EnsureAppVolumes writes it into the first insert),
// so a stateful app first materialized here is necessarily scheduled back here —
// and when the host holding its data is not live it is placed NOWHERE, because
// booting it elsewhere would run a customer's database off an empty disk.
//
// The scheduler is additionally TOLD the affinity and its answer is then
// VERIFIED against it (ErrVolumeHostMismatch before any replica row is created).
// That check is defence in depth and is not reachable through the real
// FleetScheduler, whose affinity filter drops every other host from the
// candidate set — it exists so a future scheduler change cannot quietly place a
// stateful replica off its data.
func TestReconcileApp_StatefulPlacementFollowsTheData(t *testing.T) {
	t.Run("placed on the host that holds the bytes", func(t *testing.T) {
		app := testFleetApp(1)
		h := newOrchHarness(t, twoLiveHosts(), app)
		// A real, mountable-looking backing file: the placement precondition stats it
		// each tick and defers when it is absent.
		backing := filepath.Join(t.TempDir(), "data.ext4")
		writeSyntheticExt4(t, backing, syntheticExt4Size)
		vols := newVolRepoFake(volWithBacking(app.ID, backing))
		h.orch.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, filepath.Dir(backing), nil))

		if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
			t.Fatalf("ReconcileApp: %v", err)
		}
		reps, _ := h.replicas.ListByApp(context.Background(), app.ID)
		if len(reps) != 1 {
			t.Fatalf("replicas = %d, want 1", len(reps))
		}
		if reps[0].HostID == nil || *reps[0].HostID != testSelfHost {
			t.Fatalf("placed on %v, want the host holding the data (%v)", reps[0].HostID, testSelfHost)
		}
	})

	t.Run("the data host is down: placed nowhere, never moved", func(t *testing.T) {
		app := testFleetApp(1)
		// Only the PEER is live; this host — which holds the bytes — is not.
		hosts := oneLiveHost()
		hosts[0].ID = testForeignHost
		h := newOrchHarness(t, hosts, app)
		vols := newVolRepoFake(volWithBacking(app.ID, "/vol/data.ext4"))
		h.orch.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

		if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
			t.Fatalf("ReconcileApp: %v", err)
		}
		if got := h.replicas.count(); got != 0 {
			t.Fatalf("replicas = %d, want 0 — a stateful app is never moved off its data", got)
		}
	})
}

// ── Volume manager ───────────────────────────────────────────────────

func TestEnsureAppVolumes_NewRowCarriesSelfAffinityOnItsFirstInsert(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake()
	m := newTestVolumeManager(t, repo, &modeBackend{}, "/vol", nil)

	out, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
	if err != nil {
		t.Fatalf("EnsureAppVolumes: %v", err)
	}
	if len(out) != 1 || out[0].HostAffinity == nil || *out[0].HostAffinity != testSelfHost {
		t.Fatalf("new volume affinity = %v, want it stamped with this host ON the insert", out[0].HostAffinity)
	}
	stored, _ := repo.FindByID(context.Background(), out[0].ID)
	if stored.HostAffinity == nil || *stored.HostAffinity != testSelfHost {
		t.Fatal("the PERSISTED row has no affinity — there must never be a materialized row without one")
	}
	if repo.hostBinds != 0 {
		t.Errorf("bound the affinity in a second step (%d CAS calls) — it belongs in the insert", repo.hostBinds)
	}
}

func TestEnsureAppVolumes_ForeignRowTouchesNothing(t *testing.T) {
	appID := uuid.New()
	vol := volWithBacking(appID, "/vol/data.ext4")
	vol.HostAffinity = hostPtr(testForeignHost)
	repo := newVolRepoFake(vol)
	backend := &modeBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	_, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
	if !errors.Is(err, domain.ErrVolumeHostMismatch) {
		t.Fatalf("EnsureAppVolumes = %v, want ErrVolumeHostMismatch", err)
	}
	if len(backend.modes) != 0 {
		t.Errorf("called the backend %v for a volume on another host", backend.modes)
	}
	if repo.updates != 0 || repo.creates != 0 || repo.hostBinds != 0 {
		t.Errorf("wrote to the repository for a foreign volume (updates=%d creates=%d binds=%d)",
			repo.updates, repo.creates, repo.hostBinds)
	}
}

// TestEnsureAppVolumes_LegacyNilAffinityAdoptsBeforeItBinds: the ONLY evidence
// that this host may claim an unstamped legacy row is a local file that passes
// the backend's identity checks. A missing file must not bind and must not
// create — "the data is not here" must never become "so I will make some and
// call it mine".
func TestEnsureAppVolumes_LegacyNilAffinityAdoptsBeforeItBinds(t *testing.T) {
	t.Run("a validated local file binds the row to this host", func(t *testing.T) {
		appID := uuid.New()
		vol := volAt(appID, "/vol")
		vol.HostAffinity = nil
		repo := newVolRepoFake(vol)
		backend := &modeBackend{}
		m := newTestVolumeManager(t, repo, backend, "/vol", nil)

		out, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
		if err != nil {
			t.Fatalf("EnsureAppVolumes: %v", err)
		}
		if len(backend.modes) != 1 || backend.modes[0] != VolumeEnsureAdopt {
			t.Fatalf("backend modes = %v, want exactly one adopt (never create)", backend.modes)
		}
		if repo.hostBinds != 1 {
			t.Fatalf("host CAS calls = %d, want 1", repo.hostBinds)
		}
		if out[0].HostAffinity == nil || *out[0].HostAffinity != testSelfHost {
			t.Fatalf("affinity after adopt = %v, want this host", out[0].HostAffinity)
		}
	})

	t.Run("a missing local file binds nothing", func(t *testing.T) {
		appID := uuid.New()
		vol := volAt(appID, "/vol")
		vol.HostAffinity = nil
		repo := newVolRepoFake(vol)
		backend := &modeBackend{failWith: domain.ErrVolumeBackingFileMissing}
		m := newTestVolumeManager(t, repo, backend, "/vol", nil)

		_, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
		if !errors.Is(err, domain.ErrVolumeBackingFileMissing) {
			t.Fatalf("EnsureAppVolumes = %v, want ErrVolumeBackingFileMissing", err)
		}
		if repo.hostBinds != 0 {
			t.Fatalf("bound a row this host cannot prove it holds (%d CAS calls)", repo.hostBinds)
		}
		if backend.modes[0] != VolumeEnsureAdopt {
			t.Fatalf("backend mode = %v, want adopt — a legacy row is never created", backend.modes)
		}
	})

	t.Run("losing the CAS never deletes the legacy bytes", func(t *testing.T) {
		appID := uuid.New()
		vol := volAt(appID, "/vol")
		vol.HostAffinity = nil
		repo := newVolRepoFake(vol)
		// The other host wins the CAS while this one is adopting.
		repo.beforeHostBind = func() {
			repo.store[vol.ID].HostAffinity = hostPtr(testForeignHost)
		}
		backend := &recordingModeBackend{}
		m := newTestVolumeManager(t, repo, backend, "/vol", nil)

		_, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
		if !errors.Is(err, domain.ErrVolumeHostMismatch) {
			t.Fatalf("EnsureAppVolumes = %v, want ErrVolumeHostMismatch", err)
		}
		if len(backend.deleted) != 0 {
			t.Fatalf("the CAS loser deleted %v — it did not create those bytes", backend.deleted)
		}
		stored, _ := repo.FindByID(context.Background(), vol.ID)
		if stored.HostAffinity == nil || *stored.HostAffinity != testForeignHost {
			t.Fatalf("the winner's affinity was overwritten: %v", stored.HostAffinity)
		}
	})
}

// TestEnsureAppVolumes_CreateCompensationMatrix walks every branch of the
// filesystem/DB saga. The rule that decides all of them: a file is deleted only
// when THIS attempt created it AND an authoritative re-read proves no row owns
// it. Uncertainty always keeps the bytes.
// errRereadUnavailable is the AUTHORITATIVE re-read's failure, distinct from the
// insert's, so the matrix can prove which of the two a caller can match on.
var errRereadUnavailable = errors.New("ledger unavailable")

func TestEnsureAppVolumes_CreateCompensationMatrix(t *testing.T) {
	createErr := errors.New("insert violates a unique index")
	tests := []struct {
		name string
		// created reports whether the backend says THIS call made the file.
		created bool
		// winner installs a committed row for the same mount before the re-read.
		winner func(appID uuid.UUID, attemptDir string) *domain.Volume
		// relistErr makes the authoritative re-read fail.
		relistErr  bool
		wantDelete bool
		wantErrIs  error
		// wantVolume asserts the call RETURNED the committed volume rather than an
		// empty set — the lost-ack branch must not drop it.
		wantVolume bool
	}{
		{
			name:       "created, proven no winner: this attempt's file is residue",
			created:    true,
			wantDelete: true,
			wantErrIs:  createErr,
		},
		{
			name:       "NOT created, proven no winner: the file predates this call",
			created:    false,
			wantDelete: false,
			wantErrIs:  createErr,
		},
		{
			name: "the committed winner IS this attempt: adopt it, return it, delete nothing",
			// A lost ack / retried write. Deleting the file would destroy the volume
			// the ledger now promises — and DROPPING it from the result set would tell
			// ProvisionApp the app is stateless, which is what enforces the
			// single-writer scale guard.
			created:    true,
			winner:     sameAttemptWinner,
			wantDelete: false,
			wantVolume: true,
		},
		{
			name:       "a DIFFERENT winner holds the mount: reclaim only our own file",
			created:    true,
			winner:     differentWinner,
			wantDelete: true,
			wantErrIs:  createErr,
		},
		{
			name:       "a winner on ANOTHER host holds the mount",
			created:    true,
			winner:     foreignWinner,
			wantDelete: true,
			wantErrIs:  domain.ErrVolumeHostMismatch,
		},
		{
			name: "the re-read itself fails: ownership is unknown, keep the bytes",
			// Uncertain ownership is never permission to delete possible customer
			// data; the report-only ledger reconciler surfaces the unattributed file.
			// The RE-READ error is what is returned and matchable — it is the thing
			// that made the outcome undecidable, and rendering it as %v text inside
			// the create error would hide it from errors.Is.
			created:    true,
			relistErr:  true,
			wantDelete: false,
			wantErrIs:  errRereadUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appID := uuid.New()
			repo := newVolRepoFake()
			backend := &recordingModeBackend{created: tt.created}
			m := newTestVolumeManager(t, repo, backend, "/vol", nil)

			repo.createErr = createErr
			repo.onCreate = func(attempt *domain.Volume) {
				if tt.relistErr {
					repo.listErr = errRereadUnavailable
					return
				}
				if tt.winner == nil {
					return
				}
				w := tt.winner(appID, attempt.BackingPath)
				if w.ID == uuid.Nil {
					w.ID = attempt.ID
				}
				repo.put(w)
			}

			out, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
			if tt.wantVolume {
				if err != nil {
					t.Fatalf("EnsureAppVolumes = %v, want nil (the committed row is this attempt)", err)
				}
				if len(out) != 1 {
					t.Fatalf("returned %d volumes, want 1 — the committed winner must flow into the result set", len(out))
				}
				if out[0].MountPath != "/data" || out[0].BackingPath == "" {
					t.Fatalf("returned volume = %+v, want the committed row for /data", out[0])
				}
				if out[0].HostAffinity == nil || *out[0].HostAffinity != testSelfHost {
					t.Fatalf("returned volume affinity = %v, want this host", out[0].HostAffinity)
				}
			} else {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("EnsureAppVolumes = %v, want errors.Is(_, %v)", err, tt.wantErrIs)
				}
				if len(out) != 0 {
					t.Fatalf("returned %d volumes on a failed ensure, want 0", len(out))
				}
			}
			if got := len(backend.deleted) > 0; got != tt.wantDelete {
				t.Fatalf("deleted = %v (%v), want %v", got, backend.deleted, tt.wantDelete)
			}
		})
	}
}

// TestEnsureAppVolumes_LostAckKeepsEarlierSpecVolumes: the lost-ack branch used
// to return (nil, nil), which dropped not only the winner but every volume
// already ensured in this call. ProvisionApp reads that length to decide whether
// the app is volume-bearing, so a stateful app would have passed the
// single-writer scale guard as if it were stateless.
func TestEnsureAppVolumes_LostAckKeepsEarlierSpecVolumes(t *testing.T) {
	appID := uuid.New()
	repo := newVolRepoFake()
	backend := &recordingModeBackend{created: true}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	// The FIRST spec inserts normally; the SECOND loses its ack and is adopted.
	first := true
	repo.createErr = errors.New("insert violates a unique index")
	repo.onCreate = func(attempt *domain.Volume) {
		// The committed winner IS this attempt (a lost ack): same id, same path.
		w := sameAttemptWinner(appID, attempt.BackingPath)
		w.ID = attempt.ID
		w.MountPath = attempt.MountPath
		repo.put(w)
	}
	repo.beforeCreate = func() bool {
		if first {
			first = false
			return true // let the first insert through
		}
		return false
	}

	out, err := m.EnsureAppVolumes(context.Background(), appID, []VolumeSpecInput{
		{SizeMB: 64, MountPath: "/data"},
		{SizeMB: 64, MountPath: "/wal"},
	})
	if err != nil {
		t.Fatalf("EnsureAppVolumes: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("returned %d volumes, want 2 (the ack-losing spec AND the one before it)", len(out))
	}
	mounts := map[string]bool{out[0].MountPath: true, out[1].MountPath: true}
	if !mounts["/data"] || !mounts["/wal"] {
		t.Fatalf("returned mounts = %v, want both /data and /wal", mounts)
	}
	if len(backend.deleted) != 0 {
		t.Fatalf("deleted %v — the adopted winner's bytes are the ledger's promise", backend.deleted)
	}
}

func sameAttemptWinner(appID uuid.UUID, path string) *domain.Volume {
	return &domain.Volume{
		AppID: &appID, MountPath: "/data", BackingPath: path,
		HostAffinity: hostPtr(testSelfHost), Status: domain.VolumeStatusAvailable,
	}
}

func differentWinner(appID uuid.UUID, _ string) *domain.Volume {
	return &domain.Volume{
		ID: uuid.New(), AppID: &appID, MountPath: "/data", BackingPath: "/vol/somebody-else.ext4",
		HostAffinity: hostPtr(testSelfHost), Status: domain.VolumeStatusAvailable,
	}
}

func foreignWinner(appID uuid.UUID, _ string) *domain.Volume {
	return &domain.Volume{
		ID: uuid.New(), AppID: &appID, MountPath: "/data", BackingPath: "/vol/elsewhere.ext4",
		HostAffinity: hostPtr(testForeignHost), Status: domain.VolumeStatusAvailable,
	}
}

// TestVolumeManagerPreflight_AllOrNothingOnOneForeignRow. The preflight reads
// the WHOLE set before any write, so a single foreign row leaves zero partial
// effects — a fence that refused after the first unlink or the first attach
// would have protected nothing.
func TestVolumeManagerPreflight_AllOrNothingOnOneForeignRow(t *testing.T) {
	verbs := map[string]func(m *FleetVolumeManager, appID uuid.UUID) error{
		"DeleteAppVolumes": func(m *FleetVolumeManager, appID uuid.UUID) error {
			return m.DeleteAppVolumes(context.Background(), appID)
		},
		"BindToResource": func(m *FleetVolumeManager, appID uuid.UUID) error {
			return m.BindToResource(context.Background(), appID, uuid.New())
		},
		"AttachTo": func(m *FleetVolumeManager, appID uuid.UUID) error {
			return m.AttachTo(context.Background(), appID, uuid.New())
		},
		"DetachFrom": func(m *FleetVolumeManager, appID uuid.UUID) error {
			return m.DetachFrom(context.Background(), appID)
		},
	}
	for name, verb := range verbs {
		t.Run(name, func(t *testing.T) {
			appID := uuid.New()
			local := volWithBacking(appID, "/vol/a.ext4")
			foreign := volWithBacking(appID, "/vol/b.ext4")
			foreign.HostAffinity = hostPtr(testForeignHost)
			repo := newVolRepoFake(local, foreign)
			backend := &recordingBackend{}
			m := newTestVolumeManager(t, repo, backend, "/vol", nil)

			if err := verb(m, appID); !errors.Is(err, domain.ErrVolumeHostMismatch) {
				t.Fatalf("%s = %v, want ErrVolumeHostMismatch", name, err)
			}
			if len(backend.deleted) != 0 {
				t.Errorf("%s deleted %v despite one foreign row in the set", name, backend.deleted)
			}
			if repo.updates != 0 {
				t.Errorf("%s wrote %d row updates despite one foreign row in the set", name, repo.updates)
			}
			if repo.count() != 2 {
				t.Errorf("%s left %d rows, want both", name, repo.count())
			}
		})
	}
}

// TestAffinityHost_RefusesAnythingItCannotAnswerUnanimously. It is the input the
// scheduler pins a stateful placement with, so a first-match answer over a mixed
// set would place a replica on a host holding only part of its data.
func TestAffinityHost_RefusesAnythingItCannotAnswerUnanimously(t *testing.T) {
	tests := []struct {
		name       string
		vols       func(appID uuid.UUID) []*domain.Volume
		wantPinned bool
		wantErr    bool
	}{
		{
			name:       "no volumes: a stateless app is pinned to nothing",
			vols:       func(uuid.UUID) []*domain.Volume { return nil },
			wantPinned: false,
		},
		{
			name: "one local volume",
			vols: func(appID uuid.UUID) []*domain.Volume {
				return []*domain.Volume{volWithBacking(appID, "/vol/a.ext4")}
			},
			wantPinned: true,
		},
		{
			name: "several volumes agreeing on one host",
			vols: func(appID uuid.UUID) []*domain.Volume {
				return []*domain.Volume{volWithBacking(appID, "/vol/a.ext4"), volWithBacking(appID, "/vol/b.ext4")}
			},
			wantPinned: true,
		},
		{
			name: "one unstamped volume: the app's data has no provable location",
			vols: func(appID uuid.UUID) []*domain.Volume {
				a := volWithBacking(appID, "/vol/a.ext4")
				b := volWithBacking(appID, "/vol/b.ext4")
				b.HostAffinity = nil
				return []*domain.Volume{a, b}
			},
			wantErr: true,
		},
		{
			name: "volumes pinned to DIFFERENT hosts: no single host holds the data",
			vols: func(appID uuid.UUID) []*domain.Volume {
				a := volWithBacking(appID, "/vol/a.ext4")
				b := volWithBacking(appID, "/vol/b.ext4")
				b.HostAffinity = hostPtr(testForeignHost)
				return []*domain.Volume{a, b}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appID := uuid.New()
			m := newTestVolumeManager(t, newVolRepoFake(tt.vols(appID)...), &recordingBackend{}, "/vol", nil)
			host, pinned, err := m.AffinityHost(context.Background(), appID)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrVolumeHostMismatch) {
					t.Fatalf("AffinityHost err = %v, want ErrVolumeHostMismatch", err)
				}
				if host != nil {
					t.Fatalf("AffinityHost guessed a host (%v) it could not prove", host)
				}
				return
			}
			if err != nil {
				t.Fatalf("AffinityHost: %v", err)
			}
			if pinned != tt.wantPinned {
				t.Fatalf("pinned = %v, want %v", pinned, tt.wantPinned)
			}
		})
	}
}

// TestMarkDegraded_StaysLedgerOnlyAndUnfenced pins the DELIBERATE exception. The
// condition it records is "the host holding this data is gone", so requiring
// that host to record it would make the fact unwritable by construction. It
// touches no byte, no guest and no attachment — and this exception must not be
// widened to any other verb.
func TestMarkDegraded_StaysLedgerOnlyAndUnfenced(t *testing.T) {
	appID := uuid.New()
	vol := volWithBacking(appID, "/vol/a.ext4")
	vol.HostAffinity = hostPtr(testForeignHost)
	repo := newVolRepoFake(vol)
	backend := &recordingBackend{}
	m := newTestVolumeManager(t, repo, backend, "/vol", nil)

	if err := m.MarkDegraded(context.Background(), appID); err != nil {
		t.Fatalf("MarkDegraded on a foreign volume = %v, want nil", err)
	}
	got, _ := repo.FindByID(context.Background(), vol.ID)
	if got.Status != domain.VolumeStatusDegraded {
		t.Fatalf("status = %q, want degraded", got.Status)
	}
	if got.HostAffinity == nil || *got.HostAffinity != testForeignHost {
		t.Error("MarkDegraded moved the affinity — it is a ledger annotation, nothing more")
	}
	if len(backend.deleted) != 0 {
		t.Errorf("MarkDegraded touched bytes: %v", backend.deleted)
	}
}

// ── Net lease release ────────────────────────────────────────────────

// TestNetAllocatorRelease_OnlyReleasesItsOwnHostsLease. Acquire always refused to
// adopt a foreign lease; Release did not check at all, so a teardown on the wrong
// host deleted the row fencing a LIVE VM elsewhere — and the next boot there
// would be handed that VM's /30, uid and chroot.
func TestNetAllocatorRelease_OnlyReleasesItsOwnHostsLease(t *testing.T) {
	kind := domain.NetLeaseOwnerReplica

	t.Run("no lease at all is idempotent success", func(t *testing.T) {
		leases := &fenceLeaseRepo{findErr: domain.ErrNetLeaseNotFound}
		a := NewFleetNetAllocator(leases, testSelfHost, 0, 100000, 1000)
		if err := a.Release(context.Background(), kind, uuid.New()); err != nil {
			t.Fatalf("Release = %v, want nil (teardown retries must converge)", err)
		}
		if leases.releases != 0 {
			t.Errorf("called the repository release %d times for a lease that does not exist", leases.releases)
		}
	})

	t.Run("an own-host lease is released exactly once", func(t *testing.T) {
		owner := uuid.New()
		leases := &fenceLeaseRepo{lease: &domain.NetLease{
			ID: uuid.New(), HostID: testSelfHost, OwnerKind: kind, OwnerID: owner, NetIndex: 7,
		}}
		a := NewFleetNetAllocator(leases, testSelfHost, 0, 100000, 1000)
		if err := a.Release(context.Background(), kind, owner); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if leases.releases != 1 {
			t.Fatalf("repository releases = %d, want 1", leases.releases)
		}
	})

	t.Run("a foreign lease is refused and never deleted", func(t *testing.T) {
		owner := uuid.New()
		leases := &fenceLeaseRepo{lease: &domain.NetLease{
			ID: uuid.New(), HostID: testForeignHost, OwnerKind: kind, OwnerID: owner, NetIndex: 1031,
		}}
		a := NewFleetNetAllocator(leases, testSelfHost, 0, 100000, 1000)
		err := a.Release(context.Background(), kind, owner)
		if !errors.Is(err, domain.ErrNetLeaseConflict) {
			t.Fatalf("Release = %v, want ErrNetLeaseConflict", err)
		}
		if leases.releases != 0 {
			t.Fatalf("freed another host's live addressing (%d releases)", leases.releases)
		}
	})

	t.Run("a lookup failure never deletes", func(t *testing.T) {
		leases := &fenceLeaseRepo{findErr: errors.New("ledger unavailable")}
		a := NewFleetNetAllocator(leases, testSelfHost, 0, 100000, 1000)
		if err := a.Release(context.Background(), kind, uuid.New()); err == nil {
			t.Fatal("Release = nil, want the lookup error")
		}
		if leases.releases != 0 {
			t.Fatalf("deleted a lease under uncertainty (%d releases)", leases.releases)
		}
	})
}

// fenceLeaseRepo is a NetLeaseRepository that answers one scripted lookup and
// counts releases. Only the two methods the release fence uses are meaningful.
type fenceLeaseRepo struct {
	lease    *domain.NetLease
	findErr  error
	releases int
}

var _ repository.NetLeaseRepository = (*fenceLeaseRepo)(nil)

func (f *fenceLeaseRepo) Acquire(context.Context, *domain.NetLease) error { return nil }
func (f *fenceLeaseRepo) UsedSlots(context.Context, uuid.UUID) ([]int, error) {
	return nil, nil
}
func (f *fenceLeaseRepo) FindByOwner(context.Context, domain.NetLeaseOwnerKind, uuid.UUID) (*domain.NetLease, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.lease, nil
}
func (f *fenceLeaseRepo) Release(context.Context, domain.NetLeaseOwnerKind, uuid.UUID) error {
	f.releases++
	return nil
}
func (f *fenceLeaseRepo) ListByHost(context.Context, uuid.UUID) ([]domain.NetLease, error) {
	return nil, nil
}
func (f *fenceLeaseRepo) EnsureHostOrdinal(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}

// ── Snapshot ─────────────────────────────────────────────────────────

// TestSnapshotAppVolumes_RefusesForeignVolumeBeforeAnyProtectionWork.
//
// Two distinct claims, and the second is the subtle one: a routing refusal must
// NOT be recorded as a snapshot failure against the resource. Recording it would
// start a consecutive-failure streak, raise a protection condition and
// eventually report a healthy customer database as unprotected — describing the
// ledger's host stamps rather than the data.
func TestSnapshotAppVolumes_RefusesForeignVolumeBeforeAnyProtectionWork(t *testing.T) {
	tests := []struct {
		name string
		host *uuid.UUID
	}{
		{"volume pinned to the other fleet host", hostPtr(testForeignHost)},
		{"volume with no host stamp at all", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSnapshotHarness(t)
			appID, resID, _, _, _ := h.attachedVolume(t)
			h.vols.byApp[appID][0].HostAffinity = tt.host
			h.recovery.seed(&domain.FleetResource{
				ID: resID, OwnerOrg: uuid.New(), ClaimKey: "orders-db", Env: "prod",
				Class: "postgres", Tier: resourceTierDedicated, Phase: domain.FleetResourcePhaseReady,
			})

			_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
			if !errors.Is(err, domain.ErrVolumeHostMismatch) {
				t.Fatalf("SnapshotAppVolumes = %v, want ErrVolumeHostMismatch", err)
			}
			// No guest was quiesced, no byte was read, nothing reached the store.
			if evs := h.rec.all(); len(evs) != 0 {
				t.Fatalf("a routing refusal produced host/guest/store work: %v", evs)
			}
			res, rerr := h.recovery.GetResourceByHandle(context.Background(), resID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if res.ConsecutiveSnapshotFailures != 0 {
				t.Fatalf("consecutive_snapshot_failures = %d, want 0 — a routing refusal is not a failed protection attempt",
					res.ConsecutiveSnapshotFailures)
			}
			if res.LastSnapshotFailureAt != nil {
				t.Fatal("stamped a snapshot failure on the resource for a row this host does not own")
			}
			points, perr := h.recovery.ListRecoveryPoints(context.Background(), resID)
			if perr != nil {
				t.Fatal(perr)
			}
			if len(points) != 0 {
				t.Fatal("recorded a recovery point for a volume on another host")
			}
		})
	}
}

// ── Restore ──────────────────────────────────────────────────────────

// TestRestore_RefusesForeignVolumeAtAdmission. Admission is where it must be
// caught: past this point the resource is CAS'd into `restoring`, which refuses
// every boot until something releases it — so an admitted-then-refused restore
// would park a customer's database on a host that cannot finish the job.
func TestRestore_RefusesForeignVolumeAtAdmission(t *testing.T) {
	tests := []struct {
		name string
		host *uuid.UUID
	}{
		{"volume pinned to the other fleet host", hostPtr(testForeignHost)},
		{"volume with no host stamp at all", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			h.volumes.mu.Lock()
			h.volumes.byApp[*h.res.AppID][0].HostAffinity = tt.host
			h.volumes.mu.Unlock()

			_, err := h.uc.Restore(context.Background(), RestoreResourceInput{Resource: h.res, RecoveryPoint: h.rp})
			if !errors.Is(err, domain.ErrVolumeHostMismatch) {
				t.Fatalf("Restore = %v, want ErrVolumeHostMismatch", err)
			}
			if got := h.resource(t); got.Phase != domain.FleetResourcePhaseReady {
				t.Fatalf("phase = %q, want it untouched at ready (no CAS on a refused admission)", got.Phase)
			}
			if h.scaler.count() != 0 {
				t.Fatalf("scaled the app %d times on a refused admission", h.scaler.count())
			}
			if h.volumes.statusOf(*h.res.AppID) == domain.VolumeStatusRestoring {
				t.Fatal("raised the boot stand-off on a volume this host does not own")
			}
			if h.store.gets() != 0 {
				t.Fatalf("downloaded the recovery point (%d gets) for a volume on another host", h.store.gets())
			}
		})
	}
}

// TestRestore_ReReadsOwnershipBeforeTouchingBytes. Admission proved ownership at
// RPC time; the restore itself runs detached on the service base context and can
// start after the row changed. The re-read is what keeps the very first byte
// written from landing on a file this host no longer owns.
func TestRestore_ReReadsOwnershipBeforeTouchingBytes(t *testing.T) {
	h := newRestoreHarness(t)
	// Flip the affinity in the window between admission and the goroutine's
	// re-read: FindByID IS that re-read, so the hook makes the row change in the
	// instant before it is examined.
	h.volumes.beforeFindByID = func() {
		h.volumes.byApp[*h.res.AppID][0].HostAffinity = hostPtr(testForeignHost)
	}

	if _, err := h.run(t); err != nil {
		t.Fatalf("Restore admission: %v", err)
	}
	got := h.resource(t)
	if got.Phase == domain.FleetResourcePhaseRestoring {
		t.Fatal("the restore stayed in `restoring` — the abandon path did not run")
	}
	if got.LastError == "" {
		t.Fatal("no reason recorded for the abandoned restore")
	}
	if _, err := os.Stat(h.live + prerestoreSuffix); err == nil {
		t.Fatal("the live volume was parked — the abandon must happen BEFORE any file is moved")
	}
	if h.scaler.count() != 0 {
		t.Fatalf("drained the app (%d scale calls) after ownership was lost", h.scaler.count())
	}
}

// ─────────────────────────────────────────────────────────────────────
// Architect review of 6a3fe1c — the six agreed corrections.
// ─────────────────────────────────────────────────────────────────────

// TestActuateScheduledOwned_ConcurrentCallersBootExactlyOnce.
//
// The pass has TWO callers by design (the reconcile tick and the activator's
// wake path) and the transition underneath it is a read-then-write, not a
// compare-and-set: BootReplica does FindByID and then unconditionally persists
// `booting`. Without serialization both callers observe `scheduled` and both
// boot the SAME row — and the second boot re-derives the same jail slot, whose
// prepare() begins with os.RemoveAll(jailDir), so it deletes the live first VM's
// chroot and hands its uid and /30 to a second Firecracker.
func TestActuateScheduledOwned_ConcurrentCallersBootExactlyOnce(t *testing.T) {
	app := testFleetApp(0)
	h := newOrchHarness(t, oneLiveHost(), app)
	row := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
		State: domain.ReplicaStateScheduled, CreatedAt: time.Now().UTC(),
	}
	if err := h.replicas.Create(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = h.orch.actuateScheduledOwned(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("actuateScheduledOwned[%d]: %v", i, err)
		}
	}
	booter := h.booter()
	if n := booter.bootCount(); n != 1 {
		t.Fatalf("BootResident called %d times for ONE scheduled row, want exactly 1 — a second boot would RemoveAll the first VM's live jail", n)
	}
	got, _ := h.replicas.FindByID(context.Background(), row.ID)
	if got.State != domain.ReplicaStateResident {
		t.Fatalf("state = %q, want resident", got.State)
	}
}

// TestReconcileApp_PropagatesTheFirstLocalTeardownErrorAndHoldsPlacement.
//
// Two properties in one pass, and the second is the safety one: a retained
// `dead` row does not count as occupying, so a shortfall replacement would boot
// while the unproven VMM may still hold the app's single-writer volume.
func TestReconcileApp_PropagatesTheFirstLocalTeardownErrorAndHoldsPlacement(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{"an arbitrary teardown failure", errors.New("tap delete failed")},
		{"an unproven termination", domain.ErrVMTerminationUnproven},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := testFleetApp(1)
			h := newOrchHarness(t, oneLiveHost(), app)
			h.booter().decommErr = tt.cause
			dead := &domain.Replica{
				ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
				State: domain.ReplicaStateDead, RestartPolicy: domain.RestartPolicyAlways,
				CreatedAt: time.Now().UTC(),
			}
			pid := 4242
			dead.PID = &pid
			if err := h.replicas.Create(context.Background(), dead); err != nil {
				t.Fatal(err)
			}

			err := h.orch.ReconcileApp(context.Background(), app.ID)
			if !errors.Is(err, tt.cause) {
				t.Fatalf("ReconcileApp = %v, want errors.Is(_, %v) — the REAL cause, not a substitute", err, tt.cause)
			}
			if _, ferr := h.replicas.FindByID(context.Background(), dead.ID); ferr != nil {
				t.Fatalf("the undrained row was deleted: %v", ferr)
			}
			if n := h.replicas.count(); n != 1 {
				t.Fatalf("replicas = %d, want 1 — no REPLACEMENT may be placed while the previous VMM cannot be proven gone", n)
			}
			// And nothing was booted either, on this pass or by the actuation pass.
			h.actuate(t)
			if n := h.booter().bootCount(); n != 0 {
				t.Fatalf("booted %d replicas while a teardown was unproven, want 0", n)
			}
		})
	}
}

// TestDecommissionApp_PropagatesTheRealTeardownCause. Substituting
// ErrVMTerminationUnproven would overwrite the actual diagnosis — a TAP that
// would not delete, a lease the ledger refused — with a claim about the VMM this
// path never established.
func TestDecommissionApp_PropagatesTheRealTeardownCause(t *testing.T) {
	cause := errors.New("release microVM addressing lease: ledger unavailable")
	app := testFleetApp(1)
	h := newOrchHarness(t, oneLiveHost(), app)
	h.booter().decommErr = cause
	vols := newVolRepoFake(volWithBacking(app.ID, "/vol/data.ext4"))
	h.orch.SetVolumeManager(newTestVolumeManager(t, vols, &recordingBackend{}, "/vol", nil))

	pid := 4242
	local := &domain.Replica{
		ID: uuid.New(), AppID: app.ID, HostID: hostPtr(testSelfHost),
		State: domain.ReplicaStateResident, PID: &pid, CreatedAt: time.Now().UTC(),
	}
	if err := h.replicas.Create(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	origAlive := processAlive
	processAlive = func(int) bool { return false } // health marks it dead → teardown → fails
	defer func() { processAlive = origAlive }()

	known, err := h.orch.DecommissionApp(context.Background(), app.ID)
	if !known {
		t.Fatal("known = false, want true")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("DecommissionApp = %v, want errors.Is(_, %v)", err, cause)
	}
	if errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatal("the real cause was replaced by ErrVMTerminationUnproven")
	}
	if vols.count() != 1 || !h.apps.has(app.ID) {
		t.Fatal("volumes or the app row were reclaimed while a replica survived")
	}
}

// TestDecommissionReplica_NetCoordinateOnlyRowStillReachesTheBooter. The lease is
// taken BEFORE the TAP and the VM exist, so a boot that died in that window
// records a net index and nothing else. Skipping the booter for such a row
// deleted it and orphaned the live lease — a /30, a uid and a jail slot nothing
// would ever release.
func TestDecommissionReplica_NetCoordinateOnlyRowStillReachesTheBooter(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	rep.NetIndex = 7 // the ONLY artifact: no pid, no socket, no tap
	replicas := newRTReplicaRepo()
	if err := replicas.Create(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	booter := &recordingBooter{decommErr: domain.ErrVMTerminationUnproven}
	uc := newTestReplicaRuntime(t, fakeMaterializer{}, booter, replicas, &rtAppRepo{app: app}, t.TempDir(), "10.0.0.9")

	err := uc.DecommissionReplica(context.Background(), rep.ID)
	if booter.decommN != 1 {
		t.Fatalf("booter Decommission called %d times, want 1 — a net coordinate is a live lease", booter.decommN)
	}
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("DecommissionReplica = %v, want the booter's verdict", err)
	}
	if _, ferr := replicas.FindByID(context.Background(), rep.ID); ferr != nil {
		t.Fatalf("the row was deleted while its lease was unreleased: %v", ferr)
	}
}

// TestBootReplica_UnprovenRollbackRetainsTheStagingTree. When the row cannot be
// persisted AND the VM cannot be torn down, the staging tree is the last on-disk
// evidence of what that pid is running. Reclaiming it anyway is precisely the
// shape that produced the live orphans.
func TestBootReplica_UnprovenRollbackRetainsTheStagingTree(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	if err := replicas.Create(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("ledger unavailable")
	replicas.updateErrAfter = 2 // let `booting` through, fail the `resident` write

	workDir := t.TempDir()
	staging := filepath.Join(workDir, rep.ID.String())
	if err := mkdirAllForTest(staging); err != nil {
		t.Fatal(err)
	}
	replicas.updateErr = persistErr
	booter := &recordingBooter{
		resident:  ImageResidentResult{PID: 4242, GuestIP: "10.201.0.6"},
		decommErr: domain.ErrVMTerminationUnproven,
	}
	uc := newTestReplicaRuntime(t, fakeMaterializer{rootfs: "/work/r.ext4"}, booter, replicas,
		&rtAppRepo{app: app}, workDir, "10.0.0.9")

	err := uc.BootReplica(context.Background(), rep.ID)
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("BootReplica = %v, want the teardown's unproven verdict to propagate", err)
	}
	if !dirExistsForTest(staging) {
		t.Fatal("the staging tree was reclaimed although the VM was never proven gone")
	}
}

// ── FleetProvision: the same proof-discard bypasses ───────────────────
//
// The workload seam had the identical shape as the replica seam: a teardown
// whose verdict was logged and dropped, followed by a terminal state write. The
// state is what makes it unrecoverable — `exited`/`failed` are terminal, and the
// teardown branch only fires from `running`, so a later retry walks straight
// past a VMM that was never proven dead and a lease that was never released.

// provisionBooter records teardown calls and can script their outcome.
type provisionBooter struct {
	mu        sync.Mutex
	resident  ImageResidentResult
	resErr    error
	decommErr error
	decommN   int
	lastPID   int
}

func (b *provisionBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, nil
}
func (b *provisionBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return b.resident, b.resErr
}
func (b *provisionBooter) Decommission(_ context.Context, in ImageDecommissionInput) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decommN++
	b.lastPID = in.PID
	return b.decommErr
}

func seedRunningResident(t *testing.T, repo *fakeWorkloadRepo) *domain.ImageWorkload {
	t.Helper()
	pid := 4242
	now := time.Now().UTC()
	wl := &domain.ImageWorkload{
		ID: uuid.New(), Class: domain.ImageWorkloadClassResident,
		State: domain.ImageWorkloadStateRunning, PID: &pid,
		GuestIP: "10.201.0.6", Port: 8080, NetIndex: 7, TapName: "img7",
		SocketPath: "/run/x.sock", CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(context.Background(), wl); err != nil {
		t.Fatal(err)
	}
	return wl
}

// (6b) Decommission must not swallow a teardown error into marked-Exited.
func TestFleetProvisionDecommission_KeepsTheRetryPathOnAnUnprovenTeardown(t *testing.T) {
	repo := newFakeWorkloadRepo()
	wl := seedRunningResident(t, repo)
	booter := &provisionBooter{decommErr: domain.ErrVMTerminationUnproven}
	uc := newUC(repo, fakeMaterializer{}, booter)

	err := uc.Decommission(context.Background(), wl.ID.String())
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("Decommission = %v, want the booter's unproven verdict", err)
	}
	got, ferr := repo.FindByID(context.Background(), wl.ID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if got.State != domain.ImageWorkloadStateRunning {
		t.Fatalf("state = %q, want it left at %q — `exited` is terminal and the retry would then skip teardown entirely",
			got.State, domain.ImageWorkloadStateRunning)
	}
	// And the retry does reach the booter again.
	booter.decommErr = nil
	if rerr := uc.Decommission(context.Background(), wl.ID.String()); rerr != nil {
		t.Fatalf("retry: %v", rerr)
	}
	if booter.decommN != 2 {
		t.Fatalf("booter Decommission called %d times, want 2 (the retry must re-attempt it)", booter.decommN)
	}
	got, _ = repo.FindByID(context.Background(), wl.ID)
	if got.State != domain.ImageWorkloadStateExited {
		t.Fatalf("state after a PROVEN teardown = %q, want exited", got.State)
	}
}

// (6c) Health's dead-resident path must keep the recorded PID and must not
// commit the terminal state when teardown failed.
func TestFleetProvisionHealth_DeadResidentKeepsItsPIDAndItsRetry(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return false } // the VM process is gone
	defer func() { processAlive = origAlive }()

	t.Run("the recorded pid is handed to the teardown as its proof", func(t *testing.T) {
		repo := newFakeWorkloadRepo()
		wl := seedRunningResident(t, repo)
		booter := &provisionBooter{}
		uc := newUC(repo, fakeMaterializer{}, booter)

		if _, err := uc.Health(context.Background(), wl.ID.String()); err != nil {
			t.Fatalf("Health: %v", err)
		}
		if booter.lastPID != *wl.PID {
			t.Fatalf("teardown was given pid %d, want the recorded %d — a zero pid with live artifacts is UNPROVABLE and refuses, so clearing it turns a provable teardown into a refused one",
				booter.lastPID, *wl.PID)
		}
	})

	t.Run("a failed teardown leaves the row running so a later health retries it", func(t *testing.T) {
		repo := newFakeWorkloadRepo()
		wl := seedRunningResident(t, repo)
		booter := &provisionBooter{decommErr: domain.ErrVMTerminationUnproven}
		uc := newUC(repo, fakeMaterializer{}, booter)

		if _, err := uc.Health(context.Background(), wl.ID.String()); err != nil {
			t.Fatalf("Health: %v", err)
		}
		got, _ := repo.FindByID(context.Background(), wl.ID)
		if got.State != domain.ImageWorkloadStateRunning {
			t.Fatalf("state = %q, want it left at %q — `failed` is terminal for this branch and the retry would never tear down again",
				got.State, domain.ImageWorkloadStateRunning)
		}
		if got.PID == nil {
			t.Fatal("the recorded pid was cleared, so no later teardown can prove the process gone")
		}
		// The later Health retries and, once it succeeds, commits the terminal state.
		booter.decommErr = nil
		if _, err := uc.Health(context.Background(), wl.ID.String()); err != nil {
			t.Fatalf("Health retry: %v", err)
		}
		if booter.decommN != 2 {
			t.Fatalf("booter Decommission called %d times, want 2", booter.decommN)
		}
		got, _ = repo.FindByID(context.Background(), wl.ID)
		if got.State != domain.ImageWorkloadStateFailed {
			t.Fatalf("state after a PROVEN teardown = %q, want failed", got.State)
		}
	})
}

// (6d) runResident's persist rollback: the VM is up, the row could not be
// written, and the teardown could not prove the VM gone. The proof-bearing error
// must reach the caller — a bare "persist failed" reads as a database problem
// while a live untracked VMM is holding a lease, a TAP and a jail slot.
func TestFleetProvisionRunResident_PersistRollbackPropagatesAnUnprovenTeardown(t *testing.T) {
	repo := newFakeWorkloadRepo()
	persistErr := errors.New("ledger unavailable")
	// Let the early writes through (create + the rootfs stamp) and fail the write
	// that persists the RUNNING resident.
	repo.updateErr = persistErr
	repo.updateErrAfter = 2
	booter := &provisionBooter{
		resident:  ImageResidentResult{PID: 4242, GuestIP: "10.201.0.6", HostPort: 21000, NetIndex: 7, TapName: "img7", SocketPath: "/run/x.sock"},
		decommErr: domain.ErrVMTerminationUnproven,
	}
	uc := newUC(repo, fakeMaterializer{rootfs: "/work/r.ext4"}, booter)

	_, err := uc.Provision(context.Background(), FleetProvisionInput{
		Registry: "reg:8089", Repository: "org/app", Digest: "sha256:abc",
		WorkloadClass: "resident", Port: 8080,
	})
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("Provision = %v, want the teardown's unproven verdict to propagate", err)
	}
	if booter.decommN != 1 {
		t.Fatalf("booter Decommission called %d times, want 1", booter.decommN)
	}
}
