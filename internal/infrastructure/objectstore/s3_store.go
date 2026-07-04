// Package objectstore provides durable, content-addressed backends for
// the runtime-service ArtifactStore seam (usecase.ArtifactStore).
//
// S3ArtifactStore is the durable "source of truth" backend: artifacts
// (build outputs, and — via the snapshot service — Firecracker memory +
// state files) live in an S3/MinIO bucket so a snapshot taken on host A
// is restorable on host B. CachingStore (caching_store.go) layers a
// local FilesystemStore in front of it for the fast restore path.
//
// The ArtifactStore interface is deliberately context-free (Put/Get/
// Exists/VerifyHash take only a digest), so this adapter owns its own
// timeouts via context.Background() — mirroring the git-service S3
// adapter pattern.
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// S3Config carries the connection parameters for S3ArtifactStore. The
// adapter never reads env vars directly so tests can inject arbitrary
// values and the DI container stays the single config source.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	PathStyle bool
}

// S3ArtifactStore implements usecase.ArtifactStore backed by an
// S3-compatible bucket (MinIO in the homelab, real S3 in prod). The
// object key is the artifact digest verbatim — the same content-address
// contract FilesystemStore uses — so the two backends are
// interchangeable behind the interface.
type S3ArtifactStore struct {
	cfg    S3Config
	client *minio.Client
}

// compile-time assertion: S3ArtifactStore satisfies the port.
var _ usecase.ArtifactStore = (*S3ArtifactStore)(nil)

// NewS3ArtifactStore builds the MinIO client and verifies the bucket is
// reachable, creating it on MinIO-style backends when missing. A missing
// bucket on locked-down AWS (no CreateBucket IAM) surfaces the error to
// the caller so boot-time misconfiguration fails loud.
func NewS3ArtifactStore(cfg S3Config) (*S3ArtifactStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objectstore: s3 endpoint and bucket are required")
	}
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("objectstore: minio client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: bucket exists check: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("objectstore: create bucket %q: %w", cfg.Bucket, err)
		}
	}

	return &S3ArtifactStore{cfg: cfg, client: client}, nil
}

// Put streams the blob into the bucket under its digest. The object key
// is the digest verbatim. Size is unknown up front (the reader may be a
// stream), so -1 is passed and minio-go falls back to multipart upload.
func (s *S3ArtifactStore) Put(digest string, r io.Reader) error {
	if digest == "" {
		return errors.New("objectstore: digest is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, digest, r, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("objectstore: put object %q: %w", digest, err)
	}
	return nil
}

// Get streams the object body back. A missing object is translated to
// usecase.ErrArtifactNotFound so callers can distinguish a cache miss
// from a transport failure. The probe (StatObject) forces the missing-
// key error to surface here rather than on first Read.
func (s *S3ArtifactStore) Get(digest string) (io.ReadCloser, error) {
	ctx := context.Background()
	if _, err := s.client.StatObject(ctx, s.cfg.Bucket, digest, minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return nil, usecase.ErrArtifactNotFound
		}
		return nil, fmt.Errorf("objectstore: stat object %q: %w", digest, err)
	}
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, digest, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get object %q: %w", digest, err)
	}
	return obj, nil
}

// Exists reports presence via a HEAD-equivalent StatObject — lighter than
// a full Get when the caller only needs a cache-hit check.
func (s *S3ArtifactStore) Exists(digest string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.client.StatObject(ctx, s.cfg.Bucket, digest, minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("objectstore: stat object %q: %w", digest, err)
	}
	return true, nil
}

// VerifyHash re-hashes the stored bytes and compares against the declared
// digest, returning usecase.ErrArtifactIntegrity on mismatch and
// usecase.ErrArtifactNotFound when the object is absent. Snapshot keys
// (snapshots/<id>/mem) are NOT hex sha256 digests, so callers should only
// VerifyHash content-addressed artifacts — for those the digest is the
// expected hash.
func (s *S3ArtifactStore) VerifyHash(digest string) error {
	if digest == "" {
		return errors.New("objectstore: digest is required")
	}
	rc, err := s.Get(digest)
	if err != nil {
		return err
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return fmt.Errorf("objectstore: hash object %q: %w", digest, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != digest {
		return fmt.Errorf("%w: declared=%s actual=%s", usecase.ErrArtifactIntegrity, digest, actual)
	}
	return nil
}

// isNoSuchKey reports whether err is a MinIO "object not found" response.
// minio-go returns a typed ErrorResponse whose Code is NoSuchKey for a
// missing object and NoSuchBucket for a missing bucket; both mean "not
// present" for our purposes.
func isNoSuchKey(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == 404
}
