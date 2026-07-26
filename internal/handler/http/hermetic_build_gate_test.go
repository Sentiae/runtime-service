package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// These are the tests for #hermetic-build-routes-are-ungated. The bug was WIRING,
// not middleware: the permission middleware was installed on a chi.Group that
// registered no routes, while the routes were registered on the outer router — so
// the mutation routes ran with no check at all while a comment claimed otherwise.
// The assertions therefore live at the ROUTER, which is where the control was
// missing.

// buildGatedHermeticRouter mounts the handler exactly as routes.go does, with the
// deny-all checker that is the live default (no PermissionChecker is wired).
func buildGatedHermeticRouter(checker PermissionChecker) chi.Router {
	h := NewHermeticBuildHandler(usecase.NewHermeticBuildUseCase(nil))
	r := chi.NewRouter()
	h.RegisterRoutes(r,
		RequireRuntimePermission(checker, "hermetic_build", "write", "id"),
		RequireRuntimePermissionFromBody(checker, "hermetic_build", "write", "build_id"),
	)
	return r
}

// withUser puts an authenticated actor on the request, so a refusal below is the
// PERMISSION decision rather than a missing identity.
func withUser(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ContextKeyUserID, uuid.New()))
}

func TestHermeticBuildMutationsAreRefusedWithoutPermission(t *testing.T) {
	buildID := "00000000-0000-0000-0000-000000000001"
	completeBody, err := json.Marshal(map[string]any{
		"build_id":      buildID,
		"output_digest": "sha256:cafe",
		"artifact_ref":  "some-ref",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	tests := []struct {
		name     string
		newReq   func() *http.Request
		wantCode int
	}{
		{
			name: "POST /complete — substituting the artifact the next identical input digest resolves to",
			newReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete", bytes.NewReader(completeBody))
				req.Header.Set("Content-Type", "application/json")
				return withUser(req)
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "PUT /{id}/artifact — writing the artifact bytes",
			newReq: func() *http.Request {
				return withUser(httptest.NewRequest(http.MethodPut, "/hermetic-builds/"+buildID+"/artifact", strings.NewReader("blob")))
			},
			wantCode: http.StatusForbidden,
		},
		{
			name: "POST /complete by an unauthenticated caller",
			newReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete", bytes.NewReader(completeBody))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "PUT /{id}/artifact by an unauthenticated caller",
			newReq: func() *http.Request {
				return httptest.NewRequest(http.MethodPut, "/hermetic-builds/"+buildID+"/artifact", strings.NewReader("blob"))
			},
			wantCode: http.StatusUnauthorized,
		},
	}

	r := buildGatedHermeticRouter(denyAllPermissionChecker{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, tt.newReq())
			if rec.Code != tt.wantCode {
				t.Fatalf("got %d, want %d — this route must not reach the handler without a permission decision (body: %s)",
					rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

// A mutation whose target is not named cannot be authorized, so it is refused
// rather than checked against an empty resource id (which a permissive checker
// would happily allow).
func TestHermeticBuildCompleteRefusedWithoutABuildID(t *testing.T) {
	r := buildGatedHermeticRouter(DevAllowAllPermissionChecker{})

	tests := []struct {
		name string
		body string
	}{
		{"no build_id at all", `{"output_digest":"sha256:cafe"}`},
		{"an empty build_id", `{"build_id":"","output_digest":"sha256:cafe"}`},
		{"a build_id that is not a string", `{"build_id":42,"output_digest":"sha256:cafe"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := withUser(httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete", strings.NewReader(tt.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The body gate must RESTORE the body it buffered, or gating a route would break
// the handler behind it — a control that costs correctness gets removed later.
func TestHermeticBuildBodyGateRestoresTheBodyForTheHandler(t *testing.T) {
	r := buildGatedHermeticRouter(DevAllowAllPermissionChecker{})

	// output_digest is empty, so the HANDLER answers 400 — which it can only do if
	// it received the body the gate already read.
	body := `{"build_id":"00000000-0000-0000-0000-000000000001","output_digest":""}`
	req := withUser(httptest.NewRequest(http.MethodPost, "/hermetic-builds/complete", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 from the handler's own validation (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "output_digest") {
		t.Fatalf("expected the HANDLER's validation message, got %s", rec.Body.String())
	}
}

// The reads are deliberately NOT gated (see RegisterRoutes): assert that, so the
// scope of the change is a decision on record rather than an accident.
func TestHermeticBuildReadsStayUngated(t *testing.T) {
	r := buildGatedHermeticRouter(denyAllPermissionChecker{})

	req := httptest.NewRequest(http.MethodGet, "/hermetic-builds/00000000-0000-0000-0000-000000000001/artifact?digest=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// 503: no artifact store configured — i.e. the request reached the handler.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (the read must reach the handler) — %s", rec.Code, rec.Body.String())
	}
}
