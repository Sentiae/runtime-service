package grpc

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pkconfig "github.com/sentiae/platform-kit/config"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// NetworkFabricServer implements the FleetNetworkFabric gRPC service — the P21
// NetworkFabric provider seam for the sentiae_fleet target class (CP4.5 §9 #5,
// D-164). It is a separate service from FleetOrchestration on purpose: P21 and P7
// are two distinct ports and must not be fused at the wire.
type NetworkFabricServer struct {
	runtimev1.UnimplementedFleetNetworkFabricServer
	fabric *usecase.FleetNetworkFabric
}

// NewNetworkFabricServer constructs the handler.
func NewNetworkFabricServer(fabric *usecase.FleetNetworkFabric) *NetworkFabricServer {
	return &NetworkFabricServer{fabric: fabric}
}

// EnsureNetwork creates (or returns) the policy scope for a system×env.
func (s *NetworkFabricServer) EnsureNetwork(ctx context.Context, req *runtimev1.EnsureNetworkRequest) (*runtimev1.EnsureNetworkResponse, error) {
	if s.fabric == nil {
		return nil, status.Error(codes.Unavailable, "fleet network fabric not configured")
	}
	// The owner org is the network's tenant anchor (D-069/I28), so it is verified
	// against the attested carriage exactly as Provision verifies its own — with
	// one difference: an EMPTY org is refused rather than passed through. Provision
	// tolerates "" only for legacy CP3 test boots; a network has no legacy caller.
	ctx, err := s.authorizeNetworkOrg(ctx, req.GetOwnerOrg())
	if err != nil {
		return nil, err
	}
	out, err := s.fabric.EnsureNetwork(ctx, usecase.EnsureNetworkInput{
		SystemID: req.GetSystemId(),
		Env:      req.GetEnv(),
		OwnerOrg: req.GetOwnerOrg(),
	})
	if err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.EnsureNetworkResponse{Handle: out.Handle.String()}, nil
}

// ApplyPolicies replaces a network's complete policy set and re-realizes it.
func (s *NetworkFabricServer) ApplyPolicies(ctx context.Context, req *runtimev1.ApplyPoliciesRequest) (*runtimev1.ApplyPoliciesResponse, error) {
	if s.fabric == nil {
		return nil, status.Error(codes.Unavailable, "fleet network fabric not configured")
	}
	handle, err := uuid.Parse(req.GetHandle())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "handle is not a valid uuid")
	}
	// Carried verbatim — protocol and port are NOT normalized or defaulted here.
	// An empty protocol and a zero port must reach the validator and be rejected
	// there; "helping" the caller at this boundary is how a wildcard is born.
	specs := make([]usecase.PolicySpecInput, 0, len(req.GetPolicies()))
	for _, p := range req.GetPolicies() {
		specs = append(specs, usecase.PolicySpecInput{
			FromComponentID:   p.GetFromComponentId(),
			ToComponentID:     p.GetToComponentId(),
			Protocol:          p.GetProtocol(),
			Port:              int(p.GetPort()),
			DerivedFromEdgeID: p.GetDerivedFromEdgeId(),
		})
	}
	out, err := s.fabric.ApplyPolicies(ctx, usecase.ApplyPoliciesInput{Handle: handle, Policies: specs})
	if err != nil {
		return nil, fleetError(err)
	}
	resp := &runtimev1.ApplyPoliciesResponse{
		Aggregate: out.Aggregate,
		Policies:  make([]*runtimev1.PolicyEnforcement, 0, len(out.Policies)),
	}
	for _, r := range out.Policies {
		resp.Policies = append(resp.Policies, &runtimev1.PolicyEnforcement{
			FromComponentId: r.FromComponentID,
			ToComponentId:   r.ToComponentID,
			Port:            int32(r.Port),
			Tier:            r.Tier,
			Detail:          r.Detail,
		})
	}
	return resp, nil
}

// DeprovisionNetwork tears the scope down (chain removed, row tombstoned).
func (s *NetworkFabricServer) DeprovisionNetwork(ctx context.Context, req *runtimev1.DeprovisionNetworkRequest) (*runtimev1.DeprovisionNetworkResponse, error) {
	if s.fabric == nil {
		return nil, status.Error(codes.Unavailable, "fleet network fabric not configured")
	}
	handle, err := uuid.Parse(req.GetHandle())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "handle is not a valid uuid")
	}
	if err := s.fabric.Deprovision(ctx, handle); err != nil {
		return nil, fleetError(err)
	}
	return &runtimev1.DeprovisionNetworkResponse{}, nil
}

// authorizeNetworkOrg cross-checks the caller-supplied owner_org against the
// attested x-organization-id carriage and shadow-authorizes it (D-061), mirroring
// Provision's gate with the same enforce flag.
func (s *NetworkFabricServer) authorizeNetworkOrg(ctx context.Context, ownerOrgRaw string) (context.Context, error) {
	if ownerOrgRaw == "" {
		return ctx, status.Error(codes.InvalidArgument, "owner_org is required")
	}
	if err := requireCarriageMatch(ctx, ownerOrgRaw); err != nil {
		return ctx, err
	}
	ownerOrg, err := uuid.Parse(ownerOrgRaw)
	if err != nil {
		return ctx, status.Error(codes.InvalidArgument, "owner_org is not a valid uuid")
	}
	if err := tenant.AuthorizeOrgShadow(ctx, ownerOrg, pkconfig.OrgEnforce()); err != nil {
		return ctx, err
	}
	return tenant.WithActiveOrg(ctx, ownerOrg), nil
}
