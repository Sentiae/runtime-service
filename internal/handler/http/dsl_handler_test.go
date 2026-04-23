package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/dsl"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// fakeHermeticStarter captures StartBuild calls so the wrapper
// behavior can be asserted without wiring a real Postgres.
type fakeHermeticStarter struct {
	calls       int32
	lastInput   usecase.StartBuildInput
	returnBuild *domain.HermeticBuild
	returnHit   bool
	returnErr   error
}

func (f *fakeHermeticStarter) StartBuild(_ context.Context, in usecase.StartBuildInput) (*domain.HermeticBuild, bool, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastInput = in
	if f.returnErr != nil {
		return nil, false, f.returnErr
	}
	if f.returnBuild != nil {
		return f.returnBuild, f.returnHit, nil
	}
	return &domain.HermeticBuild{
		ID:             uuid.New(),
		OrganizationID: in.OrganizationID,
		InputDigest:    usecase.ComputeInputDigest(in.Inputs),
	}, false, nil
}

// newTestDSLHandler builds a DSLHandler wired with a registered no-op
// action we control. We sidestep NewDSLHandler's built-in action set
// because those touch TestRunRepo / HermeticBuildUseCase — the point
// of this test suite is to exercise the hermetic wrap itself, not the
// production actions.
func newTestDSLHandler(t *testing.T, starter HermeticStarter, inner dsl.Action) (*DSLHandler, *chi.Mux) {
	t.Helper()
	h := &DSLHandler{
		inner:          dsl.NewHandler(),
		trustedActions: map[string]bool{},
		starter:        starter,
	}
	h.registerHermetic("noop", inner)
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return h, r
}

func TestDSLHandler_HermeticDefaultOn_CallsStartBuild(t *testing.T) {
	starter := &fakeHermeticStarter{}
	innerCalls := 0
	_, r := newTestDSLHandler(t, starter, func(_ context.Context, _ dsl.Request) (map[string]any, error) {
		innerCalls++
		return map[string]any{"ran": true}, nil
	})

	payload := map[string]any{
		"flow_id":   uuid.New().String(),
		"step_name": "s1",
		"action":    "noop",
		"config": map[string]any{
			"organization_id": uuid.New().String(),
			"language":        "go",
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/dsl/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&starter.calls); got != 1 {
		t.Fatalf("expected 1 StartBuild call, got %d", got)
	}
	if innerCalls != 1 {
		t.Fatalf("expected inner action to run once, got %d", innerCalls)
	}
	// The action name must be tagged into the hermetic inputs so the
	// audit row is self-describing.
	if tag, ok := starter.lastInput.Inputs["build_tag"]; !ok || tag != "dsl-step:noop" {
		t.Fatalf("expected build_tag=dsl-step:noop, got %q", tag)
	}
	if act := starter.lastInput.Inputs["dsl_action"]; act != "noop" {
		t.Fatalf("expected dsl_action=noop in hermetic inputs, got %q", act)
	}
	// hermetic_build_id must surface in the step output so the DSL
	// caller can correlate the step result with the audit row.
	var resp dsl.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp.Output["hermetic_build_id"]; !ok {
		t.Fatalf("expected hermetic_build_id in output, got %+v", resp.Output)
	}
	if resp.Output["ran"] != true {
		t.Fatalf("expected inner action output merged, got %+v", resp.Output)
	}
}

func TestDSLHandler_HermeticFalse_SkipsStartBuild(t *testing.T) {
	starter := &fakeHermeticStarter{}
	innerCalls := 0
	_, r := newTestDSLHandler(t, starter, func(_ context.Context, _ dsl.Request) (map[string]any, error) {
		innerCalls++
		return map[string]any{"ran": true}, nil
	})

	payload := map[string]any{
		"flow_id":   uuid.New().String(),
		"step_name": "s1",
		"action":    "noop",
		"config": map[string]any{
			"organization_id": uuid.New().String(),
			"hermetic":        false,
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/dsl/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&starter.calls); got != 0 {
		t.Fatalf("expected StartBuild NOT to be called, got %d calls", got)
	}
	if innerCalls != 1 {
		t.Fatalf("expected inner action to still run, got %d calls", innerCalls)
	}
	// No hermetic_build_id should surface when hermetic is off.
	var resp dsl.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp.Output["hermetic_build_id"]; ok {
		t.Fatalf("expected NO hermetic_build_id when hermetic:false, got %+v", resp.Output)
	}
	if resp.Output["ran"] != true {
		t.Fatalf("expected inner action to drive output when hermetic is off, got %+v", resp.Output)
	}
}

func TestDSLHandler_HermeticTrustedAction_SkipsWrap(t *testing.T) {
	// `build_artifact` is trusted: the action already runs its own
	// hermetic lifecycle. The wrapper must NOT double-call StartBuild
	// or the audit rows would duplicate.
	starter := &fakeHermeticStarter{}
	h := &DSLHandler{
		inner:          dsl.NewHandler(),
		trustedActions: map[string]bool{"build_artifact": true},
		starter:        starter,
	}
	innerCalls := 0
	h.registerHermetic("build_artifact", func(_ context.Context, _ dsl.Request) (map[string]any, error) {
		innerCalls++
		return map[string]any{"ran": true}, nil
	})
	r := chi.NewRouter()
	h.RegisterRoutes(r)

	payload := map[string]any{
		"flow_id":   uuid.New().String(),
		"step_name": "s1",
		"action":    "build_artifact",
		"config":    map[string]any{},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/dsl/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&starter.calls); got != 0 {
		t.Fatalf("trusted action must NOT call StartBuild, got %d", got)
	}
	if innerCalls != 1 {
		t.Fatalf("expected trusted action to run, got %d", innerCalls)
	}
}
