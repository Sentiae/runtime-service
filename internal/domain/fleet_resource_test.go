package domain

import "testing"

func TestFleetResourcePhase_IsValid(t *testing.T) {
	tests := []struct {
		name string
		p    FleetResourcePhase
		want bool
	}{
		{"pending", FleetResourcePhasePending, true},
		{"provisioning", FleetResourcePhaseProvisioning, true},
		{"ready", FleetResourcePhaseReady, true},
		{"degraded", FleetResourcePhaseDegraded, true},
		{"failed", FleetResourcePhaseFailed, true},
		{"decommissioned", FleetResourcePhaseDecommissioned, true},
		{"empty", FleetResourcePhase(""), false},
		{"unknown", FleetResourcePhase("provisioned"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.IsValid(); got != tt.want {
				t.Fatalf("FleetResourcePhase(%q).IsValid() = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}
