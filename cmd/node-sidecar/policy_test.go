//go:build unit

package main

import (
	"context"
	"net/netip"
	"testing"
)

// testPolicy is one invocation's grant with a deterministic resolver.
func testPolicy(patterns []string, answers ...string) *policy {
	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		addrs = append(addrs, netip.MustParseAddr(a))
	}
	return &policy{
		token:    "b3f1c0d2e4a5968778695a4e3c2d1b0af9e8d7c6b5a4938271605f4e3d2c1b0a",
		patterns: patterns,
		own:      map[netip.Addr]bool{netip.MustParseAddr("10.201.0.2"): true},
		resolve: func(context.Context, string) ([]netip.Addr, error) {
			return addrs, nil
		},
	}
}

func bearer(p *policy) string { return "Bearer " + p.token }

// T4.5 — TestProxy_PrivateLiteralVsPublicLiteral pins the distinction §3.7
// makes deliberately: a PRIVATE literal is the attack (the metadata endpoint,
// the daemon's networks, the host) and is named private_address; a PUBLIC
// literal is merely outside the grant, which is over names, and is named
// ip_literal. Collapsing the two would erase the signal §9.4 greps for.
//
// CONTROL: drop the disallowedAddr() branch in decide's literal case so every
// literal answers ip_literal — the four private rows go red.
func TestProxy_PrivateLiteralVsPublicLiteral(t *testing.T) {
	p := testPolicy([]string{"*"})
	tests := []struct {
		name, host string
		want       string
	}{
		{"homelab address", "10.0.10.20", reasonPrivateAddress},
		{"cloud metadata", "169.254.169.254", reasonPrivateAddress},
		{"loopback", "127.0.0.1", reasonPrivateAddress},
		{"docker default pool", "172.17.0.1", reasonPrivateAddress},
		{"public literal", "93.184.216.34", reasonIPLiteral},
		{"public v6 literal", "2606:4700:4700::1111", reasonIPLiteral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.decide(context.Background(), bearer(p), tt.host, 443)
			if d.Allow {
				t.Fatalf("a literal is never allowed, got allow (%s)", d.Reason)
			}
			if d.Reason != tt.want {
				t.Fatalf("reason: got %q, want %q", d.Reason, tt.want)
			}
			if d.Status != 403 {
				t.Fatalf("status: got %d, want 403", d.Status)
			}
		})
	}
}

// T4.6 — TestProxy_TokenAndPolicy proves the bearer is a credential and not a
// formality: absent is 407/token_missing, and ANY other value is
// 403/policy_missing. The last row is the positive anchor — the real token on a
// declared host is allowed, so the refusals are not just a broken proxy.
//
// CONTROL: replace the constant-time comparison with `if token == ""` — the
// "another invocation's token" and "a prefix of the token" rows are allowed and
// go red.
func TestProxy_TokenAndPolicy(t *testing.T) {
	p := testPolicy([]string{"httpbin.org"}, "93.184.216.34")

	tests := []struct {
		name          string
		authorization string
		wantAllow     bool
		wantStatus    int
		wantReason    string
	}{
		{"no header at all", "", false, 407, reasonTokenMissing},
		{"empty bearer", "Bearer ", false, 407, reasonTokenMissing},
		{"a different scheme", "Basic " + p.token, false, 407, reasonTokenMissing},
		{"another invocation's token", "Bearer " + "a" + p.token[1:], false, 403, reasonPolicyMissing},
		{"a prefix of the token", "Bearer " + p.token[:32], false, 403, reasonPolicyMissing},
		{"this invocation's token", bearer(p), true, 200, reasonManifestExact},
		{"scheme is case-insensitive", "bearer " + p.token, true, 200, reasonManifestExact},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.decide(context.Background(), tt.authorization, "httpbin.org", 443)
			if d.Allow != tt.wantAllow {
				t.Fatalf("allow: got %v, want %v (reason %q)", d.Allow, tt.wantAllow, d.Reason)
			}
			if d.Status != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", d.Status, tt.wantStatus)
			}
			if d.Reason != tt.wantReason {
				t.Fatalf("reason: got %q, want %q", d.Reason, tt.wantReason)
			}
		})
	}
}

// T4.8 — TestProxy_PortRestriction pins the port policy to exactly 80 and 443.
// A proxy that dials any port a node names is a port scanner with a credential.
//
// CONTROL: make allowedPorts admit everything — the four refused rows are
// allowed and go red.
func TestProxy_PortRestriction(t *testing.T) {
	p := testPolicy([]string{"httpbin.org"}, "93.184.216.34")
	tests := []struct {
		port      int
		wantAllow bool
	}{
		{80, true},
		{443, true},
		{8080, false},
		{22, false},
		{5432, false},
		{3128, false},
	}
	for _, tt := range tests {
		t.Run(portName(tt.port), func(t *testing.T) {
			d := p.decide(context.Background(), bearer(p), "httpbin.org", tt.port)
			if d.Allow != tt.wantAllow {
				t.Fatalf("port %d: allow = %v, want %v (reason %q)", tt.port, d.Allow, tt.wantAllow, d.Reason)
			}
			if !tt.wantAllow && d.Reason != reasonPortNotAllowed {
				t.Fatalf("port %d: reason = %q, want %q", tt.port, d.Reason, reasonPortNotAllowed)
			}
		})
	}
}

// TestPolicy_ResolutionRefusals pins the two answers a lookup can fail with,
// and that the allow classification names WHICH pattern shape admitted the
// host — the audit's only useful "why".
//
// CONTROL: return reasonManifestExact unconditionally from classifyMatch — the
// wildcard and suffix rows go red.
func TestPolicy_ResolutionRefusals(t *testing.T) {
	t.Run("resolver error", func(t *testing.T) {
		p := testPolicy([]string{"httpbin.org"})
		p.resolve = func(context.Context, string) ([]netip.Addr, error) {
			return nil, context.DeadlineExceeded
		}
		if d := p.decide(context.Background(), bearer(p), "httpbin.org", 443); d.Reason != reasonResolveFailed {
			t.Fatalf("reason: got %q, want %q", d.Reason, reasonResolveFailed)
		}
	})
	t.Run("empty answer", func(t *testing.T) {
		p := testPolicy([]string{"httpbin.org"})
		if d := p.decide(context.Background(), bearer(p), "httpbin.org", 443); d.Reason != reasonDNSNoAnswer {
			t.Fatalf("reason: got %q, want %q", d.Reason, reasonDNSNoAnswer)
		}
	})
	t.Run("an answer that is the sidecar itself", func(t *testing.T) {
		p := testPolicy([]string{"httpbin.org"}, "10.201.0.2")
		if d := p.decide(context.Background(), bearer(p), "httpbin.org", 443); d.Reason != reasonOwnAddress {
			t.Fatalf("reason: got %q, want %q", d.Reason, reasonOwnAddress)
		}
	})
	t.Run("allow classifications", func(t *testing.T) {
		for _, tt := range []struct {
			pattern, host, want string
		}{
			{"*", "httpbin.org", reasonManifestWildcard},
			{"httpbin.org", "httpbin.org", reasonManifestExact},
			{"*.example.com", "api.example.com", reasonManifestSuffix},
		} {
			p := testPolicy([]string{tt.pattern}, "93.184.216.34")
			d := p.decide(context.Background(), bearer(p), tt.host, 443)
			if !d.Allow {
				t.Fatalf("pattern %q must admit %q, got %q", tt.pattern, tt.host, d.Reason)
			}
			if d.Reason != tt.want {
				t.Fatalf("pattern %q: reason = %q, want %q", tt.pattern, d.Reason, tt.want)
			}
		}
	})
}

// TestNormalizeHost pins the folding the grant is matched against: case, the
// trailing root dot, and punycode. Without it "HTTPBIN.ORG." and an IDN spelling
// would both miss a declared pattern that ought to admit them.
//
// CONTROL: return the host unchanged — the three folded rows go red.
func TestNormalizeHost(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"HTTPBIN.ORG", "httpbin.org"},
		{"httpbin.org.", "httpbin.org"},
		{"  httpbin.org  ", "httpbin.org"},
		{"bücher.example", "xn--bcher-kva.example"},
		{"httpbin.org", "httpbin.org"},
	} {
		if got := normalizeHost(tt.in); got != tt.want {
			t.Fatalf("normalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func portName(port int) string {
	switch port {
	case 80:
		return "http"
	case 443:
		return "https"
	}
	return "port-" + itoa(port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
