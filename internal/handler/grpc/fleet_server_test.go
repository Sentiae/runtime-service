package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// D-061 B2: the Provision org gate runs BEFORE the use case is invoked, so a
// zero-value FleetProvision (non-nil, never called on these paths) is a cheap
// seam. A deliberately-invalid workload_class forces the use case to return
// ErrUnsupportedClass (InvalidArgument) before it touches the nil repo, which
// lets the pass-through cases assert "shadow did not deny" without full wiring.
func newFleetServerForOrgTest() *FleetServer {
	return &FleetServer{provision: &usecase.FleetProvision{}}
}

func validProvisionReq(ownerOrg string) *runtimev1.ProvisionRequest {
	return &runtimev1.ProvisionRequest{
		OwnerOrg: ownerOrg,
		Descriptor_: &runtimev1.DeploymentDescriptor{
			Image: &runtimev1.OCIImageRef{
				Registry:   "reg.local",
				Repository: "app",
				Digest:     "sha256:deadbeef",
			},
			// Invalid class → use case returns ErrUnsupportedClass (InvalidArgument)
			// before any repo access, so the org gate is what we exercise.
			WorkloadClass: "bogus",
		},
	}
}

func TestFleetProvision_UnparseableOwnerOrg_InvalidArgument(t *testing.T) {
	s := newFleetServerForOrgTest()
	_, err := s.Provision(context.Background(), validProvisionReq("not-a-uuid"))
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", code, err)
	}
	if !strings.Contains(status.Convert(err).Message(), "owner_org is not a valid uuid") {
		t.Fatalf("expected owner_org parse message, got %q", status.Convert(err).Message())
	}
}

func TestFleetProvision_CarriageMismatch_InvalidArgument(t *testing.T) {
	s := newFleetServerForOrgTest()
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-organization-id", uuid.New().String()))
	_, err := s.Provision(ctx, validProvisionReq(uuid.New().String()))
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", code, err)
	}
	if !strings.Contains(status.Convert(err).Message(), "owner_org / x-organization-id mismatch") {
		t.Fatalf("expected carriage mismatch message, got %q", status.Convert(err).Message())
	}
}

func TestFleetProvision_CarriageMatch_PassesGate(t *testing.T) {
	s := newFleetServerForOrgTest()
	org := uuid.New().String()
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-organization-id", org))
	// Matching carriage clears the cross-check; the request then reaches the use
	// case and fails on the invalid class (not a mismatch/deny).
	_, err := s.Provision(ctx, validProvisionReq(org))
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument (unsupported class), got %s (%v)", code, err)
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "mismatch") {
		t.Fatalf("matching carriage should not report mismatch, got %q", msg)
	}
}

func TestFleetProvision_EmptyOwnerOrg_PassesThrough(t *testing.T) {
	s := newFleetServerForOrgTest()
	// Empty owner_org must behave exactly as today: skip the org gate, reach the
	// use case (which rejects the bogus class). No deny.
	_, err := s.Provision(context.Background(), validProvisionReq(""))
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument (unsupported class), got %s (%v)", code, err)
	}
	if code := status.Code(err); code == codes.PermissionDenied || code == codes.Unauthenticated {
		t.Fatalf("empty owner_org must not be denied, got %s", code)
	}
}

func TestFleetProvision_ShadowForeignOrg_DoesNotDeny(t *testing.T) {
	s := newFleetServerForOrgTest()
	// No principal on the context → under enforce this org would be denied
	// (Unauthenticated). In shadow (APP_AUTH_ORG_ENFORCE unset, default) the gate
	// is a no-op: the request reaches the use case and fails on the bogus class.
	_, err := s.Provision(context.Background(), validProvisionReq(uuid.New().String()))
	if code := status.Code(err); code == codes.PermissionDenied || code == codes.Unauthenticated {
		t.Fatalf("shadow mode must not deny a foreign org, got %s", code)
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument (unsupported class), got %s (%v)", code, err)
	}
}
