package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// The control-plane recovery-point mirror (D-200).
//
// ⚠ WHAT THESE TESTS DEFEND. The dangerous failure was never "the mirror broke" —
// it is "the mirror broke and the ledger still reads as protected", which converts
// an alarming state into a reassuring one. So every case below asserts what the
// LEDGER says after the pass, not what the call returned.
// ─────────────────────────────────────────────────────────────────────

// fakeMirror is a SecondDomainMirror whose outcome the test dictates.
type fakeMirror struct {
	mu   sync.Mutex
	err  error
	at   time.Time
	keys []string
}

func (f *fakeMirror) Domain() string { return "r2:test-bucket" }

func (f *fakeMirror) Mirror(_ context.Context, objectKey, expectChecksum string) (SecondDomainReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, objectKey)
	if f.err != nil {
		return SecondDomainReceipt{}, f.err
	}
	return SecondDomainReceipt{Domain: f.Domain(), Bytes: 7, Checksum: expectChecksum, At: f.at}, nil
}

func (f *fakeMirror) mirrored() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// mirrorHarness bundles a worker over a fake ledger and a fake second domain.
type mirrorHarness struct {
	w      *RecoveryPointMirrorWorker
	ledger *fakeResourceRepo
	mirror *fakeMirror
	resID  uuid.UUID
	// clock is what the worker reads for cooldowns and stamps; tests advance it
	// instead of sleeping.
	clock time.Time
}

// mirrorAt is the fixed confirmation time every seeded receipt carries.
var mirrorAt = time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)

func newMirrorHarness(t *testing.T) *mirrorHarness {
	t.Helper()
	h := &mirrorHarness{
		ledger: newFakeResourceRepo(),
		mirror: &fakeMirror{at: mirrorAt},
		resID:  uuid.New(),
		clock:  mirrorAt,
	}
	w, err := NewRecoveryPointMirrorWorker(h.ledger, h.mirror)
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	w.now = func() time.Time { return h.clock }
	h.w = w
	return h
}

// seed puts one recovery point in the ledger.
func (h *mirrorHarness) seedPoint(key string, locations domain.RecoveryPointLocations, createdAt time.Time) uuid.UUID {
	id := uuid.New()
	h.ledger.recovery[h.resID] = append(h.ledger.recovery[h.resID], domain.FleetResourceRecoveryPoint{
		ID:         id,
		ResourceID: h.resID,
		ObjectKey:  key,
		Kind:       "snapshot",
		Checksum:   "abc123",
		Locations:  locations,
		CreatedAt:  createdAt,
	})
	return id
}

// point reads one recovery point back OUT OF THE LEDGER — the ledger is what every
// durability number is computed from, so it is the only acceptable oracle here.
func (h *mirrorHarness) point(t *testing.T, id uuid.UUID) domain.FleetResourceRecoveryPoint {
	t.Helper()
	rps, err := h.ledger.ListRecoveryPoints(context.Background(), h.resID)
	if err != nil {
		t.Fatalf("list recovery points: %v", err)
	}
	for _, rp := range rps {
		if rp.ID == id {
			return rp
		}
	}
	t.Fatalf("recovery point %s not in the ledger", id)
	return domain.FleetResourceRecoveryPoint{}
}

func TestRecoveryPointMirrorWorker_LedgerOutcome(t *testing.T) {
	tests := []struct {
		name string
		// copyErr fails the copy into the second domain.
		copyErr error
		// ledgerBroken fails the ledger writes that record the outcome.
		ledgerBroken bool

		wantLocations   domain.RecoveryPointLocations
		wantStore       string
		wantStamped     bool
		wantErrRecorded bool
		wantPassErr     bool
		wantMirrored    int
		wantFailed      int
	}{
		{
			name:          "a confirmed second copy earns the two-domain claim",
			wantLocations: domain.RecoveryPointLocationsSecondDomain,
			wantStore:     "r2:test-bucket",
			wantStamped:   true,
			wantMirrored:  1,
		},
		{
			name:    "a FAILED copy leaves the row saying ONE domain, with the cause on it",
			copyErr: errors.New("dial r2: connection refused"),
			// The one line this whole component exists to forbid is the opposite of this
			// assertion: a one-domain copy reading as a two-domain one.
			wantLocations:   domain.RecoveryPointLocationsPrimaryOnly,
			wantErrRecorded: true,
			wantPassErr:     true,
			wantFailed:      1,
		},
		{
			name:         "a copy that landed but could not be recorded keeps reporting as one domain",
			ledgerBroken: true,
			// Understating durability is the safe direction of this failure: the ledger
			// must never claim a protection it did not write down.
			wantLocations: domain.RecoveryPointLocationsPrimaryOnly,
			wantPassErr:   true,
			wantFailed:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMirrorHarness(t)
			id := h.seedPoint("volumes/v1/snap.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-time.Hour))
			h.mirror.err = tt.copyErr
			if tt.ledgerBroken {
				h.ledger.mirrorLedgerErr = errors.New("control plane unreachable")
			}

			rep, err := h.w.RunOnce(context.Background())
			if tt.wantPassErr && err == nil {
				t.Fatal("the pass reported success while a recovery point went uncopied — a pass that silently succeeds is what makes a stalled mirror invisible")
			}
			if !tt.wantPassErr && err != nil {
				t.Fatalf("pass: %v", err)
			}
			if rep.Considered != 1 {
				t.Errorf("considered = %d, want 1", rep.Considered)
			}
			if rep.Mirrored != tt.wantMirrored {
				t.Errorf("mirrored = %d, want %d", rep.Mirrored, tt.wantMirrored)
			}
			if rep.Failed != tt.wantFailed {
				t.Errorf("failed = %d, want %d", rep.Failed, tt.wantFailed)
			}

			rp := h.point(t, id)
			if rp.Locations != tt.wantLocations {
				t.Errorf("ledger locations = %q, want %q", rp.Locations, tt.wantLocations)
			}
			if rp.SecondDomainStore != tt.wantStore {
				t.Errorf("second_domain_store = %q, want %q", rp.SecondDomainStore, tt.wantStore)
			}
			if tt.wantStamped {
				if rp.SecondDomainAt == nil || !rp.SecondDomainAt.Equal(mirrorAt) {
					t.Errorf("second_domain_at = %v, want %v", rp.SecondDomainAt, mirrorAt)
				}
				if rp.SecondDomainError != "" {
					t.Errorf("second_domain_error = %q on a confirmed copy, want empty", rp.SecondDomainError)
				}
			} else if rp.SecondDomainAt != nil {
				// A stamped time is the two-domain claim's evidence; it must not exist
				// without the claim.
				t.Errorf("second_domain_at = %v on a copy that is NOT in two domains", rp.SecondDomainAt)
			}
			if tt.wantErrRecorded && rp.SecondDomainError == "" {
				t.Error("a failed copy recorded NO cause — the error was discarded, which is the exact silent failure this component exists to close")
			}
			// The mirror is always asked about the object the ledger actually holds.
			if got := h.mirror.mirrored(); len(got) != 1 || got[0] != rp.ObjectKey {
				t.Errorf("mirrored keys = %v, want [%q]", got, rp.ObjectKey)
			}
		})
	}
}

// TestRecoveryPointMirrorWorker_BacklogSelection proves the worker touches exactly
// the rows the database would hand it: primary_only, oldest first, and NEVER a row
// that already holds the two-domain claim (re-copying one would restamp a
// confirmation time later than the copy it describes).
//
// `unknown` is excluded on purpose — migration 0023 is unbackfillable and the second
// bucket cannot be enumerated (LIST is 403), so those rows are permanently unknown
// and are counted as NOT-two-domains everywhere it matters.
func TestRecoveryPointMirrorWorker_BacklogSelection(t *testing.T) {
	h := newMirrorHarness(t)
	oldest := h.seedPoint("volumes/v1/old.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-72*time.Hour))
	newer := h.seedPoint("volumes/v1/new.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-time.Hour))
	h.seedPoint("volumes/v1/legacy.ext4", domain.RecoveryPointLocationsUnknown, mirrorAt.Add(-99*time.Hour))
	h.seedPoint("volumes/v1/done.ext4", domain.RecoveryPointLocationsSecondDomain, mirrorAt.Add(-98*time.Hour))

	rep, err := h.w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if rep.Considered != 2 || rep.Mirrored != 2 {
		t.Fatalf("considered=%d mirrored=%d, want 2/2 (only the primary_only rows)", rep.Considered, rep.Mirrored)
	}
	// Oldest first, because the alert this drains is an AGE: copying the newest first
	// would leave the worst number untouched while the worker looked busy.
	want := []string{"volumes/v1/old.ext4", "volumes/v1/new.ext4"}
	got := h.mirror.mirrored()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("mirrored order = %v, want %v", got, want)
	}
	for _, id := range []uuid.UUID{oldest, newer} {
		if rp := h.point(t, id); rp.Locations != domain.RecoveryPointLocationsSecondDomain {
			t.Errorf("recovery point %s locations = %q after a confirmed copy", id, rp.Locations)
		}
	}
}

// TestRecoveryPointMirrorWorker_FailedRowDoesNotStarveTheBacklog proves the cooldown
// does its one job. The batch is oldest-first, so a permanently unmirrorable row at
// the head — a blob the primary store can no longer serve, which the ledger audit
// finds live instances of — would otherwise consume a slot on every pass forever.
func TestRecoveryPointMirrorWorker_FailedRowDoesNotStarveTheBacklog(t *testing.T) {
	h := newMirrorHarness(t)
	h.seedPoint("volumes/v1/broken.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-72*time.Hour))
	h.mirror.err = errors.New("read back out of the primary store: not found")

	if _, err := h.w.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed copy must fail the pass")
	}

	// Second pass, still inside the cooldown: the row is DEFERRED, not retried.
	h.clock = h.clock.Add(time.Minute)
	rep, err := h.w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("a deferred row must not fail the pass: %v", err)
	}
	if rep.Deferred != 1 || rep.Failed != 0 {
		t.Fatalf("deferred=%d failed=%d, want 1/0", rep.Deferred, rep.Failed)
	}
	if got := len(h.mirror.mirrored()); got != 1 {
		t.Fatalf("copy attempts = %d, want 1 (the cooldown must suppress the second)", got)
	}

	// Past the cooldown, it is tried again — a transient outage must not park a row
	// forever either.
	h.clock = h.clock.Add(recoveryPointMirrorRetryAfter)
	h.mirror.err = nil
	rep, err = h.w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if rep.Mirrored != 1 {
		t.Fatalf("mirrored = %d after the cooldown expired, want 1", rep.Mirrored)
	}
}

// TestRecoveryPointMirrorWorker_UnreadableBacklogFailsThePass proves a pass that
// could not even SEE what needs copying is reported as a failure. Returning success
// for it would advance the last-success timestamp and make an unreadable ledger look
// like an empty backlog.
func TestRecoveryPointMirrorWorker_UnreadableBacklogFailsThePass(t *testing.T) {
	h := newMirrorHarness(t)
	h.ledger.mirrorBacklogErr = errors.New("database unreachable")

	rep, err := h.w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("an unreadable backlog reported success — an empty backlog and an unreadable one must never look alike")
	}
	if rep.Considered != 0 || rep.Mirrored != 0 {
		t.Errorf("report = %+v, want an empty pass", rep)
	}
}

// TestRecoveryPointMirrorWorker_StalledMirrorIsVisible pins the durability file's
// rule 2 for this worker: a mirror that stopped working must be visible AS stopped.
//
// The last-success gauge starts at MetricUnknown (-1, never 0), advances only on a
// pass that left NOTHING uncopied, and holds its old value through a failing pass —
// so an operator evaluating now() - value sees an age that keeps growing, which is
// the age-not-count form the whole instrument set is built on.
func TestRecoveryPointMirrorWorker_StalledMirrorIsVisible(t *testing.T) {
	h := newMirrorHarness(t)
	h.seedPoint("volumes/v1/a.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-time.Hour))

	// A pass that copied everything publishes its time.
	h.w.pass(context.Background())
	clean := testutil.ToFloat64(recoveryPointMirrorLastSuccess)
	if clean != float64(h.clock.Unix()) {
		t.Fatalf("last_success = %v after a clean pass, want %v", clean, float64(h.clock.Unix()))
	}
	if got := testutil.ToFloat64(recoveryPointMirrorAttempts.WithLabelValues(mirrorOutcomeMirrored)); got < 1 {
		t.Errorf("mirror_attempts{outcome=mirrored} = %v, want at least 1", got)
	}

	// A later pass that could NOT copy must not advance it: a stalled mirror that
	// keeps stamping success is indistinguishable from a healthy idle one.
	h.seedPoint("volumes/v1/b.ext4", domain.RecoveryPointLocationsPrimaryOnly, mirrorAt.Add(-time.Hour))
	h.mirror.err = errors.New("dial r2: connection refused")
	h.clock = h.clock.Add(time.Hour)
	failedBefore := testutil.ToFloat64(recoveryPointMirrorAttempts.WithLabelValues(mirrorOutcomeFailed))
	h.w.pass(context.Background())
	if got := testutil.ToFloat64(recoveryPointMirrorLastSuccess); got != clean {
		t.Errorf("last_success = %v after a FAILED pass, want the previous %v — a pass that left a recovery point on one chassis has not succeeded", got, clean)
	}
	if got := testutil.ToFloat64(recoveryPointMirrorAttempts.WithLabelValues(mirrorOutcomeFailed)); got != failedBefore+1 {
		t.Errorf("mirror_attempts{outcome=failed} = %v, want %v", got, failedBefore+1)
	}
}

// TestRecoveryPointMirrorWorker_RequiresBothDependencies proves a half-wired worker
// cannot be constructed. A non-nil worker that copies nowhere would stamp failures
// forever while looking wired — strictly worse than holding no worker at all, which
// leaves every recovery point honestly primary_only.
func TestRecoveryPointMirrorWorker_RequiresBothDependencies(t *testing.T) {
	if _, err := NewRecoveryPointMirrorWorker(nil, &fakeMirror{}); err == nil {
		t.Error("a worker with no ledger was constructed")
	}
	if _, err := NewRecoveryPointMirrorWorker(newFakeResourceRepo(), nil); err == nil {
		t.Error("a worker with no second domain was constructed")
	}
}
