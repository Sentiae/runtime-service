package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pkconfig "github.com/sentiae/platform-kit/config"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// FleetServer implements the FleetOrchestration gRPC service — the P7
// DeployTarget provider seam for the "test" and "resident" workload classes
// (runtime-fleet CP3). Scale lands with the CP4 control plane.
type FleetServer struct {
	runtimev1.UnimplementedFleetOrchestrationServer
	provision *usecase.FleetProvision
	registry  *usecase.FleetHostRegistry
}

// NewFleetServer constructs the handler.
func NewFleetServer(provision *usecase.FleetProvision, registry *usecase.FleetHostRegistry) *FleetServer {
	return &FleetServer{provision: provision, registry: registry}
}

// Provision boots a workload from a compiled OCI image.
func (s *FleetServer) Provision(ctx context.Context, req *runtimev1.ProvisionRequest) (*runtimev1.ProvisionResponse, error) {
	if s.provision == nil {
		return nil, status.Error(codes.Unavailable, "fleet provision use case not configured")
	}
	d := req.GetDescriptor_()
	if d == nil {
		return nil, status.Error(codes.InvalidArgument, "descriptor is required")
	}
	img := d.GetImage()
	if img == nil {
		return nil, status.Error(codes.InvalidArgument, "descriptor.image is required")
	}

	// D-061 verified-org boundary (shadow → flip). The caller-supplied owner_org
	// feeds secret.Principal.OrgID downstream (fleet_replica_runtime.go) — a
	// spoofed owner_org would be a spoofed secret tenant. Cross-check it against
	// the attested x-organization-id carriage, then shadow-authorize it: with
	// APP_AUTH_ORG_ENFORCE unset this is a strict no-op (divergence logged only);
	// once flipped, a foreign org is denied before the secret path runs.
	ownerOrgRaw := req.GetOwnerOrg()
	// Carriage cross-check (defense-in-depth for the delivery→runtime attested
	// carriage, B3): a present, non-empty x-organization-id MUST match owner_org.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-organization-id"); len(vals) > 0 && vals[0] != "" && vals[0] != ownerOrgRaw {
			return nil, status.Error(codes.InvalidArgument, "owner_org / x-organization-id mismatch")
		}
	}
	if ownerOrgRaw == "" {
		// Empty-org provisions exist today (CP3 test-class boots) — pass through
		// unchanged rather than hard-fail.
		logger.FromContext(ctx).Debug("fleet provision: empty owner_org, skipping org authz")
	} else {
		ownerOrg, perr := uuid.Parse(ownerOrgRaw)
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, "owner_org is not a valid uuid")
		}
		if err := tenant.AuthorizeOrgShadow(ctx, ownerOrg, pkconfig.OrgEnforce()); err != nil {
			return nil, err
		}
		ctx = tenant.WithActiveOrg(ctx, ownerOrg)
	}

	res := d.GetResources()
	out, err := s.provision.Provision(ctx, usecase.FleetProvisionInput{
		ComponentID:    d.GetComponentId(),
		Env:            d.GetEnv(),
		OwnerOrg:       req.GetOwnerOrg(),
		Registry:       img.GetRegistry(),
		Repository:     img.GetRepository(),
		Digest:         img.GetDigest(),
		ChangeID:       img.GetChangeId(),
		VCPU:           int(res.GetVcpu()),
		MemoryMB:       int(res.GetMemoryMb()),
		EnvVars:        d.GetEnvVars(),
		SecretRefs:     d.GetSecretRefs(),
		Port:           int(d.GetPort()),
		WorkloadClass:  d.GetWorkloadClass(),
		TestCommand:    d.GetTestCommand(),
		TimeoutSeconds: d.GetTimeoutSeconds(),
		// P7 RunJob seam (job class). job_command stays a LIST all the way to the
		// guest exec — it is never joined or shell-interpolated.
		JobCommand:     d.GetJobCommand(),
		IdempotencyKey: d.GetIdempotencyKey(),
		EgressAllow:    d.GetEgressAllow(),
		Volumes:        volumesFromProto(d.GetVolumes()),
		ScaleToZero:    d.GetScaleToZero(),
		IdleTTLSeconds: int(d.GetIdleTtlSeconds()),
		MinReplicas:    int(d.GetMinReplicas()),
		MaxReplicas:    int(d.GetMaxReplicas()),
		// D-125: the handed per-deployment Vault token travels MEMORY-ONLY into the
		// provision input — it is never persisted to the fleet_apps row (verified:
		// ProvisionApp/FleetApp carry no token field) nor logged.
		VaultToken: d.GetVaultToken(),
		// D-124: the handed per-deployment registry pull token likewise travels
		// MEMORY-ONLY into the provision input — never persisted to a row nor logged.
		RegistryPullToken: d.GetRegistryPullToken(),
	})
	if err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.ProvisionResponse{Handle: out.Handle, Url: out.URL}, nil
}

// authorizeHandleOrg is the by-handle counterpart of Provision's owner_org gate
// (#fleet-handle-ops-org-check, D-083): Health/Decommission/Scale act on an
// unguessable handle, so a leaked one must not let a foreign caller act on
// another org's app. It resolves the handle's owning org and shadow-authorizes
// the caller against it with the EXACT same enforce flag as Provision. An
// org-less handle (a test-class workload, or an app with no owner org) skips the
// gate exactly as Provision's empty owner_org does. On success it returns the
// (possibly org-stamped) context so the caller can propagate the active org, and
// preserves the existing not-found mapping for an unknown handle.
func (s *FleetServer) authorizeHandleOrg(ctx context.Context, handle string) (context.Context, error) {
	ownerOrg, err := s.provision.OwnerOrgForHandle(ctx, handle)
	if err != nil {
		return ctx, fleetError(err)
	}
	if ownerOrg == uuid.Nil {
		return ctx, nil
	}
	if err := tenant.AuthorizeOrgShadow(ctx, ownerOrg, pkconfig.OrgEnforce()); err != nil {
		return ctx, err
	}
	return tenant.WithActiveOrg(ctx, ownerOrg), nil
}

// Health reports the current state + health of a workload.
func (s *FleetServer) Health(ctx context.Context, req *runtimev1.FleetHealthRequest) (*runtimev1.FleetHealthResponse, error) {
	if s.provision == nil {
		return nil, status.Error(codes.Unavailable, "fleet provision use case not configured")
	}
	ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	out, err := s.provision.Health(ctx, req.GetHandle())
	if err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.FleetHealthResponse{
		State:      out.State,
		Healthy:    out.Healthy,
		ExitCode:   int32(out.ExitCode),
		Message:    out.Message,
		StdoutTail: out.StdoutTail,
		StderrTail: out.StderrTail,
		Url:        out.URL,
	}, nil
}

// Decommission tears down a workload.
func (s *FleetServer) Decommission(ctx context.Context, req *runtimev1.FleetDecommissionRequest) (*runtimev1.FleetDecommissionResponse, error) {
	if s.provision == nil {
		return nil, status.Error(codes.Unavailable, "fleet provision use case not configured")
	}
	ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	if err := s.provision.Decommission(ctx, req.GetHandle()); err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.FleetDecommissionResponse{}, nil
}

// Scale sets the desired replica count for a resident fleet app (CP4 §9#7).
func (s *FleetServer) Scale(ctx context.Context, req *runtimev1.FleetScaleRequest) (*runtimev1.FleetScaleResponse, error) {
	if s.provision == nil {
		return nil, status.Error(codes.Unavailable, "fleet provision use case not configured")
	}
	ctx, err := s.authorizeHandleOrg(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	if err := s.provision.Scale(ctx, req.GetHandle(), int(req.GetReplicas())); err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.FleetScaleResponse{}, nil
}

// RegisterHost registers (or refreshes) a fleet host in the durable inventory.
func (s *FleetServer) RegisterHost(ctx context.Context, req *runtimev1.RegisterHostRequest) (*runtimev1.RegisterHostResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	spec := req.GetHost()
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "host spec is required")
	}
	var id uuid.UUID
	if spec.GetHostId() != "" {
		parsed, err := uuid.Parse(spec.GetHostId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "host_id is not a valid uuid")
		}
		id = parsed
	}
	host, err := s.registry.RegisterHost(ctx, domain.Host{
		ID:             id,
		Region:         spec.GetRegion(),
		Labels:         spec.GetLabels(),
		CapacityVCPU:   int(spec.GetCapacityVcpu()),
		CapacityMemMB:  spec.GetCapacityMemMb(),
		CapacityDiskMB: spec.GetCapacityDiskMb(),
		Endpoint:       spec.GetEndpoint(),
	})
	if err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.RegisterHostResponse{HostId: host.ID.String()}, nil
}

// Heartbeat refreshes a host's liveness and allocatable capacity.
func (s *FleetServer) Heartbeat(ctx context.Context, req *runtimev1.HeartbeatRequest) (*runtimev1.HeartbeatResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	id, err := uuid.Parse(req.GetHostId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "host_id is not a valid uuid")
	}
	if err := s.registry.Heartbeat(ctx, id,
		int(req.GetAllocatableVcpu()),
		req.GetAllocatableMemMb(),
		req.GetAllocatableDiskMb(),
		req.GetHealth(),
	); err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.HeartbeatResponse{}, nil
}

// ListHosts returns the full fleet host inventory.
func (s *FleetServer) ListHosts(ctx context.Context, _ *runtimev1.ListHostsRequest) (*runtimev1.ListHostsResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	hosts, err := s.registry.ListHosts(ctx)
	if err != nil {
		return nil, fleetError(err)
	}
	out := make([]*runtimev1.HostInfo, 0, len(hosts))
	for i := range hosts {
		out = append(out, hostToProto(&hosts[i]))
	}
	return &runtimev1.ListHostsResponse{Hosts: out}, nil
}

// volumesFromProto maps the descriptor's VolumeSpecs to the use case input. The
// mount path is the hardcoded domain default (/data) — the proto carries none
// this cycle (rt#9 scope). An unparseable/empty id gets a fresh uuid.
func volumesFromProto(specs []*runtimev1.VolumeSpec) []usecase.VolumeSpecInput {
	if len(specs) == 0 {
		return nil
	}
	out := make([]usecase.VolumeSpecInput, 0, len(specs))
	for _, v := range specs {
		if v == nil {
			continue
		}
		id, err := uuid.Parse(v.GetId())
		if err != nil {
			id = uuid.New()
		}
		out = append(out, usecase.VolumeSpecInput{
			ID:        id,
			SizeMB:    int64(v.GetSizeMb()),
			MountPath: "/data",
		})
	}
	return out
}

// hostToProto maps a domain Host to the wire HostInfo.
func hostToProto(h *domain.Host) *runtimev1.HostInfo {
	var lastHB int64
	if h.LastHeartbeat != nil {
		lastHB = h.LastHeartbeat.Unix()
	}
	return &runtimev1.HostInfo{
		HostId:            h.ID.String(),
		Region:            h.Region,
		Labels:            h.Labels,
		CapacityVcpu:      int32(h.CapacityVCPU),
		CapacityMemMb:     h.CapacityMemMB,
		CapacityDiskMb:    h.CapacityDiskMB,
		AllocatableVcpu:   int32(h.AllocatableVCPU),
		AllocatableMemMb:  h.AllocatableMemMB,
		AllocatableDiskMb: h.AllocatableDiskMB,
		Health:            string(h.Health),
		Status:            string(h.Status),
		Endpoint:          h.Endpoint,
		LastHeartbeatUnix: lastHB,
	}
}

// fleetError maps fleet domain errors to gRPC status codes.
func fleetError(err error) error {
	switch {
	case errors.Is(err, domain.ErrWorkloadNotFound):
		return status.Error(codes.NotFound, "workload not found")
	case errors.Is(err, domain.ErrUnsupportedClass):
		return status.Error(codes.InvalidArgument, "unsupported workload class (want test|resident|job)")
	case errors.Is(err, domain.ErrSecretsNotSupported):
		return status.Error(codes.InvalidArgument, "secret_refs are only supported for resident and job workloads")
	case errors.Is(err, domain.ErrScaleNotSupported):
		return status.Error(codes.FailedPrecondition, "scale is not supported for job workloads")
	case errors.Is(err, domain.ErrTestCommandNotSupported):
		return status.Error(codes.InvalidArgument, "test_command is not supported for job workloads (use job_command)")
	case errors.Is(err, domain.ErrJobCommandNotSupported):
		return status.Error(codes.InvalidArgument, "job_command is only supported for job workloads")
	case errors.Is(err, domain.ErrIdempotencyKeyNotSupported):
		return status.Error(codes.InvalidArgument, "idempotency_key is only supported for job workloads")
	case errors.Is(err, domain.ErrIdempotencyOwnerOrgMissing):
		return status.Error(codes.InvalidArgument, "idempotency_key requires an owner org")
	case errors.Is(err, domain.ErrSecretResolverUnavailable):
		return status.Error(codes.FailedPrecondition, "secret resolver unavailable on this host")
	case errors.Is(err, domain.ErrSecretOwnerOrgMissing):
		return status.Error(codes.InvalidArgument, "secret refs require an owner org")
	case errors.Is(err, domain.ErrImageRefIncomplete):
		return status.Error(codes.InvalidArgument, "image reference requires registry, repository, and digest")
	case errors.Is(err, domain.ErrResidentPortRequired):
		return status.Error(codes.InvalidArgument, "resident workload requires a guest port")
	case errors.Is(err, domain.ErrImageBootUnavailable):
		return status.Error(codes.FailedPrecondition, "image boot requires the firecracker host")
	case errors.Is(err, domain.ErrVolumesNotSupported):
		return status.Error(codes.InvalidArgument, "volumes are only supported for resident workloads")
	case errors.Is(err, domain.ErrVolumeAppNotScalable):
		return status.Error(codes.FailedPrecondition, "a volume-bearing app cannot scale beyond one replica")
	case errors.Is(err, domain.ErrVolumeBackendUnavailable):
		return status.Error(codes.FailedPrecondition, "volumes require the firecracker host")
	case errors.Is(err, domain.ErrFleetHostNotFound):
		return status.Error(codes.NotFound, "fleet host not found")
	case errors.Is(err, domain.ErrInvalidHostHealth):
		return status.Error(codes.InvalidArgument, "invalid host health (want healthy|degraded|unhealthy|unknown)")
	default:
		return status.Error(codes.Internal, "internal server error")
	}
}
