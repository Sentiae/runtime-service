//go:build unit

package container

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"

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

func testManager(t *testing.T, docker *fakeDaemon) *SidecarManager {
	t.Helper()
	pool, err := usecase.NewSubnetPool("10.201.0.0/16", 29)
	if err != nil {
		t.Fatalf("subnet pool: %v", err)
	}
	m, err := NewSidecarManager(testNodeRunnerConfig(t), pool)
	if err != nil {
		t.Fatalf("new sidecar manager: %v", err)
	}
	m.docker = docker.fn
	m.self = "runtime-container"
	m.image = "sha256:036daa5efeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	return m
}

// readySidecarDaemon answers the readiness poll and nothing else.
func readySidecarDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	return f.on([]string{"exec", "wget"}, "ok", 0)
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
// CONTROL (CIDR): scope verifyInvocationCIDR to the uplink alone (R-14's
// original scope) — the "unrelated network" row passes wrongly, and on the
// homelab every egress invocation would instead fail at run time with docker's
// "Pool overlaps with other one on this address space" (R-25).
func TestProbe_Refusals(t *testing.T) {
	uplink := []string{"network", "inspect", "sentiae-node-egress-uplink"}

	tests := []struct {
		name    string
		daemon  func() *fakeDaemon
		wantErr string
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
			wantErr: "node runner: orphan sweep incomplete: 0 container(s), 1 network(s) remain",
		},
		{
			name: "an orphan container survives the boot sweep",
			daemon: func() *fakeDaemon {
				f := probeDaemon()
				f.on([]string{"ps", "-a", "--filter", "label=sentiae.node.invocation"},
					"sentiae-sc-inv-old\n", 0)
				return f
			},
			wantErr: "node runner: orphan sweep incomplete: 1 container(s), 0 network(s) remain",
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
			if tt.wantErr == "" {
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
				return
			}
			if err == nil {
				t.Fatalf("Probe must refuse with %q, got nil", tt.wantErr)
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
	return f
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
