package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes. The handler is exercised over interface seams so no real
// provisioner/backend is needed (D-061 carriage + D-083 by-handle gates run in
// the handler, before any use case is invoked on the deny paths).
// ─────────────────────────────────────────────────────────────────────

type fakeDedicatedProvisioner struct {
	provisionCalled bool
	out             usecase.ProvisionDedicatedOutput
	status          usecase.ResourceStatus
	err             error
}

func (f *fakeDedicatedProvisioner) ProvisionDedicated(context.Context, usecase.ProvisionDedicatedInput) (usecase.ProvisionDedicatedOutput, error) {
	f.provisionCalled = true
	return f.out, f.err
}
func (f *fakeDedicatedProvisioner) StatusOf(context.Context, uuid.UUID) (usecase.ResourceStatus, error) {
	return f.status, f.err
}
func (f *fakeDedicatedProvisioner) DecommissionDedicated(context.Context, uuid.UUID, bool) error {
	return f.err
}

type fakeSharedProvisioner struct {
	provisionCalled bool
	out             usecase.ProvisionSharedOutput
	err             error
}

func (f *fakeSharedProvisioner) ProvisionShared(context.Context, usecase.ProvisionSharedInput) (usecase.ProvisionSharedOutput, error) {
	f.provisionCalled = true
	return f.out, f.err
}

// fakeResourceRepo serves a single known resource by handle; every other lookup
// is ErrResourceNotFound. The remaining methods are no-ops (the handler never
// calls them on these paths).
type fakeResourceRepo struct {
	res *domain.FleetResource
	rps []domain.FleetResourceRecoveryPoint
}

func (f *fakeResourceRepo) SaveResource(context.Context, *domain.FleetResource) error { return nil }
func (f *fakeResourceRepo) GetResourceByHandle(_ context.Context, id uuid.UUID) (*domain.FleetResource, error) {
	if f.res != nil && f.res.ID == id {
		cp := *f.res
		return &cp, nil
	}
	return nil, domain.ErrResourceNotFound
}
func (f *fakeResourceRepo) FindResource(context.Context, uuid.UUID, string, string) (*domain.FleetResource, error) {
	return nil, domain.ErrResourceNotFound
}
func (f *fakeResourceRepo) UpdateResourcePhase(context.Context, uuid.UUID, domain.FleetResourcePhase) error {
	return nil
}
func (f *fakeResourceRepo) ListExpiredShared(context.Context, time.Time) ([]domain.FleetResource, error) {
	return nil, nil
}
func (f *fakeResourceRepo) SaveRecoveryPoint(context.Context, *domain.FleetResourceRecoveryPoint) error {
	return nil
}
func (f *fakeResourceRepo) ListRecoveryPoints(context.Context, uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	return f.rps, nil
}

var _ repository.FleetResourceRepository = (*fakeResourceRepo)(nil)

// ─────────────────────────────────────────────────────────────────────
// D-061 owner-org carriage cross-check on ProvisionResource. Copied verbatim
// from FleetServer.Provision (shared requireCarriageMatch): a present, mismatched
// x-organization-id is InvalidArgument.
// ─────────────────────────────────────────────────────────────────────

func TestProvisionResource_CarriageMismatch_InvalidArgument(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-organization-id", uuid.New().String()))
	_, err := s.ProvisionResource(ctx, &runtimev1.ProvisionResourceRequest{
		OwnerOrg: uuid.New().String(), Tier: resourceTierDedicated, Class: resourceClassPostgres,
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("carriage mismatch: want InvalidArgument, got %s (%v)", code, err)
	}
}

func TestProvisionResource_UnparseableOwnerOrg_InvalidArgument(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})
	_, err := s.ProvisionResource(context.Background(), &runtimev1.ProvisionResourceRequest{
		OwnerOrg: "not-a-uuid", Tier: resourceTierDedicated,
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("unparseable owner_org: want InvalidArgument, got %s (%v)", code, err)
	}
}

// ProvisionResource org shadow-authz denies a foreign org under enforce mode
// (PermissionDenied), matching FleetServer.Provision.
func TestProvisionResource_ForeignOrg_Enforce_PermissionDenied(t *testing.T) {
	t.Setenv("APP_AUTH_ORG_ENFORCE", "true")
	ded := &fakeDedicatedProvisioner{}
	s := NewResourceServer(ded, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})
	// Caller principal is a DIFFERENT org than owner_org → denied before the
	// provisioner is invoked.
	ctx := ctxWithOrg(uuid.New())
	_, err := s.ProvisionResource(ctx, &runtimev1.ProvisionResourceRequest{
		OwnerOrg: uuid.New().String(), Tier: resourceTierDedicated, Class: resourceClassPostgres,
	})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Fatalf("foreign org under enforce: want PermissionDenied, got %s (%v)", code, err)
	}
	if ded.provisionCalled {
		t.Fatal("provisioner must not be invoked after an org deny")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Tier routing (shadow mode default): a valid claim reaches the matching
// provisioner; an unknown tier is InvalidArgument.
// ─────────────────────────────────────────────────────────────────────

func TestProvisionResource_TierRouting(t *testing.T) {
	owner := uuid.New().String()

	t.Run("dedicated routes to dedicated provisioner", func(t *testing.T) {
		ded := &fakeDedicatedProvisioner{out: usecase.ProvisionDedicatedOutput{Handle: "h1", Phase: "provisioning"}}
		shared := &fakeSharedProvisioner{}
		s := NewResourceServer(ded, shared, nil, &fakeResourceRepo{})
		resp, err := s.ProvisionResource(context.Background(), &runtimev1.ProvisionResourceRequest{
			OwnerOrg: owner, Tier: resourceTierDedicated, Class: resourceClassPostgres,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ded.provisionCalled || shared.provisionCalled {
			t.Fatalf("dedicated tier must call ONLY the dedicated provisioner (ded=%v shared=%v)", ded.provisionCalled, shared.provisionCalled)
		}
		if resp.GetHandle() != "h1" || resp.GetPhase() != "provisioning" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	t.Run("shared routes to shared provisioner", func(t *testing.T) {
		ded := &fakeDedicatedProvisioner{}
		shared := &fakeSharedProvisioner{out: usecase.ProvisionSharedOutput{Handle: "h2", Phase: "ready", Endpoint: "pg:5432"}}
		s := NewResourceServer(ded, shared, nil, &fakeResourceRepo{})
		resp, err := s.ProvisionResource(context.Background(), &runtimev1.ProvisionResourceRequest{
			OwnerOrg: owner, Tier: resourceTierShared, Class: resourceClassPostgres,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ded.provisionCalled || !shared.provisionCalled {
			t.Fatalf("shared tier must call ONLY the shared provisioner (ded=%v shared=%v)", ded.provisionCalled, shared.provisionCalled)
		}
		if resp.GetEndpoint() != "pg:5432" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	t.Run("unknown tier is InvalidArgument", func(t *testing.T) {
		s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})
		_, err := s.ProvisionResource(context.Background(), &runtimev1.ProvisionResourceRequest{
			OwnerOrg: owner, Tier: "quantum", Class: resourceClassPostgres,
		})
		if code := status.Code(err); code != codes.InvalidArgument {
			t.Fatalf("unknown tier: want InvalidArgument, got %s (%v)", code, err)
		}
	})

	t.Run("shared tier unavailable when not configured", func(t *testing.T) {
		// nil shared provisioner (the current DI posture, pending the shared-engine
		// credential decision) answers Unavailable, not a silent fake.
		s := NewResourceServer(&fakeDedicatedProvisioner{}, nil, nil, &fakeResourceRepo{})
		_, err := s.ProvisionResource(context.Background(), &runtimev1.ProvisionResourceRequest{
			OwnerOrg: owner, Tier: resourceTierShared, Class: resourceClassPostgres,
		})
		if code := status.Code(err); code != codes.Unavailable {
			t.Fatalf("unconfigured shared tier: want Unavailable, got %s (%v)", code, err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────
// D-083 by-handle caller-org gate on the status/decommission/list RPCs.
// ─────────────────────────────────────────────────────────────────────

func TestResourceHandleOps_OrgGate(t *testing.T) {
	t.Setenv("APP_AUTH_ORG_ENFORCE", "true")

	resID := uuid.New()
	ownerOrg := uuid.New()
	foreignOrg := uuid.New()
	unknownHandle := uuid.New().String()

	newServer := func() *ResourceServer {
		repo := &fakeResourceRepo{res: &domain.FleetResource{ID: resID, OwnerOrg: ownerOrg, Tier: resourceTierDedicated}}
		return NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, repo)
	}

	rpcs := []struct {
		name   string
		invoke func(s *ResourceServer, ctx context.Context, handle string) error
	}{
		{"GetResourceStatus", func(s *ResourceServer, ctx context.Context, h string) error {
			_, err := s.GetResourceStatus(ctx, &runtimev1.GetResourceStatusRequest{Handle: h})
			return err
		}},
		{"DecommissionResource", func(s *ResourceServer, ctx context.Context, h string) error {
			_, err := s.DecommissionResource(ctx, &runtimev1.DecommissionResourceRequest{Handle: h, FinalSnapshot: true})
			return err
		}},
		{"ListResourceRecoveryPoints", func(s *ResourceServer, ctx context.Context, h string) error {
			_, err := s.ListResourceRecoveryPoints(ctx, &runtimev1.ListResourceRecoveryPointsRequest{Handle: h})
			return err
		}},
	}

	cases := []struct {
		name      string
		handle    string
		callerOrg uuid.UUID
		wantCode  codes.Code
	}{
		{"owner org allowed", resID.String(), ownerOrg, codes.OK},
		{"foreign org denied", resID.String(), foreignOrg, codes.PermissionDenied},
		{"unknown handle not found", unknownHandle, ownerOrg, codes.NotFound},
	}

	for _, rpc := range rpcs {
		for _, tc := range cases {
			t.Run(rpc.name+"/"+tc.name, func(t *testing.T) {
				err := rpc.invoke(newServer(), ctxWithOrg(tc.callerOrg), tc.handle)
				if code := status.Code(err); code != tc.wantCode {
					t.Fatalf("%s(%s): want %s, got %s (%v)", rpc.name, tc.name, tc.wantCode, code, err)
				}
			})
		}
	}
}

func TestResourceHandle_InvalidUUID_InvalidArgument(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})
	_, err := s.GetResourceStatus(context.Background(), &runtimev1.GetResourceStatusRequest{Handle: "not-a-uuid"})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("invalid handle: want InvalidArgument, got %s (%v)", code, err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Unimplemented verbs (caps report them false).
// ─────────────────────────────────────────────────────────────────────

func TestResourceUnimplementedVerbs(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, &fakeResourceRepo{})

	if _, err := s.RestoreResource(context.Background(), &runtimev1.RestoreResourceRequest{Handle: uuid.New().String()}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("RestoreResource: want Unimplemented, got %s (%v)", status.Code(err), err)
	}
	if _, err := s.RotateResourceCredentials(context.Background(), &runtimev1.RotateResourceCredentialsRequest{Handle: uuid.New().String()}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("RotateResourceCredentials: want Unimplemented, got %s (%v)", status.Code(err), err)
	}
}

// GetResourceCapabilities reports honest caps: restore + rotation always false
// (Unimplemented); the shared tier only when its provisioner is wired.
func TestGetResourceCapabilities_Honest(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, nil, nil, &fakeResourceRepo{})
	resp, err := s.GetResourceCapabilities(context.Background(), &runtimev1.GetResourceCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetClasses()) != 1 {
		t.Fatalf("want one class, got %d", len(resp.GetClasses()))
	}
	c := resp.GetClasses()[0]
	if c.GetClass() != resourceClassPostgres {
		t.Fatalf("want class postgres, got %q", c.GetClass())
	}
	if c.GetSupportsRestore() || c.GetSupportsCredentialRotation() {
		t.Fatal("restore + credential rotation must be reported false (Unimplemented in v1)")
	}
	if c.GetSupportsSnapshot() {
		t.Fatal("snapshot must be false when no snapshotter is wired")
	}
	for _, tier := range c.GetTiers() {
		if tier == resourceTierShared {
			t.Fatal("shared tier must not be advertised when its provisioner is nil")
		}
	}
}
