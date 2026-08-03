package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// ─────────────────────────────────────────────────────────────────────
// The encoding tests. What is being proven is not arithmetic — it is that the
// UNPROTECTED state can never be mistaken for the protected one:
//
//	* a resource with no recovery point reports -1, never an age of 0;
//	* a resource whose last successful snapshot is unknown reports -1, and a
//	  resource whose last successful snapshot is right now reports 0 — the two are
//	  distinguishable values, not the same one;
//	* clock skew never produces a negative age that could collide with the -1
//	  sentinel.
// ─────────────────────────────────────────────────────────────────────

func TestComputeResourceDurability(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		ts := now.Add(-d)
		return &ts
	}
	orgA := uuid.New()

	tests := []struct {
		name  string
		facts []repository.ResourceDurability
		// want* are the fleet-wide values.
		wantLive        int
		wantUnprotected int
		wantOldest      float64
		wantNewest      float64
		// wantAges maps a fact index → expected per-resource recovery-point age.
		wantAges map[int]float64
		// wantSnapAges maps a fact index → expected last-successful-snapshot age.
		wantSnapAges map[int]float64
	}{
		{
			name:            "no live resources at all reports unknown, never zero",
			facts:           nil,
			wantLive:        0,
			wantUnprotected: 0,
			wantOldest:      MetricUnknown,
			wantNewest:      MetricUnknown,
		},
		{
			name: "a resource with ZERO recovery points must not report age 0",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 0, LatestRecoveryPointAt: nil},
			},
			wantLive:        1,
			wantUnprotected: 1,
			// The fleet summary is unknown, NOT 0: "the oldest recovery point in the
			// fleet is 0 seconds old" would be the healthiest possible reading of a
			// fleet with no recovery points at all.
			wantOldest: MetricUnknown,
			wantNewest: MetricUnknown,
			wantAges:   map[int]float64{0: MetricUnknown},
		},
		{
			name: "a real age is reported in seconds",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 3, LatestRecoveryPointAt: at(2 * time.Hour)},
			},
			wantLive:        1,
			wantUnprotected: 0,
			wantOldest:      7200,
			wantNewest:      7200,
			wantAges:        map[int]float64{0: 7200},
		},
		{
			name: "the summary spans only resources that HAVE a recovery point",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 1, LatestRecoveryPointAt: at(10 * time.Second)},
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 1, LatestRecoveryPointAt: at(9 * 24 * time.Hour)},
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 0},
			},
			wantLive:        3,
			wantUnprotected: 1,
			wantOldest:      9 * 24 * 3600,
			wantNewest:      10,
			wantAges: map[int]float64{
				0: 10,
				1: 9 * 24 * 3600,
				2: MetricUnknown,
			},
		},
		{
			name: "a counted recovery point with no usable timestamp is unknown AND unprotected",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 2, LatestRecoveryPointAt: nil},
			},
			wantLive:        1,
			wantUnprotected: 1,
			wantOldest:      MetricUnknown,
			wantNewest:      MetricUnknown,
			wantAges:        map[int]float64{0: MetricUnknown},
		},
		{
			name: "a future timestamp clamps to 0 and can never look like the unknown sentinel",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 1, LatestRecoveryPointAt: at(-1 * time.Hour)},
			},
			wantLive:        1,
			wantUnprotected: 0,
			wantOldest:      0,
			wantNewest:      0,
			wantAges:        map[int]float64{0: 0},
		},
		{
			name: "never-succeeded and succeeded-just-now are DIFFERENT snapshot values",
			facts: []repository.ResourceDurability{
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 1, LatestRecoveryPointAt: at(time.Minute), LastSnapshotSuccessAt: nil},
				{ResourceID: uuid.New(), OwnerOrg: orgA, RecoveryPointCount: 1, LatestRecoveryPointAt: at(time.Minute), LastSnapshotSuccessAt: at(0)},
			},
			wantLive:        2,
			wantUnprotected: 0,
			wantOldest:      60,
			wantNewest:      60,
			wantSnapAges: map[int]float64{
				0: MetricUnknown,
				1: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeResourceDurability(tt.facts, now)

			if got.Live != tt.wantLive {
				t.Errorf("Live = %d, want %d", got.Live, tt.wantLive)
			}
			if got.Unprotected != tt.wantUnprotected {
				t.Errorf("Unprotected = %d, want %d", got.Unprotected, tt.wantUnprotected)
			}
			if got.OldestAgeSeconds != tt.wantOldest {
				t.Errorf("OldestAgeSeconds = %v, want %v", got.OldestAgeSeconds, tt.wantOldest)
			}
			if got.NewestAgeSeconds != tt.wantNewest {
				t.Errorf("NewestAgeSeconds = %v, want %v", got.NewestAgeSeconds, tt.wantNewest)
			}
			if len(got.Resources) != len(tt.facts) {
				t.Fatalf("Resources = %d, want %d (every live resource must be represented, including unprotected ones)",
					len(got.Resources), len(tt.facts))
			}
			for i, want := range tt.wantAges {
				if got.Resources[i].RecoveryPointAgeSeconds != want {
					t.Errorf("Resources[%d].RecoveryPointAgeSeconds = %v, want %v",
						i, got.Resources[i].RecoveryPointAgeSeconds, want)
				}
			}
			for i, want := range tt.wantSnapAges {
				if got.Resources[i].SnapshotLastSuccessAgeSeconds != want {
					t.Errorf("Resources[%d].SnapshotLastSuccessAgeSeconds = %v, want %v",
						i, got.Resources[i].SnapshotLastSuccessAgeSeconds, want)
				}
			}
			// The sentinel must be unreachable as a real measurement, in every case.
			for i, g := range got.Resources {
				if g.RecoveryPointAgeSeconds < 0 && g.RecoveryPointAgeSeconds != MetricUnknown {
					t.Errorf("Resources[%d].RecoveryPointAgeSeconds = %v: a negative age other than the unknown sentinel is unreadable",
						i, g.RecoveryPointAgeSeconds)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// The fail-soft test. A collection error must leave the previous (good) values
// in place — zeroing "recovery point age" because the database blinked invents a
// data-loss alert, and zeroing "unprotected resources" invents safety — while
// still being VISIBLE as a failure through the error counter and a
// last-success timestamp that stops advancing.
// ─────────────────────────────────────────────────────────────────────

type durabilityHostsFake struct {
	hosts []domain.Host
	err   error
}

func (f *durabilityHostsFake) List(context.Context) ([]domain.Host, error) {
	return f.hosts, f.err
}

type durabilityLeasesFake struct {
	byHost map[uuid.UUID][]domain.NetLease
}

func (f *durabilityLeasesFake) ListByHost(_ context.Context, hostID uuid.UUID) ([]domain.NetLease, error) {
	return f.byHost[hostID], nil
}

func TestDurabilityCollectFailureKeepsPreviousValues(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	resources := newFakeResourceRepo()
	resID := uuid.New()
	org := uuid.New()
	rpAt := now.Add(-30 * time.Minute)
	resources.byID[resID] = &domain.FleetResource{
		ID: resID, OwnerOrg: org, Phase: domain.FleetResourcePhaseReady,
		Class: "postgres", Tier: "dedicated",
	}
	resources.recovery[resID] = []domain.FleetResourceRecoveryPoint{
		{ID: uuid.New(), ResourceID: resID, ObjectKey: "k", CreatedAt: rpAt},
	}

	hostID := uuid.New()
	hosts := &durabilityHostsFake{hosts: []domain.Host{
		{ID: hostID, Status: domain.HostStatusActive, FailureDomain: "room-a/breaker-a/switch-1"},
		{ID: uuid.New(), Status: domain.HostStatusActive, FailureDomain: domain.HostFailureDomainUnattested},
	}}
	leases := &durabilityLeasesFake{byHost: map[uuid.UUID][]domain.NetLease{
		hostID: {{NetIndex: 1}, {NetIndex: 2}},
	}}

	c := NewFleetDurabilityCollector(resources, hosts, leases)
	c.now = func() time.Time { return now }

	if err := c.Collect(ctx); err != nil {
		t.Fatalf("first Collect: %v", err)
	}

	wantAge := 30 * 60.0
	if got := testutil.ToFloat64(recoveryPointAge.WithLabelValues(resID.String(), org.String())); got != wantAge {
		t.Fatalf("recovery point age after a good pass = %v, want %v", got, wantAge)
	}
	// The attestation pair is the point of the host section: two live hosts, only
	// one of which has actually stated a failure domain, is NOT two domains.
	if got := testutil.ToFloat64(hostsLive); got != 2 {
		t.Fatalf("hosts_live = %v, want 2", got)
	}
	if got := testutil.ToFloat64(hostsAttested); got != 1 {
		t.Fatalf("hosts_attested = %v, want 1 (an unattested host is not a failure domain)", got)
	}
	if got := testutil.ToFloat64(netLeasesHeld.WithLabelValues(hostID.String())); got != 2 {
		t.Fatalf("net_leases_held = %v, want 2", got)
	}
	if got := testutil.ToFloat64(durabilityLastSuccess); got != float64(now.Unix()) {
		t.Fatalf("last success timestamp = %v, want %v", got, now.Unix())
	}

	errsBefore := testutil.ToFloat64(durabilityCollectionErrors)

	// Now break the ledger read and advance the clock. A failing pass must not
	// touch a single gauge value.
	resources.durabilityErr = errors.New("boom: control plane unreachable")
	later := now.Add(10 * time.Minute)
	c.now = func() time.Time { return later }

	if err := c.Collect(ctx); err == nil {
		t.Fatal("Collect returned nil on a failed ledger read; a silent failure is the exact false-green this collector exists to prevent")
	}
	if got := testutil.ToFloat64(recoveryPointAge.WithLabelValues(resID.String(), org.String())); got != wantAge {
		t.Errorf("recovery point age after a FAILED pass = %v, want the previous %v (a DB blip must not invent a data-loss alert or erase one)", got, wantAge)
	}
	if got := testutil.ToFloat64(resourcesUnprotected); got != 0 {
		t.Errorf("resources_unprotected after a FAILED pass = %v, want the previous 0", got)
	}
	if got := testutil.ToFloat64(durabilityCollectionErrors); got != errsBefore+1 {
		t.Errorf("collection_errors_total = %v, want %v", got, errsBefore+1)
	}
	if got := testutil.ToFloat64(durabilityLastSuccess); got != float64(now.Unix()) {
		t.Errorf("last success timestamp = %v, want it to stay at %v: a failed pass must not claim success", got, now.Unix())
	}
}

// TestDurabilityCollectSurfacesAnUnprotectedResource is the smoke test for the
// gauge that matters most: a live claim with no recovery point at all.
func TestDurabilityCollectSurfacesAnUnprotectedResource(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	resources := newFakeResourceRepo()
	resID := uuid.New()
	org := uuid.New()
	resources.byID[resID] = &domain.FleetResource{
		ID: resID, OwnerOrg: org, Phase: domain.FleetResourcePhaseReady,
		Class: "postgres", Tier: "dedicated", ConsecutiveSnapshotFailures: 4,
	}

	c := NewFleetDurabilityCollector(resources, &durabilityHostsFake{}, &durabilityLeasesFake{})
	c.now = func() time.Time { return now }
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if got := testutil.ToFloat64(recoveryPointAge.WithLabelValues(resID.String(), org.String())); got != MetricUnknown {
		t.Errorf("age of a resource with no recovery point = %v, want %v", got, float64(MetricUnknown))
	}
	if got := testutil.ToFloat64(resourcesUnprotected); got != 1 {
		t.Errorf("resources_unprotected = %v, want 1", got)
	}
	if got := testutil.ToFloat64(snapshotFailures.WithLabelValues(resID.String(), org.String())); got != 4 {
		t.Errorf("snapshot_failures = %v, want 4", got)
	}
	if got := testutil.ToFloat64(snapshotLastSuccessAge.WithLabelValues(resID.String(), org.String())); got != MetricUnknown {
		t.Errorf("last-successful-snapshot age with no success ever = %v, want %v", got, float64(MetricUnknown))
	}
}

// TestPublishLedgerReportCountsEachKind proves the report-only reconciler's
// findings become countable, and that `undetermined` is NOT folded in as a
// divergence kind (an undecidable entry is not evidence of health).
func TestPublishLedgerReportCountsEachKind(t *testing.T) {
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	PublishLedgerReport(LedgerDivergenceReport{
		RowsWithoutFile:             2,
		FilesWithoutRow:             1,
		RecoveryPointsWithoutObject: 3,
		Undetermined:                5,
	}, at)

	for kind, want := range map[string]float64{
		"row_without_file":              2,
		"file_without_row":              1,
		"recovery_point_without_object": 3,
	} {
		if got := testutil.ToFloat64(ledgerDivergences.WithLabelValues(kind)); got != want {
			t.Errorf("ledger_divergences{kind=%q} = %v, want %v", kind, got, want)
		}
	}
	if got := testutil.ToFloat64(ledgerUndetermined); got != 5 {
		t.Errorf("ledger_undetermined = %v, want 5", got)
	}
	if got := testutil.ToFloat64(ledgerLastSuccess); got != float64(at.Unix()) {
		t.Errorf("ledger last success = %v, want %v", got, at.Unix())
	}
}

// The §22 outcome label separates a CALLER bug from a PLATFORM fault. Getting it
// wrong blunts the one signal the `error` label carries: a client sending bad
// claims would read as a fleet that is failing.
func TestOutcomeForSeparatesCallerBugsFromPlatformFaults(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, outcomeOK},
		{"an unsupported tier is a caller bug", domain.ErrResourceTierUnsupported, outcomeInvalid},
		// D-202 — both of these are InvalidArgument at the boundary.
		{"a durability the tier cannot hold is a caller bug", domain.ErrResourceDurabilityInvalid, outcomeInvalid},
		{"a half-written waiver is a caller bug", domain.ErrProtectionWaiverIncomplete, outcomeInvalid},
		{"a wrapped caller bug is still a caller bug", errors.Join(errors.New("provision: "), domain.ErrResourceDurabilityInvalid), outcomeInvalid},
		// ⚠ NOT invalid: the protection refusal is a true statement about the
		// platform's own state, and it must keep showing up as one.
		{"the protection refusal is a PLATFORM fact, not a caller bug", domain.ErrProtectionUnattachable, outcomeError},
		{"anything unrecognised is a platform fault", errors.New("boom"), outcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeFor(tt.err); got != tt.want {
				t.Fatalf("outcomeFor(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
