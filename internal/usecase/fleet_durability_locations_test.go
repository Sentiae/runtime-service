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
// The failure-domain census (migration 0023).
//
// What is being proven is an ENCODING, not arithmetic: a recovery point that is
// NOT provably in two failure domains must never be counted as one that is, and
// "there is nothing to report" must never render as the healthiest possible
// reading. The oldest-single-domain age is -1 when there is none and an age
// otherwise — because 0 is what a brand-new single-domain copy reports, and an
// unmoving series is what a dead collector reports.
// ─────────────────────────────────────────────────────────────────────

func TestComputeRecoveryPointLocations(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		ts := now.Add(-d)
		return &ts
	}

	two := string(domain.RecoveryPointLocationsSecondDomain)
	one := string(domain.RecoveryPointLocationsPrimaryOnly)
	unk := string(domain.RecoveryPointLocationsUnknown)

	tests := []struct {
		name       string
		facts      []repository.RecoveryPointLocationFacts
		wantCounts map[string]float64
		wantOldest float64
	}{
		{
			name:       "an empty catalog reports zeros and an UNKNOWN age, never an age of 0",
			facts:      nil,
			wantCounts: map[string]float64{two: 0, one: 0, unk: 0},
			wantOldest: MetricUnknown,
		},
		{
			name: "a fully mirrored catalog reports no single-domain age at all",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: two, Count: 9, OldestCreatedAt: at(90 * 24 * time.Hour)},
			},
			wantCounts: map[string]float64{two: 9, one: 0, unk: 0},
			// -1 and NOT the 90-day age of the mirrored population: the gauge answers
			// "how long has the least protected copy been unprotected", and a fleet with
			// none has no such copy.
			wantOldest: MetricUnknown,
		},
		{
			name: "one and two domain populations are counted apart",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: two, Count: 4, OldestCreatedAt: at(10 * time.Hour)},
				{Locations: one, Count: 2, OldestCreatedAt: at(3 * time.Hour)},
			},
			wantCounts: map[string]float64{two: 4, one: 2, unk: 0},
			wantOldest: 3 * 3600,
		},
		{
			name: "UNKNOWN counts toward the single-domain AGE and never toward two domains",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: two, Count: 1, OldestCreatedAt: at(time.Hour)},
				{Locations: one, Count: 1, OldestCreatedAt: at(2 * time.Hour)},
				// Pre-0023 rows: nothing can prove where these are, and the bucket cannot be
				// enumerated to find out (D-199). Treated as the weakest class.
				{Locations: unk, Count: 5, OldestCreatedAt: at(400 * time.Hour)},
			},
			wantCounts: map[string]float64{two: 1, one: 1, unk: 5},
			wantOldest: 400 * 3600,
		},
		{
			name: "a class with no usable timestamp is counted but contributes no age",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: one, Count: 3, OldestCreatedAt: nil},
			},
			wantCounts: map[string]float64{two: 0, one: 3, unk: 0},
			wantOldest: MetricUnknown,
		},
		{
			name: "an unrecognized class is NOT quietly folded into the protected side",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: "some_future_class", Count: 2, OldestCreatedAt: at(5 * time.Hour)},
			},
			wantCounts: map[string]float64{two: 0, one: 0, unk: 0, "some_future_class": 2},
			wantOldest: 5 * 3600,
		},
		{
			name: "a future timestamp clamps to 0 and can never collide with the -1 sentinel",
			facts: []repository.RecoveryPointLocationFacts{
				{Locations: one, Count: 1, OldestCreatedAt: at(-time.Hour)},
			},
			wantCounts: map[string]float64{two: 0, one: 1, unk: 0},
			wantOldest: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeRecoveryPointLocations(tt.facts, now)
			if len(got.CountByLocation) != len(tt.wantCounts) {
				t.Errorf("classes = %v, want %v", got.CountByLocation, tt.wantCounts)
			}
			for class, want := range tt.wantCounts {
				if got.CountByLocation[class] != want {
					t.Errorf("count[%s] = %v, want %v", class, got.CountByLocation[class], want)
				}
			}
			if got.OldestSingleDomainAgeSeconds != tt.wantOldest {
				t.Errorf("oldest single-domain age = %v, want %v", got.OldestSingleDomainAgeSeconds, tt.wantOldest)
			}
		})
	}
}

// TestDurabilityCollectPublishesTheFailureDomainCensus drives the census through the
// real collector and the real gauges — the encoding is only worth anything if it
// reaches Prometheus.
func TestDurabilityCollectPublishesTheFailureDomainCensus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	resources := newFakeResourceRepo()
	resID, org := uuid.New(), uuid.New()
	resources.byID[resID] = &domain.FleetResource{
		ID: resID, OwnerOrg: org, Phase: domain.FleetResourcePhaseReady,
		Class: "postgres", Tier: "dedicated",
	}
	resources.recovery[resID] = []domain.FleetResourceRecoveryPoint{
		{ID: uuid.New(), ResourceID: resID, ObjectKey: "a", CreatedAt: now.Add(-time.Hour),
			Locations: domain.RecoveryPointLocationsSecondDomain},
		{ID: uuid.New(), ResourceID: resID, ObjectKey: "b", CreatedAt: now.Add(-6 * time.Hour),
			Locations: domain.RecoveryPointLocationsPrimaryOnly},
		{ID: uuid.New(), ResourceID: resID, ObjectKey: "c", CreatedAt: now.Add(-48 * time.Hour),
			Locations: domain.RecoveryPointLocationsUnknown},
	}

	c := NewFleetDurabilityCollector(resources, &durabilityHostsFake{}, &durabilityLeasesFake{})
	c.now = func() time.Time { return now }
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for class, want := range map[domain.RecoveryPointLocations]float64{
		domain.RecoveryPointLocationsSecondDomain: 1,
		domain.RecoveryPointLocationsPrimaryOnly:  1,
		domain.RecoveryPointLocationsUnknown:      1,
	} {
		if got := testutil.ToFloat64(recoveryPointsByLocation.WithLabelValues(string(class))); got != want {
			t.Errorf("recovery_points_by_location{locations=%q} = %v, want %v", class, got, want)
		}
	}
	// The 48h `unknown` row is the least protected thing in the fleet, so it — not the
	// 6h primary_only one — is what the alert must see.
	if got := testutil.ToFloat64(recoveryPointOldestSingleDomainAge); got != 48*3600 {
		t.Errorf("oldest_single_domain_age = %v, want %v", got, 48*3600.0)
	}

	// Now break ONLY the census read. A failed pass must leave the census gauges at
	// their previous values and must not claim success.
	errsBefore := testutil.ToFloat64(durabilityCollectionErrors)
	resources.locationsErr = errors.New("boom: control plane unreachable")
	if err := c.Collect(ctx); err == nil {
		t.Fatal("Collect returned nil on a failed census read")
	}
	if got := testutil.ToFloat64(recoveryPointOldestSingleDomainAge); got != 48*3600 {
		t.Errorf("oldest_single_domain_age after a FAILED pass = %v, want the previous %v (a DB blip must not report a fully mirrored fleet)", got, 48*3600.0)
	}
	if got := testutil.ToFloat64(durabilityCollectionErrors); got != errsBefore+1 {
		t.Errorf("collection_errors_total = %v, want %v", got, errsBefore+1)
	}
}

// TestDurabilityCensusStartsUnknown pins this file's rule 2 for the new gauge: a
// process that has not collected yet must not publish a number that reads as a fully
// mirrored fleet. The gauge is registered at MetricUnknown, so the assertion is that
// -1 (and not 0) is what an uncollected fleet reports.
func TestDurabilityCensusStartsUnknown(t *testing.T) {
	// Publish an empty census — the shape a first pass over an empty catalog produces.
	publishRecoveryPointLocations(ComputeRecoveryPointLocations(nil, time.Now().UTC()))
	if got := testutil.ToFloat64(recoveryPointOldestSingleDomainAge); got != MetricUnknown {
		t.Fatalf("oldest_single_domain_age with nothing to report = %v, want %v; 0 is the healthiest possible reading of the least protected possible state", got, float64(MetricUnknown))
	}
}
