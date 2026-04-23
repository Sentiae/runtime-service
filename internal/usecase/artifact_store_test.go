package usecase

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFilesystemStore_PutGetExists(t *testing.T) {
	store, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	content := []byte("hello artifact world")
	digest, err := ComputeOutputDigest(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	if ok, _ := store.Exists(digest); ok {
		t.Fatalf("exists before put should be false")
	}
	if err := store.Put(digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if ok, _ := store.Exists(digest); !ok {
		t.Fatalf("exists after put should be true")
	}

	rc, err := store.Get(digest)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %q want %q", got, content)
	}
}

func TestFilesystemStore_PutIntegrityCheck(t *testing.T) {
	store, _ := NewFilesystemStore(t.TempDir())
	// Write bytes that don't match the declared digest — must reject.
	wrongDigest := strings.Repeat("a", 64)
	err := store.Put(wrongDigest, strings.NewReader("different content"))
	if !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("expected integrity error, got %v", err)
	}
	if ok, _ := store.Exists(wrongDigest); ok {
		t.Fatalf("failed Put must not publish the file")
	}
}

func TestFilesystemStore_PutIdempotent(t *testing.T) {
	store, _ := NewFilesystemStore(t.TempDir())
	content := []byte("idempotent")
	digest, _ := ComputeOutputDigest(bytes.NewReader(content))
	for i := 0; i < 3; i++ {
		if err := store.Put(digest, bytes.NewReader(content)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	rc, _ := store.Get(digest)
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content corrupted after repeated puts")
	}
}

func TestFilesystemStore_GetMissing(t *testing.T) {
	store, _ := NewFilesystemStore(t.TempDir())
	_, err := store.Get(strings.Repeat("b", 64))
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound, got %v", err)
	}
}

func TestFilesystemStore_EmptyRootRejected(t *testing.T) {
	_, err := NewFilesystemStore("")
	if err == nil {
		t.Fatalf("empty root should error")
	}
}

func TestComputeOutputDigest_Deterministic(t *testing.T) {
	a, _ := ComputeOutputDigest(strings.NewReader("same input"))
	b, _ := ComputeOutputDigest(strings.NewReader("same input"))
	if a != b {
		t.Fatalf("digest not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
}
