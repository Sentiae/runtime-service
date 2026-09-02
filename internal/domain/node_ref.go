package domain

import (
	"regexp"
	"strings"
)

// nodeSemverRx is the semver shape a published node version carries. It is the
// structural check only: whether THIS semver was ever published is a
// node-service fact the runtime never re-derives.
var nodeSemverRx = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// nodeDigestRx pins the one image identity the sandbox may run by. A tag is not
// an identity — it can be moved after a graph is created — so the digest is
// mandatory and its shape is fixed here.
var nodeDigestRx = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// nodePortNameRx mirrors platform-kit's nodemanifest port-name rule. The two
// must agree: a port the manifest accepts and the runtime rejects would make a
// legal flow unrunnable, and the reverse would let an unnamed port through.
var nodePortNameRx = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// NodeRef is the resolved identity of the bundle a graph node runs: WHAT it is
// (@scope/name@semver), WHICH implementation (language), and the exact bytes
// (image ref + digest). Every field is required — a node that cannot name its
// digest cannot be run, only guessed at.
type NodeRef struct {
	QualifiedName string `json:"qualified_name"`
	Semver        string `json:"semver"`
	Language      string `json:"language"`
	ImageRef      string `json:"image_ref"`
	Digest        string `json:"digest"`
}

// NewNodeRef validates the reference structurally and returns it. Structural
// only, deliberately: existence, publication status and reachability are
// node-service and registry facts, and re-asserting them here would be a second
// source of truth that drifts.
func NewNodeRef(qualifiedName, semver, language, imageRef, digest string) (*NodeRef, error) {
	if !strings.HasPrefix(qualifiedName, "@") || !strings.Contains(qualifiedName, "/") {
		return nil, ErrInvalidNodeRef
	}
	if !nodeSemverRx.MatchString(semver) {
		return nil, ErrInvalidNodeRef
	}
	if language != "go" && language != "typescript" {
		return nil, ErrInvalidNodeRef
	}
	if imageRef == "" {
		return nil, ErrInvalidNodeRef
	}
	if !nodeDigestRx.MatchString(digest) {
		return nil, ErrInvalidNodeRef
	}
	return &NodeRef{
		QualifiedName: qualifiedName,
		Semver:        semver,
		Language:      language,
		ImageRef:      imageRef,
		Digest:        digest,
	}, nil
}

// Pin renders the reference as the pin literal a manifest and a .flow document
// both spell: @scope/name@semver.
func (r *NodeRef) Pin() string {
	if r == nil {
		return ""
	}
	return r.QualifiedName + "@" + r.Semver
}

// Repository is the registry repository path inside ImageRef — the host and the
// tag stripped: `10.0.10.20:8078/acme/hello.node:1.0.9-go` ⇒ `acme/hello.node`.
// The pull is by digest against this repository, never by the tag the ref
// happens to carry.
func (r *NodeRef) Repository() string {
	if r == nil || r.ImageRef == "" {
		return ""
	}
	ref := r.ImageRef
	if i := strings.Index(ref, "/"); i >= 0 {
		// A host is only a host when it looks like one; otherwise the first
		// segment is part of the repository path.
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			ref = ref[i+1:]
		}
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		return ref[:i]
	}
	return ref
}

// PortSpec is one declared port: its name and whether the node refuses to run
// without it.
type PortSpec struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// PortSpecs is a node's declared surface, carried on the row so the runtime can
// rebuild the execution plan from the database alone.
type PortSpecs struct {
	Inputs  []PortSpec `json:"inputs"`
	Outputs []PortSpec `json:"outputs"`
}

// NewPortSpecs validates both directions: every name matches the port-name rule
// and is unique WITHIN its direction (an input and an output may share a name —
// they are different ports).
func NewPortSpecs(inputs, outputs []PortSpec) (PortSpecs, error) {
	in, err := validPortSide(inputs)
	if err != nil {
		return PortSpecs{}, err
	}
	out, err := validPortSide(outputs)
	if err != nil {
		return PortSpecs{}, err
	}
	return PortSpecs{Inputs: in, Outputs: out}, nil
}

func validPortSide(ports []PortSpec) ([]PortSpec, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(ports))
	out := make([]PortSpec, 0, len(ports))
	for _, p := range ports {
		if !nodePortNameRx.MatchString(p.Name) {
			return nil, ErrInvalidPortSpec
		}
		if seen[p.Name] {
			return nil, ErrInvalidPortSpec
		}
		seen[p.Name] = true
		out = append(out, p)
	}
	return out, nil
}

// RequiredInputs is the input names the node refuses to run without, in
// declaration order.
func (p PortSpecs) RequiredInputs() []string {
	var out []string
	for _, in := range p.Inputs {
		if in.Required {
			out = append(out, in.Name)
		}
	}
	return out
}

// OutputNames is every declared output name in declaration order.
func (p PortSpecs) OutputNames() []string {
	var out []string
	for _, o := range p.Outputs {
		out = append(out, o.Name)
	}
	return out
}

// SecretSpec is a secret the node DECLARES. A value never appears here: the row
// records only what may be asked for, and the value is resolved per invocation
// and handed to the sidecar on an attached stdin stream.
type SecretSpec struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// NewSecretSpecs validates the declared secret names against the same rule the
// manifest uses, and refuses a duplicate — two specs for one name would make
// "required" ambiguous.
func NewSecretSpecs(secrets []SecretSpec) ([]SecretSpec, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(secrets))
	out := make([]SecretSpec, 0, len(secrets))
	for _, s := range secrets {
		if !nodePortNameRx.MatchString(s.Name) {
			return nil, ErrInvalidSecretSpec
		}
		if seen[s.Name] {
			return nil, ErrInvalidSecretSpec
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	return out, nil
}

// NodeRole is the node's position in the request/response shape of a flow.
type NodeRole string

const (
	// NodeRoleNone is an ordinary node: it neither starts the flow nor answers it.
	NodeRoleNone NodeRole = ""
	// NodeRoleTrigger starts the flow and takes the request object as its input.
	NodeRoleTrigger NodeRole = "trigger"
	// NodeRoleRespond produces the flow's response.
	NodeRoleRespond NodeRole = "respond"
)

// ParseNodeRole accepts exactly the three roles a plan can carry.
func ParseNodeRole(s string) (NodeRole, error) {
	switch NodeRole(s) {
	case NodeRoleNone, NodeRoleTrigger, NodeRoleRespond:
		return NodeRole(s), nil
	}
	return "", ErrInvalidRole
}
