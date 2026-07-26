package grpc

import (
	"fmt"
	"testing"

	"github.com/sentiae/runtime-service/internal/app"
	"github.com/sentiae/runtime-service/internal/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The standard-ha refusals and the host placement facts must reach a caller as
// their own codes with their own curated messages.
//
// FailedPrecondition for the placement refusals, because the claim is legitimate
// and retrying it unchanged succeeds as soon as the FLEET can satisfy it —
// InvalidArgument would tell a caller to change a request that is already correct,
// and Internal would tell them to file a bug about a fleet that is simply too
// small. The message must name the UNMET condition: "HA unavailable" would send an
// operator shopping for hardware they may already own.
func TestFleetErrorMapsStandardHASentinels(t *testing.T) {
	app.RegisterErrors()

	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"one machine is one failure domain", domain.ErrHAHostsInsufficient, codes.FailedPrecondition},
		{"domains never stated", domain.ErrHAFailureDomainUnattested, codes.FailedPrecondition},
		{"a second host is not a second domain", domain.ErrHAFailureDomainShared, codes.FailedPrecondition},
		{"the standby must be same-region", domain.ErrHARegionSplit, codes.FailedPrecondition},
		{"the invariant cannot be evaluated", domain.ErrHAPlacementUnknowable, codes.FailedPrecondition},
		{"an unknown availability class is caller input", domain.ErrHAAvailabilityClassInvalid, codes.InvalidArgument},
		{"a host that states no failure domain", domain.ErrHostFailureDomainRequired, codes.InvalidArgument},
		{"a host whose failure domain is a bare label", domain.ErrHostFailureDomainInvalid, codes.InvalidArgument},
		{"a host that states no region", domain.ErrHostRegionRequired, codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Wrapped, because the usecase wraps on the way out: the %w chain is what
			// errors.Is has to walk, and matching only on a bare sentinel would pass
			// while production fell through to Internal.
			got := fleetError(fmt.Errorf("provision dedicated engine: %w", tt.err))
			if status.Code(got) != tt.want {
				t.Fatalf("code = %v, want %v (err=%v)", status.Code(got), tt.want, got)
			}
			if status.Convert(got).Message() == "internal server error" {
				t.Fatal("the curated message was lost — this refusal is indistinguishable from a server bug")
			}
		})
	}
}

// The D-190 naming refusals used to reach a caller as Internal "internal server
// error" via resourceError's default branch, logged as "resource op failed
// (unmapped)" — so a FLEET MISCONFIGURATION (an unset APP_RESOURCE_ENDPOINT_ZONE
// or _REGION) was wire-indistinguishable from a nil-pointer panic, at exactly the
// moment an operator needed to be told which env key to set.
func TestResourceErrorMapsEndpointNamingRefusals(t *testing.T) {
	app.RegisterErrors()

	for _, err := range []error{
		domain.ErrEndpointZoneRequired,
		domain.ErrEndpointRegionRequired,
		domain.ErrEndpointZoneInvalid,
		domain.ErrEndpointRegionInvalid,
	} {
		t.Run(err.Error(), func(t *testing.T) {
			got := resourceError(fmt.Errorf("mint resource endpoint: %w", err))
			if status.Code(got) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition (err=%v)", status.Code(got), got)
			}
			if status.Convert(got).Message() == "internal server error" {
				t.Fatal("a host misconfiguration must not answer with the generic Internal message")
			}
		})
	}
}
