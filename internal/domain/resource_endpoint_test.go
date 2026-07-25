package domain

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"regexp"
	"strings"
	"testing"
)

// endpointIDShape is the shape a minted id must ALWAYS have. It is duplicated
// here on purpose rather than imported from the production regexp (there is
// none): a test that reuses the implementation's own definition of correct
// cannot catch the implementation changing its mind about it.
var endpointIDShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9]{4}$`)

func testNaming() EndpointNaming {
	return EndpointNaming{Zone: "db.sentiae.com", Region: "eu-central"}
}

// TestEndpointWordLists guards the customer-facing vocabulary: the lists must be
// large enough for the ~4×10^8 space the collision argument assumes, free of
// duplicates (a duplicate silently biases the draw), and made only of words that
// are legal inside a DNS label.
func TestEndpointWordLists(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []string
		min  int
	}{
		{"adjectives", endpointAdjectives, 180},
		{"nouns", endpointNouns, 180},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.list) < tc.min {
				t.Fatalf("%s has %d words, want >= %d", tc.name, len(tc.list), tc.min)
			}
			seen := map[string]bool{}
			for _, w := range tc.list {
				if seen[w] {
					t.Errorf("duplicate word %q biases the draw", w)
				}
				seen[w] = true
				if w != strings.ToLower(w) {
					t.Errorf("word %q is not lowercase", w)
				}
				if !isLowerAlpha(w) {
					t.Errorf("word %q is not plain a-z (illegal in an endpoint id)", w)
				}
				if len(w) < 3 || len(w) > 12 {
					t.Errorf("word %q is %d chars, want 3..12 (readability + label budget)", w, len(w))
				}
			}
		})
	}
	space := len(endpointAdjectives) * len(endpointNouns) * endpointIDNumberSpace
	if space < 300_000_000 {
		t.Fatalf("name space is %d, want >= 3e8", space)
	}
}

// TestMintProducesAServableName asserts the two properties a permanent name can
// never violate: the documented format, and the DNS wire limits.
func TestMintProducesAServableName(t *testing.T) {
	naming := testNaming()
	for i := 0; i < 500; i++ {
		ep, err := naming.Mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !endpointIDShape.MatchString(ep.ID()) {
			t.Fatalf("endpoint id %q does not match adjective-noun-NNNN", ep.ID())
		}
		host := ep.Host()
		if want := ep.ID() + ".eu-central.db.sentiae.com"; host != want {
			t.Fatalf("host = %q, want %q", host, want)
		}
		assertServableDNSName(t, host)
	}
}

// TestMintIsUnpredictable is a smoke check that consecutive mints differ. It
// cannot prove randomness (TestMintDrawsFromTheInjectedEntropySource and
// TestPackageNeverImportsMathRand do that); it catches a mint that got stuck.
func TestMintIsUnpredictable(t *testing.T) {
	naming := testNaming()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		ep, err := naming.Mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		seen[ep.ID()] = true
	}
	if len(seen) < 90 {
		t.Fatalf("100 mints produced only %d distinct ids", len(seen))
	}
}

// TestMintDrawsFromTheInjectedEntropySource proves the id comes from the entropy
// READER and nothing else: the same bytes in produce the same id out, and a
// reader that fails produces a refusal rather than a name from a weaker source.
func TestMintDrawsFromTheInjectedEntropySource(t *testing.T) {
	naming := testNaming()
	fixed := func() io.Reader { return bytes.NewReader(bytes.Repeat([]byte{0x5a, 0xc3, 0x11, 0x07}, 64)) }

	first, err := naming.mint(fixed())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	second, err := naming.mint(fixed())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("same entropy produced %q then %q — the id is not drawn from the reader", first.ID(), second.ID())
	}

	if _, err := naming.mint(failingReader{}); !errors.Is(err, ErrEndpointMintFailed) {
		t.Fatalf("failed entropy: got %v, want ErrEndpointMintFailed", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy source down") }

// TestPackageNeverImportsMathRand is the §30.7 fence, checked structurally: a
// predictable id is a guessable permanent hostname, and the failure mode of the
// wrong import is invisible at runtime.
func TestPackageNeverImportsMathRand(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for name, pkg := range pkgs {
		for file, f := range pkg.Files {
			for _, imp := range f.Imports {
				if imp.Path.Value == `"math/rand"` || imp.Path.Value == `"math/rand/v2"` {
					t.Errorf("%s (package %s) imports %s", file, name, imp.Path.Value)
				}
			}
		}
	}
}

// TestNamingValidateFailsClosed — an unconfigured or malformed naming context
// mints NOTHING. There is no default: a plausible-looking fallback produces a
// permanent name nothing will ever serve.
func TestNamingValidateFailsClosed(t *testing.T) {
	longLabel := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		naming  EndpointNaming
		wantErr error
	}{
		{"configured", EndpointNaming{Zone: "db.sentiae.com", Region: "eu-central"}, nil},
		{"max length region", EndpointNaming{Zone: "db.sentiae.com", Region: strings.Repeat("r", 63)}, nil},
		{"empty zone", EndpointNaming{Zone: "", Region: "eu-central"}, ErrEndpointZoneRequired},
		{"blank zone", EndpointNaming{Zone: "   ", Region: "eu-central"}, ErrEndpointZoneRequired},
		{"empty region", EndpointNaming{Zone: "db.sentiae.com", Region: ""}, ErrEndpointRegionRequired},
		{"blank region", EndpointNaming{Zone: "db.sentiae.com", Region: "\t"}, ErrEndpointRegionRequired},
		{"single label zone", EndpointNaming{Zone: "com", Region: "eu-central"}, ErrEndpointZoneInvalid},
		{"uppercase zone", EndpointNaming{Zone: "DB.sentiae.com", Region: "eu-central"}, ErrEndpointZoneInvalid},
		{"over long zone label", EndpointNaming{Zone: longLabel + ".com", Region: "eu"}, ErrEndpointZoneInvalid},
		{"dotted region", EndpointNaming{Zone: "db.sentiae.com", Region: "eu.central"}, ErrEndpointRegionInvalid},
		{"over long region", EndpointNaming{Zone: "db.sentiae.com", Region: longLabel}, ErrEndpointRegionInvalid},
		{"region with underscore", EndpointNaming{Zone: "db.sentiae.com", Region: "eu_central"}, ErrEndpointRegionInvalid},
		{"region leading hyphen", EndpointNaming{Zone: "db.sentiae.com", Region: "-eu"}, ErrEndpointRegionInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.naming.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
			ep, err := tt.naming.Mint()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Mint() = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !ep.IsZero() {
				t.Fatalf("a refused mint returned a name: %q", ep.Host())
			}
			if tt.wantErr == nil {
				assertServableDNSName(t, ep.Host())
			}
		})
	}
}

// TestNewResourceEndpointValidatesTheWholeName covers rebuilding an endpoint
// from stored parts — the path db-gate will use — including the DNS-253 ceiling
// that only shows up once the parts are assembled.
func TestNewResourceEndpointValidatesTheWholeName(t *testing.T) {
	// A zone that is legal on its own (every label <= 63, total <= 253) but pushes
	// the assembled host past 253 once an id and a region are prepended.
	longZone := strings.Repeat(strings.Repeat("z", 60)+".", 3) + strings.Repeat("y", 55) + ".com"
	tests := []struct {
		name    string
		id      string
		region  string
		zone    string
		wantErr error
	}{
		{"valid", "quiet-forest-4821", "eu-central", "db.sentiae.com", nil},
		{"leading zero suffix", "quiet-forest-0000", "eu-central", "db.sentiae.com", nil},
		{"max length region", "quiet-forest-4821", strings.Repeat("r", 63), "db.sentiae.com", nil},
		{"empty id", "", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"no number", "quiet-forest", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"three digits", "quiet-forest-482", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"five digits", "quiet-forest-48210", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"extra word", "very-quiet-forest-4821", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"uppercase id", "Quiet-forest-4821", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"digits in word", "qu1et-forest-4821", "eu-central", "db.sentiae.com", ErrEndpointIDInvalid},
		{"tenant encoded", "acme-corp-0001", "eu-central", "db.sentiae.com", nil}, // shape-legal; curation, not shape, keeps tenants out
		{"empty region", "quiet-forest-4821", "", "db.sentiae.com", ErrEndpointRegionRequired},
		{"empty zone", "quiet-forest-4821", "eu-central", "", ErrEndpointZoneRequired},
		{"host over 253 octets", "quiet-forest-4821", "eu-central", longZone, ErrEndpointHostTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, err := NewResourceEndpoint(tt.id, tt.region, tt.zone)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewResourceEndpoint(%q,%q,...) = %v, want %v", tt.id, tt.region, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if ep.ID() != tt.id || ep.Region() != tt.region || ep.Zone() != tt.zone {
				t.Fatalf("round trip lost a part: %+v", ep)
			}
			if ep.Host() != ep.String() {
				t.Fatalf("String() %q != Host() %q", ep.String(), ep.Host())
			}
			assertServableDNSName(t, ep.Host())
		})
	}
}

// TestLongestPossibleNameFitsDNS assembles the WORST case the minter can produce
// — the longest words in both lists, a maximum-length region — and asserts it is
// still a legal DNS name under the real zone.
func TestLongestPossibleNameFitsDNS(t *testing.T) {
	longest := func(list []string) string {
		out := list[0]
		for _, w := range list {
			if len(w) > len(out) {
				out = w
			}
		}
		return out
	}
	id := longest(endpointAdjectives) + "-" + longest(endpointNouns) + "-9999"
	ep, err := NewResourceEndpoint(id, strings.Repeat("r", 63), "db.sentiae.com")
	if err != nil {
		t.Fatalf("worst-case name refused: %v", err)
	}
	assertServableDNSName(t, ep.Host())
}

// assertServableDNSName is the property every customer-facing hostname must
// satisfy: <= 253 octets total, every label 1..63 octets and LDH-legal.
func assertServableDNSName(t *testing.T, host string) {
	t.Helper()
	if len(host) > 253 {
		t.Fatalf("host %q is %d octets (max 253)", host, len(host))
	}
	labels := strings.Split(host, ".")
	if len(labels) < 4 { // id + region + at least a two-label zone
		t.Fatalf("host %q has too few labels", host)
	}
	for _, l := range labels {
		if err := validateDNSLabel(l); err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
	}
}
