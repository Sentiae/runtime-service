package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeLiveHosts is the live-host inventory the standard-ha gate refuses on. It
// records the staleness it was asked with, because the gate must ask the SAME
// question the scheduler asks — a gate reading a laxer live-set would admit a
// claim over a host that is already gone.
type fakeLiveHosts struct {
	hosts         []domain.Host
	err           error
	calls         int
	lastStaleness time.Duration
}

func (f *fakeLiveHosts) ListLive(_ context.Context, staleness time.Duration) ([]domain.Host, error) {
	f.calls++
	f.lastStaleness = staleness
	if f.err != nil {
		return nil, f.err
	}
	return f.hosts, nil
}

func liveHost(region, failureDomain string) domain.Host {
	return domain.Host{
		ID:            uuid.New(),
		Region:        region,
		FailureDomain: failureDomain,
		Health:        domain.HostHealthHealthy,
		Status:        domain.HostStatusActive,
	}
}

// The slice-1 deliverable: `standard-ha` is REFUSED unless the fleet can place
// two members in different failure domains inside one region — and on today's
// single machine that refusal is the whole feature. `single` must be untouched in
// every one of these situations, because a gate that could break the tier every
// existing database uses in order to guard a tier nothing can provision would be
// a bad trade.
func TestProvisionDedicated_AvailabilityGate(t *testing.T) {
	oneHost := []domain.Host{liveHost("eu-central", "site-a/breaker-a/switch-1")}
	twoSameDomain := []domain.Host{
		liveHost("eu-central", "site-a/breaker-a/switch-1"),
		liveHost("eu-central", "site-a/breaker-a/switch-1"),
	}
	twoDifferentRegions := []domain.Host{
		liveHost("eu-central", "site-a/breaker-a/switch-1"),
		liveHost("us-east", "site-b/breaker-b/switch-2"),
	}
	twoUnattested := []domain.Host{
		liveHost("eu-central", domain.HostFailureDomainUnattested),
		liveHost("eu-central", domain.HostFailureDomainUnattested),
	}
	satisfiable := []domain.Host{
		liveHost("eu-central", "site-a/breaker-a/switch-1"),
		liveHost("eu-central", "site-b/breaker-b/switch-2"),
	}

	tests := []struct {
		name         string
		availability string
		hosts        []domain.Host
		hostsErr     error
		noLister     bool
		wantErr      error
		// wantClass is the class the accepted resource row must record.
		wantClass domain.AvailabilityClass
	}{
		{
			name:         "ha on ONE host is refused — one machine is one failure domain",
			availability: "ha",
			hosts:        oneHost,
			wantErr:      domain.ErrHAHostsInsufficient,
		},
		{
			name:         "ha on TWO hosts in the SAME domain is STILL refused — a second host is not a second domain",
			availability: "ha",
			hosts:        twoSameDomain,
			wantErr:      domain.ErrHAFailureDomainShared,
		},
		{
			name:         "ha across two REGIONS is refused — the region is baked into the permanent hostname",
			availability: "ha",
			hosts:        twoDifferentRegions,
			wantErr:      domain.ErrHARegionSplit,
		},
		{
			name:         "ha on two hosts whose domains were never stated is refused",
			availability: "ha",
			hosts:        twoUnattested,
			wantErr:      domain.ErrHAFailureDomainUnattested,
		},
		{
			name:         "an unreadable host inventory refuses — an unprovable invariant is an unmet one",
			availability: "ha",
			hostsErr:     errors.New("control-plane db unreachable"),
			wantErr:      domain.ErrHAPlacementUnknowable,
		},
		{
			name:         "no inventory wired at all refuses",
			availability: "ha",
			noLister:     true,
			wantErr:      domain.ErrHAPlacementUnknowable,
		},
		{
			name:         "an unrecognized class is refused, never quietly downgraded to single",
			availability: "highly-available",
			hosts:        satisfiable,
			wantErr:      domain.ErrHAAvailabilityClassInvalid,
		},
		{
			name:         "ha is ACCEPTED when the invariant is satisfiable",
			availability: "ha",
			hosts:        satisfiable,
			wantClass:    domain.AvailabilityClassHA,
		},
		// `single` in every situation the ha claim was refused in.
		{
			name:         "single on ONE host is unaffected",
			availability: "single",
			hosts:        oneHost,
			wantClass:    domain.AvailabilityClassSingle,
		},
		{
			name:         "single on two same-domain hosts is unaffected",
			availability: "single",
			hosts:        twoSameDomain,
			wantClass:    domain.AvailabilityClassSingle,
		},
		{
			name:         "single with an unreadable inventory is unaffected — it asks nothing of placement",
			availability: "single",
			hostsErr:     errors.New("control-plane db unreachable"),
			wantClass:    domain.AvailabilityClassSingle,
		},
		{
			name:         "single with no inventory wired is unaffected",
			availability: "single",
			noLister:     true,
			wantClass:    domain.AvailabilityClassSingle,
		},
		{
			name:         "an UNSET class means single — the wire cannot express this field yet",
			availability: "",
			noLister:     true,
			wantClass:    domain.AvailabilityClassSingle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			appHandle := uuid.New()
			prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: appHandle.String()}}

			var hosts HAPlacementHosts
			fake := &fakeLiveHosts{hosts: tt.hosts, err: tt.hostsErr}
			if !tt.noLister {
				hosts = fake
			}
			uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine(), testEndpointNaming(), hosts, 90*time.Second)

			in := validDedicatedInput()
			in.AvailabilityClass = tt.availability
			out, err := uc.ProvisionDedicated(context.Background(), in)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ProvisionDedicated err = %v, want %v", err, tt.wantErr)
				}
				// The refusal must land BEFORE anything is built or recorded: a claim
				// row naming a tier nothing can build is a promise on the books, and a
				// booted VM is a resource nobody claimed.
				if prov.provisionCalls != 0 {
					t.Fatalf("a refused claim must boot nothing (provision calls = %d)", prov.provisionCalls)
				}
				if len(repo.byID) != 0 {
					t.Fatalf("a refused claim must persist nothing (%d rows)", len(repo.byID))
				}
				return
			}

			if err != nil {
				t.Fatalf("ProvisionDedicated: %v", err)
			}
			id, perr := uuid.Parse(out.Handle)
			if perr != nil {
				t.Fatalf("handle %q is not a uuid: %v", out.Handle, perr)
			}
			res := repo.byID[id]
			if res == nil {
				t.Fatal("the accepted claim was not persisted")
			}
			// The class is RECORDED, not inferred from tier or durability.
			if res.AvailabilityClass != tt.wantClass {
				t.Fatalf("persisted availability_class = %q, want %q", res.AvailabilityClass, tt.wantClass)
			}
			// fail_closed is the default and the only value the tier is designed
			// around: a primary must not be able to ack a commit it did not replicate.
			if res.SyncDegradePolicy != domain.SyncDegradePolicyFailClosed {
				t.Fatalf("persisted sync_degrade_policy = %q, want fail_closed", res.SyncDegradePolicy)
			}
			// `single` must not even look at the inventory — no new failure mode on
			// the path every existing database uses.
			if tt.wantClass == domain.AvailabilityClassSingle && fake.calls != 0 {
				t.Fatalf("a single claim consulted the host inventory %d times, want 0", fake.calls)
			}
			if tt.wantClass == domain.AvailabilityClassHA {
				if fake.calls != 1 {
					t.Fatalf("an ha claim read the host inventory %d times, want exactly 1", fake.calls)
				}
				if fake.lastStaleness != 90*time.Second {
					t.Fatalf("the gate asked for staleness %v, want the scheduler's 90s — the two must agree on which hosts are live", fake.lastStaleness)
				}
			}
		})
	}
}
