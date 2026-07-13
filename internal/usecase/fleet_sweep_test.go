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
			h := newOrchHarness(oneLiveHost(), tt.app)
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
