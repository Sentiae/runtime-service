package grpc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// recordingHostRepo is the tripwire for these tests: a refused call must reach
// NO repository method at all. An identity check that refuses AFTER writing is
// not an identity check.
type recordingHostRepo struct {
	touched bool
}

func (r *recordingHostRepo) Create(_ context.Context, _ *domain.Host) error {
	r.touched = true
	return nil
}
func (r *recordingHostRepo) Update(_ context.Context, _ *domain.Host) error {
	r.touched = true
	return nil
}
func (r *recordingHostRepo) FindByID(_ context.Context, _ uuid.UUID) (*domain.Host, error) {
	r.touched = true
	return nil, domain.ErrFleetHostNotFound
}
func (r *recordingHostRepo) List(_ context.Context) ([]domain.Host, error) {
	r.touched = true
	return nil, nil
}
func (r *recordingHostRepo) ListActive(_ context.Context) ([]domain.Host, error) {
	r.touched = true
	return nil, nil
}
func (r *recordingHostRepo) ListByStatus(_ context.Context, _ domain.HostStatus) ([]domain.Host, error) {
	r.touched = true
	return nil, nil
}
func (r *recordingHostRepo) Delete(_ context.Context, _ uuid.UUID) error {
	r.touched = true
	return nil
}

var _ repository.HostRepository = (*recordingHostRepo)(nil)

// withPeerSVID stamps the attested peer identity the SVID interceptor would have
// derived from the peer certificate in production (tenant.FromContext reads the
// same field either way).
func withPeerSVID(svid string) context.Context {
	return tenant.ContextWithPrincipal(context.Background(), tenant.Principal{ServiceSVID: svid})
}

func fleetHostSVID(id uuid.UUID) string {
	return "spiffe://sentiae.io/fleet-host/" + id.String()
}

// A host id is a TRANSPORT fact. The body may restate it and may not contradict
// it, and a caller that proves nothing is REFUSED rather than minted an identity.
func TestRegisterHostDerivesIdentityFromThePeerSVID(t *testing.T) {
	self := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	tests := []struct {
		name     string
		ctx      context.Context
		bodyID   string
		wantCode codes.Code
	}{
		{
			name:     "no peer identity at all — refused, NOT assigned a fresh uuid",
			ctx:      context.Background(),
			bodyID:   "",
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "no peer identity but a body id — the body cannot make a caller a host",
			ctx:      context.Background(),
			bodyID:   self.String(),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "a service SVID is not a host SVID",
			ctx:      withPeerSVID("spiffe://sentiae.io/svc/delivery"),
			bodyID:   self.String(),
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "a host SVID whose path is not a uuid",
			ctx:      withPeerSVID("spiffe://sentiae.io/fleet-host/not-a-uuid"),
			bodyID:   "",
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "a host may not act as ANOTHER host — body id disagrees with the SVID",
			ctx:      withPeerSVID(fleetHostSVID(self)),
			bodyID:   other.String(),
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "an unparseable body id is still a bad request",
			ctx:      withPeerSVID(fleetHostSVID(self)),
			bodyID:   "not-a-uuid",
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &recordingHostRepo{}
			s := &FleetServer{registry: usecase.NewFleetHostRegistry(repo, nil)}

			_, err := s.RegisterHost(tt.ctx, &runtimev1.RegisterHostRequest{
				Host: &runtimev1.HostSpec{
					HostId:       tt.bodyID,
					Region:       "homelab",
					CapacityVcpu: 4,
				},
			})
			if code := status.Code(err); code != tt.wantCode {
				t.Fatalf("RegisterHost = %s (%v), want %s", code, err, tt.wantCode)
			}
			if repo.touched {
				t.Fatal("a refused registration must not reach the host repository at all")
			}
		})
	}
}

// A heartbeat WRITES another host's liveness and allocatable capacity, so a
// forged host_id could hold a dead host in the placement candidate set. Same
// rule, same seam.
func TestHeartbeatDerivesIdentityFromThePeerSVID(t *testing.T) {
	self := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	other := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	tests := []struct {
		name     string
		ctx      context.Context
		bodyID   string
		wantCode codes.Code
	}{
		{"unidentified caller", context.Background(), self.String(), codes.Unauthenticated},
		{"unidentified caller, empty body id", context.Background(), "", codes.Unauthenticated},
		{"a service SVID is not a host", withPeerSVID("spiffe://sentiae.io/svc/delivery"), self.String(), codes.PermissionDenied},
		{"heartbeating for someone else", withPeerSVID(fleetHostSVID(self)), other.String(), codes.PermissionDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &recordingHostRepo{}
			s := &FleetServer{registry: usecase.NewFleetHostRegistry(repo, nil)}

			_, err := s.Heartbeat(tt.ctx, &runtimev1.HeartbeatRequest{
				HostId:          tt.bodyID,
				AllocatableVcpu: 4,
			})
			if code := status.Code(err); code != tt.wantCode {
				t.Fatalf("Heartbeat = %s (%v), want %s", code, err, tt.wantCode)
			}
			if repo.touched {
				t.Fatal("a refused heartbeat must not reach the host repository at all")
			}
		})
	}
}

// The inventory is the map of every machine holding customer state, with each
// one's gRPC endpoint. It carries no tenant data, so any ATTESTED caller may read
// it — but it is not an anonymous read.
func TestListHostsRefusesAnAnonymousCaller(t *testing.T) {
	repo := &recordingHostRepo{}
	s := &FleetServer{registry: usecase.NewFleetHostRegistry(repo, nil)}

	if _, err := s.ListHosts(context.Background(), &runtimev1.ListHostsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous ListHosts = %v, want Unauthenticated", err)
	}
	if repo.touched {
		t.Fatal("a refused read must not reach the host repository")
	}

	// An attested mesh caller is allowed — the gate answers "is this someone", and
	// a service SVID is someone.
	if _, err := s.ListHosts(withPeerSVID("spiffe://sentiae.io/svc/delivery"), &runtimev1.ListHostsRequest{}); err != nil {
		t.Fatalf("attested ListHosts = %v, want success", err)
	}
	if !repo.touched {
		t.Fatal("an attested read must reach the inventory")
	}
}
