package domain

import (
	"regexp"
	"strings"
)

// HostFailureDomainUnattested is the value migration 0022 backfilled onto every
// fleet_hosts row that existed before the column did. It is deliberately NOT a
// parseable failure domain: it has one segment where the encoding requires three,
// so ParseFailureDomain refuses it and the host is ineligible for HA placement.
//
// That is the point. "This host's failure domain was never stated" and "this host
// is in its own failure domain" are different facts, and only a human can turn the
// first into the second. Two unattested hosts are NOT two domains, and nothing may
// treat them as such — the alternative is selling a tier whose whole promise rests
// on a label nobody wrote.
const HostFailureDomainUnattested = "unattested"

// failureDomainSegmentRx bounds one segment of a failure domain: a lowercase DNS-ish
// label. Not a DNS name — a failure domain is never resolved — but the same
// character class, because these values are read by humans in incident channels and
// pasted into config files, and mixed case or spaces make two identical domains
// look different.
var failureDomainSegmentRx = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// failureDomainSegments is the fixed arity of the encoding: site/power/network.
const failureDomainSegments = 3

// FailureDomain is a host's structured, human-supplied statement of what it shares
// a fate with (D-196 amendment 2). The wire and storage form is three POSITIONAL,
// non-empty, slash-separated segments:
//
//	site/power/network        e.g.  rgalileo-room/breaker-a/switch-1
//
// A bare label is refused. Three machines in one room on one breaker are three
// hosts and ONE power domain; a label like "host-a" cannot say so, and the first
// honest answer to "what does this label mean" must not arrive during an incident.
// Fixed arity (rather than an extensible key=value form) is what stops a partial
// answer from parsing: an operator cannot state a site and leave the breaker
// unknown, because the unknown breaker is exactly the fact that decides whether a
// standby survives.
//
// ⚠ The placement rule compares the WHOLE value for equality (design §5.1:
// "different failure_domain"), never segment-wise. "Same site, different power is
// good enough" is a policy this platform has not decided; the encoding records the
// facts so that decision remains available, and until it is made the strict rule
// holds.
type FailureDomain struct {
	site    string
	power   string
	network string
}

// ParseFailureDomain validates and parses the stored/wire form. It is the ONLY way
// to obtain a FailureDomain, so an unparseable value can never reach a placement
// decision as if it were a domain — including HostFailureDomainUnattested and the
// empty string.
func ParseFailureDomain(s string) (FailureDomain, error) {
	if strings.TrimSpace(s) == "" {
		return FailureDomain{}, ErrHostFailureDomainRequired
	}
	parts := strings.Split(s, "/")
	if len(parts) != failureDomainSegments {
		return FailureDomain{}, ErrHostFailureDomainInvalid
	}
	for _, p := range parts {
		if !failureDomainSegmentRx.MatchString(p) {
			return FailureDomain{}, ErrHostFailureDomainInvalid
		}
	}
	return FailureDomain{site: parts[0], power: parts[1], network: parts[2]}, nil
}

// String returns the canonical stored form.
func (d FailureDomain) String() string {
	return d.site + "/" + d.power + "/" + d.network
}

// Site, Power and Network expose the recorded facts. They exist so a later,
// deliberately-decided placement policy can reason about them; the current rule
// does not, and must not start doing so by accident.
func (d FailureDomain) Site() string    { return d.site }
func (d FailureDomain) Power() string   { return d.power }
func (d FailureDomain) Network() string { return d.network }

// HasAttestedFailureDomain reports whether this host carries a failure domain a
// placement decision may rely on. A host that does not is not "in an unknown
// domain" — it is not a placement candidate for any tier whose promise depends on
// domains.
func (h Host) HasAttestedFailureDomain() bool {
	_, err := ParseFailureDomain(h.FailureDomain)
	return err == nil
}
