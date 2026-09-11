package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/nodeabi"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ---------------------------------------------------------------------------
// fakes: the three ports an invocation touches, each recording exactly what it
// was asked to do so a test can assert the ORDER and the CONTENT, not just the
// outcome.
// ---------------------------------------------------------------------------

type fakeBundleRunner struct {
	mu       sync.Mutex
	pulled   []string
	launch   []BundleLaunch
	result   func(BundleLaunch) (BundleRunResult, error)
	probeErr error
}

func (f *fakeBundleRunner) Probe(context.Context) error { return f.probeErr }

func (f *fakeBundleRunner) Pull(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, image)
	return nil
}

func (f *fakeBundleRunner) Run(_ context.Context, launch BundleLaunch) (BundleRunResult, error) {
	f.mu.Lock()
	f.launch = append(f.launch, launch)
	fn := f.result
	f.mu.Unlock()
	if fn == nil {
		// The default is a RESULT the hello fixture's manifest accepts: its
		// `out` output is required, so an empty document would fail validation
		// and every test would be measuring that instead of what it asserts.
		return okResult(map[string]any{"out": map[string]any{"greeting": "hello x"}}), nil
	}
	return fn(launch)
}

func (f *fakeBundleRunner) lastLaunch(t *testing.T) BundleLaunch {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launch) == 0 {
		t.Fatal("no bundle was launched")
	}
	return f.launch[len(f.launch)-1]
}

type fakeSidecarManager struct {
	mu      sync.Mutex
	opens   []SidecarOpen
	closes  []string
	sweeps  []uuid.UUID
	network string
	openErr error
}

func (f *fakeSidecarManager) Probe(context.Context) error { return nil }

func (f *fakeSidecarManager) Open(_ context.Context, in SidecarOpen) (Sidecar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, in)
	if f.openErr != nil {
		return Sidecar{}, f.openErr
	}
	sc := Sidecar{BrokerSubpath: in.InvocationID}
	if in.Binding.Egress != nil {
		sc.Network = f.network
		sc.ProxyURL = proxyURL
	}
	return sc, nil
}

func (f *fakeSidecarManager) Close(_ context.Context, invocationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, invocationID)
	return nil
}

func (f *fakeSidecarManager) SweepRun(_ context.Context, runID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, runID)
	return nil
}

func (f *fakeSidecarManager) SweepAll(context.Context) (int, int, error) { return 0, 0, nil }

// sweptRuns / closedInvocations read what a run's own goroutine recorded, so
// they take the same lock the recording side does.
func (f *fakeSidecarManager) sweptRuns() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.sweeps...)
}

func (f *fakeSidecarManager) closedInvocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closes...)
}

type resolvedSecret struct {
	value string
	found bool
	err   error
}

type fakeSecretSource struct {
	mu      sync.Mutex
	answers map[string]resolvedSecret
	asked   []string
	revoked []string
	// revokeErr is what Revoke answers after recording the token — the live
	// D-7 shape, where revoke-self is refused but the token was still handed back.
	revokeErr error
	lastEnv   string
	lastTok   string
	lastOrg   uuid.UUID
}

func (f *fakeSecretSource) Resolve(_ context.Context, org uuid.UUID, token, environment, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, name)
	f.lastEnv, f.lastTok, f.lastOrg = environment, token, org
	a, ok := f.answers[name]
	if !ok {
		return "", false, nil
	}
	return a.value, a.found, a.err
}

func (f *fakeSecretSource) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return f.revokeErr
}

func (f *fakeSecretSource) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// poolIssued / poolHeld read how many slices the pool has EVER handed out and
// how many are still on loan. They reach into the pool's own fields on purpose:
// "was a subnet taken" cannot be answered from the outside, because a released
// slice is handed straight back on the next call.
func poolIssued(p *SubnetPool) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.next
}

func poolHeld(p *SubnetPool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.taken)
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func okResult(outputs map[string]any) BundleRunResult {
	raw := map[string]json.RawMessage{}
	emitted := []string{}
	for k, v := range outputs {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		raw[k] = b
		emitted = append(emitted, k)
	}
	sortStrings(emitted)
	doc, err := json.Marshal(nodeabi.Result{ABI: nodeabi.ABI, Emitted: emitted, Outputs: raw, Status: nodeabi.StatusOK})
	if err != nil {
		panic(err)
	}
	return BundleRunResult{Stdout: doc}
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func helloNode(t *testing.T, secrets []domain.SecretSpec, egress []string) *domain.GraphNode {
	t.Helper()
	ref, err := domain.NewNodeRef("@acme/hello", "1.0.9", "go",
		"10.0.10.20:8078/acme/hello.node:1.0.9-go", "sha256:"+strings.Repeat("aa", 32))
	if err != nil {
		t.Fatalf("NewNodeRef: %v", err)
	}
	ports, err := domain.NewPortSpecs(
		[]domain.PortSpec{{Name: "name", Required: true}},
		[]domain.PortSpec{{Name: "out", Required: true}},
	)
	if err != nil {
		t.Fatalf("NewPortSpecs: %v", err)
	}
	return &domain.GraphNode{
		ID:        uuid.New(),
		Name:      "greet",
		NodeType:  domain.GraphNodeTypeBundle,
		Config:    domain.JSONMap{"prefix": "hello"},
		Resources: domain.ResourceLimit{MemoryMB: 64, TimeoutSec: 5},
		NodeRef:   ref,
		Ports:     ports,
		Secrets:   secrets,
		Egress:    egress,
	}
}

func newTestInvoker(t *testing.T, runner *fakeBundleRunner, sidecars *fakeSidecarManager, secrets *fakeSecretSource) *NodeInvoker {
	t.Helper()
	pool, err := NewSubnetPool("10.201.0.0/16", 29)
	if err != nil {
		t.Fatalf("NewSubnetPool: %v", err)
	}
	return NewNodeInvoker(runner, sidecars, secrets, pool, "10.0.10.20:8443", fixedClock{t: time.Unix(0, 0).UTC()})
}

func decodeCall(t *testing.T, raw []byte) *nodeabi.Call {
	t.Helper()
	call, verr := nodeabi.ValidateCall(raw)
	if verr != nil {
		t.Fatalf("the CALL this runtime produced is not a valid ABI document: %s: %s", verr.Code, verr.Message)
	}
	return call
}

// T2.4 — the CALL document is exactly what the ABI says, and it is built from
// the ROW: the pin the node was compiled against, the digest-pinned image, the
// declared config, and handles where secrets go.
//
// Control: change imageRef to use ref.ImageRef (the tag the pin carried)
// instead of registryHost + repository + digest ⇒ the image assertion fails.
func TestInvoke_CallDocument(t *testing.T) {
	runner := &fakeBundleRunner{}
	sidecars := &fakeSidecarManager{network: "sentiae-inv-x"}
	secrets := &fakeSecretSource{answers: map[string]resolvedSecret{
		"greeting_suffix": {value: " ::p4-secret::", found: true},
	}}
	inv := newTestInvoker(t, runner, sidecars, secrets)

	node := helloNode(t, []domain.SecretSpec{{Name: "greeting_suffix"}}, nil)
	runID := uuid.New()
	if _, err := inv.Invoke(context.Background(), InvokeNodeInput{
		RunID:       runID,
		OrgID:       uuid.New(),
		Environment: "preview",
		Node:        node,
		Inputs:      map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
		Config:      map[string]json.RawMessage{"prefix": json.RawMessage(`"hello"`)},
		SecretToken: "handed-token",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	launch := runner.lastLaunch(t)
	wantImage := "10.0.10.20:8443/acme/hello.node@sha256:" + strings.Repeat("aa", 32)
	if launch.Image != wantImage {
		t.Fatalf("image = %q, want %q (pulled by digest from the TLS registry, never by the tag)", launch.Image, wantImage)
	}
	if len(runner.pulled) != 1 || runner.pulled[0] != wantImage {
		t.Fatalf("pulled = %v, want exactly [%s]", runner.pulled, wantImage)
	}
	if launch.RunID != runID {
		t.Fatalf("launch.RunID = %s, want %s — the run label is how SweepRun finds this container", launch.RunID, runID)
	}

	call := decodeCall(t, launch.Call)
	if call.Node != "@acme/hello@1.0.9" {
		t.Fatalf("call.node = %q, want @acme/hello@1.0.9", call.Node)
	}
	if call.Invocation.RunID != runID.String() || call.Invocation.Node != "greet" {
		t.Fatalf("call.invocation = %+v", call.Invocation)
	}
	if !strings.HasPrefix(call.Invocation.ID, "inv-") {
		t.Fatalf("call.invocation.id = %q, want an inv- prefixed id", call.Invocation.ID)
	}
	if string(call.Inputs["name"]) != `"x"` {
		t.Fatalf("call.inputs = %v", call.Inputs)
	}
	if string(call.Config["prefix"]) != `"hello"` {
		t.Fatalf("call.config = %v", call.Config)
	}
	if call.Egress != nil {
		t.Fatalf("a node declaring no egress must carry no egress block, got %+v", call.Egress)
	}
	if len(call.Secrets) != 1 || !strings.HasPrefix(call.Secrets["greeting_suffix"], nodeabi.HandlePrefix) {
		t.Fatalf("call.secrets = %v, want one opaque handle", call.Secrets)
	}

	// The resolution used the run's credentials, not the process's.
	if secrets.lastEnv != "preview" || secrets.lastTok != "handed-token" {
		t.Fatalf("resolve used environment=%q token=%q", secrets.lastEnv, secrets.lastTok)
	}
}

// T2.4b — an egress-declaring node carries the proxy block, and its bearer is
// opaque: 64 lower-hex characters with no `handle:` prefix, so a grep for
// handles stays a secret-only signal.
//
// Control: mint the token with nodebroker.NewHandle() ⇒ the prefix assertion
// fails.
func TestInvoke_CallDocument_Egress(t *testing.T) {
	runner := &fakeBundleRunner{}
	sidecars := &fakeSidecarManager{network: "sentiae-inv-x"}
	inv := newTestInvoker(t, runner, sidecars, &fakeSecretSource{})

	node := helloNode(t, nil, []string{"httpbin.org"})
	if _, err := inv.Invoke(context.Background(), InvokeNodeInput{
		RunID: uuid.New(), OrgID: uuid.New(), Node: node,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	call := decodeCall(t, runner.lastLaunch(t).Call)
	if call.Egress == nil {
		t.Fatal("an egress-declaring node must carry the egress block")
	}
	if call.Egress.Proxy != "http://proxy:3128" {
		t.Fatalf("egress.proxy = %q", call.Egress.Proxy)
	}
	if len(call.Egress.Token) != 64 {
		t.Fatalf("egress token is %d chars, want 64 (32 crypto/rand bytes as hex)", len(call.Egress.Token))
	}
	if strings.HasPrefix(call.Egress.Token, nodeabi.HandlePrefix) {
		t.Fatalf("egress token %q must not carry the secret-handle prefix", call.Egress.Token)
	}
	if strings.Trim(call.Egress.Token, "0123456789abcdef") != "" {
		t.Fatalf("egress token %q is not lower hex", call.Egress.Token)
	}
	if len(call.Secrets) != 0 {
		t.Fatalf("call.secrets = %v, want none", call.Secrets)
	}
}

// T2.5 — every RESULT shape becomes exactly one outcome, and the failure text
// is the verbatim reason the engine will name the node with.
//
// Control: drop the ExitCode branch ⇒ the crashing row reports
// "crash: no result document on stdout" instead of the exit status.
func TestInvoke_Result(t *testing.T) {
	greeting := okResult(map[string]any{"out": map[string]any{"greeting": "hello x"}})
	errDoc, err := json.Marshal(nodeabi.Result{
		ABI: nodeabi.ABI, Emitted: []string{},
		Error:  &nodeabi.Error{Code: "key_missing", Message: "body has no string field name", Retryable: true},
		Status: nodeabi.StatusError,
	})
	if err != nil {
		t.Fatalf("marshal error result: %v", err)
	}

	tests := []struct {
		name    string
		result  BundleRunResult
		wantErr string
		wantOut string
	}{
		{"ok", greeting, "", `{"greeting":"hello x"}`},
		{"timeout", BundleRunResult{TimedOut: true}, "crash: timeout after 5s", ""},
		{"non-zero exit", BundleRunResult{ExitCode: 2, Stderr: "boom\npanic: nil map"}, "crash: exit status 2: panic: nil map", ""},
		{"empty stdout", BundleRunResult{}, "crash: no result document on stdout", ""},
		{"not an abi document", BundleRunResult{Stdout: []byte(`{"abi":"other/v1","emitted":[],"logs":[],"outputs":{},"status":"ok"}`)}, "abi_mismatch: abi must be sentiae.node/v1", ""},
		{"author error", BundleRunResult{Stdout: errDoc}, "key_missing: body has no string field name (retryable)", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeBundleRunner{result: func(BundleLaunch) (BundleRunResult, error) { return tt.result, nil }}
			inv := newTestInvoker(t, runner, &fakeSidecarManager{}, &fakeSecretSource{})
			out, err := inv.Invoke(context.Background(), InvokeNodeInput{
				RunID: uuid.New(), OrgID: uuid.New(), Node: helloNode(t, nil, nil),
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if got := string(out.Outputs["out"]); got != tt.wantOut {
					t.Fatalf("outputs[out] = %s, want %s", got, tt.wantOut)
				}
				if !out.Fired["out"] {
					t.Fatalf("fired = %v, want out fired", out.Fired)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Invoke error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// T2.6 — a secret VALUE reaches the sidecar's binding and NOTHING else: not the
// CALL, not the launch, not the inputs the caller handed in.
//
// Control: put the value in the CALL (call.Secrets[name] = value instead of the
// handle) ⇒ the "value in the call" assertion fires.
func TestInvoke_SecretsNeverLeaveTheBinding(t *testing.T) {
	const canary = "::p4-secret-canary::"
	runner := &fakeBundleRunner{}
	sidecars := &fakeSidecarManager{}
	secrets := &fakeSecretSource{answers: map[string]resolvedSecret{
		"greeting_suffix": {value: canary, found: true},
	}}
	inv := newTestInvoker(t, runner, sidecars, secrets)

	node := helloNode(t, []domain.SecretSpec{{Name: "greeting_suffix", Required: true}}, nil)
	inputs := map[string]json.RawMessage{"name": json.RawMessage(`"x"`)}
	if _, err := inv.Invoke(context.Background(), InvokeNodeInput{
		RunID: uuid.New(), OrgID: uuid.New(), Environment: "preview",
		Node: node, Inputs: inputs, SecretToken: "handed-token",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Anchor: the value DID travel — to the sidecar, in the binding, once.
	if len(sidecars.opens) != 1 {
		t.Fatalf("opens = %d, want exactly one sidecar", len(sidecars.opens))
	}
	answer, ok := sidecars.opens[0].Binding.Secrets["greeting_suffix"]
	if !ok || answer.Value != canary || !answer.Found {
		t.Fatalf("binding.secrets = %+v, want the resolved value", sidecars.opens[0].Binding.Secrets)
	}
	if !strings.HasPrefix(answer.Handle, nodeabi.HandlePrefix) {
		t.Fatalf("binding handle = %q, want an opaque handle", answer.Handle)
	}

	launch := runner.lastLaunch(t)
	if strings.Contains(string(launch.Call), canary) {
		t.Fatalf("the secret VALUE is in the CALL document: %s", launch.Call)
	}
	call := decodeCall(t, launch.Call)
	if call.Secrets["greeting_suffix"] != answer.Handle {
		t.Fatalf("call.secrets[greeting_suffix] = %q, want the handle %q", call.Secrets["greeting_suffix"], answer.Handle)
	}
	if string(inputs["name"]) != `"x"` || len(inputs) != 1 {
		t.Fatalf("the caller's inputs were mutated: %v", inputs)
	}
}

// T2.7 — a REQUIRED secret that does not exist stops the invocation before any
// sandbox is opened; an OPTIONAL one does not.
//
// Control: drop the `spec.Required && !found` branch ⇒ the required row runs
// the node with a handle that resolves to nothing.
func TestInvoke_RequiredSecretAbsent(t *testing.T) {
	t.Run("required and absent refuses before launch", func(t *testing.T) {
		runner := &fakeBundleRunner{}
		sidecars := &fakeSidecarManager{}
		inv := newTestInvoker(t, runner, sidecars, &fakeSecretSource{})
		node := helloNode(t, []domain.SecretSpec{{Name: "greeting_suffix", Required: true}}, nil)

		_, err := inv.Invoke(context.Background(), InvokeNodeInput{
			RunID: uuid.New(), OrgID: uuid.New(), Environment: "preview",
			Node: node, SecretToken: "handed-token",
		})
		if !errors.Is(err, domain.ErrRequiredSecretAbsent) {
			t.Fatalf("Invoke error = %v, want ErrRequiredSecretAbsent", err)
		}
		if len(sidecars.opens) != 0 {
			t.Fatalf("a refused invocation opened %d sidecar(s)", len(sidecars.opens))
		}
		if len(runner.launch) != 0 {
			t.Fatalf("a refused invocation launched %d bundle(s)", len(runner.launch))
		}
	})

	t.Run("optional and absent still runs", func(t *testing.T) {
		runner := &fakeBundleRunner{}
		sidecars := &fakeSidecarManager{}
		inv := newTestInvoker(t, runner, sidecars, &fakeSecretSource{})
		node := helloNode(t, []domain.SecretSpec{{Name: "greeting_suffix"}}, nil)

		if _, err := inv.Invoke(context.Background(), InvokeNodeInput{
			RunID: uuid.New(), OrgID: uuid.New(), Environment: "preview",
			Node: node, SecretToken: "handed-token",
		}); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if len(sidecars.opens) != 1 {
			t.Fatalf("opens = %d, want one", len(sidecars.opens))
		}
		answer := sidecars.opens[0].Binding.Secrets["greeting_suffix"]
		if answer.Found || answer.Value != "" {
			t.Fatalf("absent optional secret bound as %+v, want found:false", answer)
		}
	})
}

// T2.14 — the sidecar/bridge lifecycle is decided by what the node DECLARES,
// and both are released on the success and the failure path.
//
// Control: drop the deferred Close ⇒ the "closed" rows fail.
// Second control: call pool.Acquire for every sidecar (not only for a bridge)
// ⇒ the secrets-only row's "no subnet" assertion fails.
func TestInvoke_SidecarLifecycle(t *testing.T) {
	secretSpec := []domain.SecretSpec{{Name: "greeting_suffix"}}

	tests := []struct {
		name        string
		secrets     []domain.SecretSpec
		egress      []string
		fail        bool
		wantOpen    bool
		wantBridge  bool
		wantSubpath bool
	}{
		{name: "secrets only", secrets: secretSpec, wantOpen: true, wantSubpath: true},
		{name: "egress only", egress: []string{"httpbin.org"}, wantOpen: true, wantBridge: true},
		{name: "both", secrets: secretSpec, egress: []string{"httpbin.org"}, wantOpen: true, wantBridge: true, wantSubpath: true},
		{name: "neither", wantOpen: false},
		{name: "failure still tears down", secrets: secretSpec, egress: []string{"httpbin.org"}, fail: true, wantOpen: true, wantBridge: true, wantSubpath: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeBundleRunner{}
			if tt.fail {
				runner.result = func(BundleLaunch) (BundleRunResult, error) {
					return BundleRunResult{ExitCode: 1, Stderr: "boom"}, nil
				}
			}
			sidecars := &fakeSidecarManager{network: "sentiae-inv-test"}
			secrets := &fakeSecretSource{answers: map[string]resolvedSecret{
				"greeting_suffix": {value: "v", found: true},
			}}
			pool, err := NewSubnetPool("10.201.0.0/16", 29)
			if err != nil {
				t.Fatalf("NewSubnetPool: %v", err)
			}
			inv := NewNodeInvoker(runner, sidecars, secrets, pool, "10.0.10.20:8443", fixedClock{})

			node := helloNode(t, tt.secrets, tt.egress)
			_, invErr := inv.Invoke(context.Background(), InvokeNodeInput{
				RunID: uuid.New(), OrgID: uuid.New(), Environment: "preview",
				Node: node, SecretToken: "handed-token",
			})
			if tt.fail && invErr == nil {
				t.Fatal("expected the failing bundle to be reported")
			}
			if !tt.fail && invErr != nil {
				t.Fatalf("Invoke: %v", invErr)
			}

			if got := len(sidecars.opens); (got == 1) != tt.wantOpen {
				t.Fatalf("opens = %d, wantOpen = %v", got, tt.wantOpen)
			}
			launch := runner.lastLaunch(t)
			if !tt.wantOpen {
				if launch.Network != "" || launch.BrokerSubpath != "" {
					t.Fatalf("a node declaring neither got network=%q subpath=%q", launch.Network, launch.BrokerSubpath)
				}
				if len(sidecars.closes) != 0 {
					t.Fatalf("nothing was opened, but %d close(s) happened", len(sidecars.closes))
				}
				return
			}

			open := sidecars.opens[0]
			if tt.wantBridge {
				if open.Binding.Egress == nil {
					t.Fatal("an egress-declaring node must carry an egress binding")
				}
				if open.Binding.Egress.Subnet != "10.201.0.0/29" {
					t.Fatalf("bound subnet = %q, want the first pool slice", open.Binding.Egress.Subnet)
				}
				if launch.Network != "sentiae-inv-test" {
					t.Fatalf("launch network = %q, want the invocation bridge", launch.Network)
				}
			} else {
				if open.Binding.Egress != nil {
					t.Fatalf("a node declaring no egress got an egress binding: %+v", open.Binding.Egress)
				}
				if launch.Network != "" {
					t.Fatalf("launch network = %q, want none (the sandbox runs --network none)", launch.Network)
				}
				// Second control: the pool was never drawn from at all. Asking
				// it for a subnet afterwards would NOT discriminate — a taken
				// slice is released on teardown and handed straight back — so
				// this reads how many the pool has ever issued.
				if n := poolIssued(pool); n != 0 {
					t.Fatalf("a secrets-only invocation drew %d subnet(s) from the pool", n)
				}
			}
			if tt.wantBridge {
				if n := poolIssued(pool); n != 1 {
					t.Fatalf("an egress invocation drew %d subnet(s), want exactly 1", n)
				}
				if held := poolHeld(pool); held != 0 {
					t.Fatalf("%d subnet(s) still held after the invocation, want 0", held)
				}
			}
			if tt.wantSubpath && launch.BrokerSubpath != open.InvocationID {
				t.Fatalf("broker subpath = %q, want the invocation %q", launch.BrokerSubpath, open.InvocationID)
			}
			if !tt.wantSubpath && launch.BrokerSubpath != "" {
				t.Fatalf("a node declaring no secrets got a broker mount at %q", launch.BrokerSubpath)
			}
			if len(sidecars.closes) != 1 || sidecars.closes[0] != open.InvocationID {
				t.Fatalf("closes = %v, want exactly the opened invocation %q", sidecars.closes, open.InvocationID)
			}
		})
	}
}

// D-457 — a secret the resolver REFUSES (Vault 403) fails the invocation, and
// the caller-visible reason names the class and the secret but carries none of
// Vault's transport/permission text. The full cause is logged at ERROR for the
// operator.
//
// Control: restore `%w` on the resolver error in resolveSecrets ⇒ the
// "permission denied" assertion fails. Control for the chain: delete
// secretResolveError.Unwrap ⇒ the errors.Is assertion fails.
func TestInvoke_SecretResolveFailureIsClassifiedNotNarrated(t *testing.T) {
	const vaultErr = `resolve tenants/8a1f.../greeting#value: Error making API request. ` +
		`URL: GET https://vault:8200/v1/secret/data/tenants/8a1f/app. Code: 403. ` +
		`Errors: * 1 error occurred: * permission denied`

	cause := errors.New(vaultErr)
	runner := &fakeBundleRunner{}
	sidecars := &fakeSidecarManager{}
	secrets := &fakeSecretSource{answers: map[string]resolvedSecret{
		"greeting_suffix": {err: cause},
	}}
	inv := newTestInvoker(t, runner, sidecars, secrets)
	node := helloNode(t, []domain.SecretSpec{{Name: "greeting_suffix"}}, nil)

	var logged bytes.Buffer
	ctx := logger.NewContext(context.Background(),
		slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))

	_, err := inv.Invoke(ctx, InvokeNodeInput{
		RunID: uuid.New(), OrgID: uuid.New(), Environment: "preview",
		Node: node, SecretToken: "handed-token",
	})
	if err == nil {
		t.Fatal("a refused secret resolution must FAIL the invocation, not pass as 'not set'")
	}

	msg := err.Error()
	if !strings.Contains(msg, "secret_resolve_failed") {
		t.Fatalf("the failure lost its class: %q", msg)
	}
	if !strings.Contains(msg, "greeting_suffix") {
		t.Fatalf("the failure does not name the secret: %q", msg)
	}
	if !strings.Contains(msg, secretResolveUnavailablePhrase) {
		t.Fatalf("the failure does not carry the fixed phrase %q: %q", secretResolveUnavailablePhrase, msg)
	}
	for _, leak := range []string{"permission denied", "403", "vault:8200", "secret/data/tenants"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("the caller-visible failure leaks Vault detail %q: %q", leak, msg)
		}
	}

	// The CHAIN survives even though the TEXT does not: an in-process caller can
	// still match the resolver's own error, which is what keeps the fixed phrase
	// from being a lie by omission.
	if !errors.Is(err, cause) {
		t.Fatalf("the resolver cause is no longer reachable through errors.Is: %v", err)
	}

	// Nothing ran: a node that cannot get a declared secret never launches.
	if len(sidecars.opens) != 0 || len(runner.launch) != 0 {
		t.Fatalf("a refused invocation opened %d sidecar(s) and launched %d bundle(s)", len(sidecars.opens), len(runner.launch))
	}

	// …and the operator still gets the whole cause, at ERROR.
	out := logged.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, "secret_resolve_failed") {
		t.Fatalf("the cause was not logged at ERROR: %q", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("the server-side log dropped the Vault cause: %q", out)
	}
	if !strings.Contains(out, "greeting_suffix") {
		t.Fatalf("the server-side log does not name the secret: %q", out)
	}
}
