//go:build unit

package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/nodebroker"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// ── the fake daemon ────────────────────────────────────────────────────────
//
// It records every invocation verbatim — argv, env AND stdin — because those
// three are exactly what the security properties are about: the binding must be
// on stdin and on nothing else, and the launch argv is a pinned contract.

type daemonCall struct {
	args  []string
	env   []string
	stdin []byte
}

type daemonRule struct {
	when func(args []string) bool
	then func(args []string) (stdout, stderr string, code int)
}

type fakeDaemon struct {
	mu    sync.Mutex
	calls []daemonCall
	rules []daemonRule
}

func (f *fakeDaemon) on(tokens []string, stdout string, code int) *fakeDaemon {
	f.rules = append(f.rules, daemonRule{
		when: func(args []string) bool { return containsAll(args, tokens) },
		then: func([]string) (string, string, int) { return stdout, "", code },
	})
	return f
}

func (f *fakeDaemon) onFunc(when func([]string) bool, then func([]string) (string, string, int)) *fakeDaemon {
	f.rules = append(f.rules, daemonRule{when: when, then: then})
	return f
}

func (f *fakeDaemon) fn(_ context.Context, env []string, stdin []byte, args ...string) (string, string, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, daemonCall{
		args:  append([]string(nil), args...),
		env:   append([]string(nil), env...),
		stdin: append([]byte(nil), stdin...),
	})
	rules := f.rules
	f.mu.Unlock()

	// Last registered wins, so a test row can OVERRIDE one of the healthy
	// daemon's answers by registering its own after it.
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if r.when(args) {
			stdout, stderr, code := r.then(args)
			return stdout, stderr, code, nil
		}
	}
	return "", "", 0, nil
}

func (f *fakeDaemon) argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.args)
	}
	return out
}

// indexOf is the position of the first call whose argv contains every token.
// -1 when there is none, which is how "this never happened" is asserted.
func (f *fakeDaemon) indexOf(tokens ...string) int {
	for i, args := range f.argv() {
		if containsAll(args, tokens) {
			return i
		}
	}
	return -1
}

func (f *fakeDaemon) call(index int) daemonCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index]
}

func containsAll(args, tokens []string) bool {
	for _, tok := range tokens {
		if !slices.Contains(args, tok) {
			return false
		}
	}
	return true
}

func testNodeRunnerConfig(t *testing.T) config.NodeRunnerConfig {
	t.Helper()
	return config.NodeRunnerConfig{
		RegistryHost:        "10.0.10.20:8443",
		RegistryUser:        "registry-client",
		RunsVolume:          "sentiae-node-runs",
		RunsDir:             t.TempDir(),
		UplinkNetwork:       "sentiae-node-egress-uplink",
		InvocationCIDR:      "10.201.0.0/16",
		InvocationPrefixLen: 29,
		SidecarReadyTimeout: time.Second,
		PullTimeout:         time.Minute,
		TunnelMax:           130 * time.Second,
	}
}

// fakeAudit is the audit store the manager drains into. It records what it was
// handed, because "the sidecar was stopped and read" is only half the property —
// the other half is that the tuple actually reached the sink.
type fakeAudit struct {
	mu  sync.Mutex
	got []domain.EgressDecision
	err error
}

func (f *fakeAudit) Record(_ context.Context, d []domain.EgressDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, d...)
	return nil
}

func (f *fakeAudit) recorded() []domain.EgressDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.EgressDecision(nil), f.got...)
}

// auditBoundLine is the anchor every readable sidecar log carries: without it
// the drain is BLIND and refuses, which is what keeps "zero decisions" from
// being believed on an unreadable log.
const auditBoundLine = `{"time":"2026-09-07T10:00:00Z","level":"INFO","msg":"sidecar_bound","invocation":"inv-x","node":"echo","secret_count":0,"egress":true}`

// decisionLine is one line exactly as the sidecar's proxy writes it.
func decisionLine(seq int, decision, reason, host string, port int, run, invocation string) string {
	return fmt.Sprintf(`{"time":"2026-09-07T10:00:%02dZ","level":"INFO","msg":"egress_decision","decision":%q,"host":%q,"host_redacted":false,"port":%d,"invocation_id":%q,"node":"echo","reason":%q,"run_id":%q,"seq":%d}`,
		seq, decision, host, port, invocation, reason, run, seq)
}

func testManager(t *testing.T, docker *fakeDaemon) *SidecarManager {
	t.Helper()
	return testManagerWithAudit(t, docker, &fakeAudit{})
}

func testManagerWithAudit(t *testing.T, docker *fakeDaemon, audit *fakeAudit) *SidecarManager {
	t.Helper()
	pool, err := usecase.NewSubnetPool("10.201.0.0/16", 29)
	if err != nil {
		t.Fatalf("subnet pool: %v", err)
	}
	m, err := NewSidecarManager(testNodeRunnerConfig(t), pool, audit)
	if err != nil {
		t.Fatalf("new sidecar manager: %v", err)
	}
	m.docker = docker.fn
	m.self = "runtime-container"
	m.image = "sha256:036daa5efeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	return m
}

// readySidecarDaemon answers the readiness poll and nothing else.
// readySidecarDaemon answers the readiness poll and hands the drain a readable,
// empty audit log. Every Close in this file drains, and a log without the anchor
// is BLIND by design — that refusal is the guard working, not a flake.
func readySidecarDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	return f.on([]string{"exec", "wget"}, "ok", 0).on([]string{"logs"}, auditBoundLine+"\n", 0)
}

// T4.10 — TestSidecarManager_ArgsAndOrder pins the launch lines and the ORDER
// they happen in. Both are security properties:
//
//   - a secrets-only invocation must create no network, join none, and never
//     touch the uplink — the sidecar for a node with no declared egress must
//     have no way out;
//   - an egress invocation must get its own --internal bridge with an explicit
//     subnet, the proxy alias, and the uplink connected only AFTER the binding
//     is delivered, so the one moment the sidecar holds credentials without a
//     policy is a moment it cannot reach anything;
//   - the binding travels on stdin with an argv and an environment that carry
//     nothing (A10: a canary passed on stdin appears in no inspect, no cmdline,
//     no env, no daemon journal);
//   - the sidecar's own binary is supplied with --entrypoint, because the
//     runtime image's ENTRYPOINT is the SERVER and a trailing command would be
//     appended to it — booting a second runtime-service inside the sidecar,
//     which reports "running" and never answers /healthz (R-23).
//
// CONTROL (order): move the `network connect` block ahead of the bind exec in
// open() — the "connect after bind" assertion goes red.
// CONTROL (R-23): append sidecarBinary after the image in sidecarRunArgs
// instead of passing --entrypoint — the argv equality and the "nothing follows
// the image" assertion go red.
func TestSidecarManager_ArgsAndOrder(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-11111111-1111-1111-1111-111111111111"
	name := "sentiae-sc-" + invocation
	network := "sentiae-inv-" + invocation

	t.Run("secrets only: no bridge, no uplink, --network none", func(t *testing.T) {
		docker := readySidecarDaemon()
		m := testManager(t, docker)

		in := usecase.SidecarOpen{
			InvocationID: invocation,
			RunID:        runID,
			Node:         "greet",
			Binding: usecase.SidecarBinding{
				Invocation: invocation, Run: runID.String(), Node: "greet",
				Secrets: map[string]usecase.SecretAnswer{
					"greeting_suffix": {
						Handle: "handle:0123456789abcdef0123456789abcdef",
						Found:  true,
						Value:  "::p4-canary-secret-value::",
					},
				},
			},
		}
		sc, err := m.Open(context.Background(), in)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		if got := docker.indexOf("network", "create"); got != -1 {
			t.Fatalf("a secrets-only invocation must create no network (call %d)", got)
		}
		if got := docker.indexOf("network", "connect"); got != -1 {
			t.Fatalf("a secrets-only invocation must never touch the uplink (call %d)", got)
		}

		want := append([]string{
			"run", "-d",
			"--name", name,
			"--label", "sentiae.node.invocation=" + invocation,
			"--label", "sentiae.node.run=" + runID.String(),
		}, hardenedFlags(256, 1)...)
		want = append(want,
			"--log-driver", "local",
			"--mount", "type=volume,source=sentiae-node-runs,target=/run/sentiae-inv,volume-subpath="+invocation,
			"--network", "none",
			"--entrypoint", "/app/node-sidecar",
			m.image)
		runIdx := docker.indexOf("run", "-d")
		if runIdx == -1 {
			t.Fatal("the sidecar was never launched")
		}
		if got := docker.call(runIdx).args; !reflect.DeepEqual(got, want) {
			t.Fatalf("sidecar argv:\n got %q\nwant %q", got, want)
		}
		if got := docker.call(runIdx).args; got[len(got)-1] != m.image {
			t.Fatalf("nothing may follow the image, got %q", got[len(got)-3:])
		}

		if sc.Network != "" || sc.ProxyURL != "" {
			t.Fatalf("a secrets-only sidecar exposes no network and no proxy, got %+v", sc)
		}
		if sc.BrokerSubpath != invocation {
			t.Fatalf("broker subpath: got %q, want %q", sc.BrokerSubpath, invocation)
		}

		assertBindExec(t, docker, name, in.Binding)
	})

	t.Run("egress: its own internal bridge, alias, uplink AFTER the bind", func(t *testing.T) {
		docker := readySidecarDaemon()
		m := testManager(t, docker)

		in := usecase.SidecarOpen{
			InvocationID: invocation,
			RunID:        runID,
			Node:         "echo",
			Binding: usecase.SidecarBinding{
				Invocation: invocation, Run: runID.String(), Node: "echo",
				Secrets: map[string]usecase.SecretAnswer{},
				Egress: &usecase.EgressBinding{
					Patterns: []string{"*"},
					Token:    "b3f1c0d2e4a5968778695a4e3c2d1b0af9e8d7c6b5a4938271605f4e3d2c1b0a",
					Subnet:   "10.201.0.0/29",
				},
			},
		}
		sc, err := m.Open(context.Background(), in)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		createIdx := docker.indexOf("network", "create")
		if createIdx == -1 {
			t.Fatal("an egress invocation must create its bridge")
		}
		wantCreate := []string{
			"network", "create",
			"--internal",
			"--subnet", "10.201.0.0/29",
			"--label", "sentiae.node.invocation=" + invocation,
			"--label", "sentiae.node.run=" + runID.String(),
			network,
		}
		if got := docker.call(createIdx).args; !reflect.DeepEqual(got, wantCreate) {
			t.Fatalf("network create argv:\n got %q\nwant %q", got, wantCreate)
		}

		runIdx := docker.indexOf("run", "-d")
		runArgs := docker.call(runIdx).args
		if !containsSequence(runArgs, "--network", network) {
			t.Fatalf("the sidecar must join its invocation bridge, got %q", runArgs)
		}
		if !containsSequence(runArgs, "--network-alias", "proxy") {
			t.Fatalf("the sidecar must answer to the proxy alias, got %q", runArgs)
		}
		if !containsSequence(runArgs, "--entrypoint", "/app/node-sidecar") {
			t.Fatalf("the sidecar binary must be the entrypoint, got %q", runArgs)
		}
		if runArgs[len(runArgs)-1] != m.image {
			t.Fatalf("nothing may follow the image, got %q", runArgs[len(runArgs)-3:])
		}

		bindIdx := docker.indexOf("exec", "-i", "bind")
		connectIdx := docker.indexOf("network", "connect")
		readyIdx := docker.indexOf("exec", "wget")
		if bindIdx == -1 || connectIdx == -1 || readyIdx == -1 {
			t.Fatalf("missing step: bind=%d connect=%d ready=%d", bindIdx, connectIdx, readyIdx)
		}
		if !(createIdx < runIdx && runIdx < bindIdx && bindIdx < connectIdx && connectIdx < readyIdx) {
			t.Fatalf("order must be create < run < bind < connect < ready, got %d %d %d %d %d",
				createIdx, runIdx, bindIdx, connectIdx, readyIdx)
		}
		wantConnect := []string{"network", "connect", "sentiae-node-egress-uplink", name}
		if got := docker.call(connectIdx).args; !reflect.DeepEqual(got, wantConnect) {
			t.Fatalf("connect argv:\n got %q\nwant %q", got, wantConnect)
		}

		if sc.Network != network {
			t.Fatalf("sidecar network: got %q, want %q", sc.Network, network)
		}
		if sc.ProxyURL != "http://proxy:3128" {
			t.Fatalf("proxy url: got %q", sc.ProxyURL)
		}

		assertBindExec(t, docker, name, in.Binding)
	})
}

// assertBindExec pins THE channel: argv carries the container and the
// subcommand and nothing else, the environment is empty, and the document is on
// stdin.
func assertBindExec(t *testing.T, docker *fakeDaemon, name string, want usecase.SidecarBinding) {
	t.Helper()
	idx := docker.indexOf("exec", "-i", "bind")
	if idx == -1 {
		t.Fatal("the binding was never delivered")
	}
	call := docker.call(idx)

	wantArgs := []string{"exec", "-i", name, "/app/node-sidecar", "bind"}
	if !reflect.DeepEqual(call.args, wantArgs) {
		t.Fatalf("bind argv:\n got %q\nwant %q", call.args, wantArgs)
	}
	if len(call.env) != 0 {
		t.Fatalf("the bind exec must carry no environment, got %q", call.env)
	}
	if len(call.stdin) == 0 {
		t.Fatal("the binding must be on stdin")
	}

	var got usecase.SidecarBinding
	if err := json.Unmarshal(call.stdin, &got); err != nil {
		t.Fatalf("stdin is not the binding document: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("binding on stdin:\n got %+v\nwant %+v", got, want)
	}

	// Nothing else may carry any of it: no argv anywhere in the whole
	// conversation may contain a handle, a value or the token.
	for _, args := range docker.argv() {
		for _, arg := range args {
			for _, secret := range secretStrings(want) {
				if strings.Contains(arg, secret) {
					t.Fatalf("a secret reached the argv: %q", arg)
				}
			}
		}
	}
}

func secretStrings(b usecase.SidecarBinding) []string {
	var out []string
	for _, a := range b.Secrets {
		if a.Handle != "" {
			out = append(out, a.Handle)
		}
		if a.Value != "" {
			out = append(out, a.Value)
		}
	}
	if b.Egress != nil && b.Egress.Token != "" {
		out = append(out, b.Egress.Token)
	}
	return out
}

func containsSequence(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// T4.11 — TestProbe_Refusals proves the boot gate. Every row is a topology this
// process cannot isolate an invocation on, and each refusal names the ONE thing
// that is wrong, verbatim (§3.9) — "the uplink is bad" is not something an
// operator can act on.
//
// The uplink itself is created by deploy.sh (R-14/E6), so Probe is a pure
// VERIFIER: it never creates or repairs, because a runtime silently rebuilding
// static infrastructure hides the fact that someone removed it.
//
// CONTROL (survivor): make SweepAll return the counts it removed instead of
// re-enumerating — the survivor row reports 0/0 and passes wrongly.
// CONTROL (R-30 order): move the SweepAll block back below the CIDR check — the
// survivor rows reach the census first and their "not reached" assertion goes
// red.
// CONTROL (CIDR): scope verifyInvocationCIDR to the uplink alone (R-14's
// original scope) — the "unrelated network" row passes wrongly, and on the
// homelab every egress invocation would instead fail at run time with docker's
// "Pool overlaps with other one on this address space" (R-25).
func TestProbe_Refusals(t *testing.T) {
	uplink := []string{"network", "inspect", "sentiae-node-egress-uplink"}

	tests := []struct {
		name   string
		daemon func() *fakeDaemon
		// wantErr is the whole refusal, verbatim. wantErrContains is for the
		// rows whose refusal quotes a randomly-minted invocation id, where a
		// verbatim match is not expressible — the substring is still the ONE
		// thing that names the fault.
		wantErr         string
		wantErrContains string
		// wantAttempt is the removal call a survivor refusal is only legal
		// AFTER (R-30): "still there" is a measurement, and refusing without
		// having tried is the deadlock this ruling removed. Set on the survivor
		// rows only; those rows also prove the CIDR census was never reached.
		wantAttempt []string
	}{
		{
			name: "uplink absent",
			daemon: func() *fakeDaemon {
				return probeDaemon().on(uplink, "", 1)
			},
			wantErr: "node runner: network sentiae-node-egress-uplink does not exist",
		},
		{
			name: "uplink is not a bridge",
			daemon: func() *fakeDaemon {
				return probeDaemon().on(uplink, "macvlan|false|false\n", 0)
			},
			wantErr: "node runner: network sentiae-node-egress-uplink must use bridge driver",
		},
		{
			name: "uplink is internal",
			daemon: func() *fakeDaemon {
				return probeDaemon().on(uplink, "bridge|true|false\n", 0)
			},
			wantErr: "node runner: network sentiae-node-egress-uplink must not be internal",
		},
		{
			name: "uplink allows container-to-container traffic",
			daemon: func() *fakeDaemon {
				return probeDaemon().on(uplink, "bridge|false|true\n", 0)
			},
			wantErr: "node runner: network sentiae-node-egress-uplink must set com.docker.network.bridge.enable_icc=false",
		},
		{
			name: "an unrelated network overlaps the invocation range",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.on([]string{"network", "ls", "-q"}, "netaaa\nnetbbb\n", 0)
				f.onFunc(
					func(args []string) bool { return containsAll(args, []string{"network", "inspect", "netbbb"}) },
					func([]string) (string, string, int) { return "p4ovl|10.201.5.0/24 \n", "", 0 },
				)
				return f
			},
			wantErr: "node runner: invocation cidr 10.201.0.0/16 overlaps docker network p4ovl (10.201.5.0/24)",
		},
		{
			name: "an orphan network survives the boot sweep",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.on([]string{"network", "ls", "--filter", "label=sentiae.node.invocation"},
					"sentiae-inv-inv-old\n", 0)
				return f
			},
			wantErr:     "node runner: orphan sweep incomplete: 0 container(s), 1 network(s) remain",
			wantAttempt: []string{"network", "rm"},
		},
		{
			name: "an orphan container survives the boot sweep",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.on([]string{"ps", "-a", "--filter", "label=sentiae.node.invocation"},
					"sentiae-sc-inv-old\n", 0)
				return f
			},
			wantErr:     "node runner: orphan sweep incomplete: 1 container(s), 0 network(s) remain",
			wantAttempt: []string{"rm", "-f"},
		},
		{
			// The redemption is REFUSED. A broker that answers 403 to the handle
			// the probe itself just bound is not a working secret path, however
			// healthy every container looks.
			name: "the probe's own handle is refused by the broker",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				// The refusal code is nodebroker's, not ours: bind the fixture to the
				// exported constant so a rename there is a COMPILE error here, instead
				// of leaving this test green against a string the broker no longer
				// emits (§9.8/D-1: the literals live in platform-kit and nowhere else).
				f.on(redeemCall, `redeem: status=403 code="`+nodebroker.CodeSecretNotDeclared+`" found=false`+"\n", 0)
				return f
			},
			wantErrContains: `got "redeem: status=403 code=\"` + nodebroker.CodeSecretNotDeclared + `\" found=false"`,
		},
		{
			// The probe binds Found:false with an empty value. A broker that
			// answers found=true is answering with something the probe never
			// bound, and boot must not proceed past that.
			name: "the broker answers a secret the probe never bound",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.on(redeemCall, `redeem: status=200 code="" found=true`+"\n", 0)
				return f
			},
			wantErrContains: `got "redeem: status=200 code=\"\" found=true"`,
		},
		{
			// THE 2026-09-03 CASE, seen from the node's side: the socket exists
			// and the sidecar is healthy, but the node's uid class cannot get
			// through the directory to it.
			name: "the node-shaped container cannot dial the broker socket",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.onFunc(
					func(args []string) bool { return containsAll(args, redeemCall) },
					func([]string) (string, string, int) {
						return "", "node sidecar: redeem: dial /run/sentiae/broker.sock: " +
							"connect: permission denied", 1
					},
				)
				return f
			},
			wantErrContains: "dial /run/sentiae/broker.sock: connect: permission denied",
		},
		{
			// THE 2026-09-03 CASE, seen from the sidecar's side, with the real
			// live text: nodebroker.Listen refuses a directory it cannot make
			// searchable, the bind exec fails, and boot must stop here rather
			// than report healthy and fail on the first customer invocation.
			name: "the sidecar refuses its binding because the socket dir denies search",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.onFunc(
					func(args []string) bool { return containsAll(args, []string{"exec", "-i", "bind"}) },
					func([]string) (string, string, int) {
						return "", "node sidecar: bind: broker socket /run/sentiae-inv/broker.sock: " +
							"broker socket dir /run/sentiae-inv: mode 0700 denies search to some uid class; " +
							"chmod 0755: chmod /run/sentiae-inv: operation not permitted", 1
					},
				)
				return f
			},
			wantErrContains: "broker socket dir /run/sentiae-inv: mode 0700 denies search to some uid class; " +
				"chmod 0755: chmod /run/sentiae-inv: operation not permitted",
		},
		{
			name:    "anchor: a correct topology probes green",
			daemon:  probeDaemon,
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docker := tt.daemon()
			m := testManager(t, docker)
			m.image = "" // the probe resolves it, as it does at boot

			err := m.Probe(context.Background())
			if tt.wantErr == "" && tt.wantErrContains == "" {
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				// The anchor also proves the probe drove a COMPLETE egress
				// cycle: a bridge was created, the sidecar was bound, and the
				// uplink was connected — the shape §9.1 asserts in the logs.
				if docker.indexOf("network", "create", "--internal") == -1 {
					t.Fatal("the boot probe must drive the egress variant")
				}
				if docker.indexOf("exec", "-i", "bind") == -1 {
					t.Fatal("the boot probe must deliver a binding")
				}
				if docker.indexOf("network", "connect") == -1 {
					t.Fatal("the boot probe must connect the uplink")
				}
				if docker.indexOf("rm", "-f") == -1 {
					t.Fatal("the boot probe must tear its own sidecar down")
				}
				assertProbeRedeemedOneSecret(t, docker)
				return
			}
			if err == nil {
				t.Fatalf("Probe must refuse with %q%q, got nil", tt.wantErr, tt.wantErrContains)
			}
			if tt.wantAttempt != nil {
				if got := docker.indexOf(tt.wantAttempt...); got == -1 {
					t.Fatalf("a survivor refusal is legal only after removal was attempted: "+
						"no %q call was ever issued", tt.wantAttempt)
				}
				// And the refusal is the SWEEP's, taken before the address
				// space was ever looked at: under R-30's order the census
				// cannot have run, so a passing census here would mean the
				// old, deadlocking sequence is back.
				if got := docker.indexOf("network", "ls", "-q"); got != -1 {
					t.Fatalf("the CIDR census must not be reached before the sweep refuses (call %d)", got)
				}
			}
			if tt.wantErrContains != "" {
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("refusal:\n got %q\nmust contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("refusal:\n got %q\nwant %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// probeDaemon is a daemon whose topology is CORRECT: the uplink is a
// non-internal bridge with icc off, no network overlaps the invocation range,
// nothing labelled survives, and a sidecar comes up ready.
func probeDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	// The pair the runtime image and the homelab host actually ship
	// (docker-cli 28.3.3 in alpine 3.22, daemon 29.5.3).
	f.on([]string{"version", "--format"}, "28.3.3 29.5.3\n", 0)
	f.on([]string{"network", "inspect", "sentiae-node-egress-uplink"}, "bridge|false|false\n", 0)
	f.on([]string{"network", "ls", "-q"}, "netaaa\n", 0)
	f.on([]string{"network", "inspect", "netaaa"}, "sentiae-network|172.20.0.0/16 \n", 0)
	f.on([]string{"inspect", "runtime-container"},
		"sha256:036daa5efeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface\n", 0)
	f.on([]string{"exec", "wget"}, "ok", 0)
	// …and its sidecar's log is readable, carrying the anchor and no decision:
	// the boot probe's own Close drains it, and an unreadable log is a BLIND
	// refusal by design.
	f.on([]string{"logs"}, auditBoundLine+"\n", 0)
	// The probe's node-shaped container redeems the one bound secret and answers
	// the only line that means "a node reached this broker through the directory
	// and the socket": 200, no refusal code, and the empty answer that was bound.
	f.on(redeemCall, probeRedeemOK+"\n", 0)
	return f
}

// redeemCall is the token set that identifies the boot probe's redemption
// container. `run --rm` is the node launch line and `redeem` is the subcommand
// after the image; the sidecar's own `run -d` matches neither.
var redeemCall = []string{"run", "--rm", "redeem"}

var probeHandleRx = regexp.MustCompile(`^handle:[0-9a-f]{32}$`)

// assertProbeRedeemedOneSecret is the D-393 half of the boot gate. Before this
// decision the probe bound ZERO secrets, so `if len(b.Secrets) > 0` in the
// sidecar's apply() meant it never started a broker and nodebroker.Listen never
// ran: the service reported healthy at 02:17:38 on 2026-09-03 and the first
// secret-bearing invocation failed at 02:25:39. Guard coverage must be the
// population at risk.
//
// It asserts the whole shape, not just that a redemption happened: exactly one
// secret, bound with a real handle and NOTHING to leak (Found:false, empty
// value), redeemed AFTER the bind and BEFORE the teardown, over stdin with no
// environment, and with no handle anywhere on any argv.
//
// CONTROL (coverage): restore `Secrets: map[string]usecase.SecretAnswer{}` in
// bootProbeCycle — the one-secret assertion goes red and the probe is blind to
// the entire broker path again.
// CONTROL (uid class): dial the socket from this process instead of from
// probeRedeemArgs' container — the "a redeem container ran" assertion goes red.
// CONTROL (channel): pass the request as an argv token instead of on stdin —
// the stdin assertion and the no-handle-on-argv assertion both go red.
func assertProbeRedeemedOneSecret(t *testing.T, docker *fakeDaemon) {
	t.Helper()

	bindIdx := docker.indexOf("exec", "-i", "bind")
	if bindIdx == -1 {
		t.Fatal("the boot probe must deliver a binding")
	}
	var bound usecase.SidecarBinding
	if err := json.Unmarshal(docker.call(bindIdx).stdin, &bound); err != nil {
		t.Fatalf("the bind stdin is not a binding document: %v", err)
	}
	if len(bound.Secrets) != 1 {
		t.Fatalf("the boot probe must bind exactly ONE secret, got %d — with zero the sidecar "+
			"never starts a broker and the probe cannot fail on the socket at all", len(bound.Secrets))
	}
	answer, declared := bound.Secrets[probeSecretName]
	if !declared {
		t.Fatalf("the probe's secret must be named %q, got %v", probeSecretName, bound.Secrets)
	}
	if answer.Found || answer.Value != "" {
		t.Fatalf("the probe's secret must redeem NOTHING, got found=%v value=%q", answer.Found, answer.Value)
	}
	if !probeHandleRx.MatchString(answer.Handle) {
		t.Fatalf("the probe's handle %q does not match %s", answer.Handle, probeHandleRx)
	}

	redeemIdx := docker.indexOf(redeemCall...)
	if redeemIdx == -1 {
		t.Fatal("the boot probe must redeem its secret from a node-shaped container")
	}
	teardownIdx := docker.indexOf("rm", "-f")
	if teardownIdx == -1 {
		t.Fatal("the boot probe must tear its own sidecar down")
	}
	if !(bindIdx < redeemIdx && redeemIdx < teardownIdx) {
		t.Fatalf("order must be bind < redeem < teardown, got %d %d %d", bindIdx, redeemIdx, teardownIdx)
	}

	redeemCallRecord := docker.call(redeemIdx)
	if len(redeemCallRecord.env) != 0 {
		t.Fatalf("the redeem container must carry no environment, got %q", redeemCallRecord.env)
	}
	var request nodebroker.Request
	if err := json.Unmarshal(redeemCallRecord.stdin, &request); err != nil {
		t.Fatalf("the redeem stdin is not a broker request: %v", err)
	}
	if request.Handle != answer.Handle {
		t.Fatalf("the redemption presented %q, but the binding minted %q", request.Handle, answer.Handle)
	}
	if request.Invocation != bound.Invocation {
		t.Fatalf("the redemption named invocation %q, but the binding is %q",
			request.Invocation, bound.Invocation)
	}
	if request.Name != probeSecretName {
		t.Fatalf("the redemption named secret %q, want %q", request.Name, probeSecretName)
	}

	// The handle is a credential. It crossed on stdin twice and must appear on
	// no argv anywhere in the whole conversation — `docker inspect` and
	// /proc/<pid>/cmdline read argv, and R-21 F-3(i) greps for this prefix.
	for _, args := range docker.argv() {
		for _, arg := range args {
			if strings.Contains(arg, "handle:") {
				t.Fatalf("a handle reached the argv: %q", arg)
			}
		}
	}
}

// TestProbe_SweepsLeakedBridgeBeforeCIDRCheck is R-30, modelled on the daemon
// that produced it. A crash left one labelled invocation bridge on
// 10.201.250.0/29 with its labelled node container still attached and EXITED —
// the sidecar launch carries no --rm and both container classes carry
// sentiae.node.invocation, so a SIGKILL leaks the container as well as its
// bridge. Every invocation bridge is carved out of invocation_cidr by
// construction (subnet_pool.go), so the address-space check running FIRST
// refused on the runtime's own residue and the only step that could remove it
// never ran: the homelab crash-looped on `node runner: invocation cidr
// 10.201.0.0/16 overlaps docker network sentiae-inv-… (10.201.250.0/29)` through
// every restart, and no restart could ever end it.
//
// The fake behaves like docker in the two ways that make this a proof rather
// than a mock: `network rm` fails with "has active endpoints" until the attached
// container is gone, and every listing — both labelled ones and the unfiltered
// census — drops the object once it is actually removed.
//
// CONTROL: move the SweepAll block back below verifyInvocationCIDR — Probe
// refuses with the overlap error above and this test goes red.
func TestProbe_SweepsLeakedBridgeBeforeCIDRCheck(t *testing.T) {
	const (
		orphan      = "inv-p4-orphan"
		orphanNet   = "sentiae-inv-" + orphan
		orphanNetID = "netorphan"
		orphanCtr   = "sentiae-node-" + orphan
	)

	var mu sync.Mutex
	containerGone := false
	networkGone := false

	docker := probeDaemon()

	// The labelled container census: the exited node container until it is
	// force-removed, nothing after.
	docker.onFunc(
		func(args []string) bool {
			return containsAll(args, []string{"ps", "-a", "--filter", "label=" + labelInvocation})
		},
		func([]string) (string, string, int) {
			mu.Lock()
			defer mu.Unlock()
			if containerGone {
				return "", "", 0
			}
			return orphanCtr + "\n", "", 0
		},
	)
	docker.onFunc(
		func(args []string) bool { return containsAll(args, []string{"rm", "-f"}) },
		func([]string) (string, string, int) {
			mu.Lock()
			defer mu.Unlock()
			containerGone = true
			return "", "", 0
		},
	)

	// The labelled network census, and docker's real refusal: a bridge with an
	// attached container cannot be removed, however dead that container is.
	docker.onFunc(
		func(args []string) bool {
			return containsAll(args, []string{"network", "ls", "--filter", "label=" + labelInvocation})
		},
		func([]string) (string, string, int) {
			mu.Lock()
			defer mu.Unlock()
			if networkGone {
				return "", "", 0
			}
			return orphanNet + "\n", "", 0
		},
	)
	docker.onFunc(
		func(args []string) bool { return containsAll(args, []string{"network", "rm"}) },
		func([]string) (string, string, int) {
			mu.Lock()
			defer mu.Unlock()
			if !containerGone {
				return "", "Error response from daemon: error while removing network: network " +
					orphanNet + " id 0f1e2d has active endpoints", 1
			}
			networkGone = true
			return orphanNet + "\n", "", 0
		},
	)

	// The unfiltered census verifyInvocationCIDR reads. The leaked bridge is in
	// it until it is removed — which is the whole deadlock.
	docker.onFunc(
		func(args []string) bool { return containsAll(args, []string{"network", "ls", "-q"}) },
		func([]string) (string, string, int) {
			mu.Lock()
			defer mu.Unlock()
			if networkGone {
				return "netaaa\n", "", 0
			}
			return "netaaa\n" + orphanNetID + "\n", "", 0
		},
	)
	docker.on([]string{"network", "inspect", orphanNetID}, orphanNet+"|10.201.250.0/29 \n", 0)

	m := testManager(t, docker)
	m.image = "" // the probe resolves it, as it does at boot

	if err := m.Probe(context.Background()); err != nil {
		t.Fatalf("Probe must recover from its own leaked bridge, got: %v", err)
	}

	removeIdx := docker.indexOf("rm", "-f")
	networkRemoveIdx := docker.indexOf("network", "rm")
	censusIdx := docker.indexOf("network", "ls", "-q")
	if removeIdx == -1 {
		t.Fatal("the leaked container was never removed")
	}
	if networkRemoveIdx == -1 {
		t.Fatal("the leaked bridge was never removed")
	}
	if censusIdx == -1 {
		t.Fatal("the address space was never checked")
	}
	if !(removeIdx < networkRemoveIdx && networkRemoveIdx < censusIdx) {
		t.Fatalf("order must be rm -f < network rm < network ls -q, got %d %d %d",
			removeIdx, networkRemoveIdx, censusIdx)
	}

	cycleIdx := docker.indexOf("run", "-d")
	if cycleIdx == -1 {
		t.Fatal("the boot probe cycle must still run after the sweep and the census")
	}
	if cycleIdx < censusIdx {
		t.Fatalf("the cycle must run last, got cycle=%d census=%d", cycleIdx, censusIdx)
	}
}

// TestProbe_Order pins the boot sequence itself on a daemon where every step
// passes. The order is a property in its own right: R-30 was a defect in which
// each individual check was correct and their sequence was not, so a green
// probe proves nothing about it and only the indices do.
//
// CONTROL: swap any adjacent pair of the five steps in Probe — the index
// comparison goes red.
func TestProbe_Order(t *testing.T) {
	docker := probeDaemon()
	m := testManager(t, docker)
	m.image = "" // the probe resolves it, as it does at boot

	if err := m.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	steps := []struct {
		name   string
		tokens []string
	}{
		// R-27: the CLI that issues every line below.
		{"cli", []string{"version", "--format"}},
		// R-14: deploy-owned static infrastructure, verified before mutation.
		{"uplink", []string{"network", "inspect", "sentiae-node-egress-uplink"}},
		// R-30: the sweep's own first census — it must precede the address check.
		{"sweep", []string{"ps", "-a", "--filter", "label=" + labelInvocation}},
		// R-25: every network REMAINING after that sweep.
		{"cidr", []string{"network", "ls", "-q"}},
		// R-24: the full cycle last; it registers the first live invocation.
		{"cycle", []string{"run", "-d"}},
	}

	previous := -1
	for _, step := range steps {
		idx := docker.indexOf(step.tokens...)
		if idx == -1 {
			t.Fatalf("step %q never ran; the boot order is cli < uplink < sweep < cidr < cycle", step.name)
		}
		if idx <= previous {
			t.Fatalf("step %q ran at call %d, after a step that must follow it (call %d); "+
				"the boot order is cli < uplink < sweep < cidr < cycle", step.name, idx, previous)
		}
		previous = idx
	}
}

// TestVerifyInvocationCIDR_IncludesLabelledNetworks is R-25's predicate, which
// R-30 did NOT amend: the check refuses on EVERY network still present, and a
// network carrying sentiae.node.invocation is not exempt. A survivor of the boot
// sweep still occupies pool address space, and one created after the sweep must
// not slip past the boot guard into a per-invocation failure at run time.
//
// CONTROL: skip networks whose name starts with sentiae-inv- in
// verifyInvocationCIDR — this test goes red.
func TestVerifyInvocationCIDR_IncludesLabelledNetworks(t *testing.T) {
	docker := &fakeDaemon{}
	docker.on([]string{"network", "ls", "-q"}, "netaaa\nnetorphan\n", 0)
	docker.on([]string{"network", "inspect", "netaaa"}, "sentiae-network|172.20.0.0/16 \n", 0)
	docker.on([]string{"network", "inspect", "netorphan"},
		"sentiae-inv-inv-p4-orphan|10.201.250.0/29 \n", 0)

	m := testManager(t, docker)

	err := m.verifyInvocationCIDR(context.Background())
	if err == nil {
		t.Fatal("a labelled invocation network inside the range must still refuse")
	}
	want := "node runner: invocation cidr 10.201.0.0/16 overlaps docker network " +
		"sentiae-inv-inv-p4-orphan (10.201.250.0/29)"
	if err.Error() != want {
		t.Fatalf("refusal:\n got %q\nwant %q", err.Error(), want)
	}
}

// TestProbe_DockerCLIFloor is the runtime half of the P4 §9 guard. The deploy
// refused to boot with `unexpected key 'volume-subpath'` because the CLI that
// issues every docker-out-of-docker line is the one INSIDE the image (alpine
// 3.19 → 25.0.5), while the measurement that cleared the feature was taken on
// the HOST (29.5.3). The Dockerfile now holds a build-time floor; this is the
// runtime refusal, because an image can be run against any daemon and its CLI
// can be replaced in place.
//
// A major skew is a LOG and never a refusal: docker negotiates its API version,
// so any fixed maximum skew would be an invented rule — the gap is a discovery
// signal instead.
//
// CONTROL (floor): delete the verifyDockerCLI call from Probe — the 25.0.5 rows
// stop refusing and report "must refuse …, got nil".
// CONTROL (R-30 order): move the SweepAll block ahead of verifyDockerCLI — the
// refused rows sweep with a CLI the process just refused to trust, and the
// "must not sweep" assertion goes red.
// CONTROL (skew): delete the skew branch — the legal-skew row reports the
// missing docker_cli_skew log.
// CONTROL (fail-closed): return nil instead of the unreadable-version error —
// the malformed rows pass wrongly.
func TestProbe_DockerCLIFloor(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		wantErr  string
		wantSkew bool
	}{
		{
			name:    "the image's own CLI is below the floor",
			version: "25.0.5 25.0.5\n",
			wantErr: "node runner: docker CLI 25.0.5 is older than the minimum 26.0 required for --mount volume-subpath",
		},
		{
			name:    "the real §9 case: image CLI 25.0.5 against host daemon 29.5.3",
			version: "25.0.5 29.5.3\n",
			wantErr: "node runner: docker CLI 25.0.5 is older than the minimum 26.0 required for --mount volume-subpath",
		},
		{
			name:    "the shipped pair: 28.3.3 against 29.5.3 — one major apart, no signal",
			version: "28.3.3 29.5.3\n",
		},
		{
			name:     "a large but legal skew is a signal, not a refusal",
			version:  "26.1.4 41.0.1\n",
			wantSkew: true,
		},
		{
			name:    "unparseable output is a refusal, and says what it saw",
			version: "Client: 28.3.3\n",
			wantErr: `node runner: unreadable docker version "Client: 28.3.3": want a <client> <server> version pair`,
		},
		{
			name:    "an empty version line is a refusal",
			version: "\n",
			wantErr: `node runner: unreadable docker version "": want a <client> <server> version pair`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docker := probeDaemon()
			// Registered last, so it overrides probeDaemon's healthy answer.
			docker.on([]string{"version", "--format"}, tt.version, 0)
			m := testManager(t, docker)
			m.image = "" // the probe resolves it, as it does at boot

			sink := &lockedBuffer{}
			ctx := logger.NewContext(context.Background(),
				logger.New(logger.Config{Level: "debug", Format: "json", Writer: sink}))

			err := m.Probe(ctx)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Probe must refuse with %q, got nil", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("refusal:\n got %q\nwant %q", err.Error(), tt.wantErr)
				}
				// The floor is checked BEFORE the cycle it protects: a refused
				// CLI must never have launched a sidecar.
				if got := docker.indexOf("run", "-d"); got != -1 {
					t.Fatalf("a refused docker CLI must launch nothing (call %d)", got)
				}
				// And before the sweep it protects (R-30). The sweep MUTATES
				// this daemon; a CLI this process has already refused to trust
				// is not the CLI to force-remove containers and bridges with.
				if got := docker.indexOf("ps", "-a", "--filter", "label="+labelInvocation); got != -1 {
					t.Fatalf("a refused docker CLI must not sweep (call %d)", got)
				}
				if got := docker.indexOf("rm", "-f"); got != -1 {
					t.Fatalf("a refused docker CLI must remove nothing (call %d)", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			// The version check does not replace the end-to-end proof.
			if docker.indexOf("run", "-d") == -1 {
				t.Fatal("the boot probe cycle must still run after the version check")
			}
			logged := strings.Contains(sink.String(), `"msg":"docker_cli_skew"`)
			if logged != tt.wantSkew {
				t.Fatalf("docker_cli_skew logged=%v, want %v; logs:\n%s", logged, tt.wantSkew, sink.String())
			}
			if tt.wantSkew {
				client, server, _ := strings.Cut(strings.TrimSpace(tt.version), " ")
				for _, want := range []string{`"client":"` + client + `"`, `"server":"` + server + `"`} {
					if !strings.Contains(sink.String(), want) {
						t.Fatalf("docker_cli_skew must carry %s; logs:\n%s", want, sink.String())
					}
				}
			}
		})
	}
}

// lockedBuffer is a race-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// T4.15 — TestSweeper_TouchesOnlyOrphanSidecars is the safety property R-24
// exists for. The periodic sweeper runs while work is in flight, so it may
// remove ONLY sidecars whose invocation this process does not have live — and
// NEVER a node container, which carries the same invocation label but is the
// hostile workload itself, mid-run.
//
// CONTROL: drop the `name=sentiae-sc-` filter from the sweeper's container
// query — the daemon then also lists the live node container, the sweeper
// removes it, and the "no node container was touched" assertion goes red.
func TestSweeper_TouchesOnlyOrphanSidecars(t *testing.T) {
	live := "inv-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	orphan := "inv-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	node := "inv-cccccccc-cccc-cccc-cccc-cccccccccccc"

	docker := &fakeDaemon{}
	// The daemon models docker's own filtering: the node container is listed
	// ONLY when the name filter is absent.
	docker.onFunc(
		func(args []string) bool { return containsAll(args, []string{"ps", "-a"}) },
		func(args []string) (string, string, int) {
			listed := []string{"sentiae-sc-" + live, "sentiae-sc-" + orphan}
			if !containsAll(args, []string{"name=sentiae-sc-"}) {
				listed = append(listed, "sentiae-node-"+node)
			}
			return strings.Join(listed, "\n") + "\n", "", 0
		},
	)
	docker.on([]string{"network", "ls", "--filter"},
		"sentiae-inv-"+live+"\nsentiae-inv-"+orphan+"\n", 0)
	docker.on([]string{"logs"}, auditBoundLine+"\n", 0)

	m := testManager(t, docker)
	m.live[live] = liveInvocation{run: uuid.New(), bridge: true}

	m.sweepOrphans(context.Background())

	removedContainers := removedNames(docker, "rm", "-f")
	if !reflect.DeepEqual(removedContainers, []string{"sentiae-sc-" + orphan}) {
		t.Fatalf("removed containers: got %q, want only the orphan sidecar", removedContainers)
	}
	for _, name := range removedContainers {
		if strings.HasPrefix(name, "sentiae-node-") {
			t.Fatalf("the sweeper removed a NODE container: %q", name)
		}
	}

	removedNetworks := removedNames(docker, "network", "rm")
	if !reflect.DeepEqual(removedNetworks, []string{"sentiae-inv-" + orphan}) {
		t.Fatalf("removed networks: got %q, want only the orphan bridge", removedNetworks)
	}
}

// removedNames lists the targets of every removal call of one shape.
func removedNames(docker *fakeDaemon, tokens ...string) []string {
	var out []string
	for _, args := range docker.argv() {
		if containsAll(args, tokens) && len(args) > 0 {
			out = append(out, args[len(args)-1])
		}
	}
	return out
}

// TestSweepRun_ContainersBeforeNetworks pins the teardown order a completed,
// cancelled or timed-out run gets: containers first, then networks, then the
// directories — a network cannot be removed while a container is attached, and
// a directory is only an orphan once nothing can still be writing to it.
//
// CONTROL: swap the two removal blocks in SweepRun — the order assertion fails.
func TestSweepRun_ContainersBeforeNetworks(t *testing.T) {
	runID := uuid.New()
	invocation := "inv-dddddddd-dddd-dddd-dddd-dddddddddddd"

	docker := &fakeDaemon{}
	docker.on([]string{"ps", "-a"}, "sentiae-sc-"+invocation+"\nsentiae-node-"+invocation+"\n", 0)
	docker.on([]string{"network", "ls"}, "sentiae-inv-"+invocation+"\n", 0)
	docker.on([]string{"logs"}, auditBoundLine+"\n", 0)

	m := testManager(t, docker)
	m.live[invocation] = liveInvocation{run: runID, bridge: true}

	if err := m.SweepRun(context.Background(), runID); err != nil {
		t.Fatalf("SweepRun: %v", err)
	}

	firstRemove := docker.indexOf("rm", "-f")
	firstNetworkRemove := docker.indexOf("network", "rm")
	if firstRemove == -1 || firstNetworkRemove == -1 {
		t.Fatalf("both classes must be swept: container=%d network=%d", firstRemove, firstNetworkRemove)
	}
	if firstRemove > firstNetworkRemove {
		t.Fatal("containers must be removed before networks")
	}
	// A run sweep DOES remove the run's node containers — that is what it is
	// for. Only the periodic sweeper is forbidden from touching them.
	removed := removedNames(docker, "rm", "-f")
	if !slices.Contains(removed, "sentiae-node-"+invocation) {
		t.Fatalf("the run sweep must remove the run's node container, got %q", removed)
	}
	if m.isLive(invocation) {
		t.Fatal("a swept invocation must no longer be live")
	}
}

// ── the egress audit drain (D-395) ─────────────────────────────────────────

// egressOpen is one invocation with a bridge: the shape whose sidecar can hold
// egress decisions at all.
func egressOpen(invocation string, runID uuid.UUID) usecase.SidecarOpen {
	return usecase.SidecarOpen{
		InvocationID: invocation,
		RunID:        runID,
		Node:         "echo",
		Binding: usecase.SidecarBinding{
			Invocation: invocation, Run: runID.String(), Node: "echo",
			Secrets: map[string]usecase.SecretAnswer{},
			Egress: &usecase.EgressBinding{
				Patterns: []string{"httpbin.org"},
				Token:    "b3f1c0d2e4a5968778695a4e3c2d1b0af9e8d7c6b5a4938271605f4e3d2c1b0a",
				Subnet:   "10.201.0.0/29",
			},
		},
	}
}

// TestSidecarManager_DrainsAuditBeforeRemove is the whole point of D-395: the
// container's log is the write-ahead buffer, so the ONLY safe order is stop (to
// close the unflushed-tail window) → logs → record → rm. Removing first deletes
// the only copy of what the tenant's node was allowed to reach.
//
// CONTROL: delete the drainAudit call in close() — no logs call, red.
// CONTROL: move the `rm -f` above the drain — the order assertion, red.
func TestSidecarManager_DrainsAuditBeforeRemove(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-11111111-1111-1111-1111-111111111111"
	name := "sentiae-sc-" + invocation

	docker := readySidecarDaemon()
	docker.on([]string{"logs"}, auditBoundLine+"\n"+
		decisionLine(1, "allow", "manifest_wildcard", "httpbin.org", 443, runID.String(), invocation)+"\n"+
		decisionLine(2, "allow", "manifest_wildcard", "httpbin.org", 443, runID.String(), invocation)+"\n", 0)

	audit := &fakeAudit{}
	m := testManagerWithAudit(t, docker, audit)

	if _, err := m.Open(context.Background(), egressOpen(invocation, runID)); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(context.Background(), invocation); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stopIdx := docker.indexOf("stop", "-t", "2", name)
	logsIdx := docker.indexOf("logs", name)
	rmIdx := docker.indexOf("rm", "-f", name)
	if stopIdx == -1 || logsIdx == -1 || rmIdx == -1 {
		t.Fatalf("missing step: stop=%d logs=%d rm=%d", stopIdx, logsIdx, rmIdx)
	}
	if !(stopIdx < logsIdx && logsIdx < rmIdx) {
		t.Fatalf("order must be stop < logs < rm, got %d %d %d", stopIdx, logsIdx, rmIdx)
	}

	got := audit.recorded()
	want := []domain.EgressDecision{{
		RunID: runID, InvocationID: invocation, Node: "echo",
		Decision: domain.EgressAllow, Reason: "manifest_wildcard",
		Host: "httpbin.org", Port: 443, Hits: 2,
		FirstAt: time.Date(2026, 9, 7, 10, 0, 1, 0, time.UTC),
		LastAt:  time.Date(2026, 9, 7, 10, 0, 2, 0, time.UTC),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded:\n got %+v\nwant %+v", got, want)
	}
}

// TestSidecarManager_AuditLossIsLoud pins the four ways the record can fail to
// land. Every one of them is counted on its own series, logged, and RETURNED —
// an audit that fails silently is worse than no audit, because the metric would
// read as "nothing was ever lost".
//
// The write row is the one with retention: the store refused, so the log is
// still the only copy and the container must SURVIVE (stopped, detached from
// both networks) for the sweeper to retry. The other three have already lost or
// never had the record, so holding the container would only leak a bridge.
//
// CONTROL: return nil from auditLost — every row goes red on the error.
// CONTROL: always `rm -f` in close() — the write row's survival assertion, red.
func TestSidecarManager_AuditLossIsLoud(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-11111111-1111-1111-1111-111111111111"
	name := "sentiae-sc-" + invocation
	line1 := decisionLine(1, "deny", "host_not_declared", "example.com", 443, runID.String(), invocation)
	line3 := decisionLine(3, "deny", "host_not_declared", "example.com", 443, runID.String(), invocation)
	sinkErr := errors.New("db down")

	for _, tt := range []struct {
		name       string
		logs       string
		sinkErr    error
		wantErr    error
		wantReason string
		wantRetain bool
	}{
		{"blind", line1 + "\n", nil, domain.ErrEgressAuditBlind, "blind", false},
		{"gap", auditBoundLine + "\n" + line1 + "\n" + line3 + "\n", nil, domain.ErrEgressAuditGap, "gap", false},
		{"malformed", auditBoundLine + "\nnot json\n", nil, domain.ErrEgressAuditMalformed, "malformed", false},
		{"write", auditBoundLine + "\n" + line1 + "\n", sinkErr, sinkErr, "write", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			docker := readySidecarDaemon()
			docker.on([]string{"logs"}, tt.logs, 0)
			audit := &fakeAudit{err: tt.sinkErr}
			m := testManagerWithAudit(t, docker, audit)

			before := testutil.ToFloat64(auditFailures.WithLabelValues(tt.wantReason))

			if _, err := m.Open(context.Background(), egressOpen(invocation, runID)); err != nil {
				t.Fatalf("Open: %v", err)
			}
			err := m.Close(context.Background(), invocation)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Close error: got %v, want %v", err, tt.wantErr)
			}
			if delta := testutil.ToFloat64(auditFailures.WithLabelValues(tt.wantReason)) - before; delta != 1 {
				t.Fatalf("node_egress_audit_failures_total{reason=%q} delta: got %v, want 1", tt.wantReason, delta)
			}
			if got := len(audit.recorded()); got != 0 {
				t.Fatalf("nothing may be recorded when the drain fails, got %d rows", got)
			}

			rmIdx := docker.indexOf("rm", "-f", name)
			if tt.wantRetain {
				if rmIdx != -1 {
					t.Fatalf("a sidecar whose rows were refused must SURVIVE for the retry (removed at call %d)", rmIdx)
				}
				bridgeIdx := docker.indexOf("network", "disconnect", "-f", "sentiae-inv-"+invocation, name)
				uplinkIdx := docker.indexOf("network", "disconnect", "-f", "sentiae-node-egress-uplink", name)
				netRmIdx := docker.indexOf("network", "rm", "sentiae-inv-"+invocation)
				if bridgeIdx == -1 || uplinkIdx == -1 || netRmIdx == -1 {
					t.Fatalf("missing step: bridge=%d uplink=%d network rm=%d", bridgeIdx, uplinkIdx, netRmIdx)
				}
				if !(bridgeIdx < netRmIdx && uplinkIdx < netRmIdx) {
					t.Fatalf("both attachments must be released BEFORE the bridge is removed, got %d %d %d",
						bridgeIdx, uplinkIdx, netRmIdx)
				}
				return
			}
			if rmIdx == -1 {
				t.Fatal("a sidecar whose record cannot be recovered must still be removed")
			}
		})
	}
}

// TestSidecarManager_MissingSidecarIsNotAnAuditLoss — a container that never
// existed (a secrets-only invocation swept twice, an Open that failed before the
// run) destroyed no record, so it must not count as one. A drain that counted
// every absent container would make the failure metric meaningless.
//
// CONTROL: drop the "No such container" branch in drainAudit — the counter
// assertion goes red.
func TestSidecarManager_MissingSidecarIsNotAnAuditLoss(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-11111111-1111-1111-1111-111111111111"

	docker := readySidecarDaemon()
	docker.onFunc(
		func(args []string) bool { return containsAll(args, []string{"logs"}) },
		func([]string) (string, string, int) {
			return "", "Error response from daemon: No such container: sentiae-sc-" + invocation, 1
		},
	)
	audit := &fakeAudit{}
	m := testManagerWithAudit(t, docker, audit)

	before := map[string]float64{}
	for _, reason := range auditFailureReasons {
		before[reason] = testutil.ToFloat64(auditFailures.WithLabelValues(reason))
	}
	if _, err := m.Open(context.Background(), egressOpen(invocation, runID)); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(context.Background(), invocation); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, reason := range auditFailureReasons {
		if got := testutil.ToFloat64(auditFailures.WithLabelValues(reason)); got != before[reason] {
			t.Fatalf("reason %q moved (%v → %v): an absent container destroyed no record", reason, before[reason], got)
		}
	}
}

// TestSidecarManager_OpenFailureDoesNotDrain — Open's own teardown runs BEFORE
// the node was ever launched, so that sidecar can hold no decision. Draining it
// would spend two docker calls on every failed open and, worse, would make a
// blind refusal out of a container that never had anything to say.
//
// CONTROL: pass drain=true on Open's failure path — the logs/stop assertions go red.
func TestSidecarManager_OpenFailureDoesNotDrain(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-11111111-1111-1111-1111-111111111111"

	docker := readySidecarDaemon()
	docker.on([]string{"exec", "-i", "bind"}, "", 1)
	m := testManagerWithAudit(t, docker, &fakeAudit{})

	if _, err := m.Open(context.Background(), egressOpen(invocation, runID)); err == nil {
		t.Fatal("a failed bind must fail the open")
	}
	if got := docker.indexOf("logs"); got != -1 {
		t.Fatalf("Open's teardown must not read a log that cannot exist (call %d)", got)
	}
	if got := docker.indexOf("stop"); got != -1 {
		t.Fatalf("Open's teardown must not stop-then-read (call %d)", got)
	}
	if docker.indexOf("rm", "-f", "sentiae-sc-"+invocation) == -1 {
		t.Fatal("Open's teardown must still remove the half-open sidecar")
	}
}

// TestSweep_DrainsSidecarsOnly — the sweeps are removal paths too, and after a
// restart they are the ONLY ones: nothing else will ever read those logs. A node
// container is not a sidecar and has no audit; draining one would be a log read
// of tenant code's stdout.
//
// CONTROL: remove the drain from removeContainers — both order assertions red.
func TestSweep_DrainsSidecarsOnly(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	sidecar := "sentiae-sc-" + invocation
	node := "sentiae-node-" + invocation
	logs := auditBoundLine + "\n" +
		decisionLine(1, "deny", "host_not_declared", "example.com", 443, runID.String(), invocation) + "\n"

	newDaemon := func() *fakeDaemon {
		f := &fakeDaemon{}
		f.on([]string{"ps", "-a"}, sidecar+"\n"+node+"\n", 0)
		f.on([]string{"network", "ls"}, "", 0)
		f.on([]string{"logs"}, logs, 0)
		return f
	}

	assertDrained := func(t *testing.T, docker *fakeDaemon, audit *fakeAudit) {
		t.Helper()
		logsIdx := docker.indexOf("logs", sidecar)
		rmIdx := docker.indexOf("rm", "-f", sidecar)
		if logsIdx == -1 || rmIdx == -1 {
			t.Fatalf("the sidecar must be drained then removed: logs=%d rm=%d", logsIdx, rmIdx)
		}
		if logsIdx > rmIdx {
			t.Fatal("the sidecar's log must be read BEFORE the container is removed")
		}
		if got := docker.indexOf("logs", node); got != -1 {
			t.Fatalf("a NODE container's log was read (call %d)", got)
		}
		if got := len(audit.recorded()); got != 1 {
			t.Fatalf("recorded rows: got %d, want 1", got)
		}
	}

	t.Run("SweepAll", func(t *testing.T) {
		docker := newDaemon()
		audit := &fakeAudit{}
		m := testManagerWithAudit(t, docker, audit)
		if _, _, err := m.SweepAll(context.Background()); err != nil {
			t.Fatalf("SweepAll: %v", err)
		}
		assertDrained(t, docker, audit)
	})

	t.Run("SweepRun", func(t *testing.T) {
		docker := newDaemon()
		audit := &fakeAudit{}
		m := testManagerWithAudit(t, docker, audit)
		if err := m.SweepRun(context.Background(), runID); err != nil {
			t.Fatalf("SweepRun: %v", err)
		}
		assertDrained(t, docker, audit)
	})
}

// TestSweeper_RetriesRetainedAuditSidecar is the deferral half of D-395: when
// Postgres refuses the write, the record is NOT lost — the stopped sidecar and
// its log stay, and every 60 s tick tries again until the INSERT lands. Without
// the retry the retention would merely leak a container.
//
// CONTROL: always `rm -f` in sweepOrphans — tick 2 records nothing, red.
func TestSweeper_RetriesRetainedAuditSidecar(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	invocation := "inv-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	name := "sentiae-sc-" + invocation

	docker := &fakeDaemon{}
	docker.on([]string{"ps", "-a"}, name+"\n", 0)
	docker.on([]string{"network", "ls"}, "", 0)
	docker.on([]string{"logs"}, auditBoundLine+"\n"+
		decisionLine(1, "deny", "host_not_declared", "example.com", 443, runID.String(), invocation)+"\n", 0)

	audit := &fakeAudit{err: errors.New("db down")}
	m := testManagerWithAudit(t, docker, audit)

	before := testutil.ToFloat64(auditFailures.WithLabelValues("write"))

	m.sweepOrphans(context.Background())
	if got := docker.indexOf("rm", "-f", name); got != -1 {
		t.Fatalf("tick 1: the only copy of the record was destroyed (call %d)", got)
	}
	if delta := testutil.ToFloat64(auditFailures.WithLabelValues("write")) - before; delta != 1 {
		t.Fatalf("tick 1: write failures delta: got %v, want 1", delta)
	}

	audit.mu.Lock()
	audit.err = nil
	audit.mu.Unlock()

	m.sweepOrphans(context.Background())
	if docker.indexOf("rm", "-f", name) == -1 {
		t.Fatal("tick 2: once the write lands the sidecar must be removed")
	}
	if got := len(audit.recorded()); got != 1 {
		t.Fatalf("tick 2: recorded rows: got %d, want 1", got)
	}
}

// TestParseSidecarAudit pins the parser's aggregation AND its strictness. The
// refusals matter more than the happy path: a drain that skipped a line it could
// not read would report fewer decisions than the node actually made, and the
// resulting table would be a quiet lie.
//
// CONTROL: skip a malformed line instead of returning — the malformed rows go red.
// CONTROL: drop the `!anchored` check — the blind row goes red.
func TestParseSidecarAudit(t *testing.T) {
	run := "22222222-2222-2222-2222-222222222222"
	runID := uuid.MustParse(run)
	inv := "inv-11111111-1111-1111-1111-111111111111"
	allow := decisionLine(1, "allow", "manifest_exact", "httpbin.org", 443, run, inv)
	deny := decisionLine(2, "deny", "host_not_declared", "example.com", 443, run, inv)

	t.Run("two tuples in first-seen order", func(t *testing.T) {
		got, err := ParseSidecarAudit([]byte(auditBoundLine + "\n" + allow + "\n" + deny + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := []domain.EgressDecision{
			{RunID: runID, InvocationID: inv, Node: "echo", Decision: domain.EgressAllow,
				Reason: "manifest_exact", Host: "httpbin.org", Port: 443, Hits: 1,
				FirstAt: time.Date(2026, 9, 7, 10, 0, 1, 0, time.UTC),
				LastAt:  time.Date(2026, 9, 7, 10, 0, 1, 0, time.UTC)},
			{RunID: runID, InvocationID: inv, Node: "echo", Decision: domain.EgressDeny,
				Reason: "host_not_declared", Host: "example.com", Port: 443, Hits: 1,
				FirstAt: time.Date(2026, 9, 7, 10, 0, 2, 0, time.UTC),
				LastAt:  time.Date(2026, 9, 7, 10, 0, 2, 0, time.UTC)},
		}
		if !reflect.DeepEqual(got.Decisions, want) {
			t.Fatalf("decisions:\n got %+v\nwant %+v", got.Decisions, want)
		}
		if got.Capped {
			t.Fatal("nothing capped this log")
		}
	})

	t.Run("the same tuple three times is one row with min and max times", func(t *testing.T) {
		lines := auditBoundLine + "\n" +
			decisionLine(1, "deny", "host_not_declared", "example.com", 443, run, inv) + "\n" +
			decisionLine(9, "deny", "host_not_declared", "example.com", 443, run, inv) + "\n" +
			decisionLine(5, "deny", "host_not_declared", "example.com", 443, run, inv) + "\n"
		// seq 1,9,5 has a hole, so renumber to a complete 1..3 run with the
		// timestamps deliberately out of order.
		lines = strings.Replace(lines, `"seq":9`, `"seq":2`, 1)
		lines = strings.Replace(lines, `"seq":5`, `"seq":3`, 1)
		got, err := ParseSidecarAudit([]byte(lines))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got.Decisions) != 1 || got.Decisions[0].Hits != 3 {
			t.Fatalf("aggregation: %+v", got.Decisions)
		}
		if !got.Decisions[0].FirstAt.Equal(time.Date(2026, 9, 7, 10, 0, 1, 0, time.UTC)) ||
			!got.Decisions[0].LastAt.Equal(time.Date(2026, 9, 7, 10, 0, 9, 0, time.UTC)) {
			t.Fatalf("first/last: %+v", got.Decisions[0])
		}
	})

	t.Run("a capped log marks every row", func(t *testing.T) {
		capped := `{"time":"2026-09-07T10:00:03Z","level":"WARN","msg":"egress_audit_capped","invocation_id":"` +
			inv + `","node":"echo","run_id":"` + run + `","cap":10000}`
		got, err := ParseSidecarAudit([]byte(auditBoundLine + "\n" + allow + "\n" + deny + "\n" + capped + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !got.Capped {
			t.Fatal("the audit must be marked capped")
		}
		for _, d := range got.Decisions {
			if !d.Capped {
				t.Fatalf("every row of a capped log is a floor: %+v", d)
			}
		}
	})

	t.Run("an anchor with no decision is an empty audit, not an error", func(t *testing.T) {
		got, err := ParseSidecarAudit([]byte(auditBoundLine + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got.Decisions) != 0 {
			t.Fatalf("decisions: %+v", got.Decisions)
		}
	})

	for _, tt := range []struct {
		name  string
		log   string
		wantE error
	}{
		{"no anchor is blind", allow + "\n", domain.ErrEgressAuditBlind},
		{"a repeated seq is a gap", auditBoundLine + "\n" + allow + "\n" + allow + "\n", domain.ErrEgressAuditGap},
		{"a missing seq is a gap", auditBoundLine + "\n" +
			decisionLine(1, "deny", "host_not_declared", "example.com", 443, run, inv) + "\n" +
			decisionLine(2, "deny", "host_not_declared", "other.example", 443, run, inv) + "\n" +
			decisionLine(4, "deny", "host_not_declared", "third.example", 443, run, inv) + "\n",
			domain.ErrEgressAuditGap},
		{"a seq of zero is a gap", strings.Replace(auditBoundLine+"\n"+allow+"\n", `"seq":1`, `"seq":0`, 1),
			domain.ErrEgressAuditGap},
		{"a line that is not JSON is malformed", auditBoundLine + "\nnot json\n", domain.ErrEgressAuditMalformed},
		{"an over-long host is malformed", auditBoundLine + "\n" +
			decisionLine(1, "deny", "host_not_declared", strings.Repeat("a", 254), 443, run, inv) + "\n",
			domain.ErrEgressAuditMalformed},
		{"an unknown verdict is malformed", auditBoundLine + "\n" +
			decisionLine(1, "maybe", "host_not_declared", "example.com", 443, run, inv) + "\n",
			domain.ErrEgressAuditMalformed},
		{"a run id that is not a uuid is malformed", auditBoundLine + "\n" +
			decisionLine(1, "deny", "host_not_declared", "example.com", 443, "x", inv) + "\n",
			domain.ErrEgressAuditMalformed},
		{"a redaction flag that disagrees with the host is malformed", auditBoundLine + "\n" +
			strings.Replace(decisionLine(1, "deny", "host_not_declared", "[redacted]", 443, run, inv),
				`"host_redacted":false`, `"host_redacted":false`, 1) + "\n",
			domain.ErrEgressAuditMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSidecarAudit([]byte(tt.log))
			if !errors.Is(err, tt.wantE) {
				t.Fatalf("error: got %v, want %v", err, tt.wantE)
			}
			if len(got.Decisions) != 0 {
				t.Fatalf("a refused log yields nothing, got %+v", got.Decisions)
			}
		})
	}

	t.Run("a redacted host with its flag set parses", func(t *testing.T) {
		line := strings.Replace(decisionLine(1, "deny", "host_not_declared", "[redacted]", 443, run, inv),
			`"host_redacted":false`, `"host_redacted":true`, 1)
		got, err := ParseSidecarAudit([]byte(auditBoundLine + "\n" + line + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got.Decisions) != 1 || !got.Decisions[0].HostRedacted ||
			got.Decisions[0].Host != domain.RedactedEgressHost {
			t.Fatalf("redacted row: %+v", got.Decisions)
		}
	})
}

// TestNewSidecarManager_PreRegistersFailureSeries — the alert on this metric is
// the only thing that turns a lost audit into a page, and a promauto counter
// with no observation exports NO SERIES AT ALL. An absent series is not "no
// failures"; it is "no signal", and the two are indistinguishable to the query.
// Every reason therefore has to exist from the first scrape.
//
// It asserts PRESENCE, not the value: other tests in this process increment the
// same global counters, and asserting 0 would only be true of the first test to
// run.
//
// CONTROL: delete the pre-registration loop in NewSidecarManager — the "read"
// series (which no test ever increments) is missing, red.
func TestNewSidecarManager_PreRegistersFailureSeries(t *testing.T) {
	_ = testManager(t, &fakeDaemon{})

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	present := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "node_egress_audit_failures_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					present[l.GetValue()] = true
				}
			}
		}
	}
	for _, reason := range auditFailureReasons {
		if !present[reason] {
			t.Fatalf("node_egress_audit_failures_total{reason=%q} exports no series: an absent series reads as no failures", reason)
		}
	}
}
