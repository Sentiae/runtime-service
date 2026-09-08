package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// dialTimeout bounds one upstream connection attempt.
const dialTimeout = 10 * time.Second

// auditLineCap bounds what one invocation may write to its own audit. The
// runtime reads this log back at teardown, and a node that floods its proxy must
// not be able to flood the host's disk on the way. Enforcement never stops at the
// cap — only the per-decision lines do; the cap line tells the runtime that its
// rows are a floor, not a total.
const auditLineCap = 10_000

// redactedHost replaces a requested host that carries one of this invocation's
// own secrets. The audit keeps the fact of the request and refuses the string:
// a tenant may put its own credential in a hostname, and the row would then be
// a secret at rest in a table nothing decrypts.
const redactedHost = "[redacted]"

// minSensitiveLen is the shortest bound string that may trigger redaction. A
// shorter secret would match so many ordinary hostnames that the audit would
// redact itself into uselessness.
const minSensitiveLen = 8

// hopByHopHeaders never cross a proxy. Proxy-Authorization is in the list for
// the reason that matters most here: the invocation's bearer is a credential
// for THIS proxy, and forwarding it upstream would hand it to whatever the node
// asked us to fetch.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// proxy is the invocation's egress boundary. Every request crosses decide()
// first; nothing else in this file may dial, and the only address it may dial
// is the one decide() already checked.
type proxy struct {
	pol       policy
	log       *slog.Logger
	tunnelMax time.Duration

	// audit identity, carried on every decision line.
	invocation string
	node       string
	run        string

	// seq numbers every decision line from 1 so the drain can prove it read all
	// of them; auditCap is auditLineCap outside tests.
	seq      atomic.Uint64
	auditCap uint64

	// sensitive is every bound secret value and handle, lower-cased. A host
	// containing one of them is recorded as redactedHost.
	sensitive []string

	// dial is the ONE way out. It takes the already-checked address; it never
	// takes a hostname, so no code path here can resolve a name a second time.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

// startProxy binds the egress proxy to this sidecar's address on the invocation
// bridge — never 0.0.0.0. The sidecar is also attached to the shared uplink,
// and a wildcard bind would publish an authenticated open proxy there.
func (s *sidecar) startProxy(b binding) (net.Listener, error) {
	ifaceAddrs, err := s.localAddr()
	if err != nil {
		return nil, fmt.Errorf("node sidecar: read interface addresses: %w", err)
	}
	local, err := localAddrInSubnet(ifaceAddrs, b.Egress.Subnet)
	if err != nil {
		return nil, err
	}

	p := &proxy{
		pol: policy{
			token:    b.Egress.Token,
			patterns: append([]string(nil), b.Egress.Patterns...),
			own:      ownAddresses(ifaceAddrs),
			resolve:  resolveIP,
		},
		log:        s.log,
		tunnelMax:  s.opt.TunnelMax,
		invocation: b.Invocation,
		node:       b.Node,
		run:        b.Run,
		auditCap:   auditLineCap,
		sensitive:  sensitiveStrings(b.Secrets),
		dial:       dialTCP,
	}

	lis, err := net.Listen("tcp", net.JoinHostPort(local.String(), strconv.Itoa(s.opt.ProxyPort)))
	if err != nil {
		return nil, fmt.Errorf("node sidecar: listen %s: %w", local, err)
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		defer func() { _ = recover() }()
		_ = srv.Serve(lis)
	}()
	return lis, nil
}

// ServeHTTP is the whole request path: classify, audit, then act.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, err := requestTarget(r)
	if err != nil {
		http.Error(w, "bad proxy request", http.StatusBadRequest)
		return
	}

	d := p.pol.decide(r.Context(), r.Header.Get("Proxy-Authorization"), host, port)
	p.audit(d)

	if !d.Allow {
		writeDeny(w, d)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, d)
		return
	}
	p.forward(w, r, d)
}

// audit emits exactly ONE structured line per decision (§3.7), numbered, up to
// auditLineCap. It carries no credential: the token, the handle and every
// secret value are refused by the logger's field filter, are never passed to it
// in the first place, and a host the NODE built out of one of its own secrets
// is redacted here before it can become a durable audit row.
func (p *proxy) audit(d decision) {
	seq := p.seq.Add(1)
	if seq > p.auditCap {
		if seq == p.auditCap+1 {
			p.log.Warn(eventEgressAuditCapped,
				"invocation_id", p.invocation, "node", p.node, "run_id", p.run, "cap", p.auditCap)
		}
		return
	}
	verdict := "deny"
	if d.Allow {
		verdict = "allow"
	}
	host, redacted := redactAuditHost(d.Host, p.sensitive)
	p.log.Info(eventEgressDecision,
		"decision", verdict,
		"host", host,
		"host_redacted", redacted,
		"port", d.Port,
		"invocation_id", p.invocation,
		"node", p.node,
		"reason", d.Reason,
		"run_id", p.run,
		"seq", seq,
	)
}

// sensitiveStrings is what a host may not contain: every bound handle and every
// bound value long enough that a match means something. The comparison is
// case-folded because a host is folded to lower case before it is decided.
func sensitiveStrings(secrets map[string]secretAnswer) []string {
	var out []string
	for _, answer := range secrets {
		for _, candidate := range []string{answer.Handle, answer.Value} {
			if len(candidate) >= minSensitiveLen {
				out = append(out, strings.ToLower(candidate))
			}
		}
	}
	return out
}

// redactAuditHost answers the host the audit keeps. It is deterministic and
// total: either the host carries one of this invocation's secrets and the audit
// records that it was redacted, or the host is kept verbatim.
func redactAuditHost(host string, sensitive []string) (string, bool) {
	folded := strings.ToLower(host)
	for _, s := range sensitive {
		if strings.Contains(folded, s) {
			return redactedHost, true
		}
	}
	return host, false
}

// tunnel answers CONNECT by joining the client to the ONE checked address.
func (p *proxy) tunnel(w http.ResponseWriter, r *http.Request, d decision) {
	upstream, err := p.dial(r.Context(), pinnedAddr(d))
	if err != nil {
		writeStatus(w, http.StatusBadGateway, "Bad Gateway", "", "")
		return
	}
	defer func() { _ = upstream.Close() }()

	client, buf, err := hijack(w)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	if _, err := buf.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}

	// The tunnel is opaque, so the only bound available is time. Both ends get
	// the same deadline; when it expires the copies unblock and both sides close.
	if p.tunnelMax > 0 {
		deadline := time.Now().Add(p.tunnelMax)
		_ = client.SetDeadline(deadline)
		_ = upstream.SetDeadline(deadline)
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { _ = recover() }()
		_, _ = io.Copy(upstream, buf)
		done <- struct{}{}
	}()
	go func() {
		defer func() { _ = recover() }()
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

// forward answers a plain http:// request. It uses RoundTrip and not a Client,
// so a redirect is RETURNED to the node and never followed here: following one
// would reach a host the node never declared, decided by the response instead
// of by the grant.
func (p *proxy) forward(w http.ResponseWriter, r *http.Request, d decision) {
	addr := pinnedAddr(d)
	transport := &http.Transport{
		// The address argument is DISCARDED: the pinned address is the only one
		// this request may reach, and re-resolving here is the rebinding hole.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dial(ctx, addr)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	defer transport.CloseIdleConnections()

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = d.Host
	stripHopByHop(out.Header)

	resp, err := transport.RoundTrip(out)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, "Bad Gateway", "", "")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	stripHopByHop(resp.Header)
	// The verdict header is THIS proxy's, and only this proxy's. An origin that
	// set it would make the node report a denial that never happened — and the
	// node SDKs treat its presence as the whole test, precisely because the
	// header cannot come from anywhere but here.
	resp.Header.Del(denyHeader)
	for k, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// writeDeny refuses a request with the reason in three places: the status text
// (the only thing a Go client can see when CONNECT fails), the header (what a
// forwarded response carries), and the body (what a human reads).
func writeDeny(w http.ResponseWriter, d decision) {
	writeStatus(w, d.Status, d.Reason, d.Reason,
		fmt.Sprintf("egress denied: %s:%d (%s)", d.Host, d.Port, d.Reason))
}

// writeStatus writes a raw response so the status TEXT is ours. net/http
// derives the reason phrase from the code, and the reason phrase is exactly
// what a CONNECT failure surfaces to the caller.
func writeStatus(w http.ResponseWriter, status int, statusText, denyReason, body string) {
	conn, buf, err := hijack(w)
	if err != nil {
		// Not hijackable (a test recorder, an HTTP/2 stream): the header and the
		// body still carry the reason.
		if denyReason != "" {
			w.Header().Set(denyHeader, denyReason)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	defer func() { _ = conn.Close() }()

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, statusText)
	if denyReason != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", denyHeader, denyReason)
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	b.WriteString("Connection: close\r\n\r\n")
	b.WriteString(body)

	_, _ = buf.WriteString(b.String())
	_ = buf.Flush()
}

// hijack takes the raw connection so this proxy owns the bytes on the wire.
func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer is not hijackable")
	}
	return hj.Hijack()
}

// stripHopByHop removes the per-connection headers, including the ones a
// Connection header names.
func stripHopByHop(h http.Header) {
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			h.Del(name)
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// requestTarget is the host and port a request is asking for.
func requestTarget(r *http.Request) (string, int, error) {
	if r.Method == http.MethodConnect {
		host, portText, err := net.SplitHostPort(r.Host)
		if err != nil {
			return "", 0, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return "", 0, err
		}
		return host, port, nil
	}
	if r.URL == nil || r.URL.Host == "" {
		return "", 0, fmt.Errorf("not an absolute proxy request")
	}
	host := r.URL.Hostname()
	port := 80
	if r.URL.Scheme == "https" {
		port = 443
	}
	if p := r.URL.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, err
		}
		port = n
	}
	return host, port, nil
}

// pinnedAddr renders the ONE address this decision permits.
func pinnedAddr(d decision) string {
	return net.JoinHostPort(d.Addr.String(), strconv.Itoa(d.Port))
}

// resolveIP is the real resolver: ONE lookup per request, whose answers are all
// classified before any of them is dialled.
func resolveIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialTCP is the real dialer. Its argument is always an address literal.
func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
}
