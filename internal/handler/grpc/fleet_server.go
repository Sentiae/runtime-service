package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// FleetServer implements the FleetOrchestration gRPC service — the P7
// DeployTarget provider seam for the "test" and "resident" workload classes
// (runtime-fleet CP3). Scale/Cutover land with the CP4 control plane.
type FleetServer struct {
	runtimev1.UnimplementedFleetOrchestrationServer
	provision *usecase.FleetProvision
}

// NewFleetServer constructs the handler.
func NewFleetServer(provision *usecase.FleetProvision) *FleetServer {
	return &FleetServer{provision: provision}
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

	res := d.GetResources()
	out, err := s.provision.Provision(ctx, usecase.FleetProvisionInput{
		ComponentID:    d.GetComponentId(),
		Env:            d.GetEnv(),
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
	})
	if err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.ProvisionResponse{Handle: out.Handle, Url: out.URL}, nil
}

// Health reports the current state + health of a workload.
func (s *FleetServer) Health(ctx context.Context, req *runtimev1.FleetHealthRequest) (*runtimev1.FleetHealthResponse, error) {
	if s.provision == nil {
		return nil, status.Error(codes.Unavailable, "fleet provision use case not configured")
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
	if err := s.provision.Decommission(ctx, req.GetHandle()); err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.FleetDecommissionResponse{}, nil
}

// Scale lands with the CP4 fleet control plane.
func (s *FleetServer) Scale(context.Context, *runtimev1.FleetScaleRequest) (*runtimev1.FleetScaleResponse, error) {
	return nil, status.Error(codes.Unimplemented, "lands with the CP4 fleet control plane")
}

// Cutover lands with the CP4 fleet control plane.
func (s *FleetServer) Cutover(context.Context, *runtimev1.FleetCutoverRequest) (*runtimev1.FleetCutoverResponse, error) {
	return nil, status.Error(codes.Unimplemented, "lands with the CP4 fleet control plane")
}

// fleetError maps fleet domain errors to gRPC status codes.
func fleetError(err error) error {
	switch {
	case errors.Is(err, domain.ErrWorkloadNotFound):
		return status.Error(codes.NotFound, "workload not found")
	case errors.Is(err, domain.ErrUnsupportedClass):
		return status.Error(codes.InvalidArgument, "unsupported workload class (want test|resident)")
	case errors.Is(err, domain.ErrSecretsNotSupported):
		return status.Error(codes.InvalidArgument, "secret_refs are not supported in CP3")
	case errors.Is(err, domain.ErrImageRefIncomplete):
		return status.Error(codes.InvalidArgument, "image reference requires registry, repository, and digest")
	case errors.Is(err, domain.ErrResidentPortRequired):
		return status.Error(codes.InvalidArgument, "resident workload requires a guest port")
	case errors.Is(err, domain.ErrImageBootUnavailable):
		return status.Error(codes.FailedPrecondition, "image boot requires the firecracker host")
	default:
		return status.Error(codes.Internal, "internal server error")
	}
}
