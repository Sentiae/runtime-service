package domain

import (
	crand "crypto/rand"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// The customer-facing name of a database (D-190).
//
// ⚠ WHY THIS IS A VALUE OBJECT AND NOT A SPRINTF AT THE CALL SITE. A hostname is
// the single most PERMANENT artifact this platform hands a customer: the moment
// it is pasted into an application's config, changing it breaks that
// application. There is therefore exactly ONE place a customer-facing hostname
// is computed, and every rule that makes such a name safe to keep forever lives
// here:
//
//	<endpoint-id>.<region>.<zone>   e.g. quiet-forest-4821.eu-central.db.sentiae.com
//
//   - The endpoint id is READABLE RANDOM (adjective-noun-NNNN), minted from
//     crypto/rand at resource birth and IMMUTABLE for the life of the resource.
//     It is derived from NOTHING — not the claim key, the org, the app, the host
//     or the resource uuid — which is what lets a claim be renamed and every
//     internal key (object store prefix, lease, uid) be rotated without touching
//     a connection string a customer already holds.
//   - NOTHING ABOUT THE TENANT IS ENCODED. SNI and DNS resolver logs are
//     semi-public, so `acme-corp.db.sentiae.com` would publish the customer list.
//     This is why a customer-chosen label was rejected despite being friendlier.
//   - The REGION is encoded, deliberately: data never silently crosses one, so
//     encoding it avoids a global routing tier on the connect path at the cost of
//     one wildcard certificate per region. Moving a database across regions
//     yields a new hostname, which is honest rather than hidden.
//   - The zone and the region are CONFIG, and empty is a REFUSAL, never a
//     default. A plausible-looking fallback (`fleet.sentiae.local` is the
//     in-repo anti-pattern) is how a resource is born carrying a name no gate
//     will ever serve — and by the time anyone notices, the name is permanent.
//
// This is a SEPARATE naming path from hostForApp/sanitizeSlug on purpose: that
// one is lossy by its own admission and already overflows 63 octets into a hash
// truncation. Neither may be reused for the other.

const (
	// endpointIDNumberSpace is the exclusive upper bound of the 4-digit suffix.
	// Leading zeros are kept (0000 is a legal suffix), so the suffix contributes a
	// full 10^4 rather than the ~9·10^3 a "no leading zero" rule would leave.
	endpointIDNumberSpace = 10000

	// dnsMaxLabel / dnsMaxName are the wire limits of a DNS name (RFC 1035): 63
	// octets per label, 253 octets for the presentation form of the whole name.
	// They are asserted on EVERY minted endpoint rather than assumed from the word
	// lists, because the region and the zone are operator input and a name that
	// exceeds them is unresolvable — which is a customer-visible outage that no
	// amount of later care can fix, since the name is permanent.
	dnsMaxLabel = 63
	dnsMaxName  = 253
)

// ResourceEndpoint is the customer-facing DNS identity of a provisioned
// resource: a minted, immutable endpoint id inside a region inside a zone. It is
// immutable, validated on construction, and identity-by-value.
type ResourceEndpoint struct {
	id     string
	region string
	zone   string
}

// EndpointNaming is the config-provided naming context an endpoint is minted
// into: the DB zone (e.g. `db.sentiae.com`) and the region label (e.g.
// `eu-central`). Both are REQUIRED — see Validate.
type EndpointNaming struct {
	Zone   string
	Region string
}

// Validate reports whether this naming context can mint a servable name. It is
// separate from Mint so a caller can refuse a provision BEFORE it does any work
// (boots a VM, allocates a volume) on a host that could never produce a valid
// hostname for what it is about to create.
func (n EndpointNaming) Validate() error {
	if strings.TrimSpace(n.Zone) == "" {
		return ErrEndpointZoneRequired
	}
	if strings.TrimSpace(n.Region) == "" {
		return ErrEndpointRegionRequired
	}
	if err := validateEndpointZone(n.Zone); err != nil {
		return err
	}
	return validateEndpointRegion(n.Region)
}

// Mint mints a NEW endpoint in this naming context, drawing every random
// component from crypto/rand. It is the ONLY way an endpoint id comes into
// existence.
//
// A mint can collide with a name already taken (the unique index on
// fleet_resources.endpoint_id is the arbiter, never this function), so the
// caller retries a bounded number of times — see ErrEndpointMintExhausted.
func (n EndpointNaming) Mint() (ResourceEndpoint, error) {
	return n.mint(crand.Reader)
}

// mint is Mint with an injectable entropy source, so a test can force a specific
// id. Production always passes crypto/rand.Reader; nothing in this package
// imports math/rand, and a test asserts that.
func (n EndpointNaming) mint(entropy io.Reader) (ResourceEndpoint, error) {
	if err := n.Validate(); err != nil {
		return ResourceEndpoint{}, err
	}
	adjective, err := pickWord(entropy, endpointAdjectives)
	if err != nil {
		return ResourceEndpoint{}, err
	}
	noun, err := pickWord(entropy, endpointNouns)
	if err != nil {
		return ResourceEndpoint{}, err
	}
	number, err := randomIndex(entropy, endpointIDNumberSpace)
	if err != nil {
		return ResourceEndpoint{}, err
	}
	return NewResourceEndpoint(fmt.Sprintf("%s-%s-%04d", adjective, noun, number), n.Region, n.Zone)
}

// NewResourceEndpoint rebuilds an endpoint from its stored parts (the id and
// region persisted on the resource row) plus the configured zone, validating the
// whole name. A stored id that no longer validates is an error, not a silent
// repair: the name is permanent, so the only honest answers are "serve exactly
// this" or "refuse".
func NewResourceEndpoint(id, region, zone string) (ResourceEndpoint, error) {
	if err := validateEndpointID(id); err != nil {
		return ResourceEndpoint{}, err
	}
	if err := validateEndpointRegion(region); err != nil {
		return ResourceEndpoint{}, err
	}
	if err := validateEndpointZone(zone); err != nil {
		return ResourceEndpoint{}, err
	}
	ep := ResourceEndpoint{id: id, region: region, zone: zone}
	if host := ep.Host(); len(host) > dnsMaxName {
		return ResourceEndpoint{}, fmt.Errorf("%w: %d octets (max %d)", ErrEndpointHostTooLong, len(host), dnsMaxName)
	}
	return ep, nil
}

// ID is the minted, immutable endpoint id (`quiet-forest-4821`).
func (e ResourceEndpoint) ID() string { return e.id }

// Region is the region label encoded in the name.
func (e ResourceEndpoint) Region() string { return e.region }

// Zone is the DB zone the name lives under.
func (e ResourceEndpoint) Zone() string { return e.zone }

// Host is the full customer-facing hostname.
func (e ResourceEndpoint) Host() string {
	return e.id + "." + e.region + "." + e.zone
}

// String renders the hostname (there is nothing else an endpoint means).
func (e ResourceEndpoint) String() string { return e.Host() }

// IsZero reports whether this is the empty endpoint (no identity).
func (e ResourceEndpoint) IsZero() bool { return e.id == "" }

// validateEndpointID enforces the minted shape: <adjective>-<noun>-<4 digits>,
// lowercase ASCII, and a legal DNS label. The shape is checked structurally
// rather than against the word lists on purpose — the lists may GROW (a name
// already minted from an older list must keep validating forever), but the shape
// never changes.
func validateEndpointID(id string) error {
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		return fmt.Errorf("%w: %q is not <adjective>-<noun>-<nnnn>", ErrEndpointIDInvalid, id)
	}
	for _, word := range parts[:2] {
		if word == "" || !isLowerAlpha(word) {
			return fmt.Errorf("%w: %q has a non-alphabetic word", ErrEndpointIDInvalid, id)
		}
	}
	if len(parts[2]) != 4 || !isDigits(parts[2]) {
		return fmt.Errorf("%w: %q does not end in 4 digits", ErrEndpointIDInvalid, id)
	}
	if err := validateDNSLabel(id); err != nil {
		return fmt.Errorf("%w: %s", ErrEndpointIDInvalid, err)
	}
	return nil
}

// validateEndpointRegion enforces that the region is exactly ONE legal DNS
// label. A dotted region would silently deepen the name and break the one
// wildcard certificate per region that makes this scheme O(1).
func validateEndpointRegion(region string) error {
	if region == "" {
		return ErrEndpointRegionRequired
	}
	if strings.Contains(region, ".") {
		return fmt.Errorf("%w: %q must be a single label", ErrEndpointRegionInvalid, region)
	}
	if err := validateDNSLabel(region); err != nil {
		return fmt.Errorf("%w: %s", ErrEndpointRegionInvalid, err)
	}
	return nil
}

// validateEndpointZone enforces a real, multi-label zone. A single-label zone
// (a bare TLD, or a stray hostname) is refused: it is never a zone this platform
// can hold a wildcard certificate for, and accepting it is how a resource is
// born with an unservable permanent name.
func validateEndpointZone(zone string) error {
	if zone == "" {
		return ErrEndpointZoneRequired
	}
	labels := strings.Split(zone, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: %q is not a delegable zone", ErrEndpointZoneInvalid, zone)
	}
	for _, label := range labels {
		if err := validateDNSLabel(label); err != nil {
			return fmt.Errorf("%w: %s", ErrEndpointZoneInvalid, err)
		}
	}
	if len(zone) > dnsMaxName {
		return fmt.Errorf("%w: %d octets (max %d)", ErrEndpointZoneInvalid, len(zone), dnsMaxName)
	}
	return nil
}

// validateDNSLabel enforces the LDH label rule (RFC 1035 + RFC 1123): 1..63
// octets of lowercase letters, digits and hyphens, not starting or ending with a
// hyphen. Uppercase is rejected rather than folded — a name that is only valid
// after a repair is a name two components can disagree about.
func validateDNSLabel(label string) error {
	if label == "" {
		return fmt.Errorf("empty DNS label")
	}
	if len(label) > dnsMaxLabel {
		return fmt.Errorf("DNS label %q is %d octets (max %d)", label, len(label), dnsMaxLabel)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("DNS label %q starts or ends with a hyphen", label)
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return fmt.Errorf("DNS label %q contains an illegal character %q", label, string(c))
		}
	}
	return nil
}

func isLowerAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pickWord draws one word uniformly from list using the given entropy source.
func pickWord(entropy io.Reader, list []string) (string, error) {
	i, err := randomIndex(entropy, len(list))
	if err != nil {
		return "", err
	}
	return list[i], nil
}

// randomIndex returns a uniform value in [0,n) from the entropy source. It is
// crypto/rand's rejection-sampling Int — never a modulo of a raw read, which
// skews, and never math/rand, which is predictable (§30.7).
func randomIndex(entropy io.Reader, n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: empty selection space", ErrEndpointMintFailed)
	}
	v, err := crand.Int(entropy, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrEndpointMintFailed, err)
	}
	return int(v.Int64()), nil
}
