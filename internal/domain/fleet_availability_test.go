package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func host(region, failureDomain string) Host {
	return Host{ID: uuid.New(), Region: region, FailureDomain: failureDomain}
}

// The placement invariant, stated as a table: two live hosts, DIFFERENT failure
// domain, SAME region — or the resource is not provisioned.
//
// The negative controls are the point of the tier. A second HOST is not a second
// DOMAIN, and a second domain in another REGION is not a standby the customer's
// permanent hostname can name.
func TestRequireHAPlacement(t *testing.T) {
	tests := []struct {
		name  string
		hosts []Host
		want  error
	}{
		{
			name:  "no hosts at all",
			hosts: nil,
			want:  ErrHAHostsInsufficient,
		},
		{
			name:  "ONE host — the true state of the fleet today; one machine is one failure domain",
			hosts: []Host{host("eu-central", "site-a/breaker-a/switch-1")},
			want:  ErrHAHostsInsufficient,
		},
		{
			name: "TWO hosts in the SAME failure domain — a second host is not a second domain",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", "site-a/breaker-a/switch-1"),
			},
			want: ErrHAFailureDomainShared,
		},
		{
			name: "two hosts, same site+power, only the SWITCH differs — different domain, so satisfiable",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", "site-a/breaker-a/switch-2"),
			},
			want: nil,
		},
		{
			name: "different failure domains but DIFFERENT REGIONS — the region is in the permanent hostname",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("us-east", "site-b/breaker-b/switch-9"),
			},
			want: ErrHARegionSplit,
		},
		{
			name: "three hosts: two domains exist but never inside one region",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("us-east", "site-b/breaker-b/switch-9"),
			},
			want: ErrHARegionSplit,
		},
		{
			name: "the invariant is satisfiable: two domains inside one region",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", "site-b/breaker-b/switch-2"),
			},
			want: nil,
		},
		{
			name: "a cross-region pair does NOT rescue a same-region pair that shares a domain",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("us-east", "site-a/breaker-a/switch-1"),
			},
			want: ErrHAFailureDomainShared,
		},
		{
			name: "two hosts, one UNATTESTED — the sentinel migration 0022 backfilled is not a domain",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("eu-central", HostFailureDomainUnattested),
			},
			want: ErrHAFailureDomainUnattested,
		},
		{
			name: "BOTH unattested — two unknowns are not two domains, however different the strings look",
			hosts: []Host{
				host("eu-central", HostFailureDomainUnattested),
				host("eu-central", HostFailureDomainUnattested),
			},
			want: ErrHAFailureDomainUnattested,
		},
		{
			name: "a BARE label is not a failure domain — this is the fail-open the encoding closes",
			hosts: []Host{
				host("eu-central", "host-a"),
				host("eu-central", "host-b"),
			},
			want: ErrHAFailureDomainUnattested,
		},
		{
			name: "an attested host with NO REGION cannot satisfy the same-region half",
			hosts: []Host{
				host("eu-central", "site-a/breaker-a/switch-1"),
				host("", "site-b/breaker-b/switch-2"),
			},
			want: ErrHAFailureDomainUnattested,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequireHAPlacement(tt.hosts)
			if !errors.Is(err, tt.want) {
				t.Fatalf("RequireHAPlacement = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseFailureDomain(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"the canonical form", "rgalileo-room/breaker-a/switch-1", nil},
		{"digits are fine", "site1/pdu2/sw3", nil},
		{"empty is refused, not defaulted", "", ErrHostFailureDomainRequired},
		{"whitespace only is empty", "   ", ErrHostFailureDomainRequired},
		{"a bare label — the fail-open", "host-a", ErrHostFailureDomainInvalid},
		{"the migration sentinel must NOT parse", HostFailureDomainUnattested, ErrHostFailureDomainInvalid},
		{"two segments — a partial answer must not parse", "site-a/breaker-a", ErrHostFailureDomainInvalid},
		{"four segments", "site-a/breaker-a/switch-1/extra", ErrHostFailureDomainInvalid},
		{"an empty middle segment (the unknown breaker)", "site-a//switch-1", ErrHostFailureDomainInvalid},
		{"a trailing slash", "site-a/breaker-a/", ErrHostFailureDomainInvalid},
		{"uppercase would make one domain look like two", "Site-A/Breaker-A/Switch-1", ErrHostFailureDomainInvalid},
		{"spaces", "site a/breaker a/switch 1", ErrHostFailureDomainInvalid},
		{"a leading dash", "-site/breaker/switch", ErrHostFailureDomainInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFailureDomain(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ParseFailureDomain(%q) err = %v, want %v", tt.in, err, tt.want)
			}
			if err == nil && got.String() != tt.in {
				t.Fatalf("round trip = %q, want %q", got.String(), tt.in)
			}
		})
	}
}

// The two frozen wire contracts (D-196 amendment 4). These strings are carried
// inside published engine images: a standby's application_name IS the synchronous
// quorum fence, and the cert CN is what verify-full pins. A change here orphans
// every streaming standby mid-flight, so the syntax is asserted literally.
func TestFrozenReplicationIdentities(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	if got, want := StandbyApplicationName(id, 1), "std-11111111-2222-3333-4444-555555555555-1"; got != want {
		t.Fatalf("StandbyApplicationName = %q, want %q (FROZEN: std-<resource_id>-<generation>)", got, want)
	}
	// Generation-scoped, so a revived generation-N standby cannot satisfy
	// generation N+1's quorum.
	if StandbyApplicationName(id, 1) == StandbyApplicationName(id, 2) {
		t.Fatal("the application name must differ per generation, or a stale standby can ack a new timeline's commits")
	}
	if got, want := ReplicationCertCommonName(id), "resource:11111111-2222-3333-4444-555555555555"; got != want {
		t.Fatalf("ReplicationCertCommonName = %q, want %q (FROZEN: CN=resource:<id>)", got, want)
	}
}

// The taxonomies must match the migration 0022 CHECKs exactly: a value the code
// considers legal but the DDL rejects is an insert that fails in production only.
func TestAvailabilityTaxonomiesAreClosed(t *testing.T) {
	for _, c := range []AvailabilityClass{AvailabilityClassSingle, AvailabilityClassHA} {
		if !c.IsValid() {
			t.Fatalf("availability class %q must be valid", c)
		}
	}
	for _, bad := range []AvailabilityClass{"", "HA", "ha-3", "dedicated", "single "} {
		if bad.IsValid() {
			t.Fatalf("availability class %q must NOT be valid (migration 0022 CHECK would refuse it)", bad)
		}
	}
	for _, p := range []SyncDegradePolicy{SyncDegradePolicyFailClosed, SyncDegradePolicyFailOpen} {
		if !p.IsValid() {
			t.Fatalf("sync degrade policy %q must be valid", p)
		}
	}
	for _, bad := range []SyncDegradePolicy{"", "failclosed", "closed", "async"} {
		if bad.IsValid() {
			t.Fatalf("sync degrade policy %q must NOT be valid", bad)
		}
	}
	for _, c := range []FailoverCause{FailoverCauseReal, FailoverCauseDrill, FailoverCauseSwitchover} {
		if !c.IsValid() {
			t.Fatalf("failover cause %q must be valid", c)
		}
	}
	// The distinction the published RTO depends on: a drill and a switchover are
	// not the same population, and neither is a real failure.
	for _, bad := range []FailoverCause{"", "planned", "test", "manual"} {
		if bad.IsValid() {
			t.Fatalf("failover cause %q must NOT be valid", bad)
		}
	}
}
