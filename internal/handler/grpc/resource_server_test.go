package grpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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
	finalRP         *domain.FleetResourceRecoveryPoint
	err             error
}

func (f *fakeDedicatedProvisioner) ProvisionDedicated(context.Context, usecase.ProvisionDedicatedInput) (usecase.ProvisionDedicatedOutput, error) {
	f.provisionCalled = true
	return f.out, f.err
}
func (f *fakeDedicatedProvisioner) StatusOf(context.Context, uuid.UUID) (usecase.ResourceStatus, error) {
	return f.status, f.err
}
func (f *fakeDedicatedProvisioner) DecommissionDedicated(context.Context, uuid.UUID, bool) (*domain.FleetResourceRecoveryPoint, error) {
	return f.finalRP, f.err
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
func (f *fakeResourceRepo) CompareAndSwapPhase(context.Context, uuid.UUID, []domain.FleetResourcePhase, domain.FleetResourcePhase) (bool, error) {
	return true, nil
}
func (f *fakeResourceRepo) SetResourceLastError(context.Context, uuid.UUID, string) error { return nil }
func (f *fakeResourceRepo) ListResourcesByPhase(context.Context, domain.FleetResourcePhase) ([]domain.FleetResource, error) {
	return nil, nil
}

// GetRecoveryPointByRef filters on BOTH resource_id and object_key, exactly like
// the postgres repo: a ref belonging to another resource must not resolve.
func (f *fakeResourceRepo) GetRecoveryPointByRef(_ context.Context, resourceID uuid.UUID, objectKey string) (*domain.FleetResourceRecoveryPoint, error) {
	for i := range f.rps {
		if f.rps[i].ResourceID == resourceID && f.rps[i].ObjectKey == objectKey {
			cp := f.rps[i]
			return &cp, nil
		}
	}
	return nil, domain.ErrRecoveryPointNotFound
}
func (f *fakeResourceRepo) MarkRecoveryPointRestoredInPlace(context.Context, uuid.UUID) error {
	return nil
}

var _ repository.FleetResourceRepository = (*fakeResourceRepo)(nil)

// ─────────────────────────────────────────────────────────────────────
// D-061 owner-org carriage cross-check on ProvisionResource. Copied verbatim
// from FleetServer.Provision (shared requireCarriageMatch): a present, mismatched
// x-organization-id is InvalidArgument.
// ─────────────────────────────────────────────────────────────────────

func TestProvisionResource_CarriageMismatch_InvalidArgument(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
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
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
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
	s := NewResourceServer(ded, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
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
		s := NewResourceServer(ded, shared, nil, nil, &fakeResourceRepo{})
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
		s := NewResourceServer(ded, shared, nil, nil, &fakeResourceRepo{})
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
		s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
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
		s := NewResourceServer(&fakeDedicatedProvisioner{}, nil, nil, nil, &fakeResourceRepo{})
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
		return NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, repo)
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
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
	_, err := s.GetResourceStatus(context.Background(), &runtimev1.GetResourceStatusRequest{Handle: "not-a-uuid"})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("invalid handle: want InvalidArgument, got %s (%v)", code, err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Unimplemented verbs (caps report them false).
// ─────────────────────────────────────────────────────────────────────

func TestResourceUnimplementedVerbs(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})

	if _, err := s.RotateResourceCredentials(context.Background(), &runtimev1.RotateResourceCredentialsRequest{Handle: uuid.New().String()}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("RotateResourceCredentials: want Unimplemented, got %s (%v)", status.Code(err), err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The snapshot-first guarantee is only verifiable if the teardown REPORTS the
// recovery point it took. A bare OK proved a call happened, not that anything
// exists to restore from.
// ─────────────────────────────────────────────────────────────────────

func TestDecommissionResource_ReportsFinalRecoveryPoint(t *testing.T) {
	resID := uuid.New()
	takenAt := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name    string
		finalRP *domain.FleetResourceRecoveryPoint
		wantRef string
	}{
		{
			name: "final snapshot is reported back to the caller",
			finalRP: &domain.FleetResourceRecoveryPoint{
				ID: uuid.New(), ResourceID: resID, ObjectKey: "volumes/v1/final.ext4",
				Kind: "snapshot", CreatedAt: takenAt,
			},
			wantRef: "volumes/v1/final.ext4",
		},
		{
			// No final snapshot asked for / already a tombstone: nothing to report,
			// and the field stays unset rather than carrying an empty stand-in.
			name:    "no recovery point leaves the field unset",
			finalRP: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeResourceRepo{res: &domain.FleetResource{ID: resID, Tier: resourceTierDedicated}}
			s := NewResourceServer(&fakeDedicatedProvisioner{finalRP: tt.finalRP}, &fakeSharedProvisioner{}, nil, nil, repo)

			resp, err := s.DecommissionResource(context.Background(), &runtimev1.DecommissionResourceRequest{
				Handle: resID.String(), FinalSnapshot: true,
			})
			if err != nil {
				t.Fatalf("decommission: %v", err)
			}
			if tt.wantRef == "" {
				if resp.GetFinalRecoveryPoint() != nil {
					t.Fatalf("want no final recovery point, got %+v", resp.GetFinalRecoveryPoint())
				}
				return
			}
			got := resp.GetFinalRecoveryPoint()
			if got == nil {
				t.Fatal("a teardown that took a final snapshot must report it")
			}
			if got.GetRef() != tt.wantRef || got.GetKind() != "snapshot" {
				t.Fatalf("final recovery point = %+v", got)
			}
			if !got.GetAt().AsTime().Equal(takenAt) {
				t.Fatalf("taken at = %v, want %v", got.GetAt().AsTime(), takenAt)
			}
		})
	}
}

// A failed final snapshot must be LEGIBLE on the wire. The refusal is correct and
// unchanged; what changes is that the caller learns why instead of getting a bare
// Internal that looks like a crash (#resource-final-snapshot-failure-is-a-bare-500).
// The unknown-error row keeps the unmapped bucket honest: this removes ONE case
// from it, not the bucket.
func TestDecommissionResource_ErrorMapping(t *testing.T) {
	resID := uuid.New()

	tests := []struct {
		name        string
		err         error
		wantCode    codes.Code
		wantMsg     string
		wantUnmappedLog bool
	}{
		{
			name:     "missing backing file names the condition without leaking the path",
			err:      fmt.Errorf("final snapshot: upload snapshot: %w: /var/lib/fleet/volumes/abc.ext4: %w", domain.ErrVolumeBackingFileMissing, os.ErrNotExist),
			wantCode: codes.FailedPrecondition,
			wantMsg:  "the volume's backing file is missing, so a final snapshot cannot be taken",
		},
		{
			// The sibling refusal path is untouched.
			name:     "zero recovery points still refuses with its own message",
			err:      fmt.Errorf("%w: resource %s produced NO recovery point", domain.ErrResourceFinalSnapshotRequired, resID),
			wantCode: codes.FailedPrecondition,
			wantMsg:  "a durable resource requires a final snapshot to decommission",
		},
		{
			name:           "a genuinely unmapped error is still Internal and still logged",
			err:            errors.New("some brand new fleet failure"),
			wantCode:       codes.Internal,
			wantUnmappedLog: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logbuf bytes.Buffer
			log.SetOutput(&logbuf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			repo := &fakeResourceRepo{res: &domain.FleetResource{ID: resID, Tier: resourceTierDedicated}}
			s := NewResourceServer(&fakeDedicatedProvisioner{err: tt.err}, &fakeSharedProvisioner{}, nil, nil, repo)

			_, err := s.DecommissionResource(context.Background(), &runtimev1.DecommissionResourceRequest{
				Handle: resID.String(), FinalSnapshot: true,
			})
			st, _ := status.FromError(err)
			if st.Code() != tt.wantCode {
				t.Fatalf("code = %s, want %s (%v)", st.Code(), tt.wantCode, err)
			}
			if tt.wantMsg != "" && st.Message() != tt.wantMsg {
				t.Fatalf("message = %q, want %q", st.Message(), tt.wantMsg)
			}
			// Tenant-visible messages must not carry the host path or the OS text.
			if strings.Contains(st.Message(), "/var/lib") || strings.Contains(st.Message(), "no such file") {
				t.Errorf("curated message leaks host detail: %q", st.Message())
			}
			logged := strings.Contains(logbuf.String(), "resource op failed (unmapped)")
			if logged != tt.wantUnmappedLog {
				t.Errorf("unmapped log fired = %v, want %v (log: %q)", logged, tt.wantUnmappedLog, logbuf.String())
			}
		})
	}
}

// A resource whose health cannot be read reports a CONDITION, not an error: the
// status RPC is how an operator sees a stuck resource at all.
func TestGetResourceStatus_SurfacesConditions(t *testing.T) {
	resID := uuid.New()
	repo := &fakeResourceRepo{res: &domain.FleetResource{ID: resID, Tier: resourceTierDedicated}}
	dedicated := &fakeDedicatedProvisioner{status: usecase.ResourceStatus{
		Handle:     resID.String(),
		Phase:      string(domain.FleetResourcePhaseDegraded),
		Conditions: []string{"backing-app-missing"},
	}}
	s := NewResourceServer(dedicated, &fakeSharedProvisioner{}, nil, nil, repo)

	resp, err := s.GetResourceStatus(context.Background(), &runtimev1.GetResourceStatusRequest{Handle: resID.String()})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(resp.GetConditions()) != 1 || resp.GetConditions()[0] != "backing-app-missing" {
		t.Fatalf("conditions = %v, want [backing-app-missing]", resp.GetConditions())
	}
}

// ─────────────────────────────────────────────────────────────────────
// RestoreResource (D-184). The org gate runs first, then the ref is resolved
// STRICTLY within the authorized resource.
// ─────────────────────────────────────────────────────────────────────

// fakeRestorer records the input the handler hands the use case.
type fakeRestorer struct {
	called bool
	in     usecase.RestoreResourceInput
	out    usecase.RestoreResourceOutput
	err    error
}

func (f *fakeRestorer) Restore(_ context.Context, in usecase.RestoreResourceInput) (usecase.RestoreResourceOutput, error) {
	f.called = true
	f.in = in
	return f.out, f.err
}

func TestRestoreResource_RefResolution(t *testing.T) {
	t.Setenv("APP_AUTH_ORG_ENFORCE", "true")

	resID := uuid.New()
	ownerOrg := uuid.New()
	foreignResID := uuid.New()
	ownRef := "volumes/v1/rp-own.ext4"
	foreignRef := "volumes/v2/rp-foreign.ext4"

	newServer := func(r *fakeRestorer) *ResourceServer {
		repo := &fakeResourceRepo{
			res: &domain.FleetResource{ID: resID, OwnerOrg: ownerOrg, Tier: resourceTierDedicated},
			rps: []domain.FleetResourceRecoveryPoint{
				{ID: uuid.New(), ResourceID: resID, ObjectKey: ownRef},
				// A recovery point of ANOTHER resource, present in the same catalog.
				{ID: uuid.New(), ResourceID: foreignResID, ObjectKey: foreignRef},
			},
		}
		return NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, r, repo)
	}

	cases := []struct {
		name        string
		ref         string
		callerOrg   uuid.UUID
		wantCode    codes.Code
		wantInvoked bool
	}{
		{"own ref admitted", ownRef, ownerOrg, codes.OK, true},
		{"foreign resource's ref is NOT resolvable", foreignRef, ownerOrg, codes.NotFound, false},
		{"unknown ref", "volumes/v9/nope.ext4", ownerOrg, codes.NotFound, false},
		{"foreign caller denied before any ref lookup", ownRef, uuid.New(), codes.PermissionDenied, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRestorer{out: usecase.RestoreResourceOutput{Handle: resID.String(), Phase: "restoring"}}
			resp, err := newServer(r).RestoreResource(ctxWithOrg(tc.callerOrg), &runtimev1.RestoreResourceRequest{
				Handle: resID.String(), RecoveryPointRef: tc.ref,
			})
			if code := status.Code(err); code != tc.wantCode {
				t.Fatalf("want %s, got %s (%v)", tc.wantCode, code, err)
			}
			if r.called != tc.wantInvoked {
				t.Fatalf("restorer invoked = %v, want %v", r.called, tc.wantInvoked)
			}
			if tc.wantCode != codes.OK {
				return
			}
			if resp.GetPhase() != "restoring" || resp.GetHandle() != resID.String() {
				t.Fatalf("unexpected response: %+v", resp)
			}
			if r.in.Resource == nil || r.in.Resource.ID != resID {
				t.Fatalf("use case must receive the authorized resource, got %+v", r.in.Resource)
			}
			if r.in.RecoveryPoint == nil || r.in.RecoveryPoint.ResourceID != resID {
				t.Fatalf("use case must receive a recovery point of the SAME resource, got %+v", r.in.RecoveryPoint)
			}
		})
	}
}

func TestRestoreResource_NotConfigured_Unavailable(t *testing.T) {
	s := NewResourceServer(&fakeDedicatedProvisioner{}, &fakeSharedProvisioner{}, nil, nil, &fakeResourceRepo{})
	_, err := s.RestoreResource(context.Background(), &runtimev1.RestoreResourceRequest{Handle: uuid.New().String()})
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("unwired restorer: want Unavailable, got %s (%v)", code, err)
	}
}

// GetResourceCapabilities reports honest caps: rotation always false
// (Unimplemented); snapshot/restore only when their use cases are wired; the
// shared tier only when its provisioner is wired.
func TestGetResourceCapabilities_Honest(t *testing.T) {
	t.Run("nothing wired", func(t *testing.T) {
		s := NewResourceServer(&fakeDedicatedProvisioner{}, nil, nil, nil, &fakeResourceRepo{})
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
		if c.GetSupportsCredentialRotation() {
			t.Fatal("credential rotation must be reported false (Unimplemented in v1)")
		}
		if c.GetSupportsRestore() {
			t.Fatal("restore must be false when no restorer is wired (e.g. no artifact store)")
		}
		if c.GetSupportsSnapshot() {
			t.Fatal("snapshot must be false when no snapshotter is wired")
		}
		for _, tier := range c.GetTiers() {
			if tier == resourceTierShared {
				t.Fatal("shared tier must not be advertised when its provisioner is nil")
			}
		}
	})

	t.Run("restorer wired", func(t *testing.T) {
		s := NewResourceServer(&fakeDedicatedProvisioner{}, nil, nil, &fakeRestorer{}, &fakeResourceRepo{})
		resp, err := s.GetResourceCapabilities(context.Background(), &runtimev1.GetResourceCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.GetClasses()[0].GetSupportsRestore() {
			t.Fatal("restore must be true once the restorer is wired")
		}
	})
}
