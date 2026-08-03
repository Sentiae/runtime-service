package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeCapture records what the cadence worker asked to be captured, and can fail
// a chosen subset so the failure-isolation rule is drivable.
type fakeCapture struct {
	calls []capturedPair
	// failFirst makes the first N captures fail.
	failFirst int
	err       error
}

type capturedPair struct{ resourceID, appID uuid.UUID }

func (f *fakeCapture) Capture(_ context.Context, resourceID, appID uuid.UUID) error {
	f.calls = append(f.calls, capturedPair{resourceID, appID})
	if f.failFirst > 0 {
		f.failFirst--
		if f.err != nil {
			return f.err
		}
		return errors.New("fake: capture failed")
	}
	return nil
}

func dueResource() domain.FleetResource {
	app := uuid.New()
	cadence := 3600
	return domain.FleetResource{
		ID: uuid.New(), AppID: &app,
		Class: resourceClassPostgres, Tier: resourceTierDedicated,
		Durability: domain.DurabilityDurable, ProtectionCadenceSeconds: &cadence,
		Phase: domain.FleetResourcePhaseReady,
	}
}

// The pin is not optional. A worker that cannot name its own host would have to
// beat a scope it invented — and a `cadence` beat under the wrong scope tells the
// accept gate that a host it is not running on takes scheduled snapshots.
func TestCadenceWorker_ConstructionRefusesAnUnpinnedWorker(t *testing.T) {
	repo := newFakeResourceRepo()
	capture := &fakeCapture{}

	if _, err := NewFleetProtectionCadenceWorker(repo, capture, uuid.Nil); err == nil {
		t.Fatal("a worker with no fleet host identity must not be constructible")
	}
	if _, err := NewFleetProtectionCadenceWorker(nil, capture, uuid.New()); err == nil {
		t.Fatal("a worker with no ledger must not be constructible")
	}
	if _, err := NewFleetProtectionCadenceWorker(repo, nil, uuid.New()); err == nil {
		t.Fatal("a worker with no capture port must not be constructible — it would beat while capturing nothing")
	}
	if _, err := NewVolumeSnapshotCapture(nil); err == nil {
		t.Fatal("a capture adapter over a nil snapshotter must not be constructible")
	}
}

// The work list is asked for by HOST, and every due resource is captured.
func TestCadenceWorker_CapturesDueResourcesOfItsOwnHost(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.due = []domain.FleetResource{dueResource(), dueResource()}
	capture := &fakeCapture{}
	self := uuid.New()

	w, err := NewFleetProtectionCadenceWorker(repo, capture, self)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if repo.lastDueHost != self {
		t.Fatalf("the work list was asked for host %s, want THIS host %s", repo.lastDueHost, self)
	}
	if repo.lastDueLimit != protectionCadenceBatch {
		t.Fatalf("batch = %d, want %d — each capture freezes a customer guest", repo.lastDueLimit, protectionCadenceBatch)
	}
	if len(capture.calls) != 2 {
		t.Fatalf("captured %d resources, want 2", len(capture.calls))
	}
	for i, want := range repo.due {
		if capture.calls[i].resourceID != want.ID || capture.calls[i].appID != *want.AppID {
			t.Fatalf("capture %d = %+v, want (%s, %s)", i, capture.calls[i], want.ID, *want.AppID)
		}
	}
}

// ⚠ THE PASS IS THE FACT. An idle fleet still beats: a heartbeat written only
// when work was found would go stale on a healthy, fully-snapshotted fleet and
// refuse every new durable provision.
func TestCadenceWorker_BeatsOnAnEmptyPass(t *testing.T) {
	repo := newFakeResourceRepo()
	capture := &fakeCapture{}
	self := uuid.New()

	w, _ := NewFleetProtectionCadenceWorker(repo, capture, self)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(repo.upserted) != 1 {
		t.Fatalf("an empty pass wrote %d heartbeats, want 1", len(repo.upserted))
	}
	beat := repo.upserted[0]
	if beat.Component != domain.ProtectionComponentCadence || beat.Scope != self.String() {
		t.Fatalf("beat = %s/%s, want cadence scoped to THIS host %s", beat.Component, beat.Scope, self)
	}
}

// Start's first beat is SYNCHRONOUS: the accept gate must see a live fact the
// moment this process serves, not one tick later.
func TestCadenceWorker_StartBeatsSynchronously(t *testing.T) {
	repo := newFakeResourceRepo()
	self := uuid.New()
	w, _ := NewFleetProtectionCadenceWorker(repo, &fakeCapture{}, self)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Read the fact the way the accept gate does — through the ledger, immediately.
	beat, err := repo.GetProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, self.String())
	if err != nil {
		t.Fatalf("no heartbeat exists the instant Start returned: %v", err)
	}
	if time.Since(beat.BeatenAt) > time.Minute {
		t.Fatalf("the first beat is %s old — it was not written by Start", time.Since(beat.BeatenAt))
	}
}

// One resource's failure never aborts the pass: the others are due too, and a
// single unsnapshottable database must not stop a whole host's protection.
func TestCadenceWorker_OneFailureDoesNotAbortThePass(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.due = []domain.FleetResource{dueResource(), dueResource(), dueResource()}
	capture := &fakeCapture{failFirst: 1}

	w, _ := NewFleetProtectionCadenceWorker(repo, capture, uuid.New())
	err := w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("the pass must REPORT the failure it isolated, never swallow it")
	}
	if len(capture.calls) != 3 {
		t.Fatalf("captured %d resources after one failure, want all 3 attempted", len(capture.calls))
	}
}

// The heartbeat only ADVERTISES protection; the pass IS protection. A failed beat
// must never cost a recovery point — and it fails safe anyway (accepts refuse).
func TestCadenceWorker_HeartbeatFailureDoesNotAbortThePass(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.due = []domain.FleetResource{dueResource()}
	repo.upsertErr = errors.New("control-plane db unreachable")
	capture := &fakeCapture{}

	w, _ := NewFleetProtectionCadenceWorker(repo, capture, uuid.New())
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("a failed heartbeat must not fail the pass: %v", err)
	}
	if len(capture.calls) != 1 {
		t.Fatalf("the backlog must still be captured (%d captures)", len(capture.calls))
	}
}

// An unreadable work list is a pass that proved nothing, and it says so.
func TestCadenceWorker_UnreadableWorkListFailsThePass(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.dueErr = errors.New("control-plane db unreachable")
	capture := &fakeCapture{}

	w, _ := NewFleetProtectionCadenceWorker(repo, capture, uuid.New())
	if err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("a pass that could not see its work list must report the failure")
	}
	if len(capture.calls) != 0 {
		t.Fatalf("nothing may be captured from an unreadable work list (%d captures)", len(capture.calls))
	}
	// It still beat: the worker IS alive and looking, which is what the fact means.
	if len(repo.upserted) != 1 {
		t.Fatalf("the pass beat %d times, want 1", len(repo.upserted))
	}
}

// The D-202 adapter drives the EXISTING snapshotter and discards its recovery
// points — the snapshotter already owns the streak columns and the location
// stamp, and a second interpretation here could only disagree with the first.
func TestVolumeSnapshotCapture_DrivesTheSnapshotter(t *testing.T) {
	snap := &fakeSnapshotter{}
	capture, err := NewVolumeSnapshotCapture(snap)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resourceID, appID := uuid.New(), uuid.New()
	if err := capture.Capture(context.Background(), resourceID, appID); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if snap.calls != 1 || snap.resourceID != resourceID || snap.appID != appID {
		t.Fatalf("snapshotter got calls=%d resource=%s app=%s", snap.calls, snap.resourceID, snap.appID)
	}

	snap.err = errors.New("guest could not be quiesced")
	if err := capture.Capture(context.Background(), resourceID, appID); err == nil {
		t.Fatal("a failed snapshot must reach the worker as a failed capture")
	}
}
