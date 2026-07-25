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

	// pgReady decides whether the provisioned engine ADMITS clients, over and
	// above the app health probe's process-alive + TCP-dial (see engineAdmits). A
	// field rather than a direct call so tests drive both verdicts without a live
	// engine; production is always probePostgresReady. The dedicated tier is
	// postgres only (see ProvisionDedicated's class gate), so the engine-specific
	// probe is exact here in a way it would not be on the general replica health
	// path. Same seam shape as FleetVolumeRestorer.pgReady on purpose — the two
	// readiness gates must never drift.
	pgReady func(ctx context.Context, host string, port int) error
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
		pgReady:     probePostgresReady,
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
	// RestoredInPlaceOK mirrors the domain field: this point was restored in
	// place and the engine came back admitting clients. NOT a verification drill.
	RestoredInPlaceOK bool
	CreatedAt         time.Time
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
	// conditionEngineNotAdmitting — the backing VM is alive and listening, but the
	// engine refuses (or cannot be proven to accept) a client connection. This is
	// the false-green case: process-alive + TCP-dial pass while every customer
	// connection is rejected (#p19-restore-false-green-health). The refusal detail
	// — SQLSTATE and message — is operator-facing and goes to the log, never here.
	conditionEngineNotAdmitting = "engine-not-admitting"
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
// would silently converge the app to something the claim never asked for. That
// single seam is also why the org namespace below cannot drift: the first
// provision and the recovery re-provision derive the SAME component id, so a
// recovery can never fall back to the old org-blind key and re-collide.
func (uc *FleetResourceProvisioner) dedicatedDescriptor(in ProvisionDedicatedInput) FleetProvisionInput {
	return FleetProvisionInput{
		// The owning org is INSIDE the component id, not just beside it in a column:
		// the ingress host is derived from the component id
		// (sanitizeSlug(component_id)-sanitizeSlug(env), unique-indexed by
		// migrations/0006), so an org-blind 'resource/<claim_key>' collides across
		// organisations on the route even once the app row itself is org-scoped.
		ComponentID:   "resource/" + in.OwnerOrg + "/" + in.ClaimKey,
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
		// One replica listing answers both questions below — whether the engine
		// admits a client, and what the private endpoint is — so it is read once
		// here rather than by each of them.
		reps, reperr := uc.replicas.ListByApp(ctx, *res.AppID)
		if reperr != nil {
			logger.FromContext(ctx).Warn("fleet resource: list replicas", "resource_id", res.ID, "app_id", res.AppID, "err", reperr)
		}
		// D-184 — a restore owns the phase while it runs. Auto-advancing to ready
		// here would race the restore's own verification window: the OLD engine can
		// still be healthy the instant before the restore drains it, and the
		// restored one is not proven until the restore says so.
		if herr == nil && h.Healthy && res.Phase != domain.FleetResourcePhaseRestoring {
			// Probed only here, never above: admission is a live network round trip,
			// and its verdict is unused unless the app is already healthy and not
			// mid-restore. Reading it eagerly would put a dial on the path of every
			// status read of an app that is already known to be down.
			aerr := uc.engineAdmits(ctx, *res.AppID, reps, reperr)
			if aerr == nil {
				status.Phase = string(domain.FleetResourcePhaseReady)
				// Keep the durable row honest: advance provisioning → ready once observed.
				if res.Phase != domain.FleetResourcePhaseReady {
					if uerr := uc.resources.UpdateResourcePhase(ctx, res.ID, domain.FleetResourcePhaseReady); uerr != nil {
						logger.FromContext(ctx).Warn("fleet resource: persist ready phase", "resource_id", res.ID, "err", uerr)
					}
				}
			} else {
				// Alive but refusing clients: the resource EXISTS and is impaired, which
				// is exactly what degraded means. Reported, never persisted — a stored
				// ready is left alone here. What a demotion should mean durably (and
				// what would then reconcile it back) is a separate decision; until it is
				// made, this call only tells the truth about the observation it just
				// took, and never rewrites the row on a probe that could itself be the
				// thing that is wrong.
				status.Phase = string(domain.FleetResourcePhaseDegraded)
				status.Conditions = append(status.Conditions, conditionEngineNotAdmitting)
				logger.FromContext(ctx).Warn("fleet resource: backing app is healthy but the engine does not admit clients",
					"resource_id", res.ID, "app_id", res.AppID, "err", aerr)
			}
		}
		if ep := residentEndpointOf(reps); ep != "" {
			status.Endpoint = ep
		}
	}

	if rps, rerr := uc.resources.ListRecoveryPoints(ctx, res.ID); rerr != nil {
		logger.FromContext(ctx).Warn("fleet resource: list recovery points", "resource_id", res.ID, "err", rerr)
	} else if len(rps) > 0 {
		rp := rps[0] // newest first
		status.LastRecoveryPoint = &RecoveryPointInfo{
			ID:                rp.ID.String(),
			ObjectKey:         rp.ObjectKey,
			Kind:              rp.Kind,
			SizeBytes:         rp.SizeBytes,
			RestoredInPlaceOK: rp.RestoredInPlaceOK,
			CreatedAt:         rp.CreatedAt,
		}
	}
	return status, nil
}

// engineAdmits checks that every resident replica of the app lets a client
// through pg_hba to authentication, using the credential-free probe. It takes
// the already-read replica listing (and the error that produced it) so StatusOf
// reads the store once.
//
// This is READINESS, and it is deliberately NOT folded into the app health probe
// that drives it (FleetReplicaRuntime.RefreshHealth stays process-alive + TCP
// dial). That probe is LIVENESS: the reconciler restarts what it calls unhealthy,
// so teaching it about pg_hba would turn a broken-but-alive engine into a restart
// crashloop that destroys the very state an operator needs to repair. Liveness
// decides whether to restart; readiness decides what the customer is told.
//
// Fail-closed throughout, matching FleetVolumeRestorer.engineAdmits: no probe
// wired, a repository error, a replica listed as resident with no guest address,
// and an app with no resident replica at all are ALL failures, because each of
// them means the engine cannot be PROVEN usable — and an unprovable engine must
// never be reported as ready over customer data.
func (uc *FleetResourceProvisioner) engineAdmits(ctx context.Context, appID uuid.UUID, reps []domain.Replica, listErr error) error {
	if uc.pgReady == nil {
		return fmt.Errorf("no readiness probe is wired, so the engine of app %s cannot be confirmed usable", appID)
	}
	if listErr != nil {
		return fmt.Errorf("list replicas of app %s: %w", appID, listErr)
	}
	probed := 0
	for i := range reps {
		r := &reps[i]
		if r.State != domain.ReplicaStateResident {
			continue
		}
		if r.GuestIP == "" || r.Port <= 0 {
			return fmt.Errorf("replica %s is resident but carries no guest address to probe", r.ID)
		}
		if perr := uc.pgReady(ctx, r.GuestIP, r.Port); perr != nil {
			return fmt.Errorf("replica %s: %w", r.ID, perr)
		}
		probed++
	}
	if probed == 0 {
		return fmt.Errorf("app %s has no resident replica to probe", appID)
	}
	return nil
}

// residentEndpointOf returns the private "guest-ip:5432" of the first resident
// replica (same scheme as fleet_replica_runtime), or "" when none is resident.
func residentEndpointOf(replicas []domain.Replica) string {
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateResident && replicas[i].GuestIP != "" {
			return fmt.Sprintf("%s:%d", replicas[i].GuestIP, residentPGPort)
		}
	}
	return ""
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
			// An app row that is ALREADY GONE is not a teardown failure — there is
			// nothing left to tear down, and the only work still owed is the tombstone.
			// Without this the resource is stuck forever: fleet_volumes cascades on
			// fleet_apps (migrations/0001 :89) while fleet_resources.app_id carries no
			// FK (migrations/0012), so a teardown that deletes the app and then fails
			// writing the tombstone leaves a row pointing at a vanished app that can
			// neither boot (recoverExisting refuses it, correctly — a re-provision
			// would mint a NEW empty volume) nor retire (this call aborted). The
			// snapshot-first guarantee above is UNTOUCHED: reaching here means a
			// recovery point provably exists, and a resource with none is still refused.
			//
			// The tolerance lives HERE and not in FleetProvision.Decommission on
			// purpose: making an unknown handle idempotent success at the P7 seam would
			// widen that shared contract's semantics for every caller of it.
			if !errors.Is(derr, domain.ErrWorkloadNotFound) && !errors.Is(derr, domain.ErrFleetAppNotFound) {
				return nil, fmt.Errorf("decommission app: %w", derr)
			}
			// Abnormal path — an operator needs to see that this teardown completed
			// over a backing app that had already vanished, and on which recovery
			// point it is leaving the customer.
			reliedOn := "none"
			if final != nil {
				reliedOn = final.ID.String()
			}
			logger.FromContext(ctx).Warn("fleet decommission: backing app was already gone; retiring the resource row on its existing recovery point",
				"resource_id", res.ID, "missing_app_id", res.AppID, "recovery_point_id", reliedOn, "err", derr)
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
