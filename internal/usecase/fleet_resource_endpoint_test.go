package usecase

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// The customer-facing endpoint identity at the ONE moment it is decided:
// resource birth (D-190). Everything here is about a name that can never be
// changed afterwards.

var mintedIDShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]{4}$`)

// endpointOf fails the test (rather than panicking) when a resource carries no
// endpoint identity — the state this whole change exists to make impossible.
func endpointOf(t *testing.T, res *domain.FleetResource) string {
	t.Helper()
	if res.EndpointID == nil {
		t.Fatalf("resource %s has no endpoint identity", res.ID)
	}
	return *res.EndpointID
}

// provisionOnce runs one dedicated provision against a fresh fake repo and
// returns the stored row.
func provisionOnce(t *testing.T, repo *fakeResourceRepo, naming domain.EndpointNaming, in ProvisionDedicatedInput) (*domain.FleetResource, error) {
	t.Helper()
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, &fakeVolumeBinder{}, testEngine(), naming, nil, 0)
	out, err := uc.ProvisionDedicated(context.Background(), in)
	if err != nil {
		return nil, err
	}
	id, perr := uuid.Parse(out.Handle)
	if perr != nil {
		t.Fatalf("handle %q is not a uuid: %v", out.Handle, perr)
	}
	res, gerr := repo.GetResourceByHandle(context.Background(), id)
	if gerr != nil {
		t.Fatalf("stored resource: %v", gerr)
	}
	return res, nil
}

// TestProvisionMintsEndpointIdentityAtBirth — the row that is created carries
// its permanent name and its region from the first INSERT, plus generation 1.
func TestProvisionMintsEndpointIdentityAtBirth(t *testing.T) {
	repo := newFakeResourceRepo()
	res, err := provisionOnce(t, repo, testEndpointNaming(), validDedicatedInput())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.EndpointID == nil {
		t.Fatal("resource was born without an endpoint identity")
	}
	if !mintedIDShape.MatchString(endpointOf(t, res)) {
		t.Fatalf("endpoint id %q does not match adjective-noun-NNNN", endpointOf(t, res))
	}
	if res.Region != "eu-central" {
		t.Fatalf("region = %q, want the configured eu-central", res.Region)
	}
	if res.Generation != domain.FleetResourceInitialGeneration {
		t.Fatalf("generation = %d, want %d", res.Generation, domain.FleetResourceInitialGeneration)
	}
	// The name must be servable as configured.
	ep, err := domain.NewResourceEndpoint(endpointOf(t, res), res.Region, "db.sentiae.test")
	if err != nil {
		t.Fatalf("minted identity does not assemble into a valid host: %v", err)
	}
	if ep.Host() != endpointOf(t, res)+".eu-central.db.sentiae.test" {
		t.Fatalf("host = %q", ep.Host())
	}
}

// TestEndpointIdentityIsDerivedFromNothing — the name must not be a function of
// the claim. Two orgs claiming the SAME claim key in the same env get DIFFERENT
// endpoints; if either were derived from the claim they would collide, and one
// tenant's connection string would resolve to another tenant's database.
func TestEndpointIdentityIsDerivedFromNothing(t *testing.T) {
	repo := newFakeResourceRepo()

	inA := validDedicatedInput()
	inA.ClaimKey = "orders-db"
	inB := validDedicatedInput() // a different OwnerOrg by construction
	inB.ClaimKey = "orders-db"
	if inA.OwnerOrg == inB.OwnerOrg {
		t.Fatal("test setup: both claims are in the same org")
	}

	resA, err := provisionOnce(t, repo, testEndpointNaming(), inA)
	if err != nil {
		t.Fatalf("provision A: %v", err)
	}
	resB, err := provisionOnce(t, repo, testEndpointNaming(), inB)
	if err != nil {
		t.Fatalf("provision B: %v", err)
	}
	idA, idB := endpointOf(t, resA), endpointOf(t, resB)
	if idA == idB {
		t.Fatalf("same claim key in two orgs produced the same endpoint %q", idA)
	}
	// And neither leaks the tenant or the claim into the public name.
	for _, res := range []*domain.FleetResource{resA, resB} {
		id := endpointOf(t, res)
		if id == res.ClaimKey || id == res.OwnerOrg.String() || id == res.ID.String() {
			t.Fatalf("endpoint %q is derived from the claim/org/resource id", id)
		}
	}
}

// TestReProvisionKeepsTheExistingEndpoint — the immutability guarantee. A
// re-provision of an existing claim is the post-restart recovery path and runs
// often; it must return the claim as it stands and never mint a second name.
func TestReProvisionKeepsTheExistingEndpoint(t *testing.T) {
	repo := newFakeResourceRepo()
	in := validDedicatedInput()

	first, err := provisionOnce(t, repo, testEndpointNaming(), in)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	mintsAfterFirst := len(repo.mintedEndpointIDs)

	second, err := provisionOnce(t, repo, testEndpointNaming(), in)
	if err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-provision created a second resource (%s then %s)", first.ID, second.ID)
	}
	if endpointOf(t, second) != endpointOf(t, first) {
		t.Fatalf("endpoint changed on re-provision: %q -> %q", endpointOf(t, first), endpointOf(t, second))
	}
	if len(repo.mintedEndpointIDs) != mintsAfterFirst {
		t.Fatalf("re-provision minted again: %v", repo.mintedEndpointIDs)
	}
}

// TestEndpointCollisionRetries — the unique index is the arbiter, so a
// collision means RE-MINT (a different id), not a failed provision.
func TestEndpointCollisionRetries(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.endpointTaken = 2 // the first two minted ids are already taken

	res, err := provisionOnce(t, repo, testEndpointNaming(), validDedicatedInput())
	if err != nil {
		t.Fatalf("provision must survive a collision: %v", err)
	}
	if len(repo.mintedEndpointIDs) != 3 {
		t.Fatalf("save attempts = %d, want 3 (two collisions + the winner)", len(repo.mintedEndpointIDs))
	}
	seen := map[string]bool{}
	for _, id := range repo.mintedEndpointIDs {
		if seen[id] {
			t.Fatalf("retry re-used the colliding id %q instead of re-minting", id)
		}
		seen[id] = true
		if !mintedIDShape.MatchString(id) {
			t.Fatalf("attempt %q is not a well-formed id", id)
		}
	}
	if endpointOf(t, res) != repo.mintedEndpointIDs[len(repo.mintedEndpointIDs)-1] {
		t.Fatalf("stored endpoint %q is not the one that won", endpointOf(t, res))
	}
}

// TestEndpointCollisionRetryIsBounded — the retry must not loop forever. At
// ~4×10^8 combinations, endless collisions mean a broken entropy source or a
// broken store, and both are refusals.
func TestEndpointCollisionRetryIsBounded(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.endpointTaken = 1000 // every save collides

	_, err := provisionOnce(t, repo, testEndpointNaming(), validDedicatedInput())
	if !errors.Is(err, domain.ErrEndpointMintExhausted) {
		t.Fatalf("got %v, want ErrEndpointMintExhausted", err)
	}
	// The ceiling is a LITERAL, not endpointMintAttempts: a test that reads the
	// bound from the code under test cannot notice the bound being raised.
	const sane = 8
	attempts := len(repo.mintedEndpointIDs)
	if attempts < 2 || attempts > sane {
		t.Fatalf("attempts = %d, want a small bounded retry (2..%d)", attempts, sane)
	}
	if attempts != endpointMintAttempts {
		t.Fatalf("attempts = %d, want exactly the configured bound %d", attempts, endpointMintAttempts)
	}
}

// TestProvisionRefusesWithoutAConfiguredName — fail-closed, and BEFORE anything
// is created: an unconfigured host must not boot a VM and materialize a volume
// for a database that could never be given a servable permanent name.
func TestProvisionRefusesWithoutAConfiguredName(t *testing.T) {
	tests := []struct {
		name    string
		naming  domain.EndpointNaming
		wantErr error
	}{
		{"no zone", domain.EndpointNaming{Region: "eu-central"}, domain.ErrEndpointZoneRequired},
		{"no region", domain.EndpointNaming{Zone: "db.sentiae.test"}, domain.ErrEndpointRegionRequired},
		{"nothing configured", domain.EndpointNaming{}, domain.ErrEndpointZoneRequired},
		{"bogus zone", domain.EndpointNaming{Zone: "localhost", Region: "eu"}, domain.ErrEndpointZoneInvalid},
		{"bogus region", domain.EndpointNaming{Zone: "db.sentiae.test", Region: "eu central"}, domain.ErrEndpointRegionInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
			uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, &fakeVolumeBinder{}, testEngine(), tt.naming, nil, 0)

			_, err := uc.ProvisionDedicated(context.Background(), validDedicatedInput())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
			if prov.provisionCalls != 0 {
				t.Fatalf("a VM was provisioned for a resource that can have no name (calls=%d)", prov.provisionCalls)
			}
			if len(repo.mintedEndpointIDs) != 0 {
				t.Fatalf("a name was minted anyway: %v", repo.mintedEndpointIDs)
			}
		})
	}
}
