package usecase

import (
	"sync"

	"github.com/google/uuid"
)

// FleetRegistryTokenStore holds each resident app's handed per-deployment OCI
// registry PULL token IN MEMORY ONLY (D-124): keyed by app id, never persisted to
// any row, rootfs, runtime.json, or log. ProvisionApp stashes it, the
// reconciler-driven replica runtime reads it at materialize (the pull), and
// DecommissionApp drops it.
//
// Unlike the Vault secret-broker token (FleetSecretTokenStore), the registry
// token is a stateless, short-lived HMAC bearer — it is neither renewable nor
// revocable at the registry — so this store is a plain in-memory map with no
// renewer/revoker goroutine (D-124: the image materializes once, the short TTL is
// correct, no renewal). When empty (pre-cutover / no token handed) the reader
// falls back to the shared registry service key (back-compat).
type FleetRegistryTokenStore struct {
	mu      sync.Mutex
	entries map[uuid.UUID]string
}

// NewFleetRegistryTokenStore constructs an empty store.
func NewFleetRegistryTokenStore() *FleetRegistryTokenStore {
	return &FleetRegistryTokenStore{entries: make(map[uuid.UUID]string)}
}

// Put stores (or replaces) the handed pull token for an app. An empty token is a
// no-op (a pre-cutover / non-fleet deploy hands none), leaving the reader on the
// shared-service-key fallback.
func (s *FleetRegistryTokenStore) Put(appID uuid.UUID, token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[appID] = token
}

// Get returns the app's handed pull token, if any.
func (s *FleetRegistryTokenStore) Get(appID uuid.UUID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.entries[appID]
	return t, ok
}

// Delete drops the app's handed token (called on DecommissionApp so the host
// stops bearing it). Idempotent: an unknown app is a no-op.
func (s *FleetRegistryTokenStore) Delete(appID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, appID)
}
