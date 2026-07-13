package domain

import "testing"

func TestVolumeStatus_IsValid(t *testing.T) {
	tests := []struct {
		name string
		s    VolumeStatus
		want bool
	}{
		{"available", VolumeStatusAvailable, true},
		{"attached", VolumeStatusAttached, true},
		{"degraded", VolumeStatusDegraded, true},
		{"empty", VolumeStatus(""), false},
		{"unknown", VolumeStatus("mounted"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.IsValid(); got != tt.want {
				t.Fatalf("VolumeStatus(%q).IsValid() = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}
