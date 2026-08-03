package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

func TestSweepIdle(t *testing.T) {
	// Resident replicas stay healthy during the drain-reconcile (guest IP left
	// empty so RefreshHealth never dials a fake endpoint), mirroring the
	// ReconcileApp test.
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	now := time.Now().UTC()
	mkApp := func(stz bool, ttl int, lastActive time.Time) *domain.FleetApp {
		return &domain.FleetApp{
			ID: uuid.New(), ComponentID: uuid.NewString(), Env: "prod",
			ImageRepository: "org/app", ImageDigest: "sha256:abc",
			DesiredReplicas: 1, ScaleToZero: stz, IdleTTLSeconds: ttl,
			LastActiveAt: lastActive, Port: 8080, ResourcesVCPU: 1, ResourcesMemMB: 512,
			RestartPolicy: domain.RestartPolicyAlways, CreatedAt: now, UpdatedAt: now,
		}
	}

	tests := []struct {
		name             string
		app              *domain.FleetApp
		seedResident     bool
		wantScaledToZero bool
	}{
		{"idle scale-to-zero app with resident is swept", mkApp(true, 30, now.Add(-time.Minute)), true, true},
		{"scale-to-zero disabled is never swept", mkApp(false, 30, now.Add(-time.Minute)), true, false},
		{"zero idle_ttl is never swept", mkApp(true, 0, now.Add(-time.Minute)), true, false},
		{"recently active is never swept", mkApp(true, 30, now.Add(-5*time.Second)), true, false},
		{"no resident replica is skipped", mkApp(true, 30, now.Add(-time.Minute)), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newOrchHarness(t, oneLiveHost(), tt.app)
			if tt.seedResident {
				pid := 4000
				if err := h.replicas.Create(context.Background(), &domain.Replica{
					ID: uuid.New(), AppID: tt.app.ID, State: domain.ReplicaStateResident,
					PID: &pid, RestartPolicy: domain.RestartPolicyAlways, CreatedAt: now,
				}); err != nil {
					t.Fatalf("seed replica: %v", err)
				}
			}

			if err := h.orch.SweepIdle(context.Background()); err != nil {
				t.Fatalf("SweepIdle: %v", err)
			}

			got, err := h.apps.FindByID(context.Background(), tt.app.ID)
			if err != nil {
				t.Fatalf("FindByID: %v", err)
			}
			scaled := got.DesiredReplicas == 0
			if scaled != tt.wantScaledToZero {
				t.Fatalf("scaledToZero = %v, want %v (desired=%d)", scaled, tt.wantScaledToZero, got.DesiredReplicas)
			}
		})
	}
}

// fakeActivityFeed is a static ActivityFeed for the SweepIdle direct-serve guard
// (#fleet-scale-to-zero-activity-feed, D-122).
type fakeActivityFeed struct {
	warm bool
	seen map[string]time.Time
}

func (f fakeActivityFeed) LastActivity(host string) (time.Time, bool) {
	t, ok := f.seen[host]
	return t, ok
}
func (f fakeActivityFeed) Warm() bool { return f.warm }

func TestSweepIdleActivityGuard(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	const host = "busy.fleet.sentiae.local"
	now := time.Now().UTC()
	lastActive := now.Add(-time.Minute) // past the 30s ttl → otherwise eligible

	tests := []struct {
		name             string
		feed             ActivityFeed
		wantScaledToZero bool
		wantRestamped    bool
	}{
		{
			name:             "busy host with activity after last-active is not swept and re-stamped",
			feed:             fakeActivityFeed{warm: true, seen: map[string]time.Time{host: now}},
			wantScaledToZero: false,
			wantRestamped:    true,
		},
		{
			name:             "idle host with only stale activity is swept",
			feed:             fakeActivityFeed{warm: true, seen: map[string]time.Time{host: now.Add(-2 * time.Minute)}},
			wantScaledToZero: true,
		},
		{
			name:             "warm feed with unseen host is swept",
			feed:             fakeActivityFeed{warm: true, seen: map[string]time.Time{}},
			wantScaledToZero: true,
		},
		{
			name:             "cold feed fails safe - not swept",
			feed:             fakeActivityFeed{warm: false, seen: map[string]time.Time{host: now}},
			wantScaledToZero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &domain.FleetApp{
				ID: uuid.New(), ComponentID: uuid.NewString(), Env: "prod",
				ImageRepository: "org/app", ImageDigest: "sha256:abc",
				DesiredReplicas: 1, ScaleToZero: true, IdleTTLSeconds: 30,
				LastActiveAt: lastActive, Port: 8080, ResourcesVCPU: 1, ResourcesMemMB: 512,
				RestartPolicy: domain.RestartPolicyAlways, CreatedAt: now, UpdatedAt: now,
			}
			h := newOrchHarness(t, oneLiveHost(), app)

			routes := newOrchRouteRepo()
			if err := routes.Create(context.Background(), &domain.Route{
				ID: uuid.New(), AppID: app.ID, HostPattern: host, PathPrefix: "/",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed route: %v", err)
			}
			h.orch.SetIngress(routes, "fleet.sentiae.local", nil)
			h.orch.SetActivityFeed(tt.feed)

			pid := 4000
			if err := h.replicas.Create(context.Background(), &domain.Replica{
				ID: uuid.New(), AppID: app.ID, State: domain.ReplicaStateResident,
				PID: &pid, RestartPolicy: domain.RestartPolicyAlways, CreatedAt: now,
			}); err != nil {
				t.Fatalf("seed replica: %v", err)
			}

			if err := h.orch.SweepIdle(context.Background()); err != nil {
				t.Fatalf("SweepIdle: %v", err)
			}

			got, err := h.apps.FindByID(context.Background(), app.ID)
			if err != nil {
				t.Fatalf("FindByID: %v", err)
			}
			if scaled := got.DesiredReplicas == 0; scaled != tt.wantScaledToZero {
				t.Fatalf("scaledToZero = %v, want %v (desired=%d)", scaled, tt.wantScaledToZero, got.DesiredReplicas)
			}
			if tt.wantRestamped && !got.LastActiveAt.After(lastActive) {
				t.Fatalf("LastActiveAt = %v, want re-stamped after %v", got.LastActiveAt, lastActive)
			}
		})
	}
}
