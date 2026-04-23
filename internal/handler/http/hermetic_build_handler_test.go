package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// The handler requires a *usecase.HermeticBuildUseCase; without a DB
// we can still exercise the error paths that fire before the usecase
// is consulted (nil-uc guards, missing params, bad JSON, wrong method).

func TestHermeticBuildHandler_UploadArtifact_NoStoreReturns503(t *testing.T) {
	// UC without a store: upload must respond 503 so callers know the
	// artifact path isn't configured rather than silently 500.
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodPut, "/hermetic-builds/00000000-0000-0000-0000-000000000001/artifact", strings.NewReader("blob"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without configured store, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_DownloadArtifact_NoStoreReturns503(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/hermetic-builds/00000000-0000-0000-0000-000000000001/artifact?digest=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_Complete_InvalidJSON400(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete",
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_Complete_MissingDigest400(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	payload, _ := json.Marshal(map[string]any{
		"build_id":      "00000000-0000-0000-0000-000000000001",
		"artifact_ref":  "some-ref",
		"output_digest": "",
	})
	req := httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing digest, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_Resolve_InvalidOrgID400(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/hermetic-builds/resolve?org_id=not-a-uuid&input_digest=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad org_id, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_Resolve_MissingDigest400(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/hermetic-builds/resolve?org_id=00000000-0000-0000-0000-000000000001", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing input_digest, got %d", rec.Code)
	}
}

func TestHermeticBuildHandler_Verify_InvalidID400(t *testing.T) {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/hermetic-builds/not-a-uuid/verify", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad id, got %d", rec.Code)
	}
}
