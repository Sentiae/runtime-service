package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sentiae/platform-kit/middleware"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
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

// ─────────────────────────────────────────────────────────────────────
// #fleet-handle-ops-org-check (D-083): the by-handle caller-org gate on
// Health / Decommission / Scale. A leaked handle must not let a foreign org act
// on another org's app — the gate mirrors Provision's owner_org authorization.
// ─────────────────────────────────────────────────────────────────────

// fakeAppRepo serves a single known app by ID; every other app lookup is
// not-found. Mutating methods are no-ops so the reconcile path (Decommission /
// Scale) completes without real infrastructure.
type fakeAppRepo struct{ app *domain.FleetApp }

func (f *fakeAppRepo) Create(context.Context, *domain.FleetApp) error { return nil }
func (f *fakeAppRepo) Update(context.Context, *domain.FleetApp) error { return nil }
func (f *fakeAppRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	if f.app != nil && f.app.ID == id {
		cp := *f.app
		return &cp, nil
	}
	return nil, domain.ErrFleetAppNotFound
}
func (f *fakeAppRepo) FindByComponentEnv(context.Context, string, string) (*domain.FleetApp, error) {
	return nil, domain.ErrFleetAppNotFound
}
func (f *fakeAppRepo) List(context.Context) ([]domain.FleetApp, error) { return nil, nil }
func (f *fakeAppRepo) Delete(context.Context, uuid.UUID) error         { return nil }

// fakeReplicaRepo reports no replicas so the reconcile path has no shortfall to
// place (no scheduler needed) and health aggregates to an empty set.
type fakeReplicaRepo struct{}

func (fakeReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (fakeReplicaRepo) Update(context.Context, *domain.Replica) error { return nil }
func (fakeReplicaRepo) FindByID(context.Context, uuid.UUID) (*domain.Replica, error) {
	return nil, domain.ErrReplicaNotFound
}
func (fakeReplicaRepo) ListByApp(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (fakeReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (fakeReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (fakeReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }

// fakeWorkloadRepo has no workloads — every by-handle lookup is not-found, so an
// unknown handle fails closed with ErrWorkloadNotFound.
type fakeWorkloadRepo struct{}

func (fakeWorkloadRepo) Create(context.Context, *domain.ImageWorkload) error { return nil }
func (fakeWorkloadRepo) Update(context.Context, *domain.ImageWorkload) error { return nil }
func (fakeWorkloadRepo) FindByID(context.Context, uuid.UUID) (*domain.ImageWorkload, error) {
	return nil, domain.ErrWorkloadNotFound
}
func (fakeWorkloadRepo) FindActive(context.Context) ([]domain.ImageWorkload, error) { return nil, nil }
func (fakeWorkloadRepo) Delete(context.Context, uuid.UUID) error                    { return nil }
func (fakeWorkloadRepo) FindByIdempotencyKey(context.Context, string, string) (*domain.ImageWorkload, error) {
	return nil, domain.ErrWorkloadNotFound
}
func (fakeWorkloadRepo) IsDuplicateKey(error) bool { return false }

var (
	_ repository.FleetAppRepository      = (*fakeAppRepo)(nil)
	_ repository.ReplicaRepository       = fakeReplicaRepo{}
	_ repository.ImageWorkloadRepository = fakeWorkloadRepo{}
)

// newFleetServerWithApp wires a FleetServer whose provision use case knows one
// resident app (owned by ownerOrg) via a real orchestrator over fakes. Unknown
// handles fall through to the (empty) workload repo → not-found.
func newFleetServerWithApp(appID, ownerOrg uuid.UUID) *FleetServer {
	prov := usecase.NewFleetProvision(context.Background(), fakeWorkloadRepo{}, nil, nil, "", "")
	app := &domain.FleetApp{ID: appID, OwnerOrg: ownerOrg.String()}
	orch := usecase.NewFleetOrchestrator(&fakeAppRepo{app: app}, fakeReplicaRepo{}, nil, nil)
	prov.SetOrchestrator(orch)
	return &FleetServer{provision: prov}
}

// ctxWithOrg stamps a user principal whose sole org is org (CanActInOrg(org) →
// true). uuid.Nil yields an empty context (no principal).
func ctxWithOrg(org uuid.UUID) context.Context {
	if org == uuid.Nil {
		return context.Background()
	}
	return tenant.ContextWithPrincipal(context.Background(),
		tenant.Principal{Claims: &middleware.Claims{OrganizationID: org.String()}})
}

// TestFleetHandleOps_OrgGate exercises the caller-org gate on each by-handle RPC
// (Health / Decommission / Scale) in enforce mode: the owning-org caller is
// allowed, a foreign-org caller is denied with the same code Provision yields
// (PermissionDenied), and an unknown handle fails closed with NotFound.
func TestFleetHandleOps_OrgGate(t *testing.T) {
	t.Setenv("APP_AUTH_ORG_ENFORCE", "true")

	appID := uuid.New()
	ownerOrg := uuid.New()
	foreignOrg := uuid.New()
	unknownHandle := uuid.New().String()

	// invoke drives one by-handle RPC against handle under the caller ctx.
	rpcs := []struct {
		name   string
		invoke func(s *FleetServer, ctx context.Context, handle string) error
	}{
		{"Health", func(s *FleetServer, ctx context.Context, handle string) error {
			_, err := s.Health(ctx, &runtimev1.FleetHealthRequest{Handle: handle})
			return err
		}},
		{"Decommission", func(s *FleetServer, ctx context.Context, handle string) error {
			_, err := s.Decommission(ctx, &runtimev1.FleetDecommissionRequest{Handle: handle})
			return err
		}},
		{"Scale", func(s *FleetServer, ctx context.Context, handle string) error {
			// replicas=0 drains — the reconcile path needs no scheduler.
			_, err := s.Scale(ctx, &runtimev1.FleetScaleRequest{Handle: handle, Replicas: 0})
			return err
		}},
	}

	cases := []struct {
		name      string
		handle    string
		callerOrg uuid.UUID
		wantCode  codes.Code
	}{
		{"owner org allowed", appID.String(), ownerOrg, codes.OK},
		{"foreign org denied", appID.String(), foreignOrg, codes.PermissionDenied},
		{"unknown handle not found", unknownHandle, ownerOrg, codes.NotFound},
	}

	for _, rpc := range rpcs {
		for _, tc := range cases {
			t.Run(rpc.name+"/"+tc.name, func(t *testing.T) {
				s := newFleetServerWithApp(appID, ownerOrg)
				err := rpc.invoke(s, ctxWithOrg(tc.callerOrg), tc.handle)
				if code := status.Code(err); code != tc.wantCode {
					t.Fatalf("%s(%s): want %s, got %s (%v)", rpc.name, tc.name, tc.wantCode, code, err)
				}
			})
		}
	}
}

// TestFleetHandleOps_ShadowForeignOrg_DoesNotDeny confirms the gate is
// ship-neutral in shadow mode (default): a foreign-org caller is NOT denied — it
// reaches the use case exactly as before the gate was added.
func TestFleetHandleOps_ShadowForeignOrg_DoesNotDeny(t *testing.T) {
	// APP_AUTH_ORG_ENFORCE unset → shadow.
	appID := uuid.New()
	ownerOrg := uuid.New()
	foreignOrg := uuid.New()
	s := newFleetServerWithApp(appID, ownerOrg)

	_, err := s.Health(ctxWithOrg(foreignOrg), &runtimev1.FleetHealthRequest{Handle: appID.String()})
	if code := status.Code(err); code == codes.PermissionDenied || code == codes.Unauthenticated {
		t.Fatalf("shadow mode must not deny a foreign org, got %s (%v)", code, err)
	}
}
