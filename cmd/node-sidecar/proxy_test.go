//go:build unit

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "b3f1c0d2e4a5968778695a4e3c2d1b0af9e8d7c6b5a4938271605f4e3d2c1b0a"

// proxyHarness is a live proxy over fake network primitives: the resolver
// answers what the test says, and the dialer records the address it was given
// before connecting to a local upstream. Recording the address is the point —
// it is how "the proxy dials the address it checked" is proved rather than
// assumed.
type proxyHarness struct {
	srv      *httptest.Server
	logs     *bytes.Buffer
	upstream string
	// p is the proxy under test, so a test can pin the accounting bounds the
	// runtime's drain depends on (the line cap) without a second constructor.
	p *proxy

	mu       sync.Mutex
	dialed   []string
	resolves int
}

func newProxyHarness(t *testing.T, patterns []string, answers []string, upstream string) *proxyHarness {
	t.Helper()
	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		addrs = append(addrs, netip.MustParseAddr(a))
	}

	h := &proxyHarness{logs: &bytes.Buffer{}, upstream: upstream}
	p := &proxy{
		pol: policy{
			token:    testToken,
			patterns: patterns,
			own:      map[netip.Addr]bool{netip.MustParseAddr("10.201.0.2"): true},
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				h.mu.Lock()
				h.resolves++
				h.mu.Unlock()
				return addrs, nil
			},
		},
		log:        slog.New(slog.NewJSONHandler(h.logs, nil)),
		tunnelMax:  5 * time.Second,
		invocation: "inv-11111111-1111-1111-1111-111111111111",
		node:       "fetch",
		run:        "22222222-2222-2222-2222-222222222222",
		auditCap:   auditLineCap,
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			if h.upstream == "" {
				return nil, io.EOF
			}
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", h.upstream)
		},
	}
	h.p = p
	h.srv = httptest.NewServer(p)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *proxyHarness) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

func (h *proxyHarness) resolveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resolves
}

// forward issues a plain http:// request THROUGH the proxy, the way a node's
// SDK does.
func (h *proxyHarness) forward(t *testing.T, target, authorization string) *http.Response {
	t.Helper()
	proxyURL, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		// The proxy under test must not follow a redirect; neither may the test
		// client, or the assertion would be about the client.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Proxy-Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward %s: %v", target, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// connectStatusLine speaks CONNECT by hand so the RAW status line is
// observable. It is the shape that matters: a Go client turns a failed CONNECT
// into an error carrying the status TEXT and nothing else, which is why the
// reason has to live there.
func (h *proxyHarness) connectStatusLine(t *testing.T, hostport, authorization string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.srv.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := "CONNECT " + hostport + " HTTP/1.1\r\nHost: " + hostport + "\r\n"
	if authorization != "" {
		req += "Proxy-Authorization: " + authorization + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

// echoUpstream is a local server standing in for the internet. It records what
// actually arrived, which is how header stripping is proved.
type echoUpstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	headers  []http.Header
	status   int
	location string
}

func newEchoUpstream(t *testing.T) *echoUpstream {
	t.Helper()
	u := &echoUpstream{status: http.StatusOK}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.headers = append(u.headers, r.Header.Clone())
		status, location := u.status, u.location
		u.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "upstream-body")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *echoUpstream) addr() string { return strings.TrimPrefix(u.srv.URL, "http://") }

func (u *echoUpstream) received() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.headers...)
}

// T4.2 — TestProxy_ResolveThenPin proves the request is resolved EXACTLY ONCE
// and that the address the policy checked is the address the dialer is given.
// Re-resolving inside the dialer is DNS rebinding: the name that passed the
// check and the name that gets connected would be two different lookups.
//
// CONTROL: give the forward path a Transport whose DialContext dials the
// ADDRESS ARGUMENT (the hostname) instead of the pinned address — the recorded
// dial becomes "httpbin.org:80" and the resolve count becomes 2 under a real
// resolver; this test goes red on the pinned-address assertion.
func TestProxy_ResolveThenPin(t *testing.T) {
	upstream := newEchoUpstream(t)
	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, upstream.addr())

	resp := h.forward(t, "http://httpbin.org/get", "Bearer "+testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream-body" {
		t.Fatalf("body: got %q", string(body))
	}
	if got := h.resolveCount(); got != 1 {
		t.Fatalf("resolver calls: got %d, want exactly 1", got)
	}
	if got := h.dials(); len(got) != 1 || got[0] != "93.184.216.34:80" {
		t.Fatalf("dialled: got %v, want [93.184.216.34:80]", got)
	}
}

// T4.3 — TestProxy_UndeclaredHostDenied proves the grant is the boundary: a
// host the manifest never declared is refused BEFORE any dial, and the refusal
// is signalled in all three places a caller can read it — the status text (all
// a failed CONNECT surfaces), the header (what a forwarded response carries)
// and the body. The anchor is the same request against a grant that DOES
// declare the host: it is allowed and dialled.
//
// CONTROL: delete the `!nodemanifest.MatchEgress(...)` branch in decide — the
// denied rows are allowed, the dial happens, and this test goes red.
func TestProxy_UndeclaredHostDenied(t *testing.T) {
	upstream := newEchoUpstream(t)

	t.Run("undeclared host is refused before any dial", func(t *testing.T) {
		h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, upstream.addr())

		resp := h.forward(t, "http://example.com/", "Bearer "+testToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
		if resp.Status != "403 "+reasonHostNotDeclared {
			t.Fatalf("status line: got %q, want %q", resp.Status, "403 "+reasonHostNotDeclared)
		}
		if got := resp.Header.Get(denyHeader); got != reasonHostNotDeclared {
			t.Fatalf("%s: got %q, want %q", denyHeader, got, reasonHostNotDeclared)
		}
		body, _ := io.ReadAll(resp.Body)
		want := "egress denied: example.com:80 (host_not_declared)"
		if string(body) != want {
			t.Fatalf("body: got %q, want %q", string(body), want)
		}
		if got := h.dials(); len(got) != 0 {
			t.Fatalf("a denied request must not dial, got %v", got)
		}
		if !strings.Contains(h.logs.String(), `"msg":"egress_decision"`) ||
			!strings.Contains(h.logs.String(), `"decision":"deny"`) ||
			!strings.Contains(h.logs.String(), `"reason":"host_not_declared"`) {
			t.Fatalf("audit line missing or wrong: %s", h.logs.String())
		}
	})

	t.Run("CONNECT carries the reason in the status text", func(t *testing.T) {
		h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, upstream.addr())
		line := h.connectStatusLine(t, "example.com:443", "Bearer "+testToken)
		if line != "HTTP/1.1 403 "+reasonHostNotDeclared {
			t.Fatalf("CONNECT status line: got %q", line)
		}
	})

	t.Run("anchor: a declared host is allowed and dialled", func(t *testing.T) {
		h := newProxyHarness(t, []string{"example.com"}, []string{"93.184.216.34"}, upstream.addr())
		resp := h.forward(t, "http://example.com/", "Bearer "+testToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200", resp.StatusCode)
		}
		if got := h.dials(); len(got) != 1 || got[0] != "93.184.216.34:80" {
			t.Fatalf("dialled: got %v, want [93.184.216.34:80]", got)
		}
		if !strings.Contains(h.logs.String(), `"decision":"allow"`) ||
			!strings.Contains(h.logs.String(), `"reason":"manifest_exact"`) {
			t.Fatalf("audit line missing or wrong: %s", h.logs.String())
		}
	})
}

// T4.4 — TestProxy_PrivateRangeDenied_AnyAnswer proves that ONE disallowed
// answer refuses the whole request even when a public answer is also present
// and even when it comes first. Picking the good one out of a mixed answer set
// is exactly what a rebinding attack wants, and the dial-count assertion is
// what proves nothing was reached.
//
// CONTROL: make disallowedAddr return false for everything (the allow-all
// classifier) — every row is allowed, the dialer is called, and this test goes
// red on the dial assertion.
func TestProxy_PrivateRangeDenied_AnyAnswer(t *testing.T) {
	upstream := newEchoUpstream(t)
	tests := []struct {
		name    string
		answers []string
	}{
		{"private only", []string{"10.0.10.20"}},
		{"private first, public second", []string{"10.0.10.20", "93.184.216.34"}},
		{"public first, private second", []string{"93.184.216.34", "10.0.10.20"}},
		{"metadata endpoint", []string{"169.254.169.254"}},
		{"v4-mapped metadata", []string{"::ffff:169.254.169.254"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyHarness(t, []string{"httpbin.org"}, tt.answers, upstream.addr())
			resp := h.forward(t, "http://httpbin.org/get", "Bearer "+testToken)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status: got %d, want 403", resp.StatusCode)
			}
			if got := resp.Header.Get(denyHeader); got != reasonPrivateAddress {
				t.Fatalf("%s: got %q, want %q", denyHeader, got, reasonPrivateAddress)
			}
			if got := h.dials(); len(got) != 0 {
				t.Fatalf("a denied request must not dial, got %v", got)
			}
		})
	}

	t.Run("anchor: an all-public answer set is allowed", func(t *testing.T) {
		h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34", "1.1.1.1"}, upstream.addr())
		resp := h.forward(t, "http://httpbin.org/get", "Bearer "+testToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200", resp.StatusCode)
		}
		if got := h.dials(); len(got) != 1 {
			t.Fatalf("dialled: got %v, want exactly one", got)
		}
	})
}

// T4.7 — TestProxy_ForwardStripsProxyAuthAndDoesNotFollowRedirects proves two
// things that would each leak the boundary:
//
//  1. the invocation's bearer is a credential for THIS proxy and must never be
//     forwarded upstream, along with every other hop-by-hop header;
//  2. a redirect is RETURNED, never followed — following one would reach a host
//     chosen by the response instead of by the grant. The second decision, when
//     the node itself follows it, is a plain host_not_declared.
//
// CONTROL: forward with an http.Client (which follows redirects) instead of
// Transport.RoundTrip — the response becomes 200 from the redirect target and
// the "not followed" assertion goes red. Second CONTROL: drop the
// stripHopByHop(out.Header) call — the upstream sees Proxy-Authorization and
// the header assertion goes red.
func TestProxy_ForwardStripsProxyAuthAndDoesNotFollowRedirects(t *testing.T) {
	upstream := newEchoUpstream(t)
	upstream.status = http.StatusFound
	upstream.location = "http://evil.example/x"

	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, upstream.addr())

	resp := h.forward(t, "http://httpbin.org/get", "Bearer "+testToken)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (the redirect must be returned, not followed)", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "http://evil.example/x" {
		t.Fatalf("Location: got %q", got)
	}
	if got := h.dials(); len(got) != 1 {
		t.Fatalf("dialled: got %v, want exactly one — following the redirect would dial again", got)
	}

	received := upstream.received()
	if len(received) != 1 {
		t.Fatalf("upstream requests: got %d, want 1", len(received))
	}
	for _, header := range []string{"Proxy-Authorization", "Proxy-Connection", "Keep-Alive", "Te", "Upgrade"} {
		if got := received[0].Get(header); got != "" {
			t.Fatalf("upstream saw %s: %q — hop-by-hop headers must not cross the proxy", header, got)
		}
	}

	t.Run("the redirect target is a second decision", func(t *testing.T) {
		resp := h.forward(t, "http://evil.example/x", "Bearer "+testToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
		if got := resp.Header.Get(denyHeader); got != reasonHostNotDeclared {
			t.Fatalf("%s: got %q, want %q", denyHeader, got, reasonHostNotDeclared)
		}
	})
}

// TestProxy_ConnectTunnelsToThePinnedAddress is the CONNECT anchor: the allowed
// path really does join the caller to the checked address, so the refusals
// above are a policy and not a broken tunnel.
func TestProxy_ConnectTunnelsToThePinnedAddress(t *testing.T) {
	// A trivial upstream that answers one line, so the tunnel is observable.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "tunnelled\n")
	}()

	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, lis.Addr().String())

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.srv.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "CONNECT httpbin.org:443 HTTP/1.1\r\nHost: httpbin.org:443\r\n"+
		"Proxy-Authorization: Bearer "+testToken+"\r\n\r\n")

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if strings.TrimSpace(status) != "HTTP/1.1 200 Connection established" {
		t.Fatalf("status line: got %q", strings.TrimSpace(status))
	}
	// Consume the blank line that ends the response head.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read head terminator: %v", err)
	}
	payload, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read tunnelled payload: %v", err)
	}
	if strings.TrimSpace(payload) != "tunnelled" {
		t.Fatalf("payload: got %q", strings.TrimSpace(payload))
	}
	if got := h.dials(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("dialled: got %v, want [93.184.216.34:443]", got)
	}
}

// TestProxy_ForwardRemovesForgedDenyHeader proves the verdict header is the
// proxy's alone. The node SDKs treat its PRESENCE as the whole test — status
// codes and reason vocabularies are deliberately not consulted — which is only
// sound because it cannot come from anywhere but here. An origin that sets it
// would make a node report, and the platform audit, a denial that never
// happened.
//
// CONTROL: delete `resp.Header.Del(denyHeader)` from proxy.forward — the forged
// header reaches the node and this test goes red.
func TestProxy_ForwardRemovesForgedDenyHeader(t *testing.T) {
	forger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(denyHeader, reasonPrivateAddress)
		w.Header().Set("X-Origin-Marker", "reached")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "upstream-body")
	}))
	t.Cleanup(forger.Close)

	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"},
		strings.TrimPrefix(forger.URL, "http://"))

	resp := h.forward(t, "http://httpbin.org/get", "Bearer "+testToken)

	if got := resp.Header.Get(denyHeader); got != "" {
		t.Fatalf("%s: got %q from the ORIGIN — an origin must not be able to forge a denial", denyHeader, got)
	}
	// The response is otherwise untouched: the strip is surgical, not a rewrite.
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status: got %d, want 418 (the origin's own status must survive)", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Origin-Marker"); got != "reached" {
		t.Fatalf("X-Origin-Marker: got %q, want %q (anchor: the origin really answered)", got, "reached")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "upstream-body" {
		t.Fatalf("body: got %q, want %q", string(body), "upstream-body")
	}

	// The other half of the contract the SDKs depend on: when the PROXY refuses,
	// the header is there, and it names the reason. Without this the test above
	// would pass on a proxy that never emits the header at all.
	//
	// CONTROL: drop the header from writeDeny/writeStatus's deny path — this
	// subtest goes red.
	t.Run("a real CONNECT denial carries the reason in the header", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.srv.URL, "http://"), 2*time.Second)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		_, _ = io.WriteString(conn, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n"+
			"Proxy-Authorization: Bearer "+testToken+"\r\n\r\n")

		// Read the RAW response head off the hijacked connection: this is the
		// wire the SDK's OnProxyConnectResponse hook sees, and the only place a
		// tunnelled refusal is identifiable at all.
		req, err := http.NewRequest(http.MethodConnect, "https://evil.example:443", nil)
		if err != nil {
			t.Fatalf("build CONNECT: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatalf("read CONNECT response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
		if got := resp.Header.Get(denyHeader); got != reasonHostNotDeclared {
			t.Fatalf("%s: got %q, want %q — the SDK reads the verdict from this header alone",
				denyHeader, got, reasonHostNotDeclared)
		}
	})
}

// TestProxy_AuditSeqAndCap pins the drain's two accounting facts: every decision
// line carries its 1-based seq, and past the cap exactly one egress_audit_capped
// line replaces every further decision line while enforcement continues.
//
// CONTROL: delete the `seq > p.auditCap` block — four decision lines, red.
// CONTROL: emit the capped line unconditionally in that block — two, red.
func TestProxy_AuditSeqAndCap(t *testing.T) {
	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, "")
	h.p.auditCap = 2
	for i := 0; i < 4; i++ {
		resp := h.forward(t, "http://example.com/", "Bearer "+testToken)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("request %d: got %d, want 403 — enforcement must not stop at the cap", i+1, resp.StatusCode)
		}
	}
	logs := h.logs.String()
	if got := strings.Count(logs, `"msg":"egress_decision"`); got != 2 {
		t.Fatalf("decision lines: got %d, want 2:\n%s", got, logs)
	}
	for _, want := range []string{`"seq":1`, `"seq":2`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %s:\n%s", want, logs)
		}
	}
	if got := strings.Count(logs, `"msg":"egress_audit_capped"`); got != 1 {
		t.Fatalf("capped lines: got %d, want exactly 1:\n%s", got, logs)
	}
	if strings.Contains(logs, `"seq":3`) {
		t.Fatalf("a decision past the cap was written:\n%s", logs)
	}
}

// TestProxy_AuditRedactsBoundSecretHost pins the hygiene half of the audit: a
// node CAN put one of its own bound secrets in a hostname, and that hostname
// must not become a durable row. The decision itself is unchanged — the request
// is still refused for the reason the policy gave — only the recorded name is
// refused.
//
// CONTROL: return `host, false` unconditionally from redactAuditHost — the
// redaction assertions go red and the secret appears in the log.
func TestProxy_AuditRedactsBoundSecretHost(t *testing.T) {
	const secret = "s3cr3t-value-abcdef"
	h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, "")
	h.p.sensitive = []string{secret}

	resp := h.forward(t, "http://"+secret+".evil.example/", "Bearer "+testToken)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}

	logs := h.logs.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("the bound secret reached the audit line:\n%s", logs)
	}
	if !strings.Contains(logs, `"host":"[redacted]"`) || !strings.Contains(logs, `"host_redacted":true`) {
		t.Fatalf("the host was not redacted:\n%s", logs)
	}
	if !strings.Contains(logs, `"reason":"`+reasonHostNotDeclared+`"`) {
		t.Fatalf("the decision itself must be unchanged:\n%s", logs)
	}

	t.Run("anchor: an ordinary host is not redacted", func(t *testing.T) {
		h := newProxyHarness(t, []string{"httpbin.org"}, []string{"93.184.216.34"}, "")
		h.p.sensitive = []string{secret}
		resp := h.forward(t, "http://example.com/", "Bearer "+testToken)
		_ = resp.Body.Close()
		logs := h.logs.String()
		if !strings.Contains(logs, `"host":"example.com"`) || !strings.Contains(logs, `"host_redacted":false`) {
			t.Fatalf("an ordinary host must be kept verbatim:\n%s", logs)
		}
	})
}
