package domain

import "errors"

// P21 fleet network fabric errors (CP4.5 §9 #5, D-164).
var (
	// ErrFleetNetworkNotFound is returned when no active fleet network matches an
	// id or a (system_id, env). A provision naming an unknown network is REJECTED
	// rather than auto-creating one: auto-create-on-first-provision is the
	// permissive branch, and it is banned.
	ErrFleetNetworkNotFound = errors.New("fleet network not found")
	// ErrInvalidNetworkPolicy is returned when a policy is under-specified (empty
	// endpoint, or a port outside 1..65535 — port 0 NEVER means "any").
	ErrInvalidNetworkPolicy = errors.New("invalid fleet network policy")
	// ErrUnsupportedPolicyProtocol is returned when a policy names a protocol the
	// fleet does not compile. An empty protocol lands here too — it is never
	// defaulted to tcp.
	ErrUnsupportedPolicyProtocol = errors.New("fleet network policy protocol not supported")
	// ErrNetworkOwnerOrgRequired is returned when EnsureNetwork is called without
	// an attested tenant. Unlike Provision (which tolerates "" for legacy CP3 test
	// boots), a network is a net-new surface: strict from birth.
	ErrNetworkOwnerOrgRequired = errors.New("fleet network owner org is required")
	// ErrNetworkEnforcerUnavailable is returned when the iptables enforcer is not
	// available or could not prove its posture. EVERY network operation and EVERY
	// provision of a system-scoped app fails with it. Mirrors
	// ErrVolumeBackendUnavailable — a control that cannot prove itself must
	// PREVENT the operation, never wave it through.
	ErrNetworkEnforcerUnavailable = errors.New("fleet network enforcer unavailable")
	// ErrNetworkPostureUnproven is returned when the host's FORWARD program does
	// not match the enforcer's intended program exactly. It is a refusal, not a
	// warning: a layout we cannot prove is a layout we cannot trust.
	ErrNetworkPostureUnproven = errors.New("fleet network posture could not be proven")
	// ErrNetworkPolicyEgressOverlap is returned when a job's egress_allow names a
	// destination inside the fleet's own subnet. Egress is for EXTERNAL
	// destinations; inter-VM reach is governed by network policies alone.
	ErrNetworkPolicyEgressOverlap = errors.New("egress allowlist may not name fleet-internal addresses")
)
