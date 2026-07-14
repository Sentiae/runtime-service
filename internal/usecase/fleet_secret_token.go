package usecase

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
)

// DeploymentTokenOps renews and revokes a handed per-deployment Vault token
// (D-125). It is implemented in internal/infrastructure/vaulttoken over the
// Vault API and kept an interface so this use case never imports the Vault
// client. Renew returns the new granted TTL so the renewer can self-adjust its
// cadence to the token's period.
type DeploymentTokenOps interface {
	Renew(ctx context.Context, token string) (time.Duration, error)
	Revoke(ctx context.Context, token string) error
}

// tokenEntry is one deployment's handed token plus its renewer's cancel.
type tokenEntry struct {
	token  string
	cancel context.CancelFunc
}

// FleetSecretTokenStore holds each resident app's handed Vault secret-broker
// token IN MEMORY ONLY (D-125): keyed by app id, never persisted to any row,
// rootfs, runtime.json, or log. It renews each token via `token renew-self` for
// the deployment lifetime — so crash-recovery / scale / scale-to-zero wake
// re-resolve secrets with NO control-plane call — and revokes it on
// Decommission. When ops is nil (Vault unconfigured on this host) it degrades to
// a plain in-memory map with no renew/revoke (tokens self-expire).
type FleetSecretTokenStore struct {
	ops      DeploymentTokenOps
	baseCtx  context.Context
	interval time.Duration // first-renewal delay before the TTL-driven cadence takes over

	mu      sync.Mutex
	entries map[uuid.UUID]*tokenEntry
	wg      sync.WaitGroup
}

// NewFleetSecretTokenStore constructs the store. baseCtx is the service root
// context the renewer goroutines derive from (they must outlive any single
// Provision RPC). renewInterval is the initial renewal delay (the renewer then
// self-adjusts to the token's granted TTL); <=0 defaults to 30m (safe for the
// deployment-tenant role's 1h period).
func NewFleetSecretTokenStore(baseCtx context.Context, ops DeploymentTokenOps, renewInterval time.Duration) *FleetSecretTokenStore {
	if renewInterval <= 0 {
		renewInterval = 30 * time.Minute
	}
	return &FleetSecretTokenStore{
		ops:      ops,
		baseCtx:  baseCtx,
		interval: renewInterval,
		entries:  make(map[uuid.UUID]*tokenEntry),
	}
}

// Put stores (or replaces) the handed token for an app and starts its renewer.
// An empty token is a no-op (a secret-less deploy hands none). Replacing an
// existing token stops the old renewer first; the OLD token is left to
// self-expire (delivery minted a fresh one — revoking the old would need its
// value, which we are discarding). Idempotent for the same token value.
func (s *FleetSecretTokenStore) Put(appID uuid.UUID, token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.entries[appID]; ok {
		if cur.token == token {
			return
		}
		cur.cancel()
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.entries[appID] = &tokenEntry{token: token, cancel: cancel}
	if s.ops != nil {
		s.startRenewer(ctx, appID, token)
	}
}

// Get returns the app's handed token, if any.
func (s *FleetSecretTokenStore) Get(appID uuid.UUID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[appID]
	if !ok {
		return "", false
	}
	return e.token, true
}

// Revoke stops the renewer, revokes the token via `token revoke-self`
// (best-effort — a Vault error is logged, the entry is still dropped so the host
// stops bearing it), and removes the entry. Idempotent: an unknown app is a
// no-op.
func (s *FleetSecretTokenStore) Revoke(ctx context.Context, appID uuid.UUID) {
	s.mu.Lock()
	e, ok := s.entries[appID]
	if ok {
		delete(s.entries, appID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	e.cancel()
	if s.ops == nil {
		return
	}
	if err := s.ops.Revoke(ctx, e.token); err != nil {
		logger.FromContext(ctx).Warn("fleet secret token: revoke-self failed", "app_id", appID, "err", err)
	}
}

// Wait blocks until all renewer goroutines have exited (called on shutdown after
// the base context is cancelled).
func (s *FleetSecretTokenStore) Wait() { s.wg.Wait() }

// startRenewer launches the per-deployment token renewer. Ctx-aware (exits when
// the app is revoked or the service shuts down) and recover-guarded (§9/§30.4).
// It renews via renew-self and sleeps ~half the granted TTL between renewals; a
// renewal error is logged and retried at the base interval (the token is still
// valid until its current period expires — fail-safe, not fail-fast). Caller
// holds s.mu.
func (s *FleetSecretTokenStore) startRenewer(ctx context.Context, appID uuid.UUID, token string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(s.baseCtx).Error("fleet secret token renewer panicked", "app_id", appID, "panic", r)
			}
		}()

		timer := time.NewTimer(s.interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				ttl, err := s.ops.Renew(ctx, token)
				if err != nil {
					logger.FromContext(ctx).Warn("fleet secret token: renew-self failed", "app_id", appID, "err", err)
					timer.Reset(s.interval)
					continue
				}
				next := ttl / 2
				if next < time.Minute {
					next = time.Minute
				}
				timer.Reset(next)
			}
		}
	}()
}
