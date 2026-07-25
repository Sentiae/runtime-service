package grpc

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pkconfig "github.com/sentiae/platform-kit/config"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// resourceClassPostgres is the only resource class the fleet provisions this
// slice (mirrors the use-case constant; the wire filter compares against it).
const resourceClassPostgres = "postgres"

// dedicatedResourceProvisioner is the subset of *usecase.FleetResourceProvisioner
// the handler drives. An interface keeps the handler unit-testable with a fake.
type dedicatedResourceProvisioner interface {
	ProvisionDedicated(ctx context.Context, in usecase.ProvisionDedicatedInput) (usecase.ProvisionDedicatedOutput, error)
	StatusOf(ctx context.Context, resourceID uuid.UUID) (usecase.ResourceStatus, error)
	DecommissionDedicated(ctx context.Context, resourceID uuid.UUID, finalSnapshot bool) (*domain.FleetResourceRecoveryPoint, error)
}

// sharedResourceProvisioner is the subset of *usecase.FleetResourceSharedProvisioner
// the handler drives for the shared logical-database tier.
type sharedResourceProvisioner interface {
	ProvisionShared(ctx context.Context, in usecase.ProvisionSharedInput) (usecase.ProvisionSharedOutput, error)
}

// ResourceSnapshotPort is the subset of *usecase.FleetVolumeSnapshotter the
// standalone SnapshotResource RPC drives. Exported so the DI container can hold
// a genuinely-nil interface for an unwired host (a typed nil pointer wrapped in
// an interface would defeat the handler's Unavailable guard).
type ResourceSnapshotPort interface {
	SnapshotAppVolumes(ctx context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error)
}

// ResourceRestorePort is the subset of *usecase.FleetVolumeRestorer the
// RestoreResource RPC drives (D-184, in-place restore only). Exported for the
// same nil-interface reason as ResourceSnapshotPort.
type ResourceRestorePort interface {
	Restore(ctx context.Context, in usecase.RestoreResourceInput) (usecase.RestoreResourceOutput, error)
}

// ResourceServer implements the P19 ResourceProvisioning gRPC service (CP4.5
// §9 #3, D-183) — the net-new resource control-plane seam delivery drives to
// claim, snapshot, and reclaim managed resources. It is distinct from the frozen
// FleetOrchestration P7 workload seam.
type ResourceServer struct {
	runtimev1.UnimplementedResourceProvisioningServer
	dedicated   dedicatedResourceProvisioner
	shared      sharedResourceProvisioner
	snapshotter ResourceSnapshotPort
	restorer    ResourceRestorePort
	resources   repository.FleetResourceRepository
}

// NewResourceServer constructs the handler. shared, snapshotter and restorer may
// be nil on a host that does not serve those tiers (e.g. a non-firecracker host
// has no snapshotter, and a host with no artifact store cannot fetch a recovery
// point); the corresponding RPCs then answer Unavailable rather than silently
// faking a result.
func NewResourceServer(
	dedicated dedicatedResourceProvisioner,
	shared sharedResourceProvisioner,
	snapshotter ResourceSnapshotPort,
	restorer ResourceRestorePort,
	resources repository.FleetResourceRepository,
) *ResourceServer {
	return &ResourceServer{
		dedicated:   dedicated,
		shared:      shared,
		snapshotter: snapshotter,
		restorer:    restorer,
		resources:   resources,
	}
}

// GetResourceCapabilities reports the classes/tiers the fleet can honestly
// provision on this host. supports_snapshot/restore/rotation reflect what is
// actually wired: credential rotation is Unimplemented in v1 so it is always
// false; snapshot is true only when a snapshotter is wired; restore is true only
// when the restorer is (it needs both the orchestrator and a configured artifact
// store); the shared tier is advertised only when its provisioner is wired.
func (s *ResourceServer) GetResourceCapabilities(_ context.Context, req *runtimev1.GetResourceCapabilitiesRequest) (*runtimev1.GetResourceCapabilitiesResponse, error) {
	if filter := req.GetClass(); filter != "" && filter != resourceClassPostgres {
		return &runtimev1.GetResourceCapabilitiesResponse{}, nil
	}
	tiers := make([]string, 0, 2)
	if s.dedicated != nil {
		tiers = append(tiers, resourceTierDedicated)
	}
	if s.shared != nil {
		tiers = append(tiers, resourceTierShared)
	}
	return &runtimev1.GetResourceCapabilitiesResponse{
		Classes: []*runtimev1.ResourceClassCapability{{
			Class:                      resourceClassPostgres,
			Tiers:                      tiers,
			SupportsSnapshot:           s.snapshotter != nil,
			SupportsRestore:            s.restorer != nil,
			SupportsCredentialRotation: false,
		}},
	}, nil
}

// resourceTierDedicated / resourceTierShared name the two tiers the wire router
// dispatches on (mirror the use-case constants).
const (
	resourceTierDedicated = "dedicated"
	resourceTierShared    = "shared"
)

// ProvisionResource claims a managed resource. It runs the D-061 owner-org
// carriage cross-check + shadow-authz (identical to FleetServer.Provision), then
// routes by tier to the dedicated or shared provisioner.
func (s *ResourceServer) ProvisionResource(ctx context.Context, req *runtimev1.ProvisionResourceRequest) (*runtimev1.ProvisionResourceResponse, error) {
	// D-061 verified-org boundary: the caller-supplied owner_org feeds the secret
	// tenant downstream, so cross-check it against the attested x-organization-id
	// carriage and shadow-authorize it with the SAME enforce flag the workload
	// seam uses.
	ownerOrgRaw := req.GetOwnerOrg()
	if err := requireCarriageMatch(ctx, ownerOrgRaw); err != nil {
		return nil, err
	}
	if ownerOrgRaw != "" {
		ownerOrg, perr := uuid.Parse(ownerOrgRaw)
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, "owner_org is not a valid uuid")
		}
		if err := tenant.AuthorizeOrgShadow(ctx, ownerOrg, pkconfig.OrgEnforce()); err != nil {
			return nil, err
		}
		ctx = tenant.WithActiveOrg(ctx, ownerOrg)
	}

	switch req.GetTier() {
	case resourceTierDedicated:
		if s.dedicated == nil {
			return nil, status.Error(codes.Unavailable, "dedicated resource provisioner not configured")
		}
		out, err := s.dedicated.ProvisionDedicated(ctx, usecase.ProvisionDedicatedInput{
			OwnerOrg:   ownerOrgRaw,
			ClaimKey:   req.GetClaimKey(),
			Env:        req.GetEnv(),
			Revision:   int(req.GetRevision()),
			Class:      req.GetClass(),
			Tier:       req.GetTier(),
			SystemID:   req.GetSystemId(),
			SecretRefs: req.GetSecretRefs(),
			VaultToken: req.GetVaultToken(),
			SizeMB:     req.GetSizeMb(),
		})
		if err != nil {
			return nil, resourceError(err)
		}
		return &runtimev1.ProvisionResourceResponse{Handle: out.Handle, Phase: out.Phase}, nil
	case resourceTierShared:
		if s.shared == nil {
			return nil, status.Error(codes.Unavailable, "shared resource provisioner not configured")
		}
		out, err := s.shared.ProvisionShared(ctx, usecase.ProvisionSharedInput{
			OwnerOrg:     ownerOrgRaw,
			ClaimKey:     req.GetClaimKey(),
			Env:          req.GetEnv(),
			Revision:     int(req.GetRevision()),
			Class:        req.GetClass(),
			Tier:         req.GetTier(),
			SecretRefs:   req.GetSecretRefs(),
			VaultToken:   req.GetVaultToken(),
			SeedTemplate: req.GetSeedTemplateKey(),
		})
		if err != nil {
			return nil, resourceError(err)
		}
		return &runtimev1.ProvisionResourceResponse{Handle: out.Handle, Phase: out.Phase, Endpoint: out.Endpoint}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported resource tier (want dedicated|shared)")
	}
}

// GetResourceStatus reports a resource's live status. The by-handle org gate
// (D-083) runs first so a leaked handle cannot expose another org's resource.
func (s *ResourceServer) GetResourceStatus(ctx context.Context, req *runtimev1.GetResourceStatusRequest) (*runtimev1.ResourceStatusResponse, error) {
	if s.dedicated == nil {
		return nil, status.Error(codes.Unavailable, "resource provisioner not configured")
	}
	res, ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	st, err := s.dedicated.StatusOf(ctx, res.ID)
	if err != nil {
		return nil, resourceError(err)
	}
	resp := &runtimev1.ResourceStatusResponse{
		Handle:     st.Handle,
		Phase:      st.Phase,
		Endpoint:   st.Endpoint,
		SecretRefs: st.SecretRefs,
		ConnBudget: int32(st.ConnBudget),
		Conditions: st.Conditions,
	}
	if st.LastRecoveryPoint != nil {
		resp.LastRecoveryPoint = &runtimev1.RecoveryPointProto{
			Ref:  st.LastRecoveryPoint.ObjectKey,
			Kind: st.LastRecoveryPoint.Kind,
			At:   timestamppb.New(st.LastRecoveryPoint.CreatedAt),
			// wire name `verified` is frozen; the VALUE is RestoredInPlaceOK, never a drill result.
			Verified: st.LastRecoveryPoint.RestoredInPlaceOK,
		}
	}
	return resp, nil
}

// SnapshotResource takes an on-demand recovery point of a resource's volumes.
// The by-handle org gate runs first; a shared/logical resource (no backing app)
// has no volumes to snapshot and is refused.
func (s *ResourceServer) SnapshotResource(ctx context.Context, req *runtimev1.SnapshotResourceRequest) (*runtimev1.SnapshotResourceResponse, error) {
	if s.snapshotter == nil {
		return nil, status.Error(codes.Unavailable, "resource snapshotter not configured")
	}
	res, ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	if res.AppID == nil {
		return nil, status.Error(codes.FailedPrecondition, "resource has no backing app to snapshot")
	}
	points, err := s.snapshotter.SnapshotAppVolumes(ctx, res.ID, *res.AppID)
	if err != nil {
		return nil, resourceError(err)
	}
	if len(points) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "resource has no volumes to snapshot")
	}
	rp := points[0]
	return &runtimev1.SnapshotResourceResponse{
		RecoveryPoint: &runtimev1.RecoveryPointProto{
			Ref:  rp.ObjectKey,
			Kind: rp.Kind,
			At:   timestamppb.New(rp.CreatedAt),
			// wire name `verified` is frozen; the VALUE is RestoredInPlaceOK, never a drill result.
			Verified: rp.RestoredInPlaceOK,
		},
	}, nil
}

// ListResourceRecoveryPoints returns a resource's recovery catalog, newest
// first. The by-handle org gate runs first.
func (s *ResourceServer) ListResourceRecoveryPoints(ctx context.Context, req *runtimev1.ListResourceRecoveryPointsRequest) (*runtimev1.ListResourceRecoveryPointsResponse, error) {
	res, _, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	rps, err := s.resources.ListRecoveryPoints(ctx, res.ID)
	if err != nil {
		return nil, resourceError(err)
	}
	out := make([]*runtimev1.RecoveryPointProto, 0, len(rps))
	for i := range rps {
		out = append(out, &runtimev1.RecoveryPointProto{
			Ref:  rps[i].ObjectKey,
			Kind: rps[i].Kind,
			At:   timestamppb.New(rps[i].CreatedAt),
			// wire name `verified` is frozen; the VALUE is RestoredInPlaceOK, never a drill result.
			Verified: rps[i].RestoredInPlaceOK,
		})
	}
	return &runtimev1.ListResourceRecoveryPointsResponse{RecoveryPoints: out}, nil
}

// DecommissionResource tears down a resource. The by-handle org gate runs first;
// a durable tier is torn down snapshot-first inside the use case.
//
// The response carries the FINAL RECOVERY POINT whenever one was taken. That is
// what makes a snapshot-first teardown verifiable by the caller: a bare OK told
// it only that the call returned, never that a recovery point exists.
func (s *ResourceServer) DecommissionResource(ctx context.Context, req *runtimev1.DecommissionResourceRequest) (*runtimev1.DecommissionResourceResponse, error) {
	if s.dedicated == nil {
		return nil, status.Error(codes.Unavailable, "resource provisioner not configured")
	}
	res, ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	rp, err := s.dedicated.DecommissionDedicated(ctx, res.ID, req.GetFinalSnapshot())
	if err != nil {
		return nil, resourceError(err)
	}
	resp := &runtimev1.DecommissionResourceResponse{}
	if rp != nil {
		resp.FinalRecoveryPoint = &runtimev1.RecoveryPointProto{
			Ref:  rp.ObjectKey,
			Kind: rp.Kind,
			At:   timestamppb.New(rp.CreatedAt),
			// wire name `verified` is frozen; the VALUE is RestoredInPlaceOK, never a drill result.
			Verified: rp.RestoredInPlaceOK,
		}
	}
	return resp, nil
}

// RestoreResource restores a resource IN PLACE from one of its recovery points
// (D-184). The by-handle org gate runs first; the recovery-point ref is then
// resolved STRICTLY within that resource, so a leaked or guessed object key from
// another org's resource is not restorable. The call returns as soon as the
// restore is admitted (phase `restoring`); callers poll GetResourceStatus for
// the outcome.
func (s *ResourceServer) RestoreResource(ctx context.Context, req *runtimev1.RestoreResourceRequest) (*runtimev1.RestoreResourceResponse, error) {
	if s.restorer == nil {
		return nil, status.Error(codes.Unavailable, "resource restorer not configured")
	}
	res, ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	rp, err := s.resources.GetRecoveryPointByRef(ctx, res.ID, req.GetRecoveryPointRef())
	if err != nil {
		return nil, resourceError(err)
	}
	out, err := s.restorer.Restore(ctx, usecase.RestoreResourceInput{Resource: res, RecoveryPoint: rp})
	if err != nil {
		return nil, resourceError(err)
	}
	return &runtimev1.RestoreResourceResponse{Handle: out.Handle, Phase: out.Phase}, nil
}

// RotateResourceCredentials is Unimplemented in v1 (credential rotation lands
// with a later CP4.5 slice; caps report supports_credential_rotation=false).
func (s *ResourceServer) RotateResourceCredentials(context.Context, *runtimev1.RotateResourceCredentialsRequest) (*runtimev1.RotateResourceCredentialsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "resource credential rotation is not implemented in v1")
}

// authorizeHandleOrg is the by-handle counterpart of ProvisionResource's
// owner_org gate (D-083): a resource handle is unguessable, so a leaked one must
// not let a foreign caller act on another org's resource. It parses the handle,
// loads the resource, and shadow-authorizes the caller against the resource's
// owning org with the SAME enforce flag as ProvisionResource. It returns the
// loaded resource (callers reuse its ID/AppID) and the (org-stamped) context.
func (s *ResourceServer) authorizeHandleOrg(ctx context.Context, handle string) (*domain.FleetResource, context.Context, error) {
	id, err := uuid.Parse(handle)
	if err != nil {
		return nil, ctx, status.Error(codes.InvalidArgument, "handle is not a valid uuid")
	}
	res, err := s.resources.GetResourceByHandle(ctx, id)
	if err != nil {
		return nil, ctx, resourceError(err)
	}
	if res.OwnerOrg == uuid.Nil {
		return res, ctx, nil
	}
	if err := tenant.AuthorizeOrgShadow(ctx, res.OwnerOrg, pkconfig.OrgEnforce()); err != nil {
		return nil, ctx, err
	}
	return res, tenant.WithActiveOrg(ctx, res.OwnerOrg), nil
}

// resourceError maps P19 resource domain sentinels to gRPC status codes. Errors
// it does not own — notably the composed FleetProvision sentinels the dedicated
// path surfaces (image-boot, secret, volume) — fall through to fleetError, which
// consults the platform error registry and curates a leak-free Internal default.
func resourceError(err error) error {
	switch {
	case errors.Is(err, domain.ErrResourceNotFound):
		return status.Error(codes.NotFound, "resource not found")
	case errors.Is(err, domain.ErrResourceConvergeNotSupported):
		return status.Error(codes.FailedPrecondition, "resource converge/resize is not supported")
	case errors.Is(err, domain.ErrResourceOwnerOrgRequired):
		return status.Error(codes.InvalidArgument, "resource claim requires an owner org")
	case errors.Is(err, domain.ErrResourceClaimKeyRequired):
		return status.Error(codes.InvalidArgument, "resource claim requires a claim key")
	case errors.Is(err, domain.ErrResourceSecretsRequired):
		return status.Error(codes.InvalidArgument, "resource claim requires engine secret refs")
	case errors.Is(err, domain.ErrResourceVaultTokenRequired):
		return status.Error(codes.InvalidArgument, "resource claim requires a vault token")
	case errors.Is(err, domain.ErrResourceClassUnsupported):
		return status.Error(codes.InvalidArgument, "unsupported resource class (want postgres)")
	case errors.Is(err, domain.ErrResourceTierUnsupported):
		return status.Error(codes.InvalidArgument, "unsupported resource tier for this path")
	case errors.Is(err, domain.ErrResourceFinalSnapshotRequired):
		return status.Error(codes.FailedPrecondition, "a durable resource requires a final snapshot to decommission")
	case errors.Is(err, domain.ErrResourceSharedPasswordAmbiguous):
		return status.Error(codes.FailedPrecondition, "resolved secrets do not identify a single role password")
	case errors.Is(err, domain.ErrRecoveryPointNotFound):
		return status.Error(codes.NotFound, "recovery point not found for this resource")
	case errors.Is(err, domain.ErrRestoreInProgress):
		return status.Error(codes.FailedPrecondition, "restore already in progress")
	case errors.Is(err, domain.ErrRestoreNoBackingApp):
		return status.Error(codes.FailedPrecondition, "resource has no backing app to restore in place")
	case errors.Is(err, domain.ErrRestoreVolumeAmbiguous):
		return status.Error(codes.FailedPrecondition, "in-place restore requires exactly one materialized volume")
	case errors.Is(err, domain.ErrRestoreStoreUnavailable):
		return status.Error(codes.Unavailable, "restore artifact store not configured on this host")
	case errors.Is(err, domain.ErrVolumeBackingFileMissing):
		// The refusal is correct — a durable resource whose data cannot be
		// snapshotted must not be torn down — but it used to reach the caller as a
		// bare Internal. Name the CONDITION and nothing else: the raw path and the
		// OS error stay server-side (these messages are tenant-visible, which is why
		// this service curates them by hand instead of registering them).
		return status.Error(codes.FailedPrecondition, "the volume's backing file is missing, so a final snapshot cannot be taken")
	default:
		// Log the real error server-side before curating a leak-free code for the
		// caller — an unmapped failure (e.g. a fleet-side Pause/boot/volume error)
		// must never be silently swallowed into a bare Internal (observability gap
		// found on the .244 integration drive).
		log.Printf("resource op failed (unmapped): %v", err)
		return fleetError(err)
	}
}
