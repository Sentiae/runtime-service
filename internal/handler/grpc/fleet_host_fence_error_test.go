package grpc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The host-authority fence's public error semantics
// (#fleet-reconciler-acts-on-foreign-host-replicas).
//
// Codes are load-bearing: FailedPrecondition tells a caller "the call is fine,
// the state is not — an unchanged retry succeeds once it reaches the owning
// host", and Unavailable tells it "nothing was released, retry". Left to the
// default branch these would all be a flat Internal, which reads as "a bug" and
// as "your retry cannot help".
//
// MESSAGES are load-bearing too, for a different reason: fleetError's default
// echoes raw err.Error(), and these wrapped errors carry host uuids, filesystem
// paths, pids and lease coordinates. None of that may reach a tenant.
func TestFleetError_HostAuthoritySentinels(t *testing.T) {
	hostID := uuid.New()
	replicaID := uuid.New()
	volumeID := uuid.New()

	tests := []struct {
		name string
		err  error
		want codes.Code
		// leaks are substrings that must NOT appear in the curated message.
		leaks []string
	}{
		{
			name: "foreign replica",
			err: fmt.Errorf("boot replica: %w: replica %s is placed on fleet host %s",
				domain.ErrReplicaHostMismatch, replicaID, hostID),
			want:  codes.FailedPrecondition,
			leaks: []string{hostID.String(), replicaID.String()},
		},
		{
			name: "foreign volume",
			err: fmt.Errorf("ensure volumes: %w: volume %s is pinned to host %s (path /var/lib/sentiae/volumes/x.ext4)",
				domain.ErrVolumeHostMismatch, volumeID, hostID),
			want:  codes.FailedPrecondition,
			leaks: []string{hostID.String(), volumeID.String(), "/var/lib/sentiae/volumes"},
		},
		{
			name: "termination unproven",
			err: fmt.Errorf("decommission: %w: pid 858425 is still running 5s after SIGKILL (jail /srv/fc/firecracker/7)",
				domain.ErrVMTerminationUnproven),
			want:  codes.Unavailable,
			leaks: []string{"858425", "/srv/fc/firecracker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Both boundaries: resourceError falls through to fleetError, so proving
			// the mapping through each of them proves there is ONE switch, not two
			// that can drift.
			for handler, got := range map[string]error{
				"fleetError":    fleetError(tt.err),
				"resourceError": resourceError(tt.err),
			} {
				st, ok := status.FromError(got)
				if !ok {
					t.Fatalf("%s: %v is not a status error", handler, got)
				}
				if st.Code() != tt.want {
					t.Fatalf("%s code = %v, want %v", handler, st.Code(), tt.want)
				}
				for _, leak := range tt.leaks {
					if strings.Contains(st.Message(), leak) {
						t.Fatalf("%s message leaks %q: %q", handler, leak, st.Message())
					}
				}
				if strings.Contains(st.Message(), "internal server error") {
					t.Fatalf("%s fell through to the curated Internal: %q", handler, st.Message())
				}
			}
		})
	}
}
