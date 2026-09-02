//go:build unit

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

const (
	// canaryValue is the string that must not appear ANYWHERE the sidecar
	// writes. It is deliberately unmistakable: a substring search for it is the
	// whole assertion (§9.6 greps the live containers for the same shape).
	canaryValue  = "::p4-canary-secret-value::"
	canaryHandle = "handle:0123456789abcdef0123456789abcdef"
)

// shortTempDir is os.MkdirTemp with a SHORT name (R-12): a unix socket path is
// capped at 104 bytes by sun_path on darwin, and t.TempDir() embeds the test's
// full name, which pushes the path over the limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nb-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// freePort reserves and releases a port so the test can predict where the proxy
// would listen without colliding with a developer's own 3128.
func freePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

// runningSidecar starts a sidecar, delivers one binding through the REAL bind
// path (stdin → control socket → ack), and waits for readiness.
type runningSidecar struct {
	s      *sidecar
	logs   *bytes.Buffer
	runDir string
	// stop cancels the sidecar, waits for its loop to exit, restores the
	// process's real stdout/stderr and returns everything the process wrote to
	// them. It is a value the TEST asserts on — capturing in a t.Cleanup would
	// run after the assertions and prove nothing.
	stop func() (stdout, stderr string)
}

func startSidecar(t *testing.T, ctx context.Context, cancel context.CancelFunc, b binding, proxyPort int) *runningSidecar {
	t.Helper()
	dir := shortTempDir(t)
	logs := &bytes.Buffer{}
	opt := options{
		HealthListen:  "127.0.0.1:0",
		RunDir:        dir,
		ControlSocket: filepath.Join(dir, "c.sock"),
		TunnelMax:     2 * time.Second,
		ProxyPort:     proxyPort,
	}
	s := newSidecar(opt, newSidecarLogger(logs))

	// os.Stdout/os.Stderr are captured for the whole run: "the sidecar never
	// echoes the binding" is a claim about every byte the process writes, not
	// only about the ones that go through its logger.
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	run := &runningSidecar{s: s, logs: logs, runDir: dir}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()

	var stopOnce sync.Once
	var capturedOut, capturedErr string
	run.stop = func() (string, string) {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("the sidecar loop did not exit")
			}
			os.Stdout, os.Stderr = origOut, origErr
			_ = outW.Close()
			_ = errW.Close()
			out, _ := io.ReadAll(outR)
			errOut, _ := io.ReadAll(errR)
			capturedOut, capturedErr = string(out), string(errOut)
		})
		return capturedOut, capturedErr
	}
	t.Cleanup(func() { run.stop() })

	waitFor(t, 2*time.Second, func() bool {
		_, statErr := os.Stat(opt.ControlSocket)
		return statErr == nil
	}, "control socket never appeared")

	document, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	if err := runBind(bytes.NewReader(document), opt.ControlSocket, 5*time.Second); err != nil {
		t.Fatalf("bind: %v", err)
	}

	select {
	case <-s.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar never became ready")
	}
	return run
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// redeem exercises the node's half of the secret path: a POST over the unix
// socket the node mounts read-only.
func redeem(t *testing.T, socket, invocation, name, handle string) (int, map[string]any) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	body, _ := json.Marshal(map[string]string{
		"handle": handle, "invocation": invocation, "name": name, "node": "greet",
	})
	resp, err := client.Post("http://broker/v1/secret", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	return resp.StatusCode, answer
}

func canaryBinding(withEgress bool) binding {
	b := binding{
		Invocation: "inv-" + uuid.NewString(),
		Run:        uuid.NewString(),
		Node:       "greet",
		Secrets: map[string]secretAnswer{
			"greeting_suffix": {Handle: canaryHandle, Found: true, Value: canaryValue},
		},
	}
	if withEgress {
		b.Egress = &egressBinding{
			Patterns: []string{"httpbin.org"},
			Token:    testToken,
			// Loopback is the only subnet a unit test can really bind inside;
			// on the homelab this is the invocation bridge's /29.
			Subnet: "127.0.0.0/8",
		}
	}
	return b
}

// T4.13 — TestSidecar_NeverEchoesBinding is the secret-hygiene proof. The
// sidecar is run in-process with a canary binding delivered on the REAL stdin
// path; the broker then serves the canary to a legitimate redemption (the
// positive anchor — the value IS present in memory and reachable through the
// one-shot handle), and NOTHING the process wrote contains the value, the
// handle or the egress token.
//
// CONTROL: add `s.log.Info("bound", "binding", b)` at the top of apply() — the
// document is serialised under a key the filter does not refuse, the canary
// lands in the log buffer, and this test goes red.
func TestSidecar_NeverEchoesBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := canaryBinding(true)
	run := startSidecar(t, ctx, cancel, b, freePort(t))

	// Anchor: the sidecar really is holding and serving the secret.
	status, answer := redeem(t, filepath.Join(run.runDir, brokerSocketName), b.Invocation, "greeting_suffix", canaryHandle)
	if status != http.StatusOK {
		t.Fatalf("redemption status: got %d, want 200 (%v)", status, answer)
	}
	if answer["value"] != canaryValue {
		t.Fatalf("redeemed value: got %v, want the canary", answer["value"])
	}
	// And the handle is one-shot, which is what makes the value safe to serve.
	if second, _ := redeem(t, filepath.Join(run.runDir, brokerSocketName), b.Invocation, "greeting_suffix", canaryHandle); second != http.StatusConflict {
		t.Fatalf("second redemption: got %d, want 409", second)
	}

	// Provoke an audit line so the assertion below is over a NON-empty log.
	if run.s.proxyAddr() == "" {
		t.Fatal("the egress binding must have started a proxy")
	}
	denyThrough(t, run.s.proxyAddr())

	stdout, stderr := run.stop()

	logs := run.logs.String()
	if len(logs) == 0 {
		t.Fatal("no log output was captured; the absence assertion would be vacuous")
	}
	if !strings.Contains(logs, `"msg":"egress_decision"`) || !strings.Contains(logs, `"msg":"sidecar_bound"`) {
		t.Fatalf("the log does not contain the lines the sidecar is supposed to write:\n%s", logs)
	}
	for _, surface := range []struct{ name, text string }{
		{"logger", logs},
		{"stdout", stdout},
		{"stderr", stderr},
	} {
		for _, forbidden := range []string{canaryValue, canaryHandle, testToken, "handle:", "greeting_suffix"} {
			if strings.Contains(surface.text, forbidden) {
				t.Fatalf("the sidecar %s contains %q:\n%s", surface.name, forbidden, surface.text)
			}
		}
	}
}

// TestSidecarLogger_RefusesSecretKeys pins the backstop itself: a log call that
// names one of the refused keys emits the key and NOT the value.
//
// CONTROL: return `a` unchanged from filterAttr — every row goes red.
func TestSidecarLogger_RefusesSecretKeys(t *testing.T) {
	for _, key := range []string{"secrets", "token", "value", "handle"} {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			newSidecarLogger(&buf).Info("probe", key, canaryValue)
			out := buf.String()
			if strings.Contains(out, canaryValue) {
				t.Fatalf("key %q leaked its value: %s", key, out)
			}
			if !strings.Contains(out, redactedValue) {
				t.Fatalf("key %q was not marked redacted: %s", key, out)
			}
		})
	}

	t.Run("a nested group is filtered too", func(t *testing.T) {
		var buf bytes.Buffer
		newSidecarLogger(&buf).Info("probe", "group", map[string]string{})
		newSidecarLogger(&buf).With("value", canaryValue).Info("probe")
		if strings.Contains(buf.String(), canaryValue) {
			t.Fatalf("With() bypassed the filter: %s", buf.String())
		}
	})

	t.Run("anchor: an ordinary field is not redacted", func(t *testing.T) {
		var buf bytes.Buffer
		newSidecarLogger(&buf).Info("probe", "host", "httpbin.org")
		if !strings.Contains(buf.String(), "httpbin.org") {
			t.Fatalf("the filter ate an ordinary field: %s", buf.String())
		}
	})
}

// T4.14 — TestSidecar_BrokerOnlyWithoutEgress proves the two halves are
// independent: a node that declares secrets and NO egress gets a broker and no
// proxy at all. A proxy that started anyway would be an authenticated way out
// for a node whose manifest declares none.
//
// CONTROL: in apply(), substitute an empty egress binding when b.Egress is nil
// so the proxy starts unconditionally — proxyAddr() is non-empty, the port
// answers, and this test goes red on both assertions.
func TestSidecar_BrokerOnlyWithoutEgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := freePort(t)
	b := canaryBinding(false)
	run := startSidecar(t, ctx, cancel, b, port)

	if got := run.s.proxyAddr(); got != "" {
		t.Fatalf("a binding without egress must start no proxy, got %q", got)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("something is listening on the proxy port %d", port)
	}

	// Anchor: the broker half IS up and serving.
	if run.s.brokerAddr() == "" {
		t.Fatal("a binding with secrets must serve a broker")
	}
	status, answer := redeem(t, filepath.Join(run.runDir, brokerSocketName), b.Invocation, "greeting_suffix", canaryHandle)
	if status != http.StatusOK || answer["value"] != canaryValue {
		t.Fatalf("redemption: status %d answer %v", status, answer)
	}
}

// T4.9 — TestProxy_ListensOnInvocationInterfaceOnly proves the proxy binds the
// sidecar's address INSIDE the invocation subnet and nowhere else. The sidecar
// is also attached to the shared uplink, so a wildcard bind would publish an
// authenticated proxy to every other sidecar on it.
//
// CONTROL: bind ":"+port (all interfaces) instead of the in-subnet address —
// the listener's host becomes "::" and this test goes red.
func TestProxy_ListensOnInvocationInterfaceOnly(t *testing.T) {
	dir := shortTempDir(t)
	s := newSidecar(options{RunDir: dir, TunnelMax: time.Second, ProxyPort: 0}, newSidecarLogger(io.Discard))
	s.localAddr = func() ([]net.Addr, error) {
		return []net.Addr{mustTestCIDR(t, "172.20.0.5/16"), mustTestCIDR(t, "127.0.0.1/8")}, nil
	}

	b := binding{Invocation: "inv-x", Node: "fetch", Egress: &egressBinding{
		Patterns: []string{"httpbin.org"}, Token: testToken, Subnet: "127.0.0.0/8",
	}}
	lis, err := s.startProxy(b)
	if err != nil {
		t.Fatalf("startProxy: %v", err)
	}
	defer func() { _ = lis.Close() }()

	host, port, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("split listen address: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("listen host: got %q, want the in-subnet address 127.0.0.1", host)
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		t.Fatalf("the proxy must never bind a wildcard address, got %q", host)
	}

	// Anchor: it really is listening there.
	denyThrough(t, net.JoinHostPort(host, port))

	t.Run("no address in the subnet refuses", func(t *testing.T) {
		s := newSidecar(options{RunDir: dir, ProxyPort: 0}, newSidecarLogger(io.Discard))
		s.localAddr = func() ([]net.Addr, error) { return []net.Addr{mustTestCIDR(t, "172.20.0.5/16")}, nil }
		_, err := s.startProxy(binding{Egress: &egressBinding{Subnet: "10.201.0.0/29"}})
		if err == nil || err.Error() != "node sidecar: no local address in subnet 10.201.0.0/29" {
			t.Fatalf("refusal: got %v", err)
		}
	})
}

// TestBinding_MatchesRuntimeContract pins the sidecar's decoder against the
// runtime's encoder. They are two structs in two modules-worth of layering, and
// a renamed field on one side would fail SILENTLY — an empty token, an empty
// handle, a sidecar that refuses every redemption for no visible reason.
//
// CONTROL: rename any json tag on the sidecar's binding (e.g. `invocation` →
// `inv`) — the corresponding assertion goes red.
func TestBinding_MatchesRuntimeContract(t *testing.T) {
	runID := uuid.New()
	source := usecase.SidecarBinding{
		Invocation: "inv-abc",
		Run:        runID.String(),
		Node:       "greet",
		Secrets: map[string]usecase.SecretAnswer{
			"greeting_suffix": {Handle: canaryHandle, Found: true, Value: canaryValue},
		},
		Egress: &usecase.EgressBinding{
			Patterns: []string{"httpbin.org"}, Token: testToken, Subnet: "10.201.0.0/29",
		},
	}
	document, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got binding
	if err := json.Unmarshal(document, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Invocation != source.Invocation || got.Run != source.Run || got.Node != source.Node {
		t.Fatalf("identity fields did not survive: %+v", got)
	}
	answer, ok := got.Secrets["greeting_suffix"]
	if !ok {
		t.Fatal("the secret did not survive")
	}
	if answer.Handle != canaryHandle || !answer.Found || answer.Value != canaryValue {
		t.Fatalf("secret answer did not survive: %+v", answer)
	}
	if got.Egress == nil {
		t.Fatal("the egress binding did not survive")
	}
	if got.Egress.Token != testToken || got.Egress.Subnet != "10.201.0.0/29" ||
		len(got.Egress.Patterns) != 1 || got.Egress.Patterns[0] != "httpbin.org" {
		t.Fatalf("egress binding did not survive: %+v", got.Egress)
	}

	t.Run("a binding without egress omits the key entirely", func(t *testing.T) {
		document, err := json.Marshal(usecase.SidecarBinding{Invocation: "inv-abc"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(document), "egress") {
			t.Fatalf("egress must be omitted, not null: %s", document)
		}
	})
}

// denyThrough proves a listener is really a proxy: an unauthenticated request
// gets the token_missing refusal.
func denyThrough(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT httpbin.org:443 HTTP/1.1\r\nHost: httpbin.org:443\r\n\r\n")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read from proxy: %v", err)
	}
	if !strings.Contains(string(buf[:n]), reasonTokenMissing) {
		t.Fatalf("proxy answer: got %q, want a %s refusal", string(buf[:n]), reasonTokenMissing)
	}
}

func mustTestCIDR(t *testing.T, cidr string) net.Addr {
	t.Helper()
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse %s: %v", cidr, err)
	}
	return &net.IPNet{IP: ip, Mask: ipnet.Mask}
}

// TestOptionsFromEnv pins R-26: every sidecar setting has a DEFAULT, because
// the launch line sets no -e beyond the hardened flags' six and a sidecar
// inherits nothing from the runtime — a sidecar that refused to start without
// APP_RUN_DIR could never start at all. A value that IS present and unusable is
// an error, never a silent fallback.
//
// CONTROL: ignore the lookup and always return defaultOptions() — the three
// override rows and the three refusal rows go red.
func TestOptionsFromEnv(t *testing.T) {
	t.Run("absent keys yield the launch-line defaults", func(t *testing.T) {
		opt, err := optionsFromEnv(func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("an empty environment must be valid: %v", err)
		}
		if opt.HealthListen != "127.0.0.1:3129" || opt.RunDir != "/run/sentiae-inv" ||
			opt.TunnelMax != 130*time.Second || opt.ProxyPort != 3128 ||
			opt.ControlSocket != "/tmp/control.sock" {
			t.Fatalf("defaults: %+v", opt)
		}
	})

	t.Run("present keys override", func(t *testing.T) {
		env := map[string]string{
			envHealthListen: "127.0.0.1:9999",
			envTunnelMax:    "42s",
			envRunDir:       "/run/other",
		}
		opt, err := optionsFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		if err != nil {
			t.Fatalf("optionsFromEnv: %v", err)
		}
		if opt.HealthListen != "127.0.0.1:9999" || opt.TunnelMax != 42*time.Second || opt.RunDir != "/run/other" {
			t.Fatalf("overrides: %+v", opt)
		}
	})

	for _, tt := range []struct{ name, key, value string }{
		{"health listen without a port", envHealthListen, "127.0.0.1"},
		{"tunnel max is not a duration", envTunnelMax, "soon"},
		{"tunnel max is not positive", envTunnelMax, "0s"},
		{"run dir is relative", envRunDir, "run/sentiae-inv"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := optionsFromEnv(func(k string) (string, bool) {
				if k == tt.key {
					return tt.value, true
				}
				return "", false
			})
			if err == nil {
				t.Fatalf("%s=%q must be refused, not defaulted", tt.key, tt.value)
			}
		})
	}
}
