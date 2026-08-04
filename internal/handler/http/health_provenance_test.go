package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentiae/platform-kit/buildinfo"
)

// TestHealthCheck_ReportsBuildIdentity pins the read-only build-identity
// surface. The deployed image's source identity has to be queryable without
// shelling into the host: /health is the one endpoint every verifier already
// calls, and this test fails if the primary_revision / modified /
// source_manifest_digest fields are dropped or renamed — which is exactly how
// the stamp would silently rot back into marker-string guessing.
func TestHealthCheck_ReportsBuildIdentity(t *testing.T) {
	origRev, origMod, origDigest := buildinfo.Revision, buildinfo.Modified, buildinfo.SourceManifestDigest
	t.Cleanup(func() {
		buildinfo.Revision, buildinfo.Modified, buildinfo.SourceManifestDigest = origRev, origMod, origDigest
	})

	buildinfo.Revision = "1a14c2b39d0be6e7707b5e094786a5c5ff2a35c0"
	buildinfo.Modified = "false"
	buildinfo.SourceManifestDigest = "sha256:0f1e2d3c"

	s := &Server{}
	w := httptest.NewRecorder()
	s.healthCheck(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}

	var got struct {
		Success bool `json:"success"`
		Data    struct {
			Status  string `json:"status"`
			Service string `json:"service"`
			Build   struct {
				PrimaryRevision      string `json:"primary_revision"`
				Modified             bool   `json:"modified"`
				SourceManifestDigest string `json:"source_manifest_digest"`
			} `json:"build"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if got.Data.Status != "healthy" {
		t.Fatalf("status=%q want healthy", got.Data.Status)
	}
	if got.Data.Build.PrimaryRevision != "1a14c2b39d0be6e7707b5e094786a5c5ff2a35c0" {
		t.Fatalf("primary_revision=%q want the linked revision", got.Data.Build.PrimaryRevision)
	}
	if got.Data.Build.Modified {
		t.Fatalf("modified=true want false")
	}
	if got.Data.Build.SourceManifestDigest != "sha256:0f1e2d3c" {
		t.Fatalf("source_manifest_digest=%q want the linked digest", got.Data.Build.SourceManifestDigest)
	}
}
