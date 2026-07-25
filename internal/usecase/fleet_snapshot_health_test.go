package usecase

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// A failed recovery point must be LOUD and must degrade the resource. A snapshot
// failure over a durable resource means its protection has stopped, and until it
// was recorded a resource failing for a week read like one snapshotted an hour
// ago.
// ─────────────────────────────────────────────────────────────────────

// recordSink captures emitted records so a test can assert the LEVEL and the
// attributes an operator (or an alert) would actually key on.
type recordSink struct {
	mu      sync.Mutex
	records []sunkRecord
}

type sunkRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (s *recordSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *recordSink) Handle(_ context.Context, r slog.Record) error {
	rec := sunkRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return nil
}

func (s *recordSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *recordSink) WithGroup(string) slog.Handler      { return s }

// atLevel returns the captured records at exactly one level.
func (s *recordSink) atLevel(level slog.Level) []sunkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sunkRecord
	for _, r := range s.records {
		if r.level == level {
			out = append(out, r)
		}
	}
	return out
}

// seedDurableResource records the claim behind the harness's snapshot resource id
// so the snapshotter can resolve (and mark) the resource it protects.
func (h *snapHarness) seedDurableResource(resID uuid.UUID, tier string, res *domain.FleetResource) *domain.FleetResource {
	if res == nil {
		res = &domain.FleetResource{}
	}
	res.ID = resID
	res.Tier = tier
	if res.OwnerOrg == uuid.Nil {
		res.OwnerOrg = uuid.New()
	}
	if res.ClaimKey == "" {
		res.ClaimKey = "customer-primary-db"
	}
	if res.Env == "" {
		res.Env = "prod"
	}
	if res.Phase == "" {
		res.Phase = domain.FleetResourcePhaseReady
	}
	h.recovery.seed(res)
	return res
}

// reload reads the recorded claim back.
func (h *snapHarness) reload(t *testing.T, resID uuid.UUID) *domain.FleetResource {
	t.Helper()
	res, err := h.recovery.GetResourceByHandle(context.Background(), resID)
	if err != nil {
		t.Fatalf("reload resource: %v", err)
	}
	return res
}

// A failed snapshot of a durable resource must produce an ERROR-level line
// carrying the resource id, the claim key, the owning org and the underlying
// cause, and must land on the row as a failure — the log alone is not a state,
// and the state alone is not a signal.
func TestSnapshotFailure_IsLoudAndRecorded(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	res := h.seedDurableResource(resID, resourceTierDedicated, nil)
	// A guest that cannot be frozen refuses the snapshot (ErrSnapshotNotQuiescible).
	h.guest.freezeErr = errors.New("vsock: connection refused")

	sink := &recordSink{}
	ctx := logger.NewContext(context.Background(), slog.New(sink))

	points, err := h.s.SnapshotAppVolumes(ctx, resID, appID)
	// The contract the caller sees is unchanged.
	if err == nil {
		t.Fatal("snapshot must still fail")
	}
	if !errors.Is(err, domain.ErrSnapshotNotQuiescible) {
		t.Errorf("err = %v, want it to still wrap ErrSnapshotNotQuiescible", err)
	}
	if len(points) != 0 {
		t.Errorf("points = %+v, want none", points)
	}

	// Recorded on the resource.
	got := h.reload(t, resID)
	if got.ConsecutiveSnapshotFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", got.ConsecutiveSnapshotFailures)
	}
	if got.LastSnapshotFailureAt == nil {
		t.Error("last snapshot failure time not stamped")
	}
	if got.LastSnapshotError == "" {
		t.Error("last snapshot error not recorded")
	}
	if got.LastSnapshotSuccessAt != nil {
		t.Error("a failure must not stamp a success")
	}
	// The lifecycle fields are NOT touched: a failed recovery point is a protection
	// fact, not a lifecycle transition.
	if got.Phase != res.Phase {
		t.Errorf("phase = %q, want it untouched (%q)", got.Phase, res.Phase)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want it left to the lifecycle paths", got.LastError)
	}

	// Loud, at Error, with what an operator needs to act.
	errRecords := h.sinkFailureRecords(sink)
	if len(errRecords) != 1 {
		t.Fatalf("error-level snapshot-failure lines = %d, want 1: %+v", len(errRecords), sink.atLevel(slog.LevelError))
	}
	rec := errRecords[0]
	for key, want := range map[string]string{
		"resource_id": resID.String(),
		"claim_key":   res.ClaimKey,
		"owner_org":   res.OwnerOrg.String(),
	} {
		if rec.attrs[key] != want {
			t.Errorf("log attr %s = %q, want %q", key, rec.attrs[key], want)
		}
	}
	if !strings.Contains(rec.attrs["err"], "connection refused") {
		t.Errorf("log attr err = %q, want the underlying cause", rec.attrs["err"])
	}
	if rec.attrs["consecutive_failures"] != "1" {
		t.Errorf("log attr consecutive_failures = %q, want 1", rec.attrs["consecutive_failures"])
	}
	if rec.attrs["last_snapshot_success_at"] != "never" {
		t.Errorf("log attr last_snapshot_success_at = %q, want never", rec.attrs["last_snapshot_success_at"])
	}
}

// sinkFailureRecords picks the snapshot-failure lines out of a sink: the thaw and
// heartbeat paths log at Error too, so counting every Error record would make this
// assertion pass for the wrong reason.
func (h *snapHarness) sinkFailureRecords(sink *recordSink) []sunkRecord {
	var out []sunkRecord
	for _, r := range sink.atLevel(slog.LevelError) {
		if strings.Contains(r.msg, "protection has stopped") {
			out = append(out, r)
		}
	}
	return out
}

// Consecutive failures must ACCUMULATE: one blip and a week of failures have to be
// distinguishable, and a boolean cannot tell them apart.
func TestSnapshotFailure_ConsecutiveFailuresAccumulate(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.seedDurableResource(resID, resourceTierDedicated, nil)
	h.guest.freezeErr = errors.New("vsock: connection refused")

	for i := 1; i <= 3; i++ {
		if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
			t.Fatalf("attempt %d: snapshot must fail", i)
		}
		if got := h.reload(t, resID).ConsecutiveSnapshotFailures; got != i {
			t.Fatalf("after %d failures the count is %d", i, got)
		}
	}
}

// A snapshot that CAPTURES a recovery point clears the condition and resets the
// streak — and stamps the last-success timestamp an alert measures the age of.
func TestSnapshotSuccess_ClearsTheStreak(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.seedDurableResource(resID, resourceTierDedicated, &domain.FleetResource{
		ConsecutiveSnapshotFailures: 4,
		LastSnapshotError:           "vsock: connection refused",
	})

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	got := h.reload(t, resID)
	if got.ConsecutiveSnapshotFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", got.ConsecutiveSnapshotFailures)
	}
	if got.LastSnapshotError != "" {
		t.Errorf("last snapshot error = %q, want cleared", got.LastSnapshotError)
	}
	if got.LastSnapshotSuccessAt == nil {
		t.Error("last snapshot success not stamped")
	}
}

// An app with NO volumes returns ([], nil): the call worked and captured nothing.
// That must NOT clear a failure streak — no recovery point was produced, so
// nothing about the resource's protection resumed.
func TestSnapshot_ZeroVolumesLeavesTheStreakAlone(t *testing.T) {
	h := newSnapshotHarness(t)
	appID := uuid.New()
	resID := uuid.New()
	h.vols.byApp[appID] = nil
	h.seedDurableResource(resID, resourceTierDedicated, &domain.FleetResource{
		ConsecutiveSnapshotFailures: 2,
		LastSnapshotError:           "vsock: connection refused",
	})

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil || len(points) != 0 {
		t.Fatalf("points=%v err=%v, want ([], nil)", points, err)
	}
	got := h.reload(t, resID)
	if got.ConsecutiveSnapshotFailures != 2 {
		t.Errorf("consecutive failures = %d, want the streak untouched (2)", got.ConsecutiveSnapshotFailures)
	}
	if got.LastSnapshotSuccessAt != nil {
		t.Error("a call that captured nothing must not stamp a success")
	}
}

// The shared tier holds no volumes of its own, so a failure there is not this
// path's protection to mark.
func TestSnapshotFailure_SharedTierIsNotMarked(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.seedDurableResource(resID, resourceTierShared, nil)
	h.guest.freezeErr = errors.New("vsock: connection refused")

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("snapshot must fail")
	}
	if got := h.reload(t, resID).ConsecutiveSnapshotFailures; got != 0 {
		t.Errorf("consecutive failures = %d, want 0 for a shared resource", got)
	}
}

// A failure on a resource whose claim cannot be loaded is still loud: an
// on-demand snapshot of a handle that no longer resolves must not go silent.
func TestSnapshotFailure_UnloadableResourceIsStillLoud(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t) // resID is never seeded
	h.guest.freezeErr = errors.New("vsock: connection refused")

	sink := &recordSink{}
	ctx := logger.NewContext(context.Background(), slog.New(sink))
	if _, err := h.s.SnapshotAppVolumes(ctx, resID, appID); err == nil {
		t.Fatal("snapshot must fail")
	}
	found := false
	for _, r := range sink.atLevel(slog.LevelError) {
		if strings.Contains(r.msg, "could not be loaded") && r.attrs["resource_id"] == resID.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("no error-level line for a failure whose resource could not be loaded: %+v", sink.atLevel(slog.LevelError))
	}
}

// A recording that itself fails must not change the snapshot's verdict, and must
// say so — a status that still reads as protected is the dangerous outcome.
func TestSnapshotFailure_RecordingFailureIsLoudAndSwallowed(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.seedDurableResource(resID, resourceTierDedicated, nil)
	h.guest.freezeErr = errors.New("vsock: connection refused")
	h.recovery.snapshotHealthErr = errors.New("db: connection reset")

	sink := &recordSink{}
	ctx := logger.NewContext(context.Background(), slog.New(sink))
	_, err := h.s.SnapshotAppVolumes(ctx, resID, appID)
	if !errors.Is(err, domain.ErrSnapshotNotQuiescible) {
		t.Fatalf("err = %v, want the snapshot's own failure unchanged", err)
	}
	found := false
	for _, r := range sink.atLevel(slog.LevelError) {
		if strings.Contains(r.msg, "could not be recorded") {
			found = true
		}
	}
	if !found {
		t.Errorf("a failed recording must be loud: %+v", sink.atLevel(slog.LevelError))
	}
}

// ─────────────────────────────────────────────────────────────────────
// The status path: a failing streak is a CONDITION on the resource.
// ─────────────────────────────────────────────────────────────────────

func TestStatusOf_SnapshotFailingCondition(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		phase    domain.FleetResourcePhase
		want     bool
	}{
		{name: "no failures reports nothing", failures: 0, phase: domain.FleetResourcePhaseReady, want: false},
		{name: "one failure reports the condition", failures: 1, phase: domain.FleetResourcePhaseReady, want: true},
		{name: "a streak reports the condition", failures: 9, phase: domain.FleetResourcePhaseReady, want: true},
		{
			// A torn-down resource's streak is history, not a live condition.
			name:     "a tombstone reports nothing",
			failures: 9,
			phase:    domain.FleetResourcePhaseDecommissioned,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
			appID := uuid.New()
			replicas := newFakeResourceReplicaRepo()
			replicas.byApp[appID] = []domain.Replica{{
				ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
				GuestIP: "10.0.0.9", Port: residentPGPort,
			}}
			uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())
			uc.pgReady = func(context.Context, string, int) error { return nil }

			rid := uuid.New()
			repo.seed(&domain.FleetResource{
				ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
				Tier: resourceTierDedicated, Phase: tt.phase, AppID: &appID,
				ConsecutiveSnapshotFailures: tt.failures,
			})

			st, err := uc.StatusOf(context.Background(), rid)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if got := hasCondition(st.Conditions, conditionSnapshotFailing); got != tt.want {
				t.Errorf("conditions = %v, want %s present=%v", st.Conditions, conditionSnapshotFailing, tt.want)
			}
			// The operator-facing cause never reaches the tenant-visible status.
			for _, c := range st.Conditions {
				if strings.Contains(c, "connection refused") {
					t.Errorf("condition %q leaks the raw cause", c)
				}
			}
		})
	}
}

// A healthy engine whose snapshots are failing must keep its phase: callers gate
// on phase, and this change is about visibility, never about blocking.
func TestStatusOf_SnapshotFailingDoesNotBlockReady(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
	appID := uuid.New()
	replicas := newFakeResourceReplicaRepo()
	replicas.byApp[appID] = []domain.Replica{{
		ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
		GuestIP: "10.0.0.9", Port: residentPGPort,
	}}
	uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())
	uc.pgReady = func(context.Context, string, int) error { return nil }

	rid := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
		Tier: resourceTierDedicated, Phase: domain.FleetResourcePhaseProvisioning, AppID: &appID,
		ConsecutiveSnapshotFailures: 3,
	})

	st, err := uc.StatusOf(context.Background(), rid)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != string(domain.FleetResourcePhaseReady) {
		t.Errorf("phase = %q, want ready — a failing snapshot must not block a healthy engine", st.Phase)
	}
	if !hasCondition(st.Conditions, conditionSnapshotFailing) {
		t.Errorf("conditions = %v, want %s", st.Conditions, conditionSnapshotFailing)
	}
}

func hasCondition(conditions []string, want string) bool {
	for _, c := range conditions {
		if c == want {
			return true
		}
	}
	return false
}
