package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetOrchestrator is the runtime-fleet CP4 §9#7 reconciler: it drives a
// FleetApp's actual replica set toward its desired replica count by placing
// (scheduler §9#5) + booting (replica runtime §9#6) shortfall replicas,
// replacing dead ones (restart_policy), and draining surplus. ProvisionApp /
// HealthApp / ScaleApp / DecommissionApp are the app-model P7 surface the
// resident workload class binds to.
type FleetOrchestrator struct {
	apps      repository.FleetAppRepository
	replicas  repository.ReplicaRepository
	scheduler *FleetScheduler
	runtime   *FleetReplicaRuntime
}

// NewFleetOrchestrator constructs the reconciler.
func NewFleetOrchestrator(
	apps repository.FleetAppRepository,
	replicas repository.ReplicaRepository,
	scheduler *FleetScheduler,
	runtime *FleetReplicaRuntime,
) *FleetOrchestrator {
	return &FleetOrchestrator{apps: apps, replicas: replicas, scheduler: scheduler, runtime: runtime}
}

// ProvisionApp upserts the FleetApp for (ComponentID, Env) from the descriptor,
// reconciles it once synchronously, and returns the app handle plus a resident
// replica's endpoint (empty when none is resident yet — delivery polls Health).
func (uc *FleetOrchestrator) ProvisionApp(ctx context.Context, in FleetProvisionInput) (string, string, error) {
	vcpu := in.VCPU
	if vcpu < 1 {
		vcpu = 1
	}
	memMB := int64(in.MemoryMB)
	if memMB < 512 {
		memMB = 512
	}

	app, err := uc.apps.FindByComponentEnv(ctx, in.ComponentID, in.Env)
	switch {
	case err == nil:
		app.ImageRepository = in.Repository
		app.ImageDigest = in.Digest
		app.Port = in.Port
		app.ResourcesVCPU = vcpu
		app.ResourcesMemMB = memMB
		app.OwnerOrg = in.OwnerOrg
		app.SecretRefs = in.SecretRefs
		if app.DesiredReplicas < 1 {
			app.DesiredReplicas = 1
		}
		app.UpdatedAt = time.Now().UTC()
		if err := uc.apps.Update(ctx, app); err != nil {
			return "", "", fmt.Errorf("update fleet app: %w", err)
		}
	case errors.Is(err, domain.ErrFleetAppNotFound):
		now := time.Now().UTC()
		app = &domain.FleetApp{
			ID:              uuid.New(),
			ComponentID:     in.ComponentID,
			Env:             in.Env,
			OwnerOrg:        in.OwnerOrg,
			ImageRepository: in.Repository,
			ImageDigest:     in.Digest,
			SecretRefs:      in.SecretRefs,
			DesiredReplicas: 1,
			MinReplicas:     0,
			MaxReplicas:     1,
			ScaleToZero:     false,
			Port:            in.Port,
			ResourcesVCPU:   vcpu,
			ResourcesMemMB:  memMB,
			RestartPolicy:   domain.RestartPolicyAlways,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := uc.apps.Create(ctx, app); err != nil {
			return "", "", fmt.Errorf("create fleet app: %w", err)
		}
	default:
		return "", "", fmt.Errorf("lookup fleet app: %w", err)
	}

	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return "", "", err
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return "", "", fmt.Errorf("list replicas: %w", err)
	}
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateResident {
			return app.ID.String(), replicas[i].Endpoint, nil
		}
	}
	return app.ID.String(), "", nil
}

// ReconcileApp drives one app toward its desired replica count. Per-replica
// scheduling/boot failures are logged and retried next tick — only a hard repo
// error aborts. Idempotent.
func (uc *FleetOrchestrator) ReconcileApp(ctx context.Context, appID uuid.UUID) error {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return nil
		}
		return fmt.Errorf("load app: %w", err)
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	// Dead replicas → decommission (frees the handle + deletes the row). With
	// restart_policy=always the shortfall step below re-creates them.
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateDead {
			if derr := uc.runtime.DecommissionReplica(ctx, replicas[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission dead replica", "replica_id", replicas[i].ID, "err", derr)
			}
		}
	}

	// Refresh resident health; a now-dead resident is decommissioned so it is
	// replaced by the shortfall step.
	for i := range replicas {
		if replicas[i].State != domain.ReplicaStateResident {
			continue
		}
		healthy, herr := uc.runtime.RefreshHealth(ctx, &replicas[i])
		if herr != nil {
			logger.FromContext(ctx).Warn("fleet reconcile: refresh health", "replica_id", replicas[i].ID, "err", herr)
			continue
		}
		if !healthy {
			if derr := uc.runtime.DecommissionReplica(ctx, replicas[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission dead resident", "replica_id", replicas[i].ID, "err", derr)
			}
		}
	}

	// Recount occupying replicas after the dead cleanup.
	occupying := make([]domain.Replica, 0, len(replicas))
	for i := range replicas {
		switch replicas[i].State {
		case domain.ReplicaStateScheduled, domain.ReplicaStateBooting, domain.ReplicaStateResident:
			occupying = append(occupying, replicas[i])
		}
	}

	switch {
	case len(occupying) < app.DesiredReplicas:
		shortfall := app.DesiredReplicas - len(occupying)
		for n := 0; n < shortfall; n++ {
			host, serr := uc.scheduler.SelectHost(ctx, PlacementRequest{
				AppID:      app.ID,
				NeedVCPU:   app.ResourcesVCPU,
				NeedMemMB:  app.ResourcesMemMB,
				NeedDiskMB: 0,
				Constraint: domain.PlacementConstraintBinPack,
			})
			if serr != nil {
				if errors.Is(serr, domain.ErrNoSchedulableHost) {
					logger.FromContext(ctx).Warn("fleet reconcile: no schedulable host, deferring", "app_id", app.ID)
					return nil
				}
				return fmt.Errorf("select host: %w", serr)
			}
			hostID := host
			now := time.Now().UTC()
			replica := &domain.Replica{
				ID:              uuid.New(),
				AppID:           app.ID,
				HostID:          &hostID,
				ImageRepository: app.ImageRepository,
				ImageDigest:     app.ImageDigest,
				State:           domain.ReplicaStateScheduled,
				RestartPolicy:   app.RestartPolicy,
				Port:            app.Port,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if cerr := uc.replicas.Create(ctx, replica); cerr != nil {
				return fmt.Errorf("create replica: %w", cerr)
			}
			if berr := uc.runtime.BootReplica(ctx, replica.ID); berr != nil {
				// BootReplica marks the replica dead on failure; next tick retries.
				logger.FromContext(ctx).Warn("fleet reconcile: boot replica", "replica_id", replica.ID, "err", berr)
			}
		}
	case len(occupying) > app.DesiredReplicas:
		surplus := surplusOrder(occupying)
		drain := len(occupying) - app.DesiredReplicas
		for i := 0; i < drain && i < len(surplus); i++ {
			if derr := uc.runtime.DecommissionReplica(ctx, surplus[i].ID); derr != nil {
				logger.FromContext(ctx).Warn("fleet reconcile: decommission surplus replica", "replica_id", surplus[i].ID, "err", derr)
			}
		}
	}

	return nil
}

// ReconcileAll reconciles every known app. A per-app error is logged, never
// aborting the loop.
func (uc *FleetOrchestrator) ReconcileAll(ctx context.Context) error {
	apps, err := uc.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	for i := range apps {
		if rerr := uc.ReconcileApp(ctx, apps[i].ID); rerr != nil {
			logger.FromContext(ctx).Error("fleet reconcile: app failed", "app_id", apps[i].ID, "err", rerr)
		}
	}
	return nil
}

// HealthApp reports the aggregate health of an app's replica set. The bool
// reports whether appID is a known app (false → caller falls back to the
// workload path).
func (uc *FleetOrchestrator) HealthApp(ctx context.Context, appID uuid.UUID) (FleetHealthOutput, bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return FleetHealthOutput{}, false, nil
		}
		return FleetHealthOutput{}, false, fmt.Errorf("load app: %w", err)
	}

	replicas, err := uc.replicas.ListByApp(ctx, app.ID)
	if err != nil {
		return FleetHealthOutput{}, true, fmt.Errorf("list replicas: %w", err)
	}

	healthy := 0
	pending := 0
	url := ""
	for i := range replicas {
		switch replicas[i].State {
		case domain.ReplicaStateResident:
			ok, herr := uc.runtime.RefreshHealth(ctx, &replicas[i])
			if herr != nil {
				logger.FromContext(ctx).Warn("fleet health: refresh", "replica_id", replicas[i].ID, "err", herr)
			}
			if ok {
				healthy++
				if url == "" {
					url = replicas[i].Endpoint
				}
			}
		case domain.ReplicaStateScheduled, domain.ReplicaStateBooting:
			pending++
		}
	}

	out := FleetHealthOutput{URL: url}
	switch {
	case healthy > 0:
		out.State = "running"
		out.Healthy = true
	case pending > 0:
		out.State = "booting"
	default:
		out.State = "failed"
	}
	out.Message = fmt.Sprintf("healthy=%d pending=%d desired=%d replicas=%d", healthy, pending, app.DesiredReplicas, len(replicas))
	return out, true, nil
}

// DecommissionApp scales the app to zero (draining all replicas) and deletes
// the app row. The bool reports whether appID is a known app.
func (uc *FleetOrchestrator) DecommissionApp(ctx context.Context, appID uuid.UUID) (bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load app: %w", err)
	}

	app.DesiredReplicas = 0
	app.UpdatedAt = time.Now().UTC()
	if err := uc.apps.Update(ctx, app); err != nil {
		return true, fmt.Errorf("update fleet app: %w", err)
	}
	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return true, err
	}
	if err := uc.apps.Delete(ctx, app.ID); err != nil {
		return true, fmt.Errorf("delete fleet app: %w", err)
	}
	return true, nil
}

// ScaleApp sets the app's desired replica count and reconciles. The bool
// reports whether appID is a known app.
func (uc *FleetOrchestrator) ScaleApp(ctx context.Context, appID uuid.UUID, replicas int) (bool, error) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		if errors.Is(err, domain.ErrFleetAppNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load app: %w", err)
	}

	if replicas < 0 {
		replicas = 0
	}
	app.DesiredReplicas = replicas
	app.UpdatedAt = time.Now().UTC()
	if err := uc.apps.Update(ctx, app); err != nil {
		return true, fmt.Errorf("update fleet app: %w", err)
	}
	if err := uc.ReconcileApp(ctx, app.ID); err != nil {
		return true, err
	}
	return true, nil
}

// surplusOrder ranks occupying replicas for draining: prefer scheduled/booting
// over resident, and within a tier the newest first.
func surplusOrder(occupying []domain.Replica) []domain.Replica {
	ordered := make([]domain.Replica, len(occupying))
	copy(ordered, occupying)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := drainRank(ordered[i].State), drainRank(ordered[j].State)
		if pi != pj {
			return pi < pj
		}
		return ordered[i].CreatedAt.After(ordered[j].CreatedAt)
	})
	return ordered
}

// drainRank orders replica states for draining: pending states drain before
// resident ones.
func drainRank(s domain.ReplicaState) int {
	switch s {
	case domain.ReplicaStateScheduled, domain.ReplicaStateBooting:
		return 0
	default:
		return 1
	}
}
