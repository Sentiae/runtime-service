package domain

import "testing"

func TestFleetResourcePhase_IsValid(t *testing.T) {
	tests := []struct {
		name string
		p    FleetResourcePhase
		want bool
	}{
		{"provisioning", FleetResourcePhaseProvisioning, true},
		{"ready", FleetResourcePhaseReady, true},
		{"degraded", FleetResourcePhaseDegraded, true},
		{"restoring", FleetResourcePhaseRestoring, true},
		{"failed", FleetResourcePhaseFailed, true},
		{"decommissioned", FleetResourcePhaseDecommissioned, true},
		{"empty", FleetResourcePhase(""), false},
		{"unknown", FleetResourcePhase("provisioned"), false},
		// The retired phase (nothing ever wrote it) must not be accepted again: a
		// phase the fleet cannot produce but validates is a reader trap.
		{"pending is not a phase", FleetResourcePhase("pending"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.IsValid(); got != tt.want {
				t.Fatalf("FleetResourcePhase(%q).IsValid() = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}
