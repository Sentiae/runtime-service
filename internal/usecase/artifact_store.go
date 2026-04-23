package usecase

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ArtifactStore is the content-addressed storage surface the §9.2
// hermetic build subsystem writes to. Artifacts are keyed by their
// OutputDigest — a stable hash over the build output bytes — so two
// builds that produce identical outputs share one stored blob and
// `FindByDigest` becomes an O(1) deduplication primitive.
//
// The interface is intentionally small (Put/Get/Exists) so future
// backends (S3, GCS, IPFS) slot in without touching the usecase.
// FilesystemStore is the production default; in-memory stores can be
// used in tests without hitting disk.
type ArtifactStore interface {
	// Put stores the blob under the given digest. Idempotent — writing
	// the same digest+content twice is a no-op. Writing a different
	// content under an existing digest is an integrity error.
	Put(digest string, r io.Reader) error
	// Get returns a reader for the stored blob or ErrArtifactNotFound.
	Get(digest string) (io.ReadCloser, error)
	// Exists reports whether an artifact is stored for the digest.
	// Lighter than Get when the caller only needs a cache-hit check.
	Exists(digest string) (bool, error)
	// VerifyHash re-hashes the stored bytes for `digest` and returns
	// ErrArtifactIntegrity when the on-disk content no longer matches.
	// Used by §9.2 hermetic chains to detect mid-flight corruption
	// between step N and step N+1. Missing artifacts return
	// ErrArtifactNotFound so callers can distinguish corruption from
	// a fresh run.
	VerifyHash(digest string) error
}

// ErrArtifactNotFound is returned by Get/Resolve when no artifact is
// stored for the requested digest. Callers distinguish this from other
// errors to decide whether to fall through to a rebuild.
var ErrArtifactNotFound = errors.New("artifact: not found")

// ErrArtifactIntegrity is returned when a stored artifact's bytes
// don't match its digest on verification. Indicates storage
// corruption or a hash collision attempt; callers should surface it
// loudly rather than silently swallowing.
var ErrArtifactIntegrity = errors.New("artifact: integrity check failed")

// FilesystemStore is the default ArtifactStore. Layout is
// `<root>/<first-two-hex>/<digest>` to avoid single-directory
// scalability issues on filesystems with slow directory listings.
type FilesystemStore struct {
	root string
}

// NewFilesystemStore creates the root directory if needed and returns
// a store rooted there. A missing or non-writable root returns an
// error at construction so boot-time misconfiguration fails loud.
func NewFilesystemStore(root string) (*FilesystemStore, error) {
	if root == "" {
		return nil, errors.New("artifact store: root path is required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("artifact store: mkdir %s: %w", root, err)
	}
	return &FilesystemStore{root: root}, nil
}

// pathFor returns the on-disk path for a digest. Digest is expected to
// be hex-encoded; shorter-than-2-chars digests fall back to a `misc/`
// bucket so callers get a useful error at Put time rather than a panic.
func (s *FilesystemStore) pathFor(digest string) string {
	if len(digest) < 2 {
		return filepath.Join(s.root, "misc", digest)
	}
	return filepath.Join(s.root, digest[:2], digest)
}

// Put stores the blob. Writes go through a temp file + rename so a
// crash mid-write can't leave a partial artifact under the final name.
// The written bytes are hashed and compared against the declared
// digest before the rename — a mismatch returns ErrArtifactIntegrity
// without publishing the bad file.
func (s *FilesystemStore) Put(digest string, r io.Reader) error {
	if digest == "" {
		return errors.New("artifact store: digest is required")
	}
	finalPath := s.pathFor(digest)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return fmt.Errorf("artifact store: mkdir: %w", err)
	}
	// Already present? Idempotent success.
	if _, err := os.Stat(finalPath); err == nil {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(finalPath), ".artifact-*.tmp")
	if err != nil {
		return fmt.Errorf("artifact store: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	if _, err := io.Copy(mw, r); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("artifact store: copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != digest {
		cleanup()
		return fmt.Errorf("%w: declared=%s actual=%s", ErrArtifactIntegrity, digest, actual)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanup()
		return fmt.Errorf("artifact store: rename: %w", err)
	}
	return nil
}

// Get opens the artifact for read. Caller must Close the reader.
func (s *FilesystemStore) Get(digest string) (io.ReadCloser, error) {
	f, err := os.Open(s.pathFor(digest))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrArtifactNotFound
		}
		return nil, err
	}
	return f, nil
}

// Exists reports presence without opening a reader.
func (s *FilesystemStore) Exists(digest string) (bool, error) {
	_, err := os.Stat(s.pathFor(digest))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// VerifyHash re-hashes the stored artifact and compares against the
// declared digest. Implements the §9.2 integrity verification hook used
// by the hermetic chain runner between steps.
func (s *FilesystemStore) VerifyHash(digest string) error {
	if digest == "" {
		return errors.New("artifact store: digest is required")
	}
	f, err := os.Open(s.pathFor(digest))
	if err != nil {
		if os.IsNotExist(err) {
			return ErrArtifactNotFound
		}
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != digest {
		return fmt.Errorf("%w: declared=%s actual=%s", ErrArtifactIntegrity, digest, actual)
	}
	return nil
}

// ComputeOutputDigest hashes a reader into a hex sha256. Small helper
// used by callers that want to name an artifact before handing it to
// Put. Returning hex (not raw bytes) matches the input-digest format
// produced by ComputeInputDigest.
func ComputeOutputDigest(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
