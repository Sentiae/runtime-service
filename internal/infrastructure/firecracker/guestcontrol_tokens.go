package firecracker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// controlTokenBytes is the length of the per-VM control token. 32 bytes of
// crypto/rand is unguessable and collision-free for the fleet's lifetime.
const controlTokenBytes = 32

// newControlToken mints a per-VM, cryptographically-random control token
// (D-185a). crypto/rand, never math/rand: this token authenticates every
// post-boot quiesce/shutdown the host can perform on a customer database, so a
// predictable one is a remote shutdown primitive.
//
// It is deliberately NOT the boot-time bootstrap nonce. That nonce is one-shot
// by contract (D-085 Layer 2 — the guest's secret listener accepts exactly one
// push and closes); reusing it here would silently promote a one-shot
// credential into a VM-lifetime one and change that contract as a side effect.
func newControlToken() (string, error) {
	b := make([]byte, controlTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate guest control token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GuestControlTokens holds each live VM's control token IN MEMORY ONLY, keyed by
// the VM's host-view Firecracker API socket path. It is never persisted to a
// row, the rootfs, runtime.json, or a log — like the registry-pull-token store,
// and for the same reason: the host must bear the credential for the VM's
// lifetime without ever writing it down.
//
// The socket path is the key because it is the one handle both the booter (which
// mints the token) and the control client (which spends it) already carry, and
// it is unique per live VM by construction.
type GuestControlTokens struct {
	mu     sync.Mutex
	tokens map[string]string
}

// NewGuestControlTokens constructs an empty store.
func NewGuestControlTokens() *GuestControlTokens {
	return &GuestControlTokens{tokens: make(map[string]string)}
}

// Put records (or replaces) a VM's control token. An empty socket path or token
// is a no-op — a boot with no control channel must leave the store with no
// entry, so the client fails loud rather than dialing a listener that is not
// there.
func (s *GuestControlTokens) Put(socketPath, token string) {
	if socketPath == "" || token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[socketPath] = token
}

// Get returns the VM's control token, if the VM has a control channel.
func (s *GuestControlTokens) Get(socketPath string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[socketPath]
	return t, ok
}

// Delete drops a VM's token (on decommission, or on a boot that failed after the
// push). Idempotent.
func (s *GuestControlTokens) Delete(socketPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, socketPath)
}
