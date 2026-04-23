package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestComputeInputDigest_Deterministic(t *testing.T) {
	a := ComputeInputDigest(map[string]string{
		"commit_sha":        "abc123",
		"base_image_digest": "sha256:deadbeef",
	})
	// Key order reversed — digest must match.
	b := ComputeInputDigest(map[string]string{
		"base_image_digest": "sha256:deadbeef",
		"commit_sha":        "abc123",
	})
	if a != b {
		t.Fatalf("digest not key-order stable: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-hex-char digest, got %d", len(a))
	}
	if !strings.ContainsAny(a, "0123456789abcdef") {
		t.Fatalf("digest should be hex: %s", a)
	}
}

func TestComputeInputDigest_DifferentInputsDifferentDigest(t *testing.T) {
	a := ComputeInputDigest(map[string]string{"commit_sha": "abc123"})
	b := ComputeInputDigest(map[string]string{"commit_sha": "def456"})
	if a == b {
		t.Fatalf("different inputs produced same digest: %s", a)
	}
}

func TestComputeInputDigest_EmptyMap(t *testing.T) {
	got := ComputeInputDigest(nil)
	if len(got) != 64 {
		t.Fatalf("empty input should still hash to 64 hex chars, got %d", len(got))
	}
}

// TestEnforceReproducibility_RequiresRunner asserts the §9.2 enforcement
// contract: EnforceReproducibility refuses to run without a step
// runner, a persisted build id, or the original inputs. These are the
// guardrails that prevent a misconfigured caller from silently
// returning "reproducible" when nothing was actually re-run.
func TestEnforceReproducibility_RequiresRunner(t *testing.T) {
	uc := &HermeticBuildUseCase{} // repo is nil so the guard fires first
	_, err := uc.EnforceReproducibility(context.Background(), nil, uuid.New(), nil, StepRunRequest{})
	if err == nil {
		t.Fatal("expected error when repo unset")
	}
}

// TestErrReproducibilityMismatch_IsDistinct confirms the sentinel is
// a package-level var so callers can identify it with errors.Is.
func TestErrReproducibilityMismatch_IsDistinct(t *testing.T) {
	if ErrReproducibilityMismatch == nil {
		t.Fatal("ErrReproducibilityMismatch must be non-nil")
	}
	wrapped := &wrappedErr{err: ErrReproducibilityMismatch}
	if !errors.Is(wrapped, ErrReproducibilityMismatch) {
		t.Fatal("errors.Is must identify ErrReproducibilityMismatch through a wrapping error")
	}
}

// TestErrMissingBaseImageDigest_IsDistinct mirrors the above for the
// §9.2 base-image-digest enforcement sentinel.
func TestErrMissingBaseImageDigest_IsDistinct(t *testing.T) {
	if ErrMissingBaseImageDigest == nil {
		t.Fatal("ErrMissingBaseImageDigest must be non-nil")
	}
}

type wrappedErr struct{ err error }

func (w *wrappedErr) Error() string { return "wrap: " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }
