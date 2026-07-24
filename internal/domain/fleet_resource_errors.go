package domain

import "errors"

// Durable P19 resource control-plane errors (CP4.5 §9 #3, D-164/D-183).
var (
	// ErrResourceNotFound is returned when no fleet resource matches a handle or
	// an (owner_org, claim_key, env) idempotency lookup.
	ErrResourceNotFound = errors.New("fleet resource not found")
	// ErrResourceConvergeNotSupported is returned when a claim asks to converge an
	// existing resource in a way the current backend does not support (e.g. an
	// in-place tier change that would require a rebuild).
	ErrResourceConvergeNotSupported = errors.New("fleet resource converge not supported")
	// ErrResourceSecretsRequired is returned when a resource class needs engine
	// credential refs but the claim carried none.
	ErrResourceSecretsRequired = errors.New("fleet resource secrets required")
	// ErrResourceClassUnsupported is returned when a claim names a resource class
	// the fleet does not provision.
	ErrResourceClassUnsupported = errors.New("fleet resource class unsupported")
	// ErrResourceTierUnsupported is returned when a claim names a resource tier the
	// invoked provision path does not serve (e.g. a shared claim on the dedicated
	// path).
	ErrResourceTierUnsupported = errors.New("fleet resource tier unsupported")
	// ErrResourceOwnerOrgRequired is returned when a resource claim carries no
	// attested owner org — there is no tenant scope to anchor the claim to (I28).
	ErrResourceOwnerOrgRequired = errors.New("fleet resource owner org required")
	// ErrResourceClaimKeyRequired is returned when a resource claim carries no
	// idempotency claim key.
	ErrResourceClaimKeyRequired = errors.New("fleet resource claim key required")
	// ErrResourceVaultTokenRequired is returned when a resource claim needs the
	// handed per-deployment Vault token to resolve its engine credentials but the
	// claim carried none (fail-closed — the data engine cannot boot credentialless).
	ErrResourceVaultTokenRequired = errors.New("fleet resource vault token required")
	// ErrResourceFinalSnapshotRequired is returned when a durable resource is
	// decommissioned without a final snapshot — a durable tier must not be torn
	// down without a recovery point.
	ErrResourceFinalSnapshotRequired = errors.New("fleet resource final snapshot required")
	// ErrResourceSharedPasswordAmbiguous is returned when a shared-tier claim's
	// resolved secrets do not identify a single role password (neither exactly one
	// secret nor one named "password"). The shared logical role needs exactly one
	// password to CREATE ROLE with.
	ErrResourceSharedPasswordAmbiguous = errors.New("fleet resource shared password ambiguous")
)
