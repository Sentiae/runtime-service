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
	// ErrRecoveryPointNotFound is returned when no recovery point of the NAMED
	// resource matches a ref. A ref belonging to another resource resolves to
	// this error too: a leaked or guessed object key must never be restorable
	// across resources (D-184).
	ErrRecoveryPointNotFound = errors.New("fleet resource recovery point not found")
	// ErrRestoreInProgress is returned when a restore is already running for a
	// resource, or when the resource's phase is one no restore may start from.
	ErrRestoreInProgress = errors.New("fleet resource restore already in progress")
	// ErrRestoreNoBackingApp is returned when a restore targets a resource with
	// no backing app (a shared-tier or tombstoned resource). Restoring one into a
	// NEW resource is restore-as-fork, a later slice.
	ErrRestoreNoBackingApp = errors.New("fleet resource has no backing app to restore in place")
	// ErrRestoreVolumeAmbiguous is returned when the resource's app does not have
	// exactly one materialized volume. In-place restore swaps one backing file;
	// it must never guess which of several is the recovery point's target.
	ErrRestoreVolumeAmbiguous = errors.New("fleet resource restore requires exactly one materialized volume")
	// ErrRestoreStoreUnavailable is returned when no artifact store is configured
	// on this host, so the recovery point's bytes cannot be fetched.
	ErrRestoreStoreUnavailable = errors.New("fleet resource restore artifact store unavailable")
	// ErrRestoreIntegrity is returned when downloaded recovery-point bytes do not
	// match the catalog's recorded size/checksum. The live volume is untouched.
	ErrRestoreIntegrity = errors.New("fleet resource recovery point failed integrity verification")
)
