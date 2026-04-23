package http

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/dsl"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// DSLHandler exposes `POST /dsl/execute` for runtime-service DSL steps.
// §19 follow-up H5 + I3 + HermeticBuild unstub: both `run_tests` and
// `build_artifact` are now wired to real persistence (TestRun repo and
// HermeticBuildUseCase respectively).
//
// §9.2 C14 — every incoming DSL step is wrapped in a hermetic build
// envelope by default. The wrapper calls HermeticStarter.StartBuild with
// a deterministic digest derived from the step's action + config +
// input so identical inputs coalesce onto the same cached row, and the
// resulting hermetic_build_id is reported in the step output so the
// foundry worker can thread it through to downstream audit.
//
// First pass: the hermetic wrap is an audit-only envelope. We record
// the attempt, tag it with `dsl-step:{action}`, and the step body still
// runs in-process. Full Firecracker isolation is a follow-up; the audit
// row is what matters for reproducibility + post-incident review today.
type DSLHandler struct {
	inner       *dsl.Handler
	testRunRepo *postgres.TestRunRepo
	hermeticUC  *usecase.HermeticBuildUseCase
	// starter is the hermetic entrypoint the wrapper calls. Kept as an
	// interface so tests can plug a fake without a DB.
	starter HermeticStarter
	// trustedActions skip the hermetic wrap — typically because the
	// action already manages its own hermetic lifecycle (build_artifact)
	// or is a pipe-through internal call where the isolation cost isn't
	// worth paying.
	trustedActions map[string]bool
}

// HermeticStarter is the narrow slice of HermeticBuildUseCase the DSL
// wrapper needs. Declared here (not at usecase package) because it
// describes the dependency from the handler's perspective, and lets
// tests inject a fake without a real Postgres.
type HermeticStarter interface {
	StartBuild(ctx context.Context, in usecase.StartBuildInput) (*domain.HermeticBuild, bool, error)
}

// NewDSLHandler builds the handler and registers the runtime actions.
// Either dependency can be nil — the corresponding action then returns
// a clear ActionError explaining what's not configured.
func NewDSLHandler(testRunRepo *postgres.TestRunRepo, hermeticUC *usecase.HermeticBuildUseCase) *DSLHandler {
	h := &DSLHandler{
		inner:       dsl.NewHandler(),
		testRunRepo: testRunRepo,
		hermeticUC:  hermeticUC,
		// `build_artifact` already calls StartBuild itself — wrapping it
		// would double-count. Leave it alone so the existing cache-hit
		// semantics still work.
		trustedActions: map[string]bool{"build_artifact": true},
	}
	// hermeticUC concretely satisfies HermeticStarter, but nil-interface
	// pitfalls mean we only assign when the pointer is non-nil. With a
	// nil starter the wrapper still runs the action and records no
	// hermetic_build_id.
	if hermeticUC != nil {
		h.starter = hermeticUC
	}
	h.registerHermetic("run_tests", h.runTests)
	h.registerHermetic("build_artifact", h.buildArtifact)
	return h
}

// WithHermeticStarter swaps the hermetic starter. Used by tests to
// inject a fake; DI uses the default HermeticBuildUseCase wired in
// NewDSLHandler.
func (h *DSLHandler) WithHermeticStarter(s HermeticStarter) *DSLHandler {
	h.starter = s
	return h
}

// RegisterRoutes mounts the endpoint.
func (h *DSLHandler) RegisterRoutes(r chi.Router) {
	r.Method("POST", "/dsl/execute", h.inner)
}

// registerHermetic registers an action with a hermetic wrap. The wrap:
//
//   - checks the `hermetic` bool in req.Config (default true);
//   - for trusted actions, skips the wrap entirely so the inner action
//     still drives its own hermetic lifecycle;
//   - on `hermetic:false`, calls the inner action directly;
//   - on hermetic:true, calls HermeticStarter.StartBuild with a
//     deterministic digest keyed off the action + sorted config, then
//     runs the inner action, then merges `hermetic_build_id` +
//     `hermetic_cache_hit` into the output.
//
// Hermetic Start failures do NOT block the step: we log and fall
// through to the inner action so an outage of the hermetic repo
// doesn't brown-out all DSL traffic. The step output then simply
// lacks a build id — callers can infer the hermetic envelope wasn't
// recorded.
func (h *DSLHandler) registerHermetic(name string, action dsl.Action) {
	if h.trustedActions[name] {
		h.inner.Register(name, action)
		return
	}
	wrapped := func(ctx context.Context, req dsl.Request) (map[string]any, error) {
		if !hermeticRequested(req.Config) || h.starter == nil {
			return action(ctx, req)
		}
		in := hermeticInputsFor(req)
		orgID, _ := dslOptionalUUID(req.Config, "organization_id")
		pipelineRunID, _ := dslOptionalUUID(req.Config, "pipeline_run_id")
		createdBy, _ := dslOptionalUUID(req.Config, "created_by")
		build, cacheHit, err := h.starter.StartBuild(ctx, usecase.StartBuildInput{
			OrganizationID: orgID,
			PipelineRunID:  pipelineRunID,
			CreatedBy:      createdBy,
			Inputs:         in,
		})
		if err != nil {
			// Audit-only envelope: hermetic record failures must not
			// block the step, but we do want the degradation visible.
			log.Printf("[dsl] hermetic wrap skipped for action=%s step=%s: %v", req.Action, req.StepName, err)
			return action(ctx, req)
		}
		out, actionErr := action(ctx, req)
		if out == nil {
			out = map[string]any{}
		}
		if build != nil {
			out["hermetic_build_id"] = build.ID.String()
			out["hermetic_input_digest"] = build.InputDigest
			out["hermetic_cache_hit"] = cacheHit
		}
		return out, actionErr
	}
	h.inner.Register(name, wrapped)
}

// hermeticRequested reads the `hermetic` bool from req.Config with a
// default of true — every DSL step gets a hermetic envelope unless the
// flow explicitly opts out. Accepts true/false plus the JSON-friendly
// strings "true"/"false" so flows authored as plain YAML still work.
func hermeticRequested(cfg map[string]any) bool {
	v, ok := cfg["hermetic"]
	if !ok {
		return true
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return val != "false"
	default:
		return true
	}
}

// hermeticInputsFor builds the pinned-input map for the hermetic digest.
// The digest must be deterministic across semantically-identical step
// invocations — so we sort config keys and tag the action name. The
// `dsl-step:` prefix makes the audit row self-describing when a human
// reads the inputs blob in a forensic session.
func hermeticInputsFor(req dsl.Request) map[string]string {
	inputs := map[string]string{
		"dsl_action":    req.Action,
		"dsl_step_name": req.StepName,
		"dsl_flow_id":   req.FlowID.String(),
		"build_tag":     fmt.Sprintf("dsl-step:%s", req.Action),
	}
	keys := make([]string, 0, len(req.Config))
	for k := range req.Config {
		if k == "hermetic" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		inputs["cfg."+k] = fmt.Sprintf("%v", req.Config[k])
	}
	// Input map is usually small — wire values verbatim so the same
	// `(action, config, input)` tuple hits the same cached row.
	ikeys := make([]string, 0, len(req.Input))
	for k := range req.Input {
		ikeys = append(ikeys, k)
	}
	sort.Strings(ikeys)
	for _, k := range ikeys {
		inputs["in."+k] = fmt.Sprintf("%v", req.Input[k])
	}
	return inputs
}

// runTests persists a new TestRun row and resolves the runner profile.
// Config fields: organization_id (required), execution_id (required),
// test_node_id (required), language, test_type, code_node_id,
// canvas_id, max_retries.
func (h *DSLHandler) runTests(ctx context.Context, req dsl.Request) (map[string]any, error) {
	if h.testRunRepo == nil {
		return nil, &dsl.ActionError{Msg: "test run repo not configured"}
	}
	orgID, err := dslUUID(req.Config, "organization_id")
	if err != nil {
		return nil, err
	}
	execID, err := dslUUID(req.Config, "execution_id")
	if err != nil {
		return nil, err
	}
	testNodeID, err := dslUUID(req.Config, "test_node_id")
	if err != nil {
		return nil, err
	}
	language, _ := req.Config["language"].(string)
	testType, _ := req.Config["test_type"].(string)
	maxRetries := 0
	if v, ok := req.Config["max_retries"].(float64); ok {
		maxRetries = int(v)
	}
	if maxRetries <= 0 {
		maxRetries = domain.DefaultMaxTestRetries
	}
	run := &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		ExecutionID:    execID,
		TestNodeID:     testNodeID,
		Language:       domain.Language(language),
		TestType:       domain.TestType(testType).Normalize(),
		MaxRetries:     maxRetries,
		Status:         domain.TestRunStatusRunning,
	}
	if codeNodeID, err := dslOptionalUUID(req.Config, "code_node_id"); err == nil && codeNodeID != uuid.Nil {
		run.CodeNodeID = &codeNodeID
	}
	if canvasID, err := dslOptionalUUID(req.Config, "canvas_id"); err == nil && canvasID != uuid.Nil {
		run.CanvasID = &canvasID
	}
	if err := h.testRunRepo.Create(ctx, run); err != nil {
		return nil, err
	}
	profile, matched := usecase.ResolveTestRunner(run.Language, run.TestType)
	return map[string]any{
		"test_run_id":    run.ID.String(),
		"execution_id":   execID.String(),
		"status":         "queued",
		"runner_matched": matched,
		"command":        profile.Command,
		"vm_profile":     profile.VMProfile,
		"network":        profile.Network,
	}, nil
}

// buildArtifact starts a HermeticBuild row. Config:
//   - organization_id (required, uuid)
//   - pipeline_run_id (required, uuid)
//   - created_by (optional, uuid)
//   - inputs (required, map[string]string of pinned inputs —
//     commit_sha, base_image_digest, lockfile_hash, tool_versions, …)
//
// When the same input digest has been built before the cached row is
// returned (cache_hit=true) and the worker can short-circuit to the
// existing artifact ref. Otherwise a new in-progress row is persisted;
// a follow-up CompleteBuild call fills in the output digest.
func (h *DSLHandler) buildArtifact(ctx context.Context, req dsl.Request) (map[string]any, error) {
	if h.hermeticUC == nil {
		return nil, &dsl.ActionError{Msg: "hermetic build use case not configured"}
	}
	orgID, err := dslUUID(req.Config, "organization_id")
	if err != nil {
		return nil, err
	}
	pipelineRunID, err := dslUUID(req.Config, "pipeline_run_id")
	if err != nil {
		return nil, err
	}
	createdBy, _ := dslOptionalUUID(req.Config, "created_by")

	inputs, err := dslStringMap(req.Config, "inputs")
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, &dsl.ActionError{Msg: "inputs is required and must be a map of pinned inputs"}
	}

	build, cacheHit, err := h.hermeticUC.StartBuild(ctx, usecase.StartBuildInput{
		OrganizationID: orgID,
		PipelineRunID:  pipelineRunID,
		CreatedBy:      createdBy,
		Inputs:         inputs,
	})
	if err != nil {
		return nil, err
	}
	status := "queued"
	if cacheHit {
		status = "cache_hit"
	}
	return map[string]any{
		"build_id":      build.ID.String(),
		"input_digest":  build.InputDigest,
		"output_digest": build.OutputDigest,
		"artifact_ref":  build.ArtifactRef,
		"reproducible":  build.Reproducible,
		"cache_hit":     cacheHit,
		"status":        status,
	}, nil
}

// dslStringMap reads a map[string]string out of a JSON config field.
// JSON unmarshals objects as map[string]any, so we coerce per-entry.
// Non-string values return an ActionError with the offending key so the
// DSL author can find the typo in seconds instead of guessing.
func dslStringMap(m map[string]any, key string) (map[string]string, error) {
	raw, ok := m[key].(map[string]any)
	if !ok {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, &dsl.ActionError{Msg: key + "." + k + " must be a string"}
		}
		out[k] = s
	}
	return out, nil
}

// dslUUID pulls a required UUID field from a JSON config map and
// returns an ActionError on any failure so foundry sees a clear
// "field X is required" / "invalid field X" message.
func dslUUID(m map[string]any, key string) (uuid.UUID, error) {
	s, _ := m[key].(string)
	if s == "" {
		return uuid.Nil, &dsl.ActionError{Msg: key + " is required"}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, &dsl.ActionError{Msg: "invalid " + key}
	}
	return id, nil
}

// dslOptionalUUID parses an optional UUID; missing returns uuid.Nil
// without error, invalid returns an error so callers can choose.
func dslOptionalUUID(m map[string]any, key string) (uuid.UUID, error) {
	s, _ := m[key].(string)
	if s == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(s)
}
