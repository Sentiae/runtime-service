package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeAppScaler records ScaleApp calls and, on scale-to-1, marks the harness'
// replica resident so the activator's poll observes a serving replica.
type fakeAppScaler struct {
	mu       sync.Mutex
	calls    []int
	known    bool
	replicas *orchReplicaRepo
	appID    uuid.UUID
	// onScaleUp, when set, makes the replica resident on ScaleApp(1).
	onScaleUp bool
}

func (f *fakeAppScaler) ScaleApp(_ context.Context, appID uuid.UUID, replicas int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, replicas)
	if f.onScaleUp && replicas == 1 && f.replicas != nil {
		r := &domain.Replica{
			ID:       uuid.New(),
			AppID:    appID,
			State:    domain.ReplicaStateResident,
			GuestIP:  "10.201.0.5",
			Port:     8080,
			Endpoint: "http://10.201.0.5:8080",
		}
		_ = f.replicas.Create(context.Background(), r)
	}
	return f.known, nil
}

func (f *fakeAppScaler) scaleCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.calls))
	copy(out, f.calls)
	return out
}

// newActivatorHarness wires an activator over the in-file orch fakes with an
// always-pass health probe and a fast poll so tests never sleep long.
func newActivatorHarness(scaler *fakeAppScaler, routes *orchRouteRepo, apps *orchAppRepo, replicas *orchReplicaRepo, timeout time.Duration) *FleetActivator {
	act := NewFleetActivator(routes, apps, replicas, scaler, timeout)
	act.healthy = func(*domain.Replica) bool { return true }
	act.poll = time.Millisecond
	return act
}

func TestActivator_WakesAppAndReturnsEndpoint(t *testing.T) {
	appID := uuid.New()
	now := time.Now().UTC()
	apps := newOrchAppRepo(&domain.FleetApp{
		ID: appID, ComponentID: "comp-1", Env: "prod", Port: 8080,
		ScaleToZero: true, IdleTTLSeconds: 30, LastActiveAt: now.Add(-time.Hour),
		CreatedAt: now, UpdatedAt: now,
	})
	replicas := newOrchReplicaRepo()
	routes := newOrchRouteRepo()
	_ = routes.Create(context.Background(), &domain.Route{
		ID: uuid.New(), AppID: appID, HostPattern: "comp-1-prod.fleet.sentiae.local", PathPrefix: "/",
	})
	scaler := &fakeAppScaler{known: true, replicas: replicas, appID: appID, onScaleUp: true}
	act := newActivatorHarness(scaler, routes, apps, replicas, 2*time.Second)

	endpoint, err := act.Activate(context.Background(), "comp-1-prod.fleet.sentiae.local")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if endpoint != "http://10.201.0.5:8080" {
		t.Fatalf("endpoint = %q, want http://10.201.0.5:8080", endpoint)
	}
	// ScaleApp(1) must have been called to wake the app.
	calls := scaler.scaleCalls()
	if len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("scale calls = %v, want [1]", calls)
	}
	// last_active_at stamped forward (no longer an hour stale).
	stamped, _ := apps.FindByID(context.Background(), appID)
	if time.Since(stamped.LastActiveAt) > time.Minute {
		t.Fatalf("last_active_at not stamped: %v", stamped.LastActiveAt)
	}
}

func TestActivator_UnknownHostReturnsRouteNotFound(t *testing.T) {
	apps := newOrchAppRepo()
	replicas := newOrchReplicaRepo()
	routes := newOrchRouteRepo()
	scaler := &fakeAppScaler{known: true, replicas: replicas}
	act := newActivatorHarness(scaler, routes, apps, replicas, time.Second)

	if _, err := act.Activate(context.Background(), "nope.fleet.sentiae.local"); !errors.Is(err, domain.ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
	if _, err := act.Activate(context.Background(), ""); !errors.Is(err, domain.ErrRouteNotFound) {
		t.Fatalf("empty host err = %v, want ErrRouteNotFound", err)
	}
}

func TestActivator_TimeoutWhenNoResident(t *testing.T) {
	appID := uuid.New()
	apps := newOrchAppRepo(&domain.FleetApp{
		ID: appID, ComponentID: "comp-1", Env: "prod", Port: 8080, ScaleToZero: true,
	})
	replicas := newOrchReplicaRepo()
	routes := newOrchRouteRepo()
	_ = routes.Create(context.Background(), &domain.Route{
		ID: uuid.New(), AppID: appID, HostPattern: "comp-1-prod.fleet.sentiae.local", PathPrefix: "/",
	})
	// onScaleUp false → no replica ever becomes resident → the poll times out.
	scaler := &fakeAppScaler{known: true, replicas: replicas, onScaleUp: false}
	act := newActivatorHarness(scaler, routes, apps, replicas, 20*time.Millisecond)

	if _, err := act.Activate(context.Background(), "comp-1-prod.fleet.sentiae.local"); !errors.Is(err, domain.ErrActivationTimeout) {
		t.Fatalf("err = %v, want ErrActivationTimeout", err)
	}
}

func TestActivator_UnknownAppReturnsRouteNotFound(t *testing.T) {
	appID := uuid.New()
	apps := newOrchAppRepo()
	replicas := newOrchReplicaRepo()
	routes := newOrchRouteRepo()
	_ = routes.Create(context.Background(), &domain.Route{
		ID: uuid.New(), AppID: appID, HostPattern: "comp-1-prod.fleet.sentiae.local", PathPrefix: "/",
	})
	// ScaleApp reports the app is not known (isApp=false).
	scaler := &fakeAppScaler{known: false, replicas: replicas}
	act := newActivatorHarness(scaler, routes, apps, replicas, time.Second)

	if _, err := act.Activate(context.Background(), "comp-1-prod.fleet.sentiae.local"); !errors.Is(err, domain.ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
}

// The wake path carries NO caller identity (an unauthenticated loopback endpoint,
// app chosen by a caller-supplied hostname), so it may only boot an app it can
// PROVE is a plain scale-to-zero HTTP workload. A dedicated Postgres data-VM
// booted from here is a durability-relevant transition over customer data reached
// with no authority at all — and ScaleApp(…,1) would have allowed it (it refuses
// only replicas > 1 on a volume-bearing app).
func TestActivator_RefusesWhatItCannotProveIsAScaleToZeroHTTPWorkload(t *testing.T) {
	// The REAL dedicated data-engine descriptor decides what the refused app looks
	// like, so this test cannot drift from what P19 actually provisions.
	resources := NewFleetResourceProvisioner(&fakeFleetProvisioner{}, newFakeResourceRepo(), nil, &fakeSnapshotter{}, testEngine(), testEndpointNaming())
	dedicated := resources.dedicatedDescriptor(validDedicatedInput())

	tests := []struct {
		name    string
		mutate  func(*domain.FleetApp)
		wantErr error
	}{
		{
			name: "dedicated postgres data-VM as P19 provisions it",
			mutate: func(a *domain.FleetApp) {
				a.Port = dedicated.Port
				a.ScaleToZero = dedicated.ScaleToZero
				a.MinReplicas = dedicated.MinReplicas
				a.MaxReplicas = dedicated.MaxReplicas
			},
			wantErr: domain.ErrAnonymousWakeRefused,
		},
		{
			// Independent of the flags: even if a data engine were ever declared
			// scale-to-zero, the HTTP wake path must not boot the data port.
			name: "data-engine port even with scale-to-zero declared",
			mutate: func(a *domain.FleetApp) {
				a.Port = residentPGPort
				a.ScaleToZero = true
				a.MinReplicas = 0
			},
			wantErr: domain.ErrAnonymousWakeRefused,
		},
		{
			name:    "app that never opted into scale-to-zero",
			mutate:  func(a *domain.FleetApp) { a.ScaleToZero = false },
			wantErr: domain.ErrAnonymousWakeRefused,
		},
		{
			name:    "app with a positive replica floor",
			mutate:  func(a *domain.FleetApp) { a.MinReplicas = 1 },
			wantErr: domain.ErrAnonymousWakeRefused,
		},
		{
			name:    "app declaring no port to serve",
			mutate:  func(a *domain.FleetApp) { a.Port = 0 },
			wantErr: domain.ErrAnonymousWakeRefused,
		},
		{
			name:    "ordinary scale-to-zero HTTP workload still wakes",
			mutate:  func(*domain.FleetApp) {},
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appID := uuid.New()
			app := &domain.FleetApp{
				ID: appID, ComponentID: "comp-1", Env: "prod", OwnerOrg: orgA,
				Port: 8080, ScaleToZero: true, MinReplicas: 0, MaxReplicas: 1,
				IdleTTLSeconds: 30, LastActiveAt: time.Now().UTC().Add(-time.Hour),
			}
			tt.mutate(app)
			apps := newOrchAppRepo(app)
			replicas := newOrchReplicaRepo()
			routes := newOrchRouteRepo()
			const host = "comp-1-prod.fleet.sentiae.local"
			if err := routes.Create(context.Background(), &domain.Route{
				ID: uuid.New(), AppID: appID, HostPattern: host, PathPrefix: "/",
			}); err != nil {
				t.Fatalf("seed route: %v", err)
			}
			scaler := &fakeAppScaler{known: true, replicas: replicas, appID: appID, onScaleUp: true}
			act := newActivatorHarness(scaler, routes, apps, replicas, time.Second)

			_, err := act.Activate(context.Background(), host)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Activate: %v", err)
				}
				if calls := scaler.scaleCalls(); len(calls) != 1 || calls[0] != 1 {
					t.Fatalf("scale calls = %v, want [1]", calls)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// The refusal must land BEFORE the boot: ScaleApp(…,1) IS the wake
			// (it reconciles inline), so a refusal after it would be a refusal on a
			// VM already attached to the customer's data disk.
			if calls := scaler.scaleCalls(); len(calls) != 0 {
				t.Fatalf("refused wake still called ScaleApp %v — the VM was booted anyway", calls)
			}
		})
	}
}

func TestRouteRepo_FindByHost(t *testing.T) {
	appID := uuid.New()
	routes := newOrchRouteRepo()
	_ = routes.Create(context.Background(), &domain.Route{
		ID: uuid.New(), AppID: appID,
		HostPattern: "comp-1-prod.fleet.sentiae.local", CustomDomain: "www.example.com",
	})

	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"platform host", "comp-1-prod.fleet.sentiae.local", false},
		{"custom domain", "www.example.com", false},
		{"unknown", "other.host", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := routes.FindByHost(context.Background(), tt.host)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrRouteNotFound) {
					t.Fatalf("err = %v, want ErrRouteNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindByHost: %v", err)
			}
			if r.AppID != appID {
				t.Fatalf("appID = %v, want %v", r.AppID, appID)
			}
		})
	}
}
