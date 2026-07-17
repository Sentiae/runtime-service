//go:build unit

package domain

import (
	"errors"
	"testing"
)

func TestFleetNetworkPolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		policy  FleetNetworkPolicy
		wantErr error
	}{
		{
			name:    "valid tcp",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "tcp", Port: 50051},
			wantErr: nil,
		},
		{
			// The whole point: proto int32's zero value must never be read as a
			// wildcard allow.
			name:    "port zero is never any",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "tcp", Port: 0},
			wantErr: ErrInvalidNetworkPolicy,
		},
		{
			name:    "port negative",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "tcp", Port: -1},
			wantErr: ErrInvalidNetworkPolicy,
		},
		{
			name:    "port above range",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "tcp", Port: 65536},
			wantErr: ErrInvalidNetworkPolicy,
		},
		{
			name:    "port at upper bound",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "tcp", Port: 65535},
			wantErr: nil,
		},
		{
			// Empty must never default to tcp.
			name:    "empty protocol is never tcp",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "", Port: 50051},
			wantErr: ErrUnsupportedPolicyProtocol,
		},
		{
			name:    "udp unsupported",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "udp", Port: 53},
			wantErr: ErrUnsupportedPolicyProtocol,
		},
		{
			name:    "uppercase protocol is not normalized",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "b", Protocol: "TCP", Port: 50051},
			wantErr: ErrUnsupportedPolicyProtocol,
		},
		{
			name:    "empty from",
			policy:  FleetNetworkPolicy{FromComponentID: "", ToComponentID: "b", Protocol: "tcp", Port: 50051},
			wantErr: ErrInvalidNetworkPolicy,
		},
		{
			name:    "empty to",
			policy:  FleetNetworkPolicy{FromComponentID: "a", ToComponentID: "", Protocol: "tcp", Port: 50051},
			wantErr: ErrInvalidNetworkPolicy,
		},
		{
			name:    "zero value policy",
			policy:  FleetNetworkPolicy{},
			wantErr: ErrInvalidNetworkPolicy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFleetNetworkStatus_IsValid(t *testing.T) {
	tests := []struct {
		name string
		s    FleetNetworkStatus
		want bool
	}{
		{"active", FleetNetworkActive, true},
		{"deprovisioned", FleetNetworkDeprovisioned, true},
		{"empty", FleetNetworkStatus(""), false},
		{"unknown", FleetNetworkStatus("pending"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.IsValid(); got != tt.want {
				t.Fatalf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}
