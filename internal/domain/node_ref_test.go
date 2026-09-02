package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/nodemanifest"
)

const (
	testDigest    = "sha256:aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988"
	testImageRef  = "10.0.10.20:8078/acme/hello.node:1.0.9-go"
	testQualified = "@acme/hello"
)

// T1.1 — proves NewNodeRef refuses every reference the sandbox could not run by
// digest, and that Pin/Repository render the two forms the rest of the phase
// depends on.
//
// Control: drop the nodeDigestRx check from NewNodeRef ⇒ the "tag instead of
// digest" and "short digest" rows pass, i.e. a graph could be created naming an
// image identity that can be moved after the fact.
func TestNewNodeRef(t *testing.T) {
	tests := []struct {
		name      string
		qualified string
		semver    string
		language  string
		imageRef  string
		digest    string
		wantErr   error
	}{
		{"valid go", testQualified, "1.0.9", "go", testImageRef, testDigest, nil},
		{"valid typescript", testQualified, "1.0.9", "typescript", testImageRef, testDigest, nil},
		{"valid prerelease semver", testQualified, "1.0.9-rc.1", "go", testImageRef, testDigest, nil},
		{"qualified name without @", "acme/hello", "1.0.9", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"qualified name without /", "@acmehello", "1.0.9", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"empty qualified name", "", "1.0.9", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"semver with v prefix", testQualified, "v1.0.9", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"partial semver", testQualified, "1.0", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"empty semver", testQualified, "", "go", testImageRef, testDigest, ErrInvalidNodeRef},
		{"unknown language", testQualified, "1.0.9", "python", testImageRef, testDigest, ErrInvalidNodeRef},
		{"empty language", testQualified, "1.0.9", "", testImageRef, testDigest, ErrInvalidNodeRef},
		{"empty image ref", testQualified, "1.0.9", "go", "", testDigest, ErrInvalidNodeRef},
		{"tag instead of digest", testQualified, "1.0.9", "go", testImageRef, "1.0.9-go", ErrInvalidNodeRef},
		{"short digest", testQualified, "1.0.9", "go", testImageRef, "sha256:aaf52988", ErrInvalidNodeRef},
		{"uppercase digest", testQualified, "1.0.9", "go", testImageRef, "sha256:AAF52988AAF52988AAF52988AAF52988AAF52988AAF52988AAF52988AAF52988", ErrInvalidNodeRef},
		{"digest without algorithm", testQualified, "1.0.9", "go", testImageRef, "aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988", ErrInvalidNodeRef},
		{"empty digest", testQualified, "1.0.9", "go", testImageRef, "", ErrInvalidNodeRef},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := NewNodeRef(tt.qualified, tt.semver, tt.language, tt.imageRef, tt.digest)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewNodeRef error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if ref != nil {
					t.Fatalf("refused reference still returned a value: %+v", ref)
				}
				return
			}
			if got, want := ref.Pin(), tt.qualified+"@"+tt.semver; got != want {
				t.Fatalf("Pin() = %q, want %q", got, want)
			}
		})
	}
}

// T1.1 (second half) — Repository strips the registry host and the tag, which
// is what the pull-by-digest call is built from.
//
// Control: return ImageRef unchanged from Repository ⇒ every row below fails.
func TestNodeRef_Repository(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		want     string
	}{
		{"host port and tag", "10.0.10.20:8078/acme/hello.node:1.0.9-go", "acme/hello.node"},
		{"tls host and tag", "10.0.10.20:8443/acme/hello.node:1.0.9-go", "acme/hello.node"},
		{"host and digest", "10.0.10.20:8443/acme/hello.node@" + testDigest, "acme/hello.node"},
		{"no tag", "10.0.10.20:8078/acme/hello.node", "acme/hello.node"},
		{"no host", "acme/hello.node:1.0.9-go", "acme/hello.node"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := &NodeRef{ImageRef: tt.imageRef}
			if got := ref.Repository(); got != tt.want {
				t.Fatalf("Repository() = %q, want %q", got, tt.want)
			}
		})
	}
	var nilRef *NodeRef
	if got := nilRef.Repository(); got != "" {
		t.Fatalf("nil NodeRef Repository() = %q, want empty", got)
	}
	if got := nilRef.Pin(); got != "" {
		t.Fatalf("nil NodeRef Pin() = %q, want empty", got)
	}
}

// T1.2 — proves NewPortSpecs enforces the name grammar and per-direction
// uniqueness, and that the two projections the plan is rebuilt from
// (RequiredInputs, OutputNames) keep declaration order.
//
// Control: make validPortSide share ONE seen-set across both directions ⇒ the
// "same name in both directions" row fails, i.e. a legal manifest is refused.
func TestNewPortSpecs(t *testing.T) {
	tests := []struct {
		name    string
		inputs  []PortSpec
		outputs []PortSpec
		wantErr error
	}{
		{"empty", nil, nil, nil},
		{"valid", []PortSpec{{Name: "body", Required: true}}, []PortSpec{{Name: "out"}}, nil},
		{"same name in both directions", []PortSpec{{Name: "body"}}, []PortSpec{{Name: "body"}}, nil},
		{"underscores and digits", []PortSpec{{Name: "status_code2"}}, nil, nil},
		{"duplicate input", []PortSpec{{Name: "body"}, {Name: "body"}}, nil, ErrInvalidPortSpec},
		{"duplicate output", nil, []PortSpec{{Name: "out"}, {Name: "out"}}, ErrInvalidPortSpec},
		{"uppercase", []PortSpec{{Name: "Body"}}, nil, ErrInvalidPortSpec},
		{"leading digit", []PortSpec{{Name: "1body"}}, nil, ErrInvalidPortSpec},
		{"leading underscore", []PortSpec{{Name: "_body"}}, nil, ErrInvalidPortSpec},
		{"dash", []PortSpec{{Name: "content-type"}}, nil, ErrInvalidPortSpec},
		{"dot", []PortSpec{{Name: "config.key"}}, nil, ErrInvalidPortSpec},
		{"empty name", []PortSpec{{Name: ""}}, nil, ErrInvalidPortSpec},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPortSpecs(tt.inputs, tt.outputs)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewPortSpecs error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	ports, err := NewPortSpecs(
		[]PortSpec{{Name: "body", Required: true}, {Name: "headers"}, {Name: "query", Required: true}},
		[]PortSpec{{Name: "status_code"}, {Name: "body"}, {Name: "data"}},
	)
	if err != nil {
		t.Fatalf("NewPortSpecs: %v", err)
	}
	if got, want := ports.RequiredInputs(), []string{"body", "query"}; !equalStrings(got, want) {
		t.Fatalf("RequiredInputs() = %v, want %v", got, want)
	}
	if got, want := ports.OutputNames(), []string{"status_code", "body", "data"}; !equalStrings(got, want) {
		t.Fatalf("OutputNames() = %v, want %v", got, want)
	}
}

// T1.2 (secrets) — the same grammar, and no duplicate: two specs for one name
// would make `required` ambiguous.
//
// Control: drop the duplicate check from NewSecretSpecs ⇒ the duplicate row passes.
func TestNewSecretSpecs(t *testing.T) {
	tests := []struct {
		name    string
		in      []SecretSpec
		wantErr error
	}{
		{"empty", nil, nil},
		{"valid", []SecretSpec{{Name: "greeting_suffix", Required: false}}, nil},
		{"duplicate", []SecretSpec{{Name: "a"}, {Name: "a"}}, ErrInvalidSecretSpec},
		{"uppercase", []SecretSpec{{Name: "API_KEY"}}, ErrInvalidSecretSpec},
		{"dash", []SecretSpec{{Name: "api-key"}}, ErrInvalidSecretSpec},
		{"empty name", []SecretSpec{{Name: ""}}, ErrInvalidSecretSpec},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSecretSpecs(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewSecretSpecs error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// T1.3 — three roles, and nothing else. A fourth role is a graph the runtime
// cannot execute, so it is refused where it enters.
//
// Control: add a `default: return NodeRole(s), nil` fallthrough to
// ParseNodeRole ⇒ every invalid row passes.
func TestParseNodeRole(t *testing.T) {
	tests := []struct {
		in      string
		want    NodeRole
		wantErr error
	}{
		{"", NodeRoleNone, nil},
		{"trigger", NodeRoleTrigger, nil},
		{"respond", NodeRoleRespond, nil},
		{"Trigger", "", ErrInvalidRole},
		{"TRIGGER", "", ErrInvalidRole},
		{"response", "", ErrInvalidRole},
		{"transform", "", ErrInvalidRole},
		{" trigger", "", ErrInvalidRole},
	}
	for _, tt := range tests {
		t.Run("role="+tt.in, func(t *testing.T) {
			got, err := ParseNodeRole(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseNodeRole(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseNodeRole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// T1.4 — the runtime's port-name rule and platform-kit's MUST agree. They are
// two copies of one grammar (the domain may not import platform-kit, §3.3), and
// a divergence in either direction is a live defect: a name the manifest
// accepts and the runtime rejects makes a legal flow unrunnable, and the
// reverse lets an unnamed port through. This drives BOTH validators over the
// same corpus and requires the same verdict from each.
//
// Control: relax nodePortNameRx to `^[a-z][a-z0-9_-]{0,63}$` ⇒ the "dash" row
// diverges (runtime accepts, platform-kit refuses) and the test fails.
func TestPortRx_MatchesPlatformKit(t *testing.T) {
	names := []string{
		"body", "out", "status_code", "a", "a1", "a_1", "name64",
		"", "Body", "1body", "_body", "content-type", "config.key", "body ", " body",
		"a23456789012345678901234567890123456789012345678901234567890123",  // 64 chars: legal
		"a234567890123456789012345678901234567890123456789012345678901234", // 65 chars: not
	}
	for _, name := range names {
		t.Run("name="+name, func(t *testing.T) {
			runtimeOK := nodePortNameRx.MatchString(name)
			pkOK := platformKitAcceptsPortName(t, name)
			if runtimeOK != pkOK {
				t.Fatalf("port name %q: runtime accepts=%v, platform-kit accepts=%v — the two grammars have diverged", name, runtimeOK, pkOK)
			}
		})
	}
}

// platformKitAcceptsPortName asks nodemanifest itself, rather than restating
// its regex here — restating it would make this test pass against a copy of the
// bug it exists to catch.
func platformKitAcceptsPortName(t *testing.T, name string) bool {
	t.Helper()
	man := &nodemanifest.Manifest{
		Name:     "@acme/probe",
		Category: "transform",
		Shape:    "inline",
		Display:  nodemanifest.Display{Name: "Probe", Icon: "box", Description: "port-name probe"},
		Outputs:  []nodemanifest.Port{{Name: name}},
	}
	for _, d := range nodemanifest.Validate(man) {
		if d.Code == nodemanifest.CodePortNameInvalid {
			return false
		}
	}
	return true
}

// T1.8 — Validate is the fail-closed gate on a bundle row, and
// NeedsSidecar/NeedsBridge are the two independent questions the invocation
// topology turns on.
//
// Control (Validate): drop the `n.NodeRef == nil` check ⇒ the "missing node
// ref" row passes and a graph with no pin becomes creatable.
// Control (topology): make NeedsSidecar return the same as NeedsBridge ⇒ the
// secrets-only row fails, i.e. a node with a secret and no egress would launch
// with no broker to answer it.
func TestGraphNode_Validate(t *testing.T) {
	ref := &NodeRef{QualifiedName: testQualified, Semver: "1.0.9", Language: "go", ImageRef: testImageRef, Digest: testDigest}
	base := func() GraphNode {
		return GraphNode{ID: uuid.New(), GraphID: uuid.New(), NodeType: GraphNodeTypeBundle, Name: "greet", NodeRef: ref}
	}

	tests := []struct {
		name    string
		mutate  func(*GraphNode)
		wantErr error
	}{
		{"valid bundle", func(*GraphNode) {}, nil},
		{"missing id", func(n *GraphNode) { n.ID = uuid.Nil }, ErrInvalidID},
		{"missing graph id", func(n *GraphNode) { n.GraphID = uuid.Nil }, ErrInvalidID},
		{"missing name", func(n *GraphNode) { n.Name = "" }, ErrInvalidData},
		{"missing node ref", func(n *GraphNode) { n.NodeRef = nil }, ErrNodeRefRequired},
		// The interpreter's type constants are gone (S2), so a legacy row is
		// spelled the way the DATABASE spells it — the raw string — which is
		// also the only way such a row can still arrive.
		{"legacy code type", func(n *GraphNode) { n.NodeType = GraphNodeType("code") }, ErrInvalidData},
		{"legacy http type", func(n *GraphNode) { n.NodeType = GraphNodeType("http") }, ErrInvalidData},
		{"empty type", func(n *GraphNode) { n.NodeType = "" }, ErrInvalidData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := base()
			tt.mutate(&n)
			if err := n.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestGraphNode_NeedsSidecarAndBridge(t *testing.T) {
	tests := []struct {
		name       string
		secrets    []SecretSpec
		egress     []string
		wantCar    bool
		wantBridge bool
	}{
		{"neither", nil, nil, false, false},
		{"secrets only", []SecretSpec{{Name: "greeting_suffix"}}, nil, true, false},
		{"egress only", nil, []string{"httpbin.org"}, true, true},
		{"both", []SecretSpec{{Name: "greeting_suffix"}}, []string{"*"}, true, true},
		{"empty slices are neither", []SecretSpec{}, []string{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &GraphNode{Secrets: tt.secrets, Egress: tt.egress}
			if got := n.NeedsSidecar(); got != tt.wantCar {
				t.Fatalf("NeedsSidecar() = %v, want %v", got, tt.wantCar)
			}
			if got := n.NeedsBridge(); got != tt.wantBridge {
				t.Fatalf("NeedsBridge() = %v, want %v", got, tt.wantBridge)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
