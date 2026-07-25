package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/domain"
)

// recordingActivator records whether the wake use case was reached and returns a
// fixed error, so a 403 produced by the peer guard is distinguishable from
// anything the use case itself could have produced.
type recordingActivator struct {
	mu     sync.Mutex
	calls  int
	hosts  []string
	retErr error
}

func (a *recordingActivator) Activate(_ context.Context, host string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.hosts = append(a.hosts, host)
	return "", a.retErr
}

func (a *recordingActivator) called() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// TestActivatorHandler_LoopbackOnly pins the enforced peer control on the wake
// endpoint. It is mounted OUTSIDE the JWT group on a listener that binds every
// interface of the fleet host (delivery polls /fleet on the same port remotely),
// and it boots workloads — so "only the co-located gateway calls it" has to be a
// control, not a comment. The peer address is the connection's real remote
// endpoint; no header may substitute for it.
func TestActivatorHandler_LoopbackOnly(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "ipv4 loopback is the co-located gateway",
			remoteAddr: "127.0.0.1:41234",
			wantStatus: http.StatusServiceUnavailable, // reached the use case
			wantCalls:  1,
		},
		{
			name:       "ipv6 loopback",
			remoteAddr: "[::1]:41235",
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
		},
		{
			name:       "127.0.0.0/8 is all loopback",
			remoteAddr: "127.9.9.9:41236",
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
		},
		{
			name:       "LAN peer is refused",
			remoteAddr: "10.0.10.20:52000",
			wantStatus: http.StatusForbidden,
			wantCalls:  0,
		},
		{
			name:       "spoofed X-Forwarded-For does not make a LAN peer loopback",
			remoteAddr: "10.0.10.20:52001",
			headers: map[string]string{
				"X-Forwarded-For": "127.0.0.1",
				"X-Real-IP":       "127.0.0.1",
				"Forwarded":       "for=127.0.0.1",
			},
			wantStatus: http.StatusForbidden,
			wantCalls:  0,
		},
		{
			name:       "unparseable peer fails closed",
			remoteAddr: "",
			wantStatus: http.StatusForbidden,
			wantCalls:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act := &recordingActivator{retErr: domain.ErrActivationTimeout}
			r := chi.NewRouter()
			NewActivatorHandler(act).RegisterRoutes(r)

			req := httptest.NewRequest(http.MethodGet, "/_activate/index.html", nil)
			req.RemoteAddr = tt.remoteAddr
			req.Header.Set("X-Fleet-Host", "victim-prod.fleet.sentiae.local")
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := act.called(); got != tt.wantCalls {
				t.Fatalf("activator calls = %d, want %d", got, tt.wantCalls)
			}
			if tt.wantStatus == http.StatusForbidden && rec.Body.Len() != 0 {
				t.Fatalf("refusal leaked detail: %q", rec.Body.String())
			}
		})
	}
}

// A refused wake is permanent, so it must not come back as the retryable 503 the
// cold-boot path uses — that would hot-loop the caller and read as a transient
// boot in the logs.
func TestActivatorHandler_RefusedWakeIsForbiddenNotRetryable(t *testing.T) {
	act := &recordingActivator{retErr: domain.ErrAnonymousWakeRefused}
	r := chi.NewRouter()
	NewActivatorHandler(act).RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/_activate/", nil)
	req.RemoteAddr = "127.0.0.1:41240"
	req.Header.Set("X-Fleet-Host", "pg-prod.fleet.sentiae.local")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatalf("refusal carries Retry-After %q — a permanent refusal must not invite a retry", rec.Header().Get("Retry-After"))
	}
	if act.called() != 1 {
		t.Fatalf("activator calls = %d, want 1 (the refusal is the use case's verdict)", act.called())
	}
}
