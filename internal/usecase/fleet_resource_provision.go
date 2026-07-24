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

// ResourceStatus is the live status of a resource claim.
type ResourceStatus struct {
	Handle            string
	Phase             string
	Tier              string
	Endpoint          string
	SecretRefs        []string
	ConnBudget        int
	LastRecoveryPoint *RecoveryPointInfo
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
	// no-op; a different revision is a converge the current backend rejects.
	existing, err := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env)
	if err == nil {
		if existing.Revision == revision {
			return ProvisionDedicatedOutput{Handle: existing.ID.String(), Phase: string(existing.Phase)}, nil
		}
		return ProvisionDedicatedOutput{}, domain.ErrResourceConvergeNotSupported
	}
	if !errors.Is(err, domain.ErrResourceNotFound) {
		return ProvisionDedicatedOutput{}, fmt.Errorf("lookup resource claim: %w", err)
	}

	// Compose FleetProvision: a resident, single-replica, volume-bearing app.
	provOut, err := uc.provisioner.Provision(ctx, FleetProvisionInput{
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
	})
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

// StatusOf returns the live status of a dedicated resource: the backing app's
// health (healthy → ready), its private endpoint, its connection budget, and its
// newest recovery point.
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
			return ResourceStatus{}, fmt.Errorf("resource health: %w", herr)
		}
		if h.Healthy {
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
// down snapshot-first: the final snapshot MUST succeed or the decommission
// fails. The row becomes a tombstone (recovery points retained).
func (uc *FleetResourceProvisioner) DecommissionDedicated(ctx context.Context, resourceID uuid.UUID, finalSnapshot bool) error {
	res, err := uc.resources.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		return err
	}
	if res.Phase == domain.FleetResourcePhaseDecommissioned {
		return nil // idempotent
	}
	// A durable tier must not be torn down without a recovery point. Only an
	// ephemeral tier/lifecycle may skip the final snapshot — dedicated is durable.
	if !finalSnapshot && res.Tier == resourceTierDedicated {
		return domain.ErrResourceFinalSnapshotRequired
	}
	if finalSnapshot {
		if uc.snapshotter == nil {
			return domain.ErrResourceFinalSnapshotRequired
		}
		if res.AppID == nil {
			return fmt.Errorf("dedicated resource %s has no backing app to snapshot", res.ID)
		}
		// Snapshot-first: fail the decommission if the snapshot fails.
		if _, serr := uc.snapshotter.SnapshotAppVolumes(ctx, res.ID, *res.AppID); serr != nil {
			return fmt.Errorf("final snapshot: %w", serr)
		}
	}
	if res.AppID != nil {
		if derr := uc.provisioner.Decommission(ctx, res.AppID.String()); derr != nil {
			return fmt.Errorf("decommission app: %w", derr)
		}
	}
	now := time.Now().UTC()
	res.Phase = domain.FleetResourcePhaseDecommissioned
	res.DecommissionedAt = &now
	res.UpdatedAt = now
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		return fmt.Errorf("tombstone resource: %w", err)
	}
	return nil
}
