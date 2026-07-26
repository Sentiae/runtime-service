package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// The second failure domain, from the snapshot path's side (D-192/D-195).
//
// ⚠ WHAT THESE TESTS DEFEND. Before this slice every recovery point landed only in
// the MinIO container on the fleet host's own chassis, so `failure_domains = 1`. The
// dangerous failure is not "the mirror broke" — it is "the mirror broke and the
// ledger still reads as protected", which converts an alarming state into a
// reassuring one. So each case below asserts what the LEDGER says, not what the
// call returned.
// ─────────────────────────────────────────────────────────────────────

// fakeMirror is a SecondDomainMirror whose outcome the test dictates.
type fakeMirror struct {
	rec *recorder

	mu   sync.Mutex
	err  error
	at   time.Time
	keys []string
}

func (f *fakeMirror) Domain() string { return "r2:test-bucket" }

func (f *fakeMirror) Mirror(_ context.Context, objectKey, expectChecksum string) (SecondDomainReceipt, error) {
	if f.rec != nil {
		f.rec.add("mirror")
	}
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

// onlyRecoveryPoint takes the single recovery point the harness's resource holds,
// read back out of the LEDGER (not the returned value) — the ledger is what every
// durability number is computed from.
func onlyRecoveryPoint(t *testing.T, h *snapHarness, resID uuid.UUID) domain.FleetResourceRecoveryPoint {
	t.Helper()
	rps, err := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if err != nil {
		t.Fatalf("list recovery points: %v", err)
	}
	if len(rps) != 1 {
		t.Fatalf("recovery points = %d, want 1", len(rps))
	}
	return rps[0]
}

func TestSnapshot_SecondDomain(t *testing.T) {
	mirrorAt := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// wire installs (or declines to install) a mirror. Returning nil models a host
		// with no second domain at all.
		wire func(h *snapHarness) *fakeMirror
		// ledgerBroken fails the ledger writes that record the mirror's outcome.
		ledgerBroken bool

		wantLocations   domain.RecoveryPointLocations
		wantStore       string
		wantStamped     bool
		wantErrRecorded bool
	}{
		{
			name: "a confirmed second copy earns the two-domain claim",
			wire: func(h *snapHarness) *fakeMirror {
				m := &fakeMirror{rec: h.rec, at: mirrorAt}
				h.s.SetSecondDomainMirror(m)
				return m
			},
			wantLocations: domain.RecoveryPointLocationsSecondDomain,
			wantStore:     "r2:test-bucket",
			wantStamped:   true,
		},
		{
			name: "a FAILED second copy leaves the row saying ONE domain, with the cause on it",
			wire: func(h *snapHarness) *fakeMirror {
				m := &fakeMirror{rec: h.rec, at: mirrorAt, err: errors.New("dial r2: connection refused")}
				h.s.SetSecondDomainMirror(m)
				return m
			},
			wantLocations:   domain.RecoveryPointLocationsPrimaryOnly,
			wantErrRecorded: true,
		},
		{
			name: "no second domain wired at all is recorded as one domain, not as unknown",
			wire: func(*snapHarness) *fakeMirror { return nil },
			// unknown is reserved for rows that predate the column. This host KNOWS the
			// blob went to exactly one store, so it says so.
			wantLocations: domain.RecoveryPointLocationsPrimaryOnly,
		},
		{
			name: "a copy that landed but could not be recorded keeps reporting as one domain",
			wire: func(h *snapHarness) *fakeMirror {
				m := &fakeMirror{rec: h.rec, at: mirrorAt}
				h.s.SetSecondDomainMirror(m)
				return m
			},
			ledgerBroken: true,
			// Understating durability is the safe direction of this failure: the ledger
			// must never claim a protection it did not write down.
			wantLocations: domain.RecoveryPointLocationsPrimaryOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSnapshotHarness(t)
			appID, resID, _, _, _ := h.attachedVolume(t)
			m := tt.wire(h)
			if tt.ledgerBroken {
				h.recovery.mirrorLedgerErr = errors.New("control plane unreachable")
			}

			points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
			// ⚠ A failed second copy must NEVER fail the snapshot: the recovery point
			// exists and is restorable, and discarding it because a WAN transfer failed
			// would be strictly worse durability than before this path existed.
			if err != nil {
				t.Fatalf("snapshot: %v (a second-domain failure must not fail the snapshot)", err)
			}
			if len(points) != 1 {
				t.Fatalf("recovery points returned = %d, want 1", len(points))
			}

			rp := onlyRecoveryPoint(t, h, resID)
			if rp.Locations != tt.wantLocations {
				t.Errorf("ledger locations = %q, want %q", rp.Locations, tt.wantLocations)
			}
			// The value the caller (and the RPC) sees must say the same thing the ledger
			// does — two answers that can disagree is how a one-domain copy reads as a
			// two-domain one.
			if points[0].Locations != rp.Locations {
				t.Errorf("returned locations = %q but the ledger says %q", points[0].Locations, rp.Locations)
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
				// A stamped time on an unconfirmed copy is the two-domain claim's evidence;
				// it must not exist without the claim.
				t.Errorf("second_domain_at = %v on a copy that is NOT in two domains", rp.SecondDomainAt)
			}
			if tt.wantErrRecorded {
				if rp.SecondDomainError == "" {
					t.Error("a failed second copy recorded NO cause — the error was discarded, which is the exact silent-failure this slice exists to close")
				}
				if points[0].SecondDomainError == "" {
					t.Error("the returned recovery point carries no second-domain cause")
				}
			}
			// The mirror is always asked about the object that was actually stored.
			if m != nil {
				if got := m.mirrored(); len(got) != 1 || got[0] != rp.ObjectKey {
					t.Errorf("mirrored keys = %v, want [%q]", got, rp.ObjectKey)
				}
			}
		})
	}
}

// TestSnapshot_SecondDomainRunsAfterTheThaw is the ordering contract: the customer's
// filesystem must NOT be frozen for the WAN leg. The freeze is held for the whole
// primary upload by design (there is no local staging copy), so putting the second,
// far slower copy inside it would multiply the frozen window by the WAN's latency
// and hand the guest's dead-man auto-thaw a window it cannot cover.
func TestSnapshot_SecondDomainRunsAfterTheThaw(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.s.SetSecondDomainMirror(&fakeMirror{rec: h.rec, at: time.Now().UTC()})

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	events := h.rec.all()
	want := []string{"syncfs", "freeze", "upload", "thaw", "mirror"}
	if !sameOrder(events, want) {
		t.Fatalf("order = %v, want %v — the second-domain copy must run AFTER the thaw", events, want)
	}
}

// TestSnapshot_SecondDomainNotAttemptedWhenTheCaptureFailed proves the mirror is
// never reached on a capture that produced no recovery point. There would be nothing
// to copy, and an attempt would put a failure cause on a row that does not exist.
func TestSnapshot_SecondDomainNotAttemptedWhenTheCaptureFailed(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	m := &fakeMirror{rec: h.rec, at: time.Now().UTC()}
	h.s.SetSecondDomainMirror(m)
	h.guest.freezeErr = errors.New("guest not answering")

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("an unquiescible guest must refuse the snapshot")
	}
	if got := m.mirrored(); len(got) != 0 {
		t.Errorf("the mirror was invoked %v for a capture that produced no recovery point", got)
	}
}
