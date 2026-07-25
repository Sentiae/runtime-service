package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestFleetHandler_RequireServiceTokenFailsClosed pins the fail-closed contract
// of the /fleet guard. These routes sit outside the JWT group on a port that
// binds every interface on the fleet host, and include DELETE /clones/{id}, so
// absence of a credential must be a denial, never a bypass. The empty-token case
// is constructed directly because the constructor now refuses to build it — it
// models a handler assembled around that refusal.
func TestFleetHandler_RequireServiceTokenFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		setHeader  bool
		presented  string
		wantStatus int
	}{
		{"empty configured token denies missing header", "", false, "", http.StatusUnauthorized},
		{"empty configured token denies empty header", "", true, "", http.StatusUnauthorized},
		{"empty configured token denies any header", "", true, "anything", http.StatusUnauthorized},
		{"configured token denies missing header", "svc-tok", false, "", http.StatusUnauthorized},
		{"configured token denies empty header", "svc-tok", true, "", http.StatusUnauthorized},
		{"configured token denies wrong header", "svc-tok", true, "wrong", http.StatusUnauthorized},
		{"configured token denies prefix", "svc-tok", true, "svc", http.StatusUnauthorized},
		{"configured token allows exact match", "svc-tok", true, "svc-tok", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &FleetHandler{serviceToken: tt.configured}
			reached := false
			guarded := h.requireServiceToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodDelete, "/fleet/clones/1", nil)
			if tt.setHeader {
				req.Header.Set("x-api-key", tt.presented)
			}
			w := httptest.NewRecorder()
			guarded.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if wantReached := tt.wantStatus == http.StatusOK; reached != wantReached {
				t.Fatalf("next handler reached=%v want %v", reached, wantReached)
			}
		})
	}
}

// TestFleetHandler_UnauthorizedResponseIsUniform checks the denial body does not
// reveal whether a service token is configured.
func TestFleetHandler_UnauthorizedResponseIsUniform(t *testing.T) {
	var bodies []string
	for _, configured := range []string{"", "svc-tok"} {
		h := &FleetHandler{serviceToken: configured}
		guarded := h.requireServiceToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("next handler must not run on a denied request")
		}))
		req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
		req.Header.Set("x-api-key", "nope")
		w := httptest.NewRecorder()
		guarded.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", w.Code)
		}
		body := w.Body.String()
		for _, leak := range []string{"APP_GRPC_SERVICE_API_KEY", "unset", "not configured", "disabled", "trusted"} {
			if strings.Contains(body, leak) {
				t.Fatalf("401 body leaks configuration state (%q): %s", leak, body)
			}
		}
		bodies = append(bodies, body)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("401 body differs by configuration state: %q vs %q", bodies[0], bodies[1])
	}
}

// TestNewFleetHandler_RefusesWithoutToken is the boot-level half of the fix: the
// DI container calls this constructor and log.Fatalf's on error, so a tokenless
// deployment refuses to boot instead of mounting a clone-kill route that is
// either open or silently unusable.
func TestNewFleetHandler_RefusesWithoutToken(t *testing.T) {
	tests := []struct {
		name         string
		serviceToken string
		wantErr      error
	}{
		{"empty token refuses boot", "", ErrNoFleetControlToken},
		{"configured token boots", "svc-tok", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// nil pool: the warm pool may legitimately be disabled; the token
			// gate is independent of it.
			h, err := NewFleetHandler(nil, tt.serviceToken)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && h != nil {
				t.Fatalf("handler returned alongside refusal: %+v", h)
			}
			if tt.wantErr == nil && h == nil {
				t.Fatal("no handler returned on valid config")
			}
		})
	}
}

// TestFleetHandler_RoutesAreGuarded walks the real mounted router: every /fleet
// route (including the destructive ones) must 401 without the token and be
// reachable with it. Guards against a future route being added inside
// RegisterRoutes but outside the middleware.
func TestFleetHandler_RoutesAreGuarded(t *testing.T) {
	h, err := NewFleetHandler(nil, "svc-tok")
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	routes := []struct {
		method string
		path   string
		// status expected once the correct token IS presented (nil pool ⇒ the
		// mutating routes report the pool as disabled).
		wantAuthed int
	}{
		{http.MethodGet, "/fleet/", http.StatusOK},
		{http.MethodDelete, "/fleet/clones/1", http.StatusServiceUnavailable},
		{http.MethodPost, "/fleet/templates/python/refresh", http.StatusServiceUnavailable},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("no token: status=%d want 401 body=%s", w.Code, w.Body.String())
			}

			req2 := httptest.NewRequest(rt.method, rt.path, nil)
			req2.Header.Set("x-api-key", "svc-tok")
			w2 := httptest.NewRecorder()
			r.ServeHTTP(w2, req2)
			if w2.Code != rt.wantAuthed {
				t.Fatalf("with token: status=%d want %d body=%s", w2.Code, rt.wantAuthed, w2.Body.String())
			}
		})
	}
}

// TestSetupRoutes_NoFleetHandlerNoRoute confirms the mount is conditional: with
// no handler wired, the /fleet surface is not served at all (404), so a
// tokenless deployment never exposes clone-kill even if a future caller skips
// the constructor's boot refusal.
func TestSetupRoutes_NoFleetHandlerNoRoute(t *testing.T) {
	s := &Server{router: chi.NewRouter()}
	s.SetupRoutes()
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/fleet"},
		{http.MethodDelete, "/fleet/clones/1"},
	} {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unmounted %s %s status=%d want 404", rt.method, rt.path, w.Code)
		}
	}
}

// TestFleetHandler_GetFleetReportsDisabledPool keeps the nil-pool contract
// visible next to the auth change: an authorized caller still gets
// {enabled:false} rather than a 500.
func TestFleetHandler_GetFleetReportsDisabledPool(t *testing.T) {
	h, err := NewFleetHandler(nil, "svc-tok")
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	w := httptest.NewRecorder()
	h.GetFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	var body struct {
		Data struct {
			Enabled bool `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if body.Data.Enabled {
		t.Fatalf("nil pool reported enabled: %s", w.Body.String())
	}
}
