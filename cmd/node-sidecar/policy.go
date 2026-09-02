package main

import (
	"context"
	"crypto/subtle"
	"net/netip"
	"strings"

	"github.com/sentiae/platform-kit/nodemanifest"
	"golang.org/x/net/idna"
)

// denyHeader carries the machine-readable reason on every refusal. The reason
// is ALSO the HTTP status text, because a CONNECT failure surfaces to a Go
// client as the status text alone — headers are unreachable there — and the
// node SDKs turn that text into the node's error code (§3.11).
const denyHeader = "X-Sentiae-Egress-Denied"

// The refusal reasons, verbatim (§3.7/D-19). These strings are an interface:
// the fixtures assert them, §9.4 greps them, and the SDKs echo them.
const (
	reasonTokenMissing    = "token_missing"
	reasonPolicyMissing   = "policy_missing"
	reasonPrivateAddress  = "private_address"
	reasonIPLiteral       = "ip_literal"
	reasonPortNotAllowed  = "port_not_allowed"
	reasonHostNotDeclared = "host_not_declared"
	reasonResolveFailed   = "resolve_failed"
	reasonDNSNoAnswer     = "dns_no_answer"
	reasonOwnAddress      = "own_address"
)

// The allow classifications, verbatim: which shape of declared pattern admitted
// the host. An audit line that says only "allow" cannot answer the question the
// audit exists for — WHY was this allowed.
const (
	reasonManifestWildcard = "manifest_wildcard"
	reasonManifestExact    = "manifest_exact"
	reasonManifestSuffix   = "manifest_suffix"
)

// allowedPorts is the whole port policy. 80 and 443 only: a proxy that will
// dial any port is a port scanner for whoever controls the node.
var allowedPorts = map[int]bool{80: true, 443: true}

// decision is ONE resolved request. Addr is the single address the dialer is
// allowed to use — the request is resolved ONCE, here, and the checked address
// is what gets dialled. Re-resolving in the dialer is the DNS-rebinding hole
// this type exists to make impossible.
type decision struct {
	Allow  bool
	Status int
	Reason string
	Host   string
	Port   int
	Addr   netip.Addr
}

// policy holds one invocation's egress grant.
type policy struct {
	// token is the bearer minted for THIS invocation (32 crypto/rand bytes as
	// 64 lower-hex, no `handle:` prefix — R-21 F-3(i)).
	token    string
	patterns []string
	own      map[netip.Addr]bool
	resolve  func(ctx context.Context, host string) ([]netip.Addr, error)
}

// decide is the security contract, in order. Every step is a refusal except the
// last, and no step may be skipped or reordered: the IP-literal check precedes
// the port check so a private literal is named as a private address rather than
// as a port violation, and the resolve happens LAST so a name is only looked up
// once the grant already admits it.
func (p *policy) decide(ctx context.Context, authorization, host string, port int) decision {
	name := normalizeHost(host)
	deny := func(status int, reason string) decision {
		return decision{Status: status, Reason: reason, Host: name, Port: port}
	}

	token := bearerToken(authorization)
	if token == "" {
		return deny(407, reasonTokenMissing)
	}
	// Constant time: the token is a secret, and a proxy that leaks it through
	// comparison timing hands the sandbox the credential it is missing.
	if subtle.ConstantTimeCompare([]byte(token), []byte(p.token)) != 1 {
		return deny(403, reasonPolicyMissing)
	}

	// An IP literal never matches a declared pattern (patterns are names —
	// nodemanifest.ValidateEgressPattern refuses literals), so it is classified
	// on its own terms: a private literal is the interesting attack and is named
	// as such; a public one is refused because a grant is over NAMES.
	if ip, err := netip.ParseAddr(name); err == nil {
		if disallowedAddr(ip) {
			return deny(403, reasonPrivateAddress)
		}
		return deny(403, reasonIPLiteral)
	}

	if !allowedPorts[port] {
		return deny(403, reasonPortNotAllowed)
	}
	if !nodemanifest.MatchEgress(p.patterns, name) {
		return deny(403, reasonHostNotDeclared)
	}

	addrs, err := p.resolve(ctx, name)
	if err != nil {
		return deny(403, reasonResolveFailed)
	}
	if len(addrs) == 0 {
		return deny(403, reasonDNSNoAnswer)
	}
	// ANY disallowed answer refuses the whole request. Picking "the good one"
	// out of a mixed answer set is precisely what a rebinding attack wants.
	for _, a := range addrs {
		u := a.Unmap()
		if p.own[u] {
			return deny(403, reasonOwnAddress)
		}
		if disallowedAddr(u) {
			return deny(403, reasonPrivateAddress)
		}
	}
	return decision{Allow: true, Status: 200, Reason: classifyMatch(p.patterns, name),
		Host: name, Port: port, Addr: addrs[0].Unmap()}
}

// bearerToken extracts the credential from a Proxy-Authorization header.
func bearerToken(authorization string) string {
	scheme, value, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// normalizeHost folds a host to the one form the grant is matched against:
// lower case, no trailing dot, punycode. A name that cannot be converted is
// returned lower-cased and will simply fail to match any declared pattern —
// there is no reason code for "unconvertible", and inventing one would put a
// string in the audit vocabulary nothing else knows.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	if ascii, err := idna.Lookup.ToASCII(h); err == nil {
		return ascii
	}
	return h
}

// classifyMatch names WHICH declared pattern admitted the host, using the same
// matcher that made the decision so the two can never disagree.
func classifyMatch(patterns []string, host string) string {
	for _, p := range patterns {
		if !nodemanifest.MatchEgress([]string{p}, host) {
			continue
		}
		switch {
		case p == "*":
			return reasonManifestWildcard
		case strings.HasPrefix(p, "*."):
			return reasonManifestSuffix
		default:
			return reasonManifestExact
		}
	}
	return reasonManifestExact
}
