package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// AppScaler is the subset of the FleetOrchestrator the activator needs to wake a
// scaled-to-zero app: set the desired replica count and reconcile it. The bool
// reports whether appID is a known app. *FleetOrchestrator satisfies it.
type AppScaler interface {
	ScaleApp(ctx context.Context, appID uuid.UUID, replicas int) (bool, error)
}

// activatePollInterval is how often Activate re-checks the replica set while
// waiting for a resident+healthy replica after the wake.
const activatePollInterval = 250 * time.Millisecond

// FleetActivator is the scale-to-zero wake path (rt#11, D-082). Given an ingress
// host it resolves the app, scales it to one replica, blocks until a resident
// replica is health-passing (or the timeout budget elapses), stamps the app's
// activity clock, and returns the replica's private endpoint the caller proxies
// the parked request to. A timeout/unknown-host returns a retryable error so the
// caller serves a 503 rather than dropping the request.
type FleetActivator struct {
	routes   repository.RouteRepository
	apps     repository.FleetAppRepository
	replicas repository.ReplicaRepository
	orch     AppScaler
	timeout  time.Duration

	// healthy reports whether a resident replica is serving. Overridable in tests;
	// defaults to a live TCP dial of the replica's guest port.
	healthy func(replica *domain.Replica) bool
	// poll is the re-check interval while waiting for a resident replica.
	poll time.Duration
}

// NewFleetActivator constructs the activator. A non-positive timeout falls back
// to 30s (the D-082 default activate budget).
func NewFleetActivator(
	routes repository.RouteRepository,
	apps repository.FleetAppRepository,
	replicas repository.ReplicaRepository,
	orch AppScaler,
	timeout time.Duration,
) *FleetActivator {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &FleetActivator{
		routes:   routes,
		apps:     apps,
		replicas: replicas,
		orch:     orch,
		timeout:  timeout,
		healthy:  func(r *domain.Replica) bool { return dialTCP(r.GuestIP, r.Port) },
		poll:     activatePollInterval,
	}
}

// Activate wakes the app serving host and returns a resident replica endpoint
// (http://<guestIP>:<port>) once it is health-passing. Returns
// domain.ErrRouteNotFound for an unknown host and domain.ErrActivationTimeout
// when no healthy resident replica appears within the budget.
func (uc *FleetActivator) Activate(ctx context.Context, host string) (string, error) {
	if host == "" {
		return "", domain.ErrRouteNotFound
	}
	route, err := uc.routes.FindByHost(ctx, host)
	if err != nil {
		return "", err
	}

	// Wake: set desired replicas to one and reconcile (synchronous — ScaleApp
	// reconciles inline). An unknown app is treated as a missing route.
	isApp, serr := uc.orch.ScaleApp(ctx, route.AppID, 1)
	if serr != nil {
		return "", fmt.Errorf("scale app: %w", serr)
	}
	if !isApp {
		return "", domain.ErrRouteNotFound
	}

	deadline := time.Now().Add(uc.timeout)
	for {
		endpoint, ready, perr := uc.residentEndpoint(ctx, route.AppID)
		if perr != nil {
			return "", perr
		}
		if ready {
			uc.stampActive(ctx, route.AppID)
			return endpoint, nil
		}
		if !time.Now().Before(deadline) {
			logger.FromContext(ctx).Warn("fleet activate: timed out waiting for resident replica",
				"app_id", route.AppID, "host", host)
			return "", domain.ErrActivationTimeout
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(uc.poll):
		}
	}
}

// residentEndpoint returns the endpoint of the first resident, health-passing
// replica of the app (ready=true), or ready=false when none is serving yet.
func (uc *FleetActivator) residentEndpoint(ctx context.Context, appID uuid.UUID) (string, bool, error) {
	replicas, err := uc.replicas.ListByApp(ctx, appID)
	if err != nil {
		return "", false, fmt.Errorf("list replicas: %w", err)
	}
	for i := range replicas {
		if replicas[i].State != domain.ReplicaStateResident {
			continue
		}
		if !uc.healthy(&replicas[i]) {
			continue
		}
		if replicas[i].Endpoint != "" {
			return replicas[i].Endpoint, true, nil
		}
		return fmt.Sprintf("http://%s:%d", replicas[i].GuestIP, replicas[i].Port), true, nil
	}
	return "", false, nil
}

// stampActive refreshes the app's LastActiveAt so the idle sweep does not
// immediately re-drain a just-woken app. Best-effort — a stamp failure is logged,
// never failing the wake (the request has already been served an endpoint).
func (uc *FleetActivator) stampActive(ctx context.Context, appID uuid.UUID) {
	app, err := uc.apps.FindByID(ctx, appID)
	if err != nil {
		logger.FromContext(ctx).Warn("fleet activate: load app for stamp", "app_id", appID, "err", err)
		return
	}
	now := time.Now().UTC()
	app.LastActiveAt = now
	app.UpdatedAt = now
	if err := uc.apps.Update(ctx, app); err != nil {
		logger.FromContext(ctx).Warn("fleet activate: stamp last_active_at", "app_id", appID, "err", err)
	}
}
