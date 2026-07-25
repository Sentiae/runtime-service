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
// domain.ErrRouteNotFound for an unknown host, domain.ErrAnonymousWakeRefused
// when the resolved app is not a plain scale-to-zero HTTP workload, and
// domain.ErrActivationTimeout when no healthy resident replica appears within
// the budget.
//
// ⚠ This path carries NO caller identity: it is reached over plain HTTP from the
// co-located gateway and the app is chosen by a caller-supplied hostname. So the
// app it resolves is gated (wakeableApp) BEFORE anything is booted — reachability
// must never equal wake authority.
func (uc *FleetActivator) Activate(ctx context.Context, host string) (string, error) {
	if host == "" {
		return "", domain.ErrRouteNotFound
	}
	route, err := uc.routes.FindByHost(ctx, host)
	if err != nil {
		return "", err
	}

	// The gate runs BEFORE ScaleApp: ScaleApp(…, 1) is itself the boot (it
	// reconciles inline), and it refuses only replicas > 1 on a volume-bearing app,
	// so scaling a parked database up to one replica is something it would happily
	// do. A check that ran afterwards would be a check on a VM that is already
	// attached to the customer's data disk.
	app, aerr := uc.apps.FindByID(ctx, route.AppID)
	if aerr != nil {
		if errors.Is(aerr, domain.ErrFleetAppNotFound) {
			// A route pointing at an app the fleet no longer knows: as unroutable as
			// an unknown host, and the caller gets the same retryable answer it
			// already got from the isApp==false branch below.
			return "", domain.ErrRouteNotFound
		}
		return "", fmt.Errorf("load app: %w", aerr)
	}
	if werr := wakeableApp(app); werr != nil {
		// LOUD and named: an unauthenticated caller just tried to boot something the
		// wake path is not allowed to boot, and the operator needs the host it asked
		// for and the app it resolved to.
		logger.FromContext(ctx).Error("fleet activate: REFUSED an unauthenticated wake — this app is not a plain scale-to-zero HTTP workload",
			"host", host, "app_id", app.ID, "component_id", app.ComponentID, "env", app.Env,
			"owner_org", app.OwnerOrg, "port", app.Port, "scale_to_zero", app.ScaleToZero,
			"min_replicas", app.MinReplicas, "err", werr)
		return "", werr
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

// wakeableApp reports whether app may be woken by an ANONYMOUS request, and says
// why not when it may not (every refusal wraps domain.ErrAnonymousWakeRefused).
//
// It is deliberately written as a proof of what IS allowed rather than a list of
// what is not: the wake path exists for exactly one thing — an app that DECLARED
// it may be parked at zero replicas and re-served over HTTP on the next request —
// and anything this function cannot positively recognise as that is refused. A
// blocklist would have to name every future workload shape that must not be
// booted from the open port, and would silently admit the one it forgot.
//
// The three properties are each necessary:
//   - ScaleToZero: the app opted INTO being parked and cold-woken. An app that
//     never opted in is either meant to run continuously (so a wake is a no-op it
//     does not need) or is not an HTTP workload at all. A dedicated data engine's
//     descriptor sets it false (fleet_resource_provision.go dedicatedDescriptor).
//   - MinReplicas == 0: the declared floor is zero. A positive floor means the app
//     must never be at zero, so waking it from zero is not a state its owner asked
//     for. The dedicated data engine declares min=max=1.
//   - Port is an ordinary HTTP port, never the reserved in-guest data-engine port.
//     The wake path REVERSE-PROXIES the parked HTTP request to app.Port, so a port
//     that speaks a different protocol proves the app is not what this path serves.
//     Independent of the two above on purpose: it holds even if a data-engine
//     descriptor were ever written with scale-to-zero on.
//
// What it deliberately does NOT do is consult the P19 claim ledger or the volume
// table. Both would be a strictly narrower cross-check than the properties above
// (a claimed/volume-bearing app already fails them), and neither is reachable from
// the activator's current dependency set — see the note in the session report.
func wakeableApp(app *domain.FleetApp) error {
	if !app.ScaleToZero {
		return fmt.Errorf("%w: app %s did not declare scale-to-zero", domain.ErrAnonymousWakeRefused, app.ID)
	}
	if app.MinReplicas != 0 {
		return fmt.Errorf("%w: app %s declares a replica floor of %d, so it is not parked at zero by design",
			domain.ErrAnonymousWakeRefused, app.ID, app.MinReplicas)
	}
	if app.Port <= 0 {
		return fmt.Errorf("%w: app %s declares no port to serve HTTP on", domain.ErrAnonymousWakeRefused, app.ID)
	}
	if app.Port == residentPGPort {
		return fmt.Errorf("%w: app %s serves the reserved data-engine port %d, which the HTTP wake path must never boot",
			domain.ErrAnonymousWakeRefused, app.ID, residentPGPort)
	}
	return nil
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
