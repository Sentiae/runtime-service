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
	// ErrRestoreEngineNotAdmitting is returned when a restored (or rolled-back)
	// data engine listens on its port but does not ADMIT clients — the state a
	// TCP dial cannot see and which twice let a restore with a torn pg_hba.conf
	// report ready/verified while refusing every connection
	// (#p19-restore-false-green-health). It also covers the cases where the claim
	// cannot be checked at all (no addressable resident replica), because a
	// restore that cannot be proven usable must not be declared successful.
	ErrRestoreEngineNotAdmitting = errors.New("fleet resource restore: engine is listening but not admitting clients")
	// ErrRestoreNoPrerestoreAnchor is returned when a ROLLBACK has no
	// `.prerestore` original to put back. It is terminal by construction: the
	// anchor is the only surviving copy of the pre-restore data, so without it
	// there is nothing to return the live path to and no retry can invent one.
	// The forward swap does NOT use this — with neither a live file nor an anchor
	// there is nothing to park and the restore proceeds (see swapIn).
	ErrRestoreNoPrerestoreAnchor = errors.New("fleet resource restore: no pre-restore volume to roll back to")
	// ErrSnapshotNotQuiescible is returned when a volume's guest could not be
	// quiesced (syncfs + fsfreeze over the D-185a control channel) before the
	// backing file was copied. The snapshot is REFUSED rather than recorded:
	// pausing the VMM alone does not flush the guest kernel's dirty page cache, so
	// a copy taken without the freeze can be missing or torn
	// (#p19-snapshot-not-guest-consistent — two live restores produced a
	// correctly-sized, NUL-tailed pg_hba.conf). The checksum gate cannot catch
	// that: it proves transport integrity, not source consistency, so a torn
	// snapshot verifies clean forever.
	ErrSnapshotNotQuiescible = errors.New("fleet resource volume snapshot refused: guest could not be quiesced")
	// ErrVolumeBackingFileMissing is returned when a volume's backing file is not
	// on the host, so there is nothing to snapshot. It is the terminal shape of a
	// resource whose data is already gone: the refusal is correct (a durable
	// resource must not be torn down without a recovery point), but without this
	// sentinel it reached the caller as a bare Internal, indistinguishable from a
	// panic (#resource-final-snapshot-failure-is-a-bare-500).
	ErrVolumeBackingFileMissing = errors.New("fleet resource volume backing file is missing")
	// ErrVolumeIdentityMismatch is returned when the file at a volume's backing
	// path IS a Sentiae-stamped volume but belongs to a DIFFERENT volume. Adoption
	// used to be a bare os.Stat: "something is here" was accepted as "this volume's
	// data is here", which are not the same claim. A stale file left at a reused
	// uuid-derived path, or a `.prerestore`/`.failed-*` sibling an operator renamed
	// into place, would then be attached to a live database as if it were the
	// customer's current data. The filesystem UUID is set to the volume id at
	// create (mkfs.ext4 -U), so identity is checkable and a mismatch is refused.
	ErrVolumeIdentityMismatch = errors.New("fleet volume backing file belongs to a different volume")
	// ErrVolumeBackingFileUndersized is returned when a volume's backing file is
	// smaller than the size its ledger row records. Adoption ignored SizeMB
	// entirely, so a row whose size was raised silently kept attaching the old,
	// smaller filesystem and the guest simply ran out of space later, far from the
	// cause. Refusing is the honest answer: the file cannot satisfy the row, and
	// growing it is a separate, deliberate operation (resize2fs), never a side
	// effect of an attach.
	ErrVolumeBackingFileUndersized = errors.New("fleet volume backing file is smaller than its recorded size")
)
