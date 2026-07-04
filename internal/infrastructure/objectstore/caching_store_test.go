package objectstore

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// memStore is an in-memory ArtifactStore for unit tests. It tracks
// Put/Get counts so tests can assert pull-once-cache-locally behaviour
// without touching disk or network.
type memStore struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	putCalls int
	getCalls int
}

var _ usecase.ArtifactStore = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{blobs: make(map[string][]byte)}
}

func (m *memStore) Put(digest string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls++
	cp := make([]byte, len(b))
	copy(cp, b)
	m.blobs[digest] = cp
	return nil
}

func (m *memStore) Get(digest string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	b, ok := m.blobs[digest]
	if !ok {
		return nil, usecase.ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Exists(digest string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[digest]
	return ok, nil
}

func (m *memStore) VerifyHash(digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[digest]; !ok {
		return usecase.ErrArtifactNotFound
	}
	return nil
}

func (m *memStore) has(digest string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[digest]
	return ok
}

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func TestCachingStore_PutWritesBoth(t *testing.T) {
	local, remote := newMemStore(), newMemStore()
	cs, err := NewCachingStore(local, remote)
	if err != nil {
		t.Fatalf("new caching store: %v", err)
	}

	content := []byte("snapshot-mem-bytes")
	if err := cs.Put("snapshots/abc/mem", bytes.NewReader(content)); err != nil {
		t.Fatalf("put: %v", err)
	}

	if !local.has("snapshots/abc/mem") {
		t.Fatalf("expected local cache to be warmed by Put")
	}
	if !remote.has("snapshots/abc/mem") {
		t.Fatalf("expected remote (durable) to hold the artifact")
	}
	if got := remote.blobs["snapshots/abc/mem"]; !bytes.Equal(got, content) {
		t.Fatalf("remote content mismatch: got %q", got)
	}
	if got := local.blobs["snapshots/abc/mem"]; !bytes.Equal(got, content) {
		t.Fatalf("local content mismatch: got %q", got)
	}
}

func TestCachingStore_GetLocalHit(t *testing.T) {
	local, remote := newMemStore(), newMemStore()
	cs, _ := NewCachingStore(local, remote)

	content := []byte("local-hit")
	if err := local.Put("k", bytes.NewReader(content)); err != nil {
		t.Fatalf("seed local: %v", err)
	}
	remote.getCalls = 0

	rc, err := cs.Get("k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := readAll(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %q", got)
	}
	if remote.getCalls != 0 {
		t.Fatalf("local hit should not touch remote; remote.getCalls=%d", remote.getCalls)
	}
}

func TestCachingStore_GetRemoteMissPullPopulatesLocal(t *testing.T) {
	local, remote := newMemStore(), newMemStore()
	cs, _ := NewCachingStore(local, remote)

	content := []byte("remote-only-bytes")
	if err := remote.Put("k", bytes.NewReader(content)); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	if local.has("k") {
		t.Fatalf("precondition: local should be empty")
	}

	rc, err := cs.Get("k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := readAll(t, rc); !bytes.Equal(got, content) {
		t.Fatalf("content mismatch on pull: got %q", got)
	}

	// pull-once-cache-locally: the local cache must now hold the blob.
	if !local.has("k") {
		t.Fatalf("expected local cache to be populated after remote pull")
	}

	// A second Get must be served from local without re-pulling remote.
	remote.getCalls = 0
	rc2, err := cs.Get("k")
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	_ = readAll(t, rc2)
	if remote.getCalls != 0 {
		t.Fatalf("second Get should hit local cache, not remote; remote.getCalls=%d", remote.getCalls)
	}
}

func TestCachingStore_GetMissingEverywhere(t *testing.T) {
	cs, _ := NewCachingStore(newMemStore(), newMemStore())
	_, err := cs.Get("nope")
	if !errors.Is(err, usecase.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound, got %v", err)
	}
}

func TestCachingStore_ExistsEitherTier(t *testing.T) {
	local, remote := newMemStore(), newMemStore()
	cs, _ := NewCachingStore(local, remote)

	// Absent in both.
	if ok, err := cs.Exists("k"); err != nil || ok {
		t.Fatalf("expected absent, got ok=%v err=%v", ok, err)
	}

	// Present only in remote.
	_ = remote.Put("k", bytes.NewReader([]byte("x")))
	if ok, err := cs.Exists("k"); err != nil || !ok {
		t.Fatalf("expected present via remote, got ok=%v err=%v", ok, err)
	}

	// Present only in local.
	_ = local.Put("local-only", bytes.NewReader([]byte("y")))
	if ok, err := cs.Exists("local-only"); err != nil || !ok {
		t.Fatalf("expected present via local, got ok=%v err=%v", ok, err)
	}
}

func TestNewCachingStore_RequiresBoth(t *testing.T) {
	if _, err := NewCachingStore(nil, newMemStore()); err == nil {
		t.Fatalf("expected error when local is nil")
	}
	if _, err := NewCachingStore(newMemStore(), nil); err == nil {
		t.Fatalf("expected error when remote is nil")
	}
}
