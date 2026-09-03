package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sentiae/platform-kit/nodeabi"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ---------------------------------------------------------------------------
// a graph in memory: five small repositories over one shared state, so a test
// asserts on the ROWS the engine wrote rather than on calls it made.
// ---------------------------------------------------------------------------

type engineStore struct {
	mu        sync.Mutex
	graph     *domain.GraphDefinition
	nodes     []domain.GraphNode
	edges     []domain.GraphEdge
	execs     map[uuid.UUID]*domain.GraphExecution
	nodeExecs []domain.NodeExecution
	events    []string
}

func newEngineStore() *engineStore {
	return &engineStore{execs: map[uuid.UUID]*domain.GraphExecution{}}
}

func (s *engineStore) execution(id uuid.UUID) domain.GraphExecution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.execs[id]
}

func (s *engineStore) nodeRows() []domain.NodeExecution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.NodeExecution(nil), s.nodeExecs...)
}

func (s *engineStore) rowFor(t *testing.T, name string) domain.NodeExecution {
	t.Helper()
	for _, row := range s.nodeRows() {
		if row.NodeName == name {
			return row
		}
	}
	t.Fatalf("no node execution row for %q", name)
	return domain.NodeExecution{}
}

func (s *engineStore) eventNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type engineDefRepo struct{ s *engineStore }

func (r engineDefRepo) Create(context.Context, *domain.GraphDefinition) error { return nil }
func (r engineDefRepo) Update(context.Context, *domain.GraphDefinition) error { return nil }
func (r engineDefRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphDefinition, error) {
	if r.s.graph == nil {
		return nil, domain.ErrGraphNotFound
	}
	return r.s.graph, nil
}
func (r engineDefRepo) FindByOrganization(context.Context, uuid.UUID, int, int) ([]domain.GraphDefinition, int64, error) {
	return nil, 0, nil
}
func (r engineDefRepo) FindActive(context.Context, uuid.UUID) ([]domain.GraphDefinition, error) {
	return nil, nil
}
func (r engineDefRepo) Delete(context.Context, uuid.UUID) error { return nil }

type engineNodeRepo struct{ s *engineStore }

func (r engineNodeRepo) Create(context.Context, *domain.GraphNode) error       { return nil }
func (r engineNodeRepo) CreateBatch(context.Context, []domain.GraphNode) error { return nil }
func (r engineNodeRepo) Update(context.Context, *domain.GraphNode) error       { return nil }
func (r engineNodeRepo) DeleteByGraph(context.Context, uuid.UUID) error        { return nil }
func (r engineNodeRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphNode, error) {
	return nil, domain.ErrGraphNodeNotFound
}
func (r engineNodeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphNode, error) {
	return append([]domain.GraphNode(nil), r.s.nodes...), nil
}

type engineEdgeRepo struct{ s *engineStore }

func (r engineEdgeRepo) Create(context.Context, *domain.GraphEdge) error       { return nil }
func (r engineEdgeRepo) CreateBatch(context.Context, []domain.GraphEdge) error { return nil }
func (r engineEdgeRepo) DeleteByGraph(context.Context, uuid.UUID) error        { return nil }
func (r engineEdgeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphEdge, error) {
	return append([]domain.GraphEdge(nil), r.s.edges...), nil
}

type engineExecRepo struct{ s *engineStore }

func (r engineExecRepo) Create(_ context.Context, exec *domain.GraphExecution) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	copied := *exec
	r.s.execs[exec.ID] = &copied
	return nil
}
func (r engineExecRepo) Update(_ context.Context, exec *domain.GraphExecution) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	copied := *exec
	r.s.execs[exec.ID] = &copied
	return nil
}
func (r engineExecRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.GraphExecution, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	exec, ok := r.s.execs[id]
	if !ok {
		return nil, domain.ErrGraphExecutionNotFound
	}
	return exec, nil
}
func (r engineExecRepo) FindByGraph(context.Context, uuid.UUID, int, int) ([]domain.GraphExecution, int64, error) {
	return nil, 0, nil
}
func (r engineExecRepo) FindPending(context.Context, int) ([]domain.GraphExecution, error) {
	return nil, nil
}

type engineNodeExecRepo struct{ s *engineStore }

func (r engineNodeExecRepo) Create(_ context.Context, exec *domain.NodeExecution) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.nodeExecs = append(r.s.nodeExecs, *exec)
	return nil
}
func (r engineNodeExecRepo) Update(_ context.Context, exec *domain.NodeExecution) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for i := range r.s.nodeExecs {
		if r.s.nodeExecs[i].ID == exec.ID {
			r.s.nodeExecs[i] = *exec
			return nil
		}
	}
	r.s.nodeExecs = append(r.s.nodeExecs, *exec)
	return nil
}
func (r engineNodeExecRepo) FindByID(context.Context, uuid.UUID) (*domain.NodeExecution, error) {
	return nil, domain.ErrNodeExecutionNotFound
}
func (r engineNodeExecRepo) FindByGraphExecution(context.Context, uuid.UUID) ([]domain.NodeExecution, error) {
	return nil, nil
}

type recordingPublisher struct{ s *engineStore }

func (p recordingPublisher) Publish(_ context.Context, eventType, _ string, _ any) error {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	p.s.events = append(p.s.events, eventType)
	return nil
}
func (p recordingPublisher) Close() error { return nil }

// ---------------------------------------------------------------------------
// graph fixtures
// ---------------------------------------------------------------------------

type nodeSpec struct {
	name    string
	role    domain.NodeRole
	inputs  []domain.PortSpec
	outputs []domain.PortSpec
	config  domain.JSONMap
	secrets []domain.SecretSpec
	egress  []string
}

func buildNodes(t *testing.T, specs ...nodeSpec) []domain.GraphNode {
	t.Helper()
	nodes := make([]domain.GraphNode, 0, len(specs))
	for i, spec := range specs {
		ref, err := domain.NewNodeRef("@acme/"+spec.name, "1.0.0", "go",
			"10.0.10.20:8078/acme/"+spec.name+".node:1.0.0-go", "sha256:"+strings.Repeat("bc", 32))
		if err != nil {
			t.Fatalf("NewNodeRef(%s): %v", spec.name, err)
		}
		ports, err := domain.NewPortSpecs(spec.inputs, spec.outputs)
		if err != nil {
			t.Fatalf("NewPortSpecs(%s): %v", spec.name, err)
		}
		nodes = append(nodes, domain.GraphNode{
			ID:        uuid.New(),
			GraphID:   uuid.Nil,
			NodeType:  domain.GraphNodeTypeBundle,
			Name:      spec.name,
			Config:    spec.config,
			Resources: domain.ResourceLimit{MemoryMB: 64, TimeoutSec: 5},
			SortOrder: i,
			NodeRef:   ref,
			Ports:     ports,
			Role:      spec.role,
			Secrets:   spec.secrets,
			Egress:    spec.egress,
		})
	}
	return nodes
}

func edgeBetween(nodes []domain.GraphNode, from, fromPort, to, toPort string) domain.GraphEdge {
	find := func(name string) uuid.UUID {
		for _, n := range nodes {
			if n.Name == name {
				return n.ID
			}
		}
		return uuid.Nil
	}
	return domain.GraphEdge{
		ID: uuid.New(), SourceNodeID: find(from), TargetNodeID: find(to),
		SourcePort: fromPort, TargetPort: toPort,
	}
}

// triggerRespondGraph is the shape every flow has: something starts it,
// something does work, something answers.
func triggerRespondGraph(t *testing.T) ([]domain.GraphNode, []domain.GraphEdge) {
	t.Helper()
	nodes := buildNodes(t,
		nodeSpec{
			name: "intake", role: domain.NodeRoleTrigger,
			outputs: []domain.PortSpec{{Name: "body"}, {Name: "headers"}, {Name: "query"}, {Name: "method"}, {Name: "path"}},
			config:  domain.JSONMap{"method": "POST", "path": "/phase-4"},
		},
		nodeSpec{
			name:    "greet",
			inputs:  []domain.PortSpec{{Name: "body", Required: true}},
			outputs: []domain.PortSpec{{Name: "out", Required: true}},
		},
		nodeSpec{
			name: "reply", role: domain.NodeRoleRespond,
			inputs:  []domain.PortSpec{{Name: "body", Required: true}},
			outputs: []domain.PortSpec{{Name: "response", Required: true}},
		},
	)
	edges := []domain.GraphEdge{
		edgeBetween(nodes, "intake", "body", "greet", "body"),
		edgeBetween(nodes, "greet", "out", "reply", "body"),
	}
	return nodes, edges
}

// resultsByNode dispatches a canned RESULT per node slug, read off the CALL the
// runtime actually produced.
func resultsByNode(t *testing.T, results map[string]BundleRunResult) func(BundleLaunch) (BundleRunResult, error) {
	t.Helper()
	return func(launch BundleLaunch) (BundleRunResult, error) {
		call, verr := nodeabi.ValidateCall(launch.Call)
		if verr != nil {
			t.Errorf("invalid CALL: %s: %s", verr.Code, verr.Message)
			return BundleRunResult{}, errors.New(verr.Message)
		}
		res, ok := results[call.Invocation.Node]
		if !ok {
			t.Errorf("no canned result for node %q", call.Invocation.Node)
			return BundleRunResult{}, errors.New("no result")
		}
		return res, nil
	}
}

type engineHarness struct {
	engine   *GraphExecutionEngine
	store    *engineStore
	runner   *fakeBundleRunner
	sidecars *fakeSidecarManager
	secrets  *fakeSecretSource
}

func newEngineHarness(t *testing.T, nodes []domain.GraphNode, edges []domain.GraphEdge) *engineHarness {
	t.Helper()
	store := newEngineStore()
	store.graph = &domain.GraphDefinition{
		ID: uuid.New(), OrganizationID: uuid.New(), Name: "phase 4",
		Status: domain.GraphStatusActive,
	}
	store.nodes = nodes
	store.edges = edges

	runner := &fakeBundleRunner{}
	sidecars := &fakeSidecarManager{network: "sentiae-inv-test"}
	secrets := &fakeSecretSource{}
	invoker := newTestInvoker(t, runner, sidecars, secrets)

	engine := NewGraphExecutionEngine(
		engineDefRepo{store}, engineNodeRepo{store}, engineEdgeRepo{store},
		engineExecRepo{store}, engineNodeExecRepo{store},
		recordingPublisher{store}, invoker, sidecars,
	)
	return &engineHarness{engine: engine, store: store, runner: runner, sidecars: sidecars, secrets: secrets}
}

// runToTerminal starts a run and waits for it to settle. The engine runs the
// plan in its own goroutine, so a test that read the row immediately would be
// asserting on "pending" every time.
func (h *engineHarness) runToTerminal(t *testing.T, input domain.JSONMap, secretToken, environment string) domain.GraphExecution {
	t.Helper()
	exec, err := h.engine.ExecuteGraph(context.Background(), h.store.graph.ID, h.store.graph.OrganizationID,
		uuid.New(), input, false, secretToken, environment)
	if err != nil {
		t.Fatalf("ExecuteGraph: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := h.store.execution(exec.ID)
		if got.IsTerminal() {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a terminal status", exec.ID)
	return domain.GraphExecution{}
}

// waitFor polls a condition a run's own goroutine satisfies.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func responseResult(status int) BundleRunResult {
	return okResult(map[string]any{"response": map[string]any{"status": status, "body": map[string]any{"ok": true}}})
}

// T2.8 — the whole shape: the trigger receives the request object and nothing
// else, work flows along the wires, the flow answers with the respond node's
// response, and the run's resources are swept and its token revoked at the end.
//
// Control: spread the run's input into every node's inputs (as the interpreter
// did) instead of handing it only to the trigger ⇒ greet's recorded input picks
// up `body` from the root document and the "only the wire feeds it" assertion
// fails.
func TestRunPlan_TriggerRespondSingle(t *testing.T) {
	nodes, edges := triggerRespondGraph(t)
	nodes[1].Secrets = []domain.SecretSpec{{Name: "greeting_suffix"}}
	h := newEngineHarness(t, nodes, edges)
	h.secrets.answers = map[string]resolvedSecret{"greeting_suffix": {value: " ::x::", found: true}}
	h.runner.result = resultsByNode(t, map[string]BundleRunResult{
		"intake": okResult(map[string]any{"body": map[string]any{"name": "x"}}),
		"greet":  okResult(map[string]any{"out": map[string]any{"greeting": "hello x ::x::"}}),
		"reply":  responseResult(200),
	})

	// The run document carries a key NO wire delivers: if the engine ever
	// spread the root input across the flow, it would appear in greet's row.
	exec := h.runToTerminal(t, domain.JSONMap{
		"body":      map[string]any{"name": "x"},
		"root_only": "never-wired",
	}, "handed-token", "preview")

	if exec.Status != domain.GraphExecCompleted {
		t.Fatalf("status = %s (error %q), want completed", exec.Status, exec.Error)
	}
	if exec.CompletedNodes != 3 {
		t.Fatalf("completed nodes = %d, want 3", exec.CompletedNodes)
	}
	status, ok := exec.Output["status"]
	if !ok || int(status.(float64)) != 200 {
		t.Fatalf("run output = %v, want the respond node's response object", exec.Output)
	}

	// The trigger's ONE input is the request; the run's input document is never
	// spread across the flow.
	intake := h.store.rowFor(t, "intake")
	request, ok := intake.Input["request"].(map[string]any)
	if !ok || len(intake.Input) != 1 {
		t.Fatalf("trigger input = %v, want exactly {request}", intake.Input)
	}
	if request["method"] != "POST" || request["path"] != "/phase-4" {
		t.Fatalf("request = %v, want the configured method and path", request)
	}
	if body, ok := request["body"].(map[string]any); !ok || body["name"] != "x" {
		t.Fatalf("request.body = %v, want the run input's body", request["body"])
	}

	greet := h.store.rowFor(t, "greet")
	if len(greet.Input) != 1 || greet.Input["body"] == nil {
		t.Fatalf("greet input = %v, want only what its wire delivered", greet.Input)
	}
	if _, leaked := greet.Input["root_only"]; leaked {
		t.Fatalf("the run's input document leaked into a downstream node: %v", greet.Input)
	}
	if greet.NodeRef == nil || greet.NodeRef.Pin() != "@acme/greet@1.0.0" {
		t.Fatalf("greet row records node_ref %v, want the pin it ran", greet.NodeRef)
	}
	// A row never records a handle, a secret value or the CALL.
	encoded, err := json.Marshal(greet.Input)
	if err != nil {
		t.Fatalf("marshal greet input: %v", err)
	}
	if strings.Contains(string(encoded), nodeabi.HandlePrefix) || strings.Contains(string(encoded), "::x::") {
		t.Fatalf("a node execution row carries secret material: %s", encoded)
	}

	// Terminal cleanup: the run's labelled resources are swept and the handed
	// token is given back exactly once. It runs AFTER the row settles, so it is
	// waited for rather than read straight away.
	waitFor(t, "the run to be swept and its token revoked", func() bool {
		return len(h.sidecars.sweptRuns()) == 1 && len(h.secrets.revokedTokens()) == 1
	})
	if swept := h.sidecars.sweptRuns(); swept[0] != exec.ID {
		t.Fatalf("swept %v, want run %s", swept, exec.ID)
	}
	if revoked := h.secrets.revokedTokens(); revoked[0] != "handed-token" {
		t.Fatalf("revoked = %v, want the handed token", revoked)
	}
}

// T2.9 — the first failure ends the run, names the node, and stops the wave:
// nothing downstream of a failure is launched.
//
// Control: return nil from runWave on a node failure (continue the loop) ⇒
// reply runs and the "downstream never launched" assertion fails.
func TestRunPlan_FailFast(t *testing.T) {
	nodes, edges := triggerRespondGraph(t)
	h := newEngineHarness(t, nodes, edges)
	h.runner.result = resultsByNode(t, map[string]BundleRunResult{
		"intake": okResult(map[string]any{"body": map[string]any{"name": "x"}}),
		"greet":  {ExitCode: 3, Stderr: "first line\nboom"},
		"reply":  responseResult(200),
	})

	exec := h.runToTerminal(t, domain.JSONMap{}, "", "")

	if exec.Status != domain.GraphExecFailed {
		t.Fatalf("status = %s, want failed", exec.Status)
	}
	want := `node "greet" failed: crash: exit status 3: boom`
	if exec.Error != want {
		t.Fatalf("error = %q, want %q", exec.Error, want)
	}
	for _, launch := range h.runner.launch {
		call, verr := nodeabi.ValidateCall(launch.Call)
		if verr != nil {
			t.Fatalf("invalid CALL: %s", verr.Message)
		}
		if call.Invocation.Node == "reply" {
			t.Fatal("a node downstream of the failure was launched")
		}
	}
	if row := h.store.rowFor(t, "greet"); row.Status != domain.GraphExecFailed {
		t.Fatalf("greet row status = %s, want failed", row.Status)
	}
}

// T2.10 — how a flow answers is flowlang's verdict, and the ONE output the
// runtime reads for itself is checked before it is handed to a caller.
//
// Control (response_invalid): coerce a non-object response into
// {"response": <value>} instead of failing ⇒ the scalar row completes and the
// assertion that it failed goes green wrongly.
func TestRunPlan_ResultCases(t *testing.T) {
	scalarResponse := okResult(map[string]any{"response": "OK"})
	statuslessResponse := okResult(map[string]any{"response": map[string]any{"body": "hi"}})

	tests := []struct {
		name       string
		respond    int // how many respond nodes
		unfed      bool
		result     BundleRunResult
		wantStatus domain.GraphExecutionStatus
		wantErr    string
		wantOutput bool
	}{
		{name: "fire and forget", respond: 0, wantStatus: domain.GraphExecCompleted},
		{name: "single response", respond: 1, result: responseResult(201), wantStatus: domain.GraphExecCompleted, wantOutput: true},
		{name: "multiple responses", respond: 2, result: responseResult(200), wantStatus: domain.GraphExecFailed, wantErr: "multiple_responses"},
		// The respond node's `response` is a REQUIRED output, so "answered
		// nothing" cannot be a RESULT the ABI accepts — it is a respond node
		// that never ran, which is what the trigger firing nothing produces.
		{name: "no response", respond: 1, unfed: true, wantStatus: domain.GraphExecFailed, wantErr: "no_response"},
		{name: "response is not an object", respond: 1, result: scalarResponse, wantStatus: domain.GraphExecFailed,
			wantErr: `node "reply" failed: ` + responseInvalidReason},
		{name: "response has no status", respond: 1, result: statuslessResponse, wantStatus: domain.GraphExecFailed,
			wantErr: `node "reply" failed: ` + responseInvalidReason},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := []nodeSpec{{
				name: "intake", role: domain.NodeRoleTrigger,
				outputs: []domain.PortSpec{{Name: "body"}},
				config:  domain.JSONMap{"method": "POST", "path": "/p"},
			}}
			intakeResult := okResult(map[string]any{"body": map[string]any{}})
			if tt.unfed {
				intakeResult = okResult(map[string]any{})
			}
			results := map[string]BundleRunResult{"intake": intakeResult}
			var edges []domain.GraphEdge
			for i := 0; i < tt.respond; i++ {
				name := "reply"
				if i > 0 {
					name = "reply2"
				}
				specs = append(specs, nodeSpec{
					name: name, role: domain.NodeRoleRespond,
					inputs:  []domain.PortSpec{{Name: "body", Required: true}},
					outputs: []domain.PortSpec{{Name: "response", Required: true}},
				})
				results[name] = tt.result
			}
			nodes := buildNodes(t, specs...)
			for _, spec := range specs[1:] {
				edges = append(edges, edgeBetween(nodes, "intake", "body", spec.name, "body"))
			}

			h := newEngineHarness(t, nodes, edges)
			h.runner.result = resultsByNode(t, results)
			exec := h.runToTerminal(t, domain.JSONMap{}, "", "")

			if exec.Status != tt.wantStatus {
				t.Fatalf("status = %s (error %q), want %s", exec.Status, exec.Error, tt.wantStatus)
			}
			if tt.wantErr != "" && exec.Error != tt.wantErr {
				t.Fatalf("error = %q, want %q", exec.Error, tt.wantErr)
			}
			if tt.wantStatus == domain.GraphExecCompleted {
				if tt.wantOutput {
					if int(exec.Output["status"].(float64)) != 201 {
						t.Fatalf("output = %v, want the response object", exec.Output)
					}
				} else if len(exec.Output) != 0 {
					t.Fatalf("a fire-and-forget flow answered with %v, want {}", exec.Output)
				}
			}
		})
	}
}

// TestRunPlan_SkippedBranch — a node that is ready but whose required input
// never fired is RECORDED skipped, not left absent (§3.6.8).
//
// Control: drop the skip recording and only settle the status ⇒ the row and the
// event both disappear.
func TestRunPlan_SkippedBranch(t *testing.T) {
	nodes := buildNodes(t,
		nodeSpec{
			name: "intake", role: domain.NodeRoleTrigger,
			outputs: []domain.PortSpec{{Name: "body"}, {Name: "alt"}},
			config:  domain.JSONMap{"method": "POST", "path": "/p"},
		},
		nodeSpec{
			name:    "branch",
			inputs:  []domain.PortSpec{{Name: "body", Required: true}},
			outputs: []domain.PortSpec{{Name: "out", Required: true}},
		},
	)
	edges := []domain.GraphEdge{edgeBetween(nodes, "intake", "alt", "branch", "body")}

	h := newEngineHarness(t, nodes, edges)
	// The trigger fires `body` only; `alt` — the wire that feeds branch — does not.
	h.runner.result = resultsByNode(t, map[string]BundleRunResult{
		"intake": okResult(map[string]any{"body": map[string]any{}}),
	})

	exec := h.runToTerminal(t, domain.JSONMap{}, "", "")
	if exec.Status != domain.GraphExecCompleted {
		t.Fatalf("status = %s (error %q), want completed", exec.Status, exec.Error)
	}
	row := h.store.rowFor(t, "branch")
	if row.Status != domain.GraphExecSkipped {
		t.Fatalf("branch row status = %s, want skipped", row.Status)
	}
	var sawSkipEvent bool
	for _, name := range h.store.eventNames() {
		if name == EventNodeExecSkipped {
			sawSkipEvent = true
		}
	}
	if !sawSkipEvent {
		t.Fatalf("events = %v, want a %s", h.store.eventNames(), EventNodeExecSkipped)
	}
}

// T2.11 — every refusal ExecuteGraph owns, and each one refuses BEFORE a run
// row exists: a refused request must leave nothing behind to explain.
//
// Control (credentials): delete the checkRunCredentials call ⇒ all five
// token/environment rows run the graph instead of refusing.
// Control (legacy): delete the NodeRef nil check in buildRunPlan ⇒ the legacy
// row reaches PlanFromWire and panics on a nil pin.
func TestExecuteGraph_Refusals(t *testing.T) {
	secretSpec := []domain.SecretSpec{{Name: "greeting_suffix"}}

	tests := []struct {
		name    string
		mutate  func(nodes []domain.GraphNode) []domain.GraphNode
		draft   bool
		token   string
		env     string
		wantErr error
	}{
		{name: "legacy row without a pin", wantErr: domain.ErrLegacyGraph,
			mutate: func(n []domain.GraphNode) []domain.GraphNode { n[1].NodeRef = nil; return n }},
		{name: "carried order is not topological", wantErr: domain.ErrPlanInvalid,
			mutate: func(n []domain.GraphNode) []domain.GraphNode {
				n[0].SortOrder, n[1].SortOrder = n[1].SortOrder, n[0].SortOrder
				return n
			}},
		{name: "graph is not active", draft: true, wantErr: domain.ErrGraphNotActive},
		{name: "secrets declared without a token", wantErr: domain.ErrSecretTokenRequired,
			mutate: func(n []domain.GraphNode) []domain.GraphNode { n[1].Secrets = secretSpec; return n }},
		{name: "token without secrets", token: "handed-token", env: "preview", wantErr: domain.ErrSecretTokenUnexpected},
		{name: "environment without secrets", env: "preview", wantErr: domain.ErrEnvironmentUnexpected},
		{name: "token without an environment", token: "handed-token", wantErr: domain.ErrEnvironmentRequired,
			mutate: func(n []domain.GraphNode) []domain.GraphNode { n[1].Secrets = secretSpec; return n }},
		{name: "environment is not one of the three", token: "handed-token", env: "staging", wantErr: domain.ErrEnvironmentInvalid,
			mutate: func(n []domain.GraphNode) []domain.GraphNode { n[1].Secrets = secretSpec; return n }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes, edges := triggerRespondGraph(t)
			if tt.mutate != nil {
				nodes = tt.mutate(nodes)
			}
			h := newEngineHarness(t, nodes, edges)
			if tt.draft {
				h.store.graph.Status = domain.GraphStatusDraft
			}

			_, err := h.engine.ExecuteGraph(context.Background(), h.store.graph.ID,
				h.store.graph.OrganizationID, uuid.New(), domain.JSONMap{}, false, tt.token, tt.env)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ExecuteGraph error = %v, want %v", err, tt.wantErr)
			}
			h.store.mu.Lock()
			runs := len(h.store.execs)
			h.store.mu.Unlock()
			if runs != 0 {
				t.Fatalf("a refused request created %d run row(s)", runs)
			}
			if len(h.runner.launch) != 0 {
				t.Fatalf("a refused request launched %d bundle(s)", len(h.runner.launch))
			}
		})
	}
}

// T2.12 — a promoted wire delivers into the target's CONFIG, never into one of
// its inputs. Lowering already rewrote the port to `config.<key>`, so the
// runtime carries no promotion table of its own.
//
// Control: drop the flowlang.PromotedKey branch in nodeInvocation ⇒ the value
// arrives as an input named "config.url" and both assertions fail.
func TestRunPlan_PromotedEdge(t *testing.T) {
	nodes := buildNodes(t,
		nodeSpec{
			name: "intake", role: domain.NodeRoleTrigger,
			outputs: []domain.PortSpec{{Name: "body"}},
			config:  domain.JSONMap{"method": "POST", "path": "/p"},
		},
		nodeSpec{
			name:    "fetch",
			outputs: []domain.PortSpec{{Name: "status_code", Required: true}},
			config:  domain.JSONMap{"url": "https://example.test/default"},
		},
	)
	edges := []domain.GraphEdge{edgeBetween(nodes, "intake", "body", "fetch", "config.url")}

	h := newEngineHarness(t, nodes, edges)
	h.runner.result = resultsByNode(t, map[string]BundleRunResult{
		"intake": okResult(map[string]any{"body": "https://example.test/promoted"}),
		"fetch":  okResult(map[string]any{"status_code": 200}),
	})

	exec := h.runToTerminal(t, domain.JSONMap{}, "", "")
	if exec.Status != domain.GraphExecCompleted {
		t.Fatalf("status = %s (error %q), want completed", exec.Status, exec.Error)
	}

	var fetchCall *nodeabi.Call
	for _, launch := range h.runner.launch {
		call, verr := nodeabi.ValidateCall(launch.Call)
		if verr != nil {
			t.Fatalf("invalid CALL: %s", verr.Message)
		}
		if call.Invocation.Node == "fetch" {
			fetchCall = call
		}
	}
	if fetchCall == nil {
		t.Fatal("fetch was never launched")
	}
	if got := string(fetchCall.Config["url"]); got != `"https://example.test/promoted"` {
		t.Fatalf("config.url = %s, want the promoted value", got)
	}
	if len(fetchCall.Inputs) != 0 {
		t.Fatalf("a promoted wire landed in inputs: %v", fetchCall.Inputs)
	}
}

// T2.13 — the debug stepper is retired, and it says so instead of doing
// nothing. Its three entry points are the whole stepper: create and inspect
// still work, stepping does not.
//
// Control: restore any of the three bodies ⇒ that row stops refusing.
func TestDebug_Retired(t *testing.T) {
	svc := NewGraphDebugService(nil, nil, nil, nil, nil, nil, nil, nil)
	id := uuid.New()

	if err := svc.StartSession(context.Background(), id); !errors.Is(err, domain.ErrGraphDebugRetired) {
		t.Fatalf("StartSession error = %v, want ErrGraphDebugRetired", err)
	}
	if _, err := svc.StepOver(context.Background(), id); !errors.Is(err, domain.ErrGraphDebugRetired) {
		t.Fatalf("StepOver error = %v, want ErrGraphDebugRetired", err)
	}
	if err := svc.Continue(context.Background(), id); !errors.Is(err, domain.ErrGraphDebugRetired) {
		t.Fatalf("Continue error = %v, want ErrGraphDebugRetired", err)
	}
}

// D-7 — a handed token that cannot be revoked is COUNTED and logged at Error,
// and the run it belongs to still completes.
//
// The defect this pins: revoke-self answered 403 on every run for the life of
// the token role, the only signal was an unwatched WARN, and the `failed` series
// did not exist because a promauto counter with no observation exports nothing —
// so "no failures" and "never instrumented" were the same reading.
//
// Both halves are asserted deliberately:
//   - failed +1: the fault is visible as a NUMBER, not only as a log line.
//   - status still completed: cleanup runs after the terminal transition, and a
//     revoke failure must not turn a finished run into a failed one (a retry
//     would mint another token that also cannot be revoked).
//
// Control: delete the recordSecretTokenRevocation(revokeOutcomeFailed) call in
// cleanupRun ⇒ the failed counter never moves and this fails.
func TestCleanupRun_RevokeFailureIsCountedAndTheRunStillCompletes(t *testing.T) {
	nodes, edges := triggerRespondGraph(t)
	// The engine refuses a handed token for a graph that declares no secrets, so
	// the run that owns a token is the run that has one to resolve.
	nodes[1].Secrets = []domain.SecretSpec{{Name: "greeting_suffix"}}
	h := newEngineHarness(t, nodes, edges)
	h.secrets.answers = map[string]resolvedSecret{"greeting_suffix": {value: " ::x::", found: true}}
	h.secrets.revokeErr = errors.New("revoke-self: Error making API request. Code: 403. * permission denied")
	h.runner.result = resultsByNode(t, map[string]BundleRunResult{
		"intake": okResult(map[string]any{"body": map[string]any{"name": "x"}}),
		"greet":  okResult(map[string]any{"out": map[string]any{"greeting": "hello x"}}),
		"reply":  responseResult(200),
	})

	// Read the counters BEFORE the run: they are process-global on the default
	// registry, so only the delta this run produced is meaningful.
	failedBefore := testutil.ToFloat64(secretTokenRevocations.WithLabelValues(revokeOutcomeFailed))
	okBefore := testutil.ToFloat64(secretTokenRevocations.WithLabelValues(revokeOutcomeOK))

	exec := h.runToTerminal(t, domain.JSONMap{"body": map[string]any{"name": "x"}}, "handed-token", "preview")

	if exec.Status != domain.GraphExecCompleted {
		t.Fatalf("status = %s (error %q), want completed — a revoke failure must not fail the run",
			exec.Status, exec.Error)
	}

	// cleanupRun is deferred, so it lands after the terminal row is written.
	waitFor(t, "the handed token to be offered for revocation", func() bool {
		return len(h.secrets.revokedTokens()) == 1
	})
	waitFor(t, "the failed revocation to be counted", func() bool {
		return testutil.ToFloat64(secretTokenRevocations.WithLabelValues(revokeOutcomeFailed)) == failedBefore+1
	})

	if got := testutil.ToFloat64(secretTokenRevocations.WithLabelValues(revokeOutcomeOK)); got != okBefore {
		t.Fatalf("revocations{outcome=ok} = %v, want %v — a refused revocation must not count as ok", got, okBefore)
	}
	if revoked := h.secrets.revokedTokens(); revoked[0] != "handed-token" {
		t.Fatalf("revoked %v, want the token handed to this run", revoked)
	}
}
