package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentiae/runtime-service/internal/version"
)

// TestHealthCheck_ReportsDeployProvenance pins the read-only provenance surface.
// The deployed image's source identity has to be queryable without shelling into
// the host: /health is the one endpoint every verifier already calls, and this
// test fails if the vcs.revision / vcs.modified fields are dropped or renamed —
// which is exactly how the stamp would silently rot back into marker-string
// guessing.
func TestHealthCheck_ReportsDeployProvenance(t *testing.T) {
	origRev, origMod := version.Revision, version.Modified
	t.Cleanup(func() { version.Revision, version.Modified = origRev, origMod })

	version.Revision = "1a14c2b39d0be6e7707b5e094786a5c5ff2a35c0"
	version.Modified = "false"

	s := &Server{}
	w := httptest.NewRecorder()
	s.healthCheck(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}

	var got struct {
		Success bool `json:"success"`
		Data    struct {
			Status   string `json:"status"`
			Service  string `json:"service"`
			Revision string `json:"vcs.revision"`
			Modified string `json:"vcs.modified"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if got.Data.Status != "healthy" {
		t.Fatalf("status=%q want healthy", got.Data.Status)
	}
	if got.Data.Revision != "1a14c2b39d0be6e7707b5e094786a5c5ff2a35c0" {
		t.Fatalf("vcs.revision=%q want the linked revision", got.Data.Revision)
	}
	if got.Data.Modified != "false" {
		t.Fatalf("vcs.modified=%q want false", got.Data.Modified)
	}
}
