package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// resourceClassPostgres / resourceTierDedicated are the only class/tier the
// dedicated provision path serves this slice.
const (
	resourceClassPostgres = "postgres"
	resourceTierDedicated = "dedicated"
)

// ─────────────────────────────────────────────────────────────────────
// Ports the resource provisioner composes (never re-implements): the image-boot
// FleetProvision use case and the D-080 volume snapshotter.
// ─────────────────────────────────────────────────────────────────────

// FleetProvisioner is the subset of *FleetProvision the resource control plane
// composes. Injecting it (rather than the concrete type) keeps the resource
// provisioner off the boot/volume/secret internals — it inherits volume ensure,
// host affinity, P14 boot-secret vsock resolution, the D-124 pull token, and the
// P21 system_id gate for free.
type FleetProvisioner interface {
	Provision(ctx context.Context, in FleetProvisionInput) (FleetProvisionOutput, error)
	Health(ctx context.Context, handle string) (FleetHealthOutput, error)
	Decommission(ctx context.Context, handle string) error
}

// VolumeSnapshotter is the subset of *FleetVolumeSnapshotter the resource
// control plane composes for a snapshot-first decommission.
type VolumeSnapshotter interface {
	SnapshotAppVolumes(ctx context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error)
}

var (
	_ FleetProvisioner  = (*FleetProvision)(nil)
	_ VolumeSnapshotter = (*FleetVolumeSnapshotter)(nil)
)

// DedicatedEngineConfig is the resolved engine-image + status config for the
// dedicated tier (populated from APP_RESOURCE_ENGINE_PG_IMAGE_* / conn budget).
type DedicatedEngineConfig struct {
	Registry   string
	Repository string
	Digest     string
	ConnBudget int
}

// ─────────────────────────────────────────────────────────────────────
// FleetResourceProvisioner use case (R2, CP4.5 §9 #3, D-183).
// ─────────────────────────────────────────────────────────────────────

// FleetResourceProvisioner provisions a dedicated resident Postgres data-VM as a
// durable P19 resource. It composes FleetProvision (boot + volume + secret) and
// records a fleet_resources claim keyed on (owner_org, claim_key, env).
type FleetResourceProvisioner struct {
	provisioner FleetProvisioner
	resources   repository.FleetResourceRepository
	replicas    repository.ReplicaRepository
	snapshotter VolumeSnapshotter
	engine      DedicatedEngineConfig
}

// NewFleetResourceProvisioner constructs the use case.
func NewFleetResourceProvisioner(
	provisioner FleetProvisioner,
	resources repository.FleetResourceRepository,
	replicas repository.ReplicaRepository,
	snapshotter VolumeSnapshotter,
	engine DedicatedEngineConfig,
) *FleetResourceProvisioner {
	return &FleetResourceProvisioner{
		provisioner: provisioner,
		resources:   resources,
		replicas:    replicas,
		snapshotter: snapshotter,
		engine:      engine,
	}
}

// ProvisionDedicatedInput is the wire-agnostic dedicated-resource claim.
type ProvisionDedicatedInput struct {
	OwnerOrg   string
	ClaimKey   string
	Env        string
	Revision   int
	Class      string
	Tier       string
	SystemID   string
	SecretRefs []string
	VaultToken string
	SizeMB     int64
}

// ProvisionDedicatedOutput is the claim result: the resource handle + phase.
type ProvisionDedicatedOutput struct {
	Handle string
	Phase  string
}

// RecoveryPointInfo is a resource's newest recovery-point summary.
type RecoveryPointInfo struct {
	ID        string
	ObjectKey string
	Kind      string
	SizeBytes int64
	Verified  bool
	CreatedAt time.Time
}

// ResourceStatus is the live status of a resource claim. Conditions carries the
// stable reason tokens below when the resource is not simply healthy.
type ResourceStatus struct {
	Handle            string
	Phase             string
	Tier              string
	Endpoint          string
	SecretRefs        []string
	ConnBudget        int
	Conditions        []string
	LastRecoveryPoint *RecoveryPointInfo
}

// Condition tokens reported on ResourceStatus.Conditions. They are STABLE,
// machine-readable strings — never the raw error text, which is operator-facing
// detail that belongs in the log, not on a tenant-visible API.
const (
	// conditionBackingAppMissing — the resource row points at an app the fleet no
	// longer knows. Its recovery points survive, so this is a restore/rebuild
	// decision, not a dead end; it just cannot be read as health.
	conditionBackingAppMissing = "backing-app-missing"
	// conditionHealthUnavailable — the health of the backing app could not be
	// read at all (a store or transport fault). It says nothing about the data.
	conditionHealthUnavailable = "health-unavailable"
)

// healthCondition classifies a failed health probe into a condition token. The
// distinction is load-bearing for an operator: a MISSING app needs a rebuild or
// a restore, while an unreadable one needs nothing but a retry.
func healthCondition(err error) string {
	if errors.Is(err, domain.ErrWorkloadNotFound) || errors.Is(err, domain.ErrFleetAppNotFound) {
		return conditionBackingAppMissing
	}
	return conditionHealthUnavailable
}

// ProvisionDedicated declaratively ensures a dedicated Postgres data-VM for a
// claim. It is idempotent per (owner_org, claim_key, env): the same revision
// returns the current status; a different revision is REJECTED (converge/resize
// is a later slice); a concurrent insert-race returns the winner.
func (uc *FleetResourceProvisioner) ProvisionDedicated(ctx context.Context, in ProvisionDedicatedInput) (ProvisionDedicatedOutput, error) {
	if in.Class != resourceClassPostgres {
		return ProvisionDedicatedOutput{}, domain.ErrResourceClassUnsupported
	}
	if in.Tier != resourceTierDedicated {
		return ProvisionDedicatedOutput{}, domain.ErrResourceTierUnsupported
	}
	if in.OwnerOrg == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceOwnerOrgRequired
	}
	ownerUUID, err := uuid.Parse(in.OwnerOrg)
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("parse owner org: %w", err)
	}
	if in.ClaimKey == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceClaimKeyRequired
	}
	// The data engine cannot boot credential-less — fail closed (I32).
	if len(in.SecretRefs) == 0 {
		return ProvisionDedicatedOutput{}, domain.ErrResourceSecretsRequired
	}
	// The engine credentials resolve under the handed per-deployment Vault token.
	if in.VaultToken == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceVaultTokenRequired
	}
	if uc.engine.Registry == "" || uc.engine.Repository == "" || uc.engine.Digest == "" {
		return ProvisionDedicatedOutput{}, domain.ErrImageRefIncomplete
	}
	revision := in.Revision
	if revision <= 0 {
		revision = 1
	}

	// Idempotency: an existing claim with the same revision is a declarative
	// ensure; a different revision is a converge the current backend rejects.
	existing, err := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env)
	if err == nil {
		if existing.Revision == revision {
			// #p19-handed-token-not-rehandable — this is NOT a bare no-op, and must
			// never be "simplified" back into one. The per-deployment Vault token that
			// resolves this engine's boot secrets is MEMORY-ONLY BY DESIGN (D-125:
			// never a row, rootfs, runtime.json, or log), so a runtime-service restart
			// empties the store and the resource's next (re)boot fails closed with "no
			// handed token" — permanently, because nothing else ever re-hands it. A
			// declarative re-provision is the recovery a caller naturally attempts and
			// the only authenticated moment a fresh token arrives, so it is THE
			// designed post-restart recovery path. A database you lose on reboot is
			// not durable.
			uc.recoverExisting(ctx, existing, in)
			return ProvisionDedicatedOutput{Handle: existing.ID.String(), Phase: string(existing.Phase)}, nil
		}
		return ProvisionDedicatedOutput{}, domain.ErrResourceConvergeNotSupported
	}
	if !errors.Is(err, domain.ErrResourceNotFound) {
		return ProvisionDedicatedOutput{}, fmt.Errorf("lookup resource claim: %w", err)
	}

	// Compose FleetProvision: a resident, single-replica, volume-bearing app.
	provOut, err := uc.provisioner.Provision(ctx, uc.dedicatedDescriptor(in))
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("provision dedicated engine: %w", err)
	}
	appHandle, err := uuid.Parse(provOut.Handle)
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("parse app handle: %w", err)
	}

	now := time.Now().UTC()
	res := &domain.FleetResource{
		ID:         uuid.New(),
		OwnerOrg:   ownerUUID,
		ClaimKey:   in.ClaimKey,
		Env:        in.Env,
		Revision:   revision,
		Class:      resourceClassPostgres,
		Tier:       resourceTierDedicated,
		Phase:      domain.FleetResourcePhaseProvisioning,
		AppID:      &appHandle,
		SecretRefs: in.SecretRefs,
		SystemID:   in.SystemID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		// Lost the insert race on (owner_org, claim_key, env): another concurrent
		// claim won. The FleetApp upsert is idempotent per (component_id, env), so
		// both racers converged on one app — return the winning resource row.
		if winner, ferr := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env); ferr == nil {
			return ProvisionDedicatedOutput{Handle: winner.ID.String(), Phase: string(winner.Phase)}, nil
		}
		return ProvisionDedicatedOutput{}, fmt.Errorf("persist resource: %w", err)
	}
	return ProvisionDedicatedOutput{Handle: res.ID.String(), Phase: string(res.Phase)}, nil
}

// dedicatedDescriptor builds the FleetProvision descriptor for a dedicated
// claim: a resident, single-replica, volume-bearing Postgres engine. The first
// provision and the post-restart recovery re-provision BOTH build it here, so
// the two can never drift — a "recovery" that handed a different descriptor
// would silently converge the app to something the claim never asked for.
func (uc *FleetResourceProvisioner) dedicatedDescriptor(in ProvisionDedicatedInput) FleetProvisionInput {
	return FleetProvisionInput{
		ComponentID:   "resource/" + in.ClaimKey,
		Env:           in.Env,
		OwnerOrg:      in.OwnerOrg,
		Registry:      uc.engine.Registry,
		Repository:    uc.engine.Repository,
		Digest:        uc.engine.Digest,
		Port:          residentPGPort,
		WorkloadClass: string(domain.ImageWorkloadClassResident),
		SecretRefs:    in.SecretRefs,
		VaultToken:    in.VaultToken,
		SystemID:      in.SystemID,
		EnvVars:       map[string]string{"PGDATA": "/data/pgdata"},
		Volumes:       []VolumeSpecInput{{SizeMB: in.SizeMB, MountPath: "/data"}},
		MinReplicas:   1,
		MaxReplicas:   1,
		ScaleToZero:   false,
	}
}

// recoverExisting re-hands the caller's per-deployment Vault token to an already
// claimed resource's backing app and lets the reconciler boot whatever is not
// running (#p19-handed-token-not-rehandable).
//
// It re-runs the SAME composed FleetProvisioner.Provision the first provision
// used rather than inventing a recovery path: that call is an upsert keyed on
// (component_id, env), so for an app that still exists it re-hands the token
// into the in-memory store, re-ensures the SAME volume rows and ingress route,
// and then reconciles once — which is exactly the machinery that already knows
// how to leave a healthy replica alone and replace a dead or missing one, now
// with the token in hand.
//
// Best-effort by contract: the Handle/Phase this path returns is frozen and
// callers poll it, so every failure here is logged and swallowed — the resource
// is never left worse off than before, and the periodic reconcile retries.
func (uc *FleetResourceProvisioner) recoverExisting(ctx context.Context, res *domain.FleetResource, in ProvisionDedicatedInput) {
	if res.Phase == domain.FleetResourcePhaseDecommissioned {
		return // a tombstone was torn down on purpose; never re-boot it
	}
	if res.AppID == nil {
		logger.FromContext(ctx).Warn("fleet resource: re-provision cannot recover a claim with no backing app",
			"resource_id", res.ID)
		return
	}
	// The re-provision below is an upsert on (component_id, env). If the app row
	// is GONE it would mint a NEW app id — and with it a NEW, EMPTY data volume —
	// while this resource row still points at the vanished app: a live but empty
	// engine masquerading as the customer's database. Refuse loudly instead; an
	// app-row rebuild / restore-from-recovery-point is a different operation.
	if _, herr := uc.provisioner.Health(ctx, res.AppID.String()); herr != nil {
		logger.FromContext(ctx).Error("fleet resource: backing app is gone — re-provision cannot recover it, needs an explicit rebuild or restore",
			"resource_id", res.ID, "app_id", res.AppID, "err", herr)
		return
	}
	if _, perr := uc.provisioner.Provision(ctx, uc.dedicatedDescriptor(in)); perr != nil {
		logger.FromContext(ctx).Error("fleet resource: recover dedicated engine failed (token re-hand / re-drive boot)",
			"resource_id", res.ID, "app_id", res.AppID, "err", perr)
	}
}

// StatusOf returns the live status of a dedicated resource: the backing app's
// health (healthy → ready), its private endpoint, its connection budget, and its
// newest recovery point.
//
// A health probe that FAILS is reported as a condition, never as an error: this
// call is how an operator (and the portal) sees a resource at all, so a resource
// that is stuck must come back legible rather than as a broken RPC.
func (uc *FleetResourceProvisioner) StatusOf(ctx context.Context, resourceID uuid.UUID) (ResourceStatus, error) {
	res, err := uc.resources.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		return ResourceStatus{}, err
	}
	status := ResourceStatus{
		Handle:     res.ID.String(),
		Phase:      string(res.Phase),
		Tier:       res.Tier,
		Endpoint:   res.Endpoint,
		SecretRefs: res.SecretRefs,
		ConnBudget: uc.engine.ConnBudget,
	}

	if res.Tier == resourceTierDedicated && res.AppID != nil &&
		res.Phase != domain.FleetResourcePhaseDecommissioned {
		h, herr := uc.provisioner.Health(ctx, res.AppID.String())
		if herr != nil {
			// A resource whose health cannot be read is a STATE, not an API error.
			// Hard-erroring here made the one case an operator most needs to see —
			// a resource stuck because its backing app is gone — read as a broken
			// endpoint: no phase, no endpoint, no recovery catalog, nothing to act
			// on. The durable phase stands, the condition says why, and the recovery
			// points below still list (they are what a recovery is built from).
			status.Conditions = append(status.Conditions, healthCondition(herr))
			logger.FromContext(ctx).Warn("fleet resource: health probe failed, reporting it as a condition",
				"resource_id", res.ID, "app_id", res.AppID, "err", herr)
		}
		// D-184 — a restore owns the phase while it runs. Auto-advancing to ready
		// here would race the restore's own verification window: the OLD engine can
		// still be healthy the instant before the restore drains it, and the
		// restored one is not proven until the restore says so.
		if herr == nil && h.Healthy && res.Phase != domain.FleetResourcePhaseRestoring {
			status.Phase = string(domain.FleetResourcePhaseReady)
			// Keep the durable row honest: advance provisioning → ready once observed.
			if res.Phase != domain.FleetResourcePhaseReady {
				if uerr := uc.resources.UpdateResourcePhase(ctx, res.ID, domain.FleetResourcePhaseReady); uerr != nil {
					logger.FromContext(ctx).Warn("fleet resource: persist ready phase", "resource_id", res.ID, "err", uerr)
				}
			}
		}
		if ep, eerr := uc.residentEndpoint(ctx, *res.AppID); eerr != nil {
			logger.FromContext(ctx).Warn("fleet resource: resolve endpoint", "resource_id", res.ID, "err", eerr)
		} else if ep != "" {
			status.Endpoint = ep
		}
	}

	if rps, rerr := uc.resources.ListRecoveryPoints(ctx, res.ID); rerr != nil {
		logger.FromContext(ctx).Warn("fleet resource: list recovery points", "resource_id", res.ID, "err", rerr)
	} else if len(rps) > 0 {
		rp := rps[0] // newest first
		status.LastRecoveryPoint = &RecoveryPointInfo{
			ID:        rp.ID.String(),
			ObjectKey: rp.ObjectKey,
			Kind:      rp.Kind,
			SizeBytes: rp.SizeBytes,
			Verified:  rp.Verified,
			CreatedAt: rp.CreatedAt,
		}
	}
	return status, nil
}

// residentEndpoint returns the private "guest-ip:5432" of the app's resident
// replica (same scheme as fleet_replica_runtime), or "" when none is resident.
func (uc *FleetResourceProvisioner) residentEndpoint(ctx context.Context, appID uuid.UUID) (string, error) {
	replicas, err := uc.replicas.ListByApp(ctx, appID)
	if err != nil {
		return "", err
	}
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateResident && replicas[i].GuestIP != "" {
			return fmt.Sprintf("%s:%d", replicas[i].GuestIP, residentPGPort), nil
		}
	}
	return "", nil
}

// DecommissionDedicated tears down a dedicated resource. A durable tier is torn
// down snapshot-first: the final snapshot MUST succeed AND MUST have produced a
// recovery point, or the decommission fails. The row becomes a tombstone
// (recovery points retained).
//
// It RETURNS the final recovery point so the caller can verify the guarantee
// instead of inferring it from a status code. Without that, `final_snapshot=true`
// proved only that a snapshot CALL succeeded — and a call over an app with zero
// volumes succeeds while creating nothing, which is exactly the shape in which a
// resource is destroyed with nothing to restore from. The return is nil only
// when no final snapshot was asked for (an ephemeral tier) or when the resource
// was already a tombstone.
func (uc *FleetResourceProvisioner) DecommissionDedicated(ctx context.Context, resourceID uuid.UUID, finalSnapshot bool) (*domain.FleetResourceRecoveryPoint, error) {
	res, err := uc.resources.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		return nil, err
	}
	if res.Phase == domain.FleetResourcePhaseDecommissioned {
		return nil, nil // idempotent
	}
	// A durable tier must not be torn down without a recovery point. Only an
	// ephemeral tier/lifecycle may skip the final snapshot — dedicated is durable.
	if !finalSnapshot && res.Tier == resourceTierDedicated {
		return nil, domain.ErrResourceFinalSnapshotRequired
	}
	var final *domain.FleetResourceRecoveryPoint
	if finalSnapshot {
		if uc.snapshotter == nil {
			return nil, domain.ErrResourceFinalSnapshotRequired
		}
		if res.AppID == nil {
			return nil, fmt.Errorf("dedicated resource %s has no backing app to snapshot", res.ID)
		}
		// Snapshot-first: fail the decommission if the snapshot fails.
		points, serr := uc.snapshotter.SnapshotAppVolumes(ctx, res.ID, *res.AppID)
		if serr != nil {
			return nil, fmt.Errorf("final snapshot: %w", serr)
		}
		// ZERO NEW recovery points is not automatically a refusal — the guarantee
		// is that a recovery point EXISTS, not that this call produced one.
		//
		// The snapshotter walks the app's volumes, so an app carrying none returns
		// ([], nil): the call worked and captured nothing. That happens in two very
		// different situations, and conflating them creates a permanently stuck
		// resource. Consider a teardown that snapshots successfully, deletes the
		// app (cascading its volumes away), and then fails writing the tombstone:
		// the retry finds no volumes, produces no points, and would be refused
		// forever — on a resource that provably HAS a good final recovery point
		// sitting in the store. Recovery points deliberately outlive the tombstone
		// precisely so this is answerable.
		//
		// So: fall back to the catalog. A prior recovery point satisfies the
		// guarantee and the teardown proceeds. None at all means the resource
		// really would be destroyed with nothing to restore from, and that is the
		// refusal worth keeping — refusing costs an operator a question, proceeding
		// costs the data.
		if len(points) == 0 {
			prior, perr := uc.resources.ListRecoveryPoints(ctx, res.ID)
			if perr != nil {
				return nil, fmt.Errorf("resource %s produced no recovery point and its catalog is unreadable: %w", res.ID, perr)
			}
			if len(prior) == 0 {
				return nil, fmt.Errorf("%w: resource %s produced NO recovery point and has none on record, so tearing it down would leave nothing to restore from",
					domain.ErrResourceFinalSnapshotRequired, res.ID)
			}
			logger.FromContext(ctx).Warn("fleet decommission: final snapshot captured nothing; proceeding on an existing recovery point",
				"resource_id", res.ID, "prior_recovery_points", len(prior))
			final = &prior[0]
		} else {
			// The dedicated tier is single-volume by construction (dedicatedDescriptor
			// requests exactly one, and in-place restore refuses anything else), so the
			// first point IS the resource's final recovery point — the same choice the
			// SnapshotResource RPC makes.
			//
			// This MUST stay in the else. Outside it, the empty-points path above
			// indexes points[0] and panics, and it silently overwrites the prior
			// recovery point the fallback just resolved.
			final = &points[0]
		}
	}
	if res.AppID != nil {
		if derr := uc.provisioner.Decommission(ctx, res.AppID.String()); derr != nil {
			return nil, fmt.Errorf("decommission app: %w", derr)
		}
	}
	now := time.Now().UTC()
	res.Phase = domain.FleetResourcePhaseDecommissioned
	res.DecommissionedAt = &now
	res.UpdatedAt = now
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		return nil, fmt.Errorf("tombstone resource: %w", err)
	}
	return final, nil
}
