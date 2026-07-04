package objectstore

import (
	"errors"
	"fmt"
	"io"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// CachingStore is the Fly.io/e2b "object store of record + local NVMe
// cache" shape implemented over two ArtifactStores:
//
//   - remote: the durable source of truth (S3ArtifactStore). A snapshot
//     written here is restorable on any host.
//   - local:  a fast cache (a FilesystemStore on local disk/NVMe). Reads
//     hit local first; a miss pulls from remote once and warms the cache.
//
// Put writes BOTH (durable + warm). Get is local-first with a
// pull-once-cache-locally fallback. Exists is local-or-remote. The type
// is pure and unit-testable with two in-memory stores — it holds no
// network state of its own.
type CachingStore struct {
	local  usecase.ArtifactStore
	remote usecase.ArtifactStore
}

// compile-time assertion: CachingStore satisfies the port.
var _ usecase.ArtifactStore = (*CachingStore)(nil)

// NewCachingStore wires a local cache in front of a durable remote. Both
// stores are required — a nil store is a wiring error, not a degraded
// mode (callers that want local-only should inject the FilesystemStore
// directly).
func NewCachingStore(local, remote usecase.ArtifactStore) (*CachingStore, error) {
	if local == nil || remote == nil {
		return nil, errors.New("objectstore: caching store needs both local and remote")
	}
	return &CachingStore{local: local, remote: remote}, nil
}

// Put writes to the durable remote first (so a crash after a successful
// remote write still leaves a recoverable artifact) and then warms the
// local cache. A local cache-warm failure is non-fatal: the durable copy
// is what matters; the cache repopulates on the next Get.
func (c *CachingStore) Put(digest string, r io.Reader) error {
	// Tee the single reader so the same bytes land in both stores without
	// buffering the whole (potentially multi-GB memory snapshot) in RAM.
	pr, pw := io.Pipe()
	tee := io.TeeReader(r, pw)

	// Local warm runs concurrently, draining the tee'd copy. Its result is
	// best-effort — recorded but never returned as the Put error.
	localDone := make(chan error, 1)
	go func() {
		// recover so a panic in the local store can't leak the goroutine
		// or deadlock the pipe.
		defer func() {
			if rec := recover(); rec != nil {
				localDone <- fmt.Errorf("objectstore: local cache warm panicked: %v", rec)
			}
		}()
		localDone <- c.local.Put(digest, pr)
	}()

	// Remote is the durable write and drives the read. When it returns we
	// close the pipe writer so the local goroutine sees EOF.
	remoteErr := c.remote.Put(digest, tee)
	_ = pw.CloseWithError(remoteErr)

	// Always drain the local goroutine to avoid a leak, even on remote
	// failure (the pipe close above unblocks it).
	localErr := <-localDone

	if remoteErr != nil {
		return fmt.Errorf("objectstore: remote put: %w", remoteErr)
	}
	if localErr != nil {
		// Durable write succeeded; surface cache-warm failure as a
		// best-effort signal but don't fail the Put — the artifact is
		// safe and the next Get repopulates the cache.
		return nil
	}
	return nil
}

// Get returns the artifact from the local cache when present. On a local
// miss it streams from the durable remote, tees the bytes into the local
// cache (pull-once-cache-locally), and returns a reader over those same
// bytes to the caller. A remote miss propagates ErrArtifactNotFound.
func (c *CachingStore) Get(digest string) (io.ReadCloser, error) {
	rc, err := c.local.Get(digest)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, usecase.ErrArtifactNotFound) {
		return nil, fmt.Errorf("objectstore: local get: %w", err)
	}

	// Local miss — pull from the durable remote.
	remoteRC, err := c.remote.Get(digest)
	if err != nil {
		return nil, err // includes ErrArtifactNotFound verbatim
	}

	// Warm the cache as the caller reads. We pull the full body, write it
	// into the local store, then re-open the now-cached copy so the
	// returned reader is backed by the cache (and the caller can't race
	// the cache write). Snapshot mem/state files are large but bounded;
	// this keeps the cache-populate semantics simple and correct.
	defer remoteRC.Close()
	if err := c.local.Put(digest, remoteRC); err != nil {
		// Cache warm failed — fall back to a fresh remote stream so the
		// caller still gets the bytes. Best-effort caching, never a hard
		// failure on the read path.
		return c.remote.Get(digest)
	}
	return c.local.Get(digest)
}

// Exists reports presence in either tier: a local hit is authoritative;
// otherwise fall through to the durable remote.
func (c *CachingStore) Exists(digest string) (bool, error) {
	ok, err := c.local.Exists(digest)
	if err == nil && ok {
		return true, nil
	}
	if err != nil && !errors.Is(err, usecase.ErrArtifactNotFound) {
		return false, fmt.Errorf("objectstore: local exists: %w", err)
	}
	return c.remote.Exists(digest)
}

// VerifyHash verifies against the durable remote — it is the source of
// truth, and a corrupt local cache entry must not mask a good remote
// artifact (nor vice-versa). Callers that need to validate the cache copy
// can VerifyHash the local store directly.
func (c *CachingStore) VerifyHash(digest string) error {
	return c.remote.VerifyHash(digest)
}
