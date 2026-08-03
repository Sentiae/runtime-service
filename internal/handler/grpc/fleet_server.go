package grpc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pkconfig "github.com/sentiae/platform-kit/config"
	pkerrors "github.com/sentiae/platform-kit/errors"
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
	if err := requireCarriageMatch(ctx, ownerOrgRaw); err != nil {
		return nil, err
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
		ComponentID:   d.GetComponentId(),
		Env:           d.GetEnv(),
		OwnerOrg:      req.GetOwnerOrg(),
		Registry:      img.GetRegistry(),
		Repository:    img.GetRepository(),
		Digest:        img.GetDigest(),
		ChangeID:      img.GetChangeId(),
		VCPU:          int(res.GetVcpu()),
		MemoryMB:      int(res.GetMemoryMb()),
		EnvVars:       d.GetEnvVars(),
		SecretRefs:    d.GetSecretRefs(),
		Port:          int(d.GetPort()),
		WorkloadClass: d.GetWorkloadClass(),
		// CP4.5 §9 #5 — P21 network membership. Empty (every pre-#5 caller) means no
		// membership: the workload reaches no fleet peer, exactly as before.
		SystemID:       d.GetSystemId(),
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

// requireCarriageMatch is the delivery→runtime attested-carriage cross-check
// (defense-in-depth, B3): a present, non-empty x-organization-id MUST match the
// caller-supplied owner_org. Shared by Provision and the P21 network fabric so
// the two seams can never drift on this check.
func requireCarriageMatch(ctx context.Context, ownerOrgRaw string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	vals := md.Get("x-organization-id")
	if len(vals) > 0 && vals[0] != "" && vals[0] != ownerOrgRaw {
		return status.Error(codes.InvalidArgument, "owner_org / x-organization-id mismatch")
	}
	return nil
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

// svidPath returns the path component of a SPIFFE ID string —
// spiffe://<trust-domain><path> — and ok=false for anything that is not one. The
// string is split here rather than parsed with go-spiffe's spiffeid because the
// caller already holds a peer id the TLS layer validated, and the exact-shape
// checks that matter (the /fleet-host/ prefix and a real uuid) are done below.
func svidPath(raw string) (string, bool) {
	const scheme = "spiffe://"
	if !strings.HasPrefix(raw, scheme) {
		return "", false
	}
	authorityAndPath := raw[len(scheme):]
	slash := strings.Index(authorityAndPath, "/")
	if slash <= 0 {
		// No path at all (or an empty trust domain): a trust-domain id names no
		// workload, so it names no host either.
		return "", false
	}
	return authorityAndPath[slash:], true
}

// hostSVIDPathPrefix is the SPIFFE path prefix of a PER-HOST identity —
// spiffe://<domain>/fleet-host/<uuid>, minted at host birth (the same
// server-side action that already issues the SPIRE join token). It is the only
// thing that answers "which host is calling", because it is the one statement
// about the caller the caller cannot write.
const hostSVIDPathPrefix = "/fleet-host/"

// peerHostID derives the calling host's identity from its peer SVID.
//
// The identity of a host is a TRANSPORT fact, never a payload fact: the request
// body used to name the host, and an empty host_id used to mint a fresh
// uuid.New() — so any mesh caller could re-register (or, before the status fix,
// un-cordon) any host it liked, and an anonymous caller was HANDED an identity
// it never proved. A caller that cannot prove who it is gets refused, not named.
//
// ⚠ Until per-host SVIDs are minted at birth, no real caller satisfies this and
// every RPC call is refused. That is not a regression: this RPC already refuses
// every call (see RegisterHost), the live registration path is in-process
// (di.registerSelfHost, which never crosses this handler), and refusing is the
// correct posture for a host whose identity is unknowable.
//
// The SVID is read through tenant.FromContext — the same seam every other check
// in this file uses — which derives Principal.ServiceSVID from the peer
// certificate the SVID interceptor extracted. It is the identical fact, read
// where the rest of this handler's identity checks read it.
func peerHostID(ctx context.Context) (uuid.UUID, error) {
	p, ok := tenant.FromContext(ctx)
	if !ok || p.ServiceSVID == "" {
		return uuid.Nil, status.Error(codes.Unauthenticated,
			"a fleet host must prove its identity with a peer SVID (spiffe://…"+hostSVIDPathPrefix+"<uuid>); an unidentified caller is refused, never assigned an identity")
	}
	path, ok := svidPath(p.ServiceSVID)
	if !ok {
		return uuid.Nil, status.Errorf(codes.PermissionDenied, "peer SVID %q is not a valid SPIFFE ID", p.ServiceSVID)
	}
	if !strings.HasPrefix(path, hostSVIDPathPrefix) {
		return uuid.Nil, status.Errorf(codes.PermissionDenied,
			"peer SVID %q is not a fleet-host identity: only a workload holding a %s<uuid> SVID may act as a host", p.ServiceSVID, hostSVIDPathPrefix)
	}
	hostID, perr := uuid.Parse(strings.TrimPrefix(path, hostSVIDPathPrefix))
	if perr != nil || hostID == uuid.Nil {
		return uuid.Nil, status.Errorf(codes.PermissionDenied,
			"peer SVID %q does not carry a valid host uuid", p.ServiceSVID)
	}
	return hostID, nil
}

// requireSelfOrRefuse cross-checks a body-supplied host id against the attested
// peer identity. A body id is accepted only as a redundant restatement of the
// SVID; a disagreement is a forgery attempt, not a correction.
func requireSelfOrRefuse(bodyHostID string, attested uuid.UUID) error {
	if bodyHostID == "" {
		return nil
	}
	claimed, err := uuid.Parse(bodyHostID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "host_id is not a valid uuid")
	}
	if claimed != attested {
		return status.Errorf(codes.PermissionDenied,
			"host_id %s does not match the attested peer identity %s: a host may only act as itself", claimed, attested)
	}
	return nil
}

// requireAttestedCaller refuses a caller that presented no identity at all: no
// peer SVID, no verified service token, no user claims. It is deliberately the
// weakest possible check — it answers "is this someone" and nothing about
// authorization — so it belongs only on surfaces that carry no tenant data.
func requireAttestedCaller(ctx context.Context) error {
	p, ok := tenant.FromContext(ctx)
	if !ok || (p.ServiceSVID == "" && !p.ServiceAuthed && p.Claims == nil) {
		return status.Error(codes.Unauthenticated,
			"the fleet host inventory is not an anonymous read: present a peer SVID, a service token, or a user token")
	}
	return nil
}

// RegisterHost registers (or refreshes) a fleet host in the durable inventory.
//
// The host id comes from the peer SVID (see peerHostID) — never from the request
// body, and never minted for an anonymous caller.
//
// ⚠ Beyond identity, this RPC also REFUSES every call with
// ErrHostFailureDomainRequired, and that is deliberate rather than broken: since
// D-196 a host must state its structured failure domain, and `HostSpec` has no
// field to carry one — adding it is a frozen-contract (proto) change, which is
// not this slice's to make. The live path is unaffected: a fleet host
// self-registers in-process from APP_FLEET_FAILURE_DOMAIN (di.registerSelfHost),
// and this RPC has no caller. Refusing is the correct interim posture — admitting
// a host whose failure domain is unknowable is the fail-open the column exists to
// close.
func (s *FleetServer) RegisterHost(ctx context.Context, req *runtimev1.RegisterHostRequest) (*runtimev1.RegisterHostResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	spec := req.GetHost()
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "host spec is required")
	}
	// Identity BEFORE any spec validation: a caller that cannot prove which host it
	// is has nothing to say about capacity or placement facts either.
	id, err := peerHostID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireSelfOrRefuse(spec.GetHostId(), id); err != nil {
		return nil, err
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
//
// Identity comes from the peer SVID for the same reason as RegisterHost, and here
// it also guards a WRITE to another host's row: a heartbeat sets liveness and
// allocatable capacity, so a forged host_id could keep a dead host in the
// placement candidate set, or make a live one advertise capacity it does not have.
// The live self-heartbeat loop calls the use case in-process (di.startFleetHeartbeat)
// and does not pass through this handler.
func (s *FleetServer) Heartbeat(ctx context.Context, req *runtimev1.HeartbeatRequest) (*runtimev1.HeartbeatResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	id, err := peerHostID(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireSelfOrRefuse(req.GetHostId(), id); err != nil {
		return nil, err
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
//
// GATED, not declared: this is an operator/control-plane read surface and it
// carries no tenant identifier and no customer data (host ids, regions, failure
// domains, capacity, endpoints, health) — so any ATTESTED mesh caller may read it.
// What it must not be is anonymous: the inventory is a map of every machine
// holding customer state, with the gRPC endpoint of each, which is reconnaissance
// worth refusing. The requirement is enforced here rather than left to the auth
// interceptor's config (AcceptAPIKey / RequirePeerSVID), because a config-driven
// gate is exactly how a control comes to exist only on paper.
func (s *FleetServer) ListHosts(ctx context.Context, _ *runtimev1.ListHostsRequest) (*runtimev1.ListHostsResponse, error) {
	if s.registry == nil {
		return nil, status.Error(codes.Unavailable, "fleet host registry not configured")
	}
	if err := requireAttestedCaller(ctx); err != nil {
		return nil, err
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
	// The fleet-app tenancy guard (#two-orgs-same-claim-key-share-one-database). A
	// provision without an owner org is a CALLER-INPUT fault, not a host fault: the
	// app row is the tenancy boundary for fleet_apps (there is no RLS on that table)
	// and an org-less row is unscoped, so it is refused before anything is written.
	// This hand-map is the ONLY mapping for the sentinel — see the note on
	// registryOrInternal for why a registry entry would be dead code.
	case errors.Is(err, domain.ErrFleetAppOwnerOrgRequired):
		return status.Error(codes.InvalidArgument, "a resident fleet app requires an owner org")
	// AlreadyExists, not FailedPrecondition: the thing that already exists is the
	// resource this call tried to create — the ingress host — which is the textbook
	// AlreadyExists case, and it correctly reads as "retrying with the same name
	// cannot succeed". FailedPrecondition would read as "fix system state, then
	// retry", which is wrong: the app row is already committed, so a retry re-fails
	// on the same route insert forever. The message names the fix (a different
	// component id). The conflicting host is NOT echoed — it is deterministic from
	// the caller's own component id + env, and ensureRoute logs it for the operator.
	// FailedPrecondition, not PermissionDenied: the caller is allowed to tear this
	// down, just not through this verb — the app is the backing store of a durable
	// resource whose teardown must snapshot first. The message names the verb that
	// works, because the app-level call can never succeed while the claim is live.
	case errors.Is(err, domain.ErrAppBacksDurableResource):
		return status.Error(codes.FailedPrecondition, "this workload is the backing store of a durable resource — decommission the RESOURCE (DecommissionResource), which takes a final snapshot first; tearing the app down directly would destroy its data")
	case errors.Is(err, domain.ErrIngressHostTaken):
		return status.Error(codes.AlreadyExists, "the ingress host derived from this component id is already routed to another fleet app — a component id must be globally unique across the fleet")
	case errors.Is(err, domain.ErrSecretOwnerOrgMissing):
		return status.Error(codes.InvalidArgument, "secret refs require an owner org")
	case errors.Is(err, domain.ErrImageRefIncomplete):
		return status.Error(codes.InvalidArgument, "image reference requires registry, repository, and digest")
	case errors.Is(err, domain.ErrResidentPortRequired):
		return status.Error(codes.InvalidArgument, "resident workload requires a guest port")
	case errors.Is(err, domain.ErrImageBootUnavailable):
		return status.Error(codes.FailedPrecondition, "image boot requires the firecracker host")
	// SentiaeDB Phase 0 — the microVM addressing plane's refusals. All four are
	// HOST-state faults, never caller-input faults, so none of them is
	// InvalidArgument: the caller asked for something legitimate on a host that
	// cannot safely serve it. The messages say what an operator must do, because a
	// retry alone can only fix the transient conflict case.
	case errors.Is(err, domain.ErrNetPlaneUnreconciled):
		return status.Error(codes.FailedPrecondition, "this fleet host cannot prove which microVM addresses it holds, so it refuses to boot anything — see the host's startup log for the unreconciled lease and resolve it (teardown and health still work)")
	case errors.Is(err, domain.ErrHostNetOrdinalUnset):
		return status.Error(codes.FailedPrecondition, "this fleet host has no assigned microVM addressing block, so it cannot allocate an address; it must register successfully before it can boot")
	case errors.Is(err, domain.ErrNetLeaseExhausted):
		return status.Error(codes.ResourceExhausted, "this fleet host has no free microVM address slot")
	// Aborted, not Internal: a lease conflict is a lost race on a unique fence, so
	// the caller may retry — but the boot did NOT happen, which is what Aborted
	// says and Internal does not.
	case errors.Is(err, domain.ErrNetLeaseConflict):
		return status.Error(codes.Aborted, "microVM address allocation lost a race on this host; retry")
	// ⚠ Order matters, and it was wrong before: a quiesce refusal wraps BOTH
	// sentinels, so with ErrGuestControlUnavailable tested first every
	// "no channel" refusal answered with the generic channel message and only the
	// OTHER causes reached the "booted before the channel existed" text — i.e. the
	// reboot advice was given to exactly the operators it could not help. The
	// refusal is therefore matched first and split by its cause.
	case errors.Is(err, domain.ErrSnapshotNotQuiescible) && errors.Is(err, domain.ErrGuestControlUnavailable):
		return status.Error(codes.FailedPrecondition, "snapshot refused: this VM has no guest control channel — it was booted before the channel existed, so no retry can succeed; reboot the replica and snapshot again")
	case errors.Is(err, domain.ErrSnapshotNotQuiescible):
		return status.Error(codes.FailedPrecondition, "snapshot refused: the guest control channel is armed for this VM but the quiesce call failed — the guest is unreachable or refusing now; check the guest console and the runtime-service logs (a reboot is not the first move)")
	case errors.Is(err, domain.ErrGuestControlUnavailable):
		return status.Error(codes.FailedPrecondition, "guest control channel unavailable for this workload")
	case errors.Is(err, domain.ErrVolumesNotSupported):
		return status.Error(codes.InvalidArgument, "volumes are only supported for resident workloads")
	case errors.Is(err, domain.ErrVolumeAppNotScalable):
		return status.Error(codes.FailedPrecondition, "a volume-bearing app cannot scale beyond one replica")
	case errors.Is(err, domain.ErrVolumeBackendUnavailable):
		return status.Error(codes.FailedPrecondition, "volumes require the firecracker host")
	// D-203 ownership refusals. Mapped here rather than left to the default: the
	// raw errors name the owning resource and volume ids, and the default branch
	// echoes err.Error() straight to the tenant. Both stay server-side in the log.
	case errors.Is(err, domain.ErrVolumeOwnedByLiveResource):
		return status.Error(codes.FailedPrecondition, "volume is owned by a live durable resource; decommission the RESOURCE (DecommissionResource), which takes a final snapshot first")
	case errors.Is(err, domain.ErrVolumeClaimConflict):
		return status.Error(codes.FailedPrecondition, "this volume is already owned by a DIFFERENT durable resource claim — ownership is write-once and is never silently re-parented")
	// A provision that had to ADOPT an existing volume and found no data on the
	// host. Without this it reached the caller as the curated Internal, which reads
	// as "a bug, retry" — but no retry can invent the data, and the operator needs
	// to know a DISK is gone, not that the server misbehaved. The path stays
	// server-side (in the Error log); these messages are tenant-visible.
	case errors.Is(err, domain.ErrVolumeBackingFileMissing):
		return status.Error(codes.FailedPrecondition, "this app's persistent volume is recorded but its backing file is not on the host — refusing to attach an empty replacement; restore from a recovery point")
	// A DIFFERENT condition from the one above, and mapped separately for that
	// reason: the row itself is incomplete (it names no path), so there is no file to
	// look for and no recovery point that could repair it. It reached the caller as
	// Internal — "a bug, retry" — while the fix is an operator inspecting the ledger.
	// The volume id stays server-side; the raw error is logged by the caller.
	case errors.Is(err, domain.ErrVolumeBackingPathUnset):
		return status.Error(codes.FailedPrecondition, "this volume's ledger row records no backing path, so it cannot be snapshotted or attached — the row is incomplete and must be repaired before the resource can be used")
	// The identity refusals MUST be mapped, not left to the default: their errors
	// carry the host backing PATH and the foreign volume id, and the default branch
	// echoes err.Error() straight to the tenant. Both name the condition only; the
	// path stays server-side in the Error log the backend already writes.
	case errors.Is(err, domain.ErrVolumeIdentityMismatch):
		return status.Error(codes.FailedPrecondition, "the file at this volume's backing path belongs to a DIFFERENT volume — refusing to attach it; the volume's own data must be put back, or restore from a recovery point")
	case errors.Is(err, domain.ErrVolumeBackingFileUndersized):
		return status.Error(codes.FailedPrecondition, "this volume's backing file is smaller than the size recorded for it — refusing to attach it; the filesystem must be grown to match before it can be used")
	case errors.Is(err, domain.ErrFleetNetworkNotFound):
		return status.Error(codes.FailedPrecondition, "no active fleet network for this system and env — EnsureNetwork first")
	case errors.Is(err, domain.ErrNetworkEnforcerUnavailable):
		return status.Error(codes.FailedPrecondition, "fleet network enforcement requires the firecracker host")
	case errors.Is(err, domain.ErrNetworkPostureUnproven):
		return status.Error(codes.FailedPrecondition, "fleet network posture could not be proven on this host")
	case errors.Is(err, domain.ErrNetworkOwnerOrgRequired):
		return status.Error(codes.InvalidArgument, "fleet network requires an owner org")
	case errors.Is(err, domain.ErrInvalidNetworkPolicy):
		return status.Error(codes.InvalidArgument, "invalid network policy (component ids required; port must be 1..65535)")
	case errors.Is(err, domain.ErrUnsupportedPolicyProtocol):
		return status.Error(codes.InvalidArgument, "unsupported network policy protocol (want tcp)")
	case errors.Is(err, domain.ErrNetworkPolicyEgressOverlap):
		return status.Error(codes.InvalidArgument, "egress_allow may not name fleet-internal addresses")
	case errors.Is(err, domain.ErrFleetHostNotFound):
		return status.Error(codes.NotFound, "fleet host not found")
	case errors.Is(err, domain.ErrInvalidHostHealth):
		return status.Error(codes.InvalidArgument, "invalid host health (want healthy|degraded|unhealthy|unknown)")
	// SentiaeDB standard-ha — the host placement FACTS (D-196). InvalidArgument,
	// because a registration that omits them is a caller-input fault: the host
	// itself is the caller, and only its operator can supply the value.
	case errors.Is(err, domain.ErrHostFailureDomainRequired):
		return status.Error(codes.InvalidArgument, "a fleet host must state its failure domain (site/power/network, e.g. rgalileo-room/breaker-a/switch-1) — there is no default, because only a human knows which room and which breaker this machine is on")
	case errors.Is(err, domain.ErrHostFailureDomainInvalid):
		return status.Error(codes.InvalidArgument, "invalid fleet host failure domain: want three non-empty lowercase segments site/power/network (e.g. rgalileo-room/breaker-a/switch-1)")
	case errors.Is(err, domain.ErrHostRegionRequired):
		return status.Error(codes.InvalidArgument, "a fleet host must state its region")
	// SentiaeDB standard-ha — the placement invariant refusals (slice 1). All
	// FailedPrecondition: the claim is legitimate and the caller can retry it
	// unchanged once the FLEET can satisfy it — which is what FailedPrecondition
	// means and InvalidArgument does not. Each names the unmet condition, because
	// the operator action differs completely (buy a machine / state a domain / move
	// a machine / put them in one region) and "HA unavailable" would send someone
	// shopping for hardware they may already own.
	case errors.Is(err, domain.ErrHAHostsInsufficient):
		return status.Error(codes.FailedPrecondition, "standard-ha refused: it requires two live hosts in DIFFERENT failure domains, and this fleet does not have them — a highly-available database is not provisioned at all rather than provisioned as a single copy that claims otherwise")
	case errors.Is(err, domain.ErrHAFailureDomainUnattested):
		return status.Error(codes.FailedPrecondition, "standard-ha refused: fewer than two live hosts have stated a failure domain, so the fleet cannot prove two members would not die together (set APP_FLEET_FAILURE_DOMAIN on each host)")
	case errors.Is(err, domain.ErrHAFailureDomainShared):
		return status.Error(codes.FailedPrecondition, "standard-ha refused: every live host is in the SAME failure domain — a second host on one chassis, one breaker or one switch is not a second failure domain")
	case errors.Is(err, domain.ErrHARegionSplit):
		return status.Error(codes.FailedPrecondition, "standard-ha refused: the live hosts in different failure domains are in different REGIONS, and the standby must be same-region because the region is part of the database's permanent hostname")
	case errors.Is(err, domain.ErrHAAvailabilityClassInvalid):
		return status.Error(codes.InvalidArgument, "unsupported availability class (want single|ha)")
	case errors.Is(err, domain.ErrHAPlacementUnknowable):
		return status.Error(codes.FailedPrecondition, "standard-ha refused: this host cannot read the live fleet inventory, so it cannot prove the placement invariant holds")
	// The pause guard. A refusal is a permanent property of the VM class, never a
	// transient fault, so it must not reach a caller as Internal (which reads as
	// "retry") — firecracker vsock does not survive Pause/Resume, so no retry can
	// make pausing a data VM safe.
	case errors.Is(err, domain.ErrPauseUnsafeForResidentVM):
		return status.Error(codes.FailedPrecondition, "refused: this VM carries data and the component asked to hold it pauses its VMs — firecracker vsock does not survive Pause/Resume")
	case errors.Is(err, domain.ErrVMClassUndeclared):
		return status.Error(codes.FailedPrecondition, "refused: a VM handed to a pausing component must declare its class (pausable|resident)")
	// The host-authority fence (#fleet-reconciler-acts-on-foreign-host-replicas).
	// FailedPrecondition, not PermissionDenied or Internal: the call is legitimate
	// and unchanged retries succeed once it reaches the host that owns the row —
	// which is what FailedPrecondition means. NOTHING host-identifying is echoed
	// (no host uuid, path, pid or lease coordinate): the tenant learns that the
	// operation belongs elsewhere and that this host changed nothing, which is the
	// whole actionable content.
	case errors.Is(err, domain.ErrReplicaHostMismatch):
		return status.Error(codes.FailedPrecondition, "this replica belongs to another fleet host — no local action was taken; the operation is performed by the host the replica is placed on")
	case errors.Is(err, domain.ErrVolumeHostMismatch):
		return status.Error(codes.FailedPrecondition, "this volume's data lives on another fleet host — no local action was taken; the operation is performed by the host that holds the bytes")
	// Unavailable, not Internal or FailedPrecondition: the teardown was RETAINED
	// intact (nothing was released, nothing was deleted) and the same call may be
	// retried, which is exactly what Unavailable tells a caller.
	case errors.Is(err, domain.ErrVMTerminationUnproven):
		return status.Error(codes.Unavailable, "teardown retained: this workload's microVM could not be proven to have exited, so none of its resources were released — retry once the owning host can prove the VM is gone")
	default:
		return registryOrInternal(err)
	}
}

// registryOrInternal is fleetError's last resort before Internal: it consults
// the platform error registry (§16.3) for sentinels this handler does not
// hand-map — notably platform-kit/secret's P14 sentinels, registered in
// internal/app. Those are NOT runtime-local domain errors, so hand-mapping them
// here would fork their meaning away from the registry that owns it; the
// registry stays the single source of truth and this function is the seam that
// makes it FIRE (a registration nothing consults is not a control).
//
// An error the registry does not know keeps this handler's long-standing
// curated Internal. That matters: pkerrors.ToGRPC's own default echoes raw
// err.Error() to the caller, and this handler has never leaked internal error
// text (a DB or Vault error would go out verbatim). Codes registered AS
// Internal land here too, which is the identical outcome either way.
func registryOrInternal(err error) error {
	if ge := pkerrors.ToGRPC(err); status.Code(ge) != codes.Internal {
		return ge
	}
	return status.Error(codes.Internal, "internal server error")
}
