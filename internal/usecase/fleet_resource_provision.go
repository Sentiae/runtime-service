package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// resourceClassPostgres / resourceTierDedicated are the only class/tier the
// dedicated provision path serves this slice.
const (
	resourceClassPostgres = "postgres"
	resourceTierDedicated = "dedicated"
)

// ─────────────────────────────────────────────────────────────────────
// Ports the resource provisioner composes (never re-implements): the image-boot
// FleetProvision use case and the D-080 volume snapshotter.
// ─────────────────────────────────────────────────────────────────────

// FleetProvisioner is the subset of *FleetProvision the resource control plane
// composes. Injecting it (rather than the concrete type) keeps the resource
// provisioner off the boot/volume/secret internals — it inherits volume ensure,
// host affinity, P14 boot-secret vsock resolution, the D-124 pull token, and the
// P21 system_id gate for free.
type FleetProvisioner interface {
	Provision(ctx context.Context, in FleetProvisionInput) (FleetProvisionOutput, error)
	Health(ctx context.Context, handle string) (FleetHealthOutput, error)
	Decommission(ctx context.Context, handle string) error
}

// VolumeSnapshotter is the subset of *FleetVolumeSnapshotter the resource
// control plane composes for a snapshot-first decommission.
type VolumeSnapshotter interface {
	SnapshotAppVolumes(ctx context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error)
}

// ResourceVolumeBinder stamps claim ownership onto a backing app's volumes
// (D-203) and reports whether that stamp actually holds. *FleetVolumeManager
// satisfies it.
type ResourceVolumeBinder interface {
	BindToResource(ctx context.Context, appID, resourceID uuid.UUID) error
	// HasUnstampedVolumes answers the ownership question StatusOf must not guess
	// at: a volume with no claim owner is bytes the DDL guard does not protect,
	// so the error is propagated rather than folded into "stamped".
	HasUnstampedVolumes(ctx context.Context, appID uuid.UUID) (bool, error)
}

// HAPlacementHosts is the narrow slice of FleetHostRegistry the availability gate
// needs: the live-host candidate set (active + healthy + fresh heartbeat). Same
// shape as the scheduler's liveHostLister, deliberately — the gate must refuse on
// exactly the host set a placement would later have to choose from, or it would
// admit a claim the scheduler cannot satisfy.
type HAPlacementHosts interface {
	ListLive(ctx context.Context, staleness time.Duration) ([]domain.Host, error)
}

var (
	_ FleetProvisioner     = (*FleetProvision)(nil)
	_ VolumeSnapshotter    = (*FleetVolumeSnapshotter)(nil)
	_ ResourceVolumeBinder = (*FleetVolumeManager)(nil)
)

// DedicatedEngineConfig is the resolved engine-image + status config for the
// dedicated tier (populated from APP_RESOURCE_ENGINE_PG_IMAGE_* / conn budget).
type DedicatedEngineConfig struct {
	Registry   string
	Repository string
	Digest     string
	ConnBudget int
}

// ─────────────────────────────────────────────────────────────────────
// FleetResourceProvisioner use case (R2, CP4.5 §9 #3, D-183).
// ─────────────────────────────────────────────────────────────────────

// endpointMintAttempts bounds the re-mint retry on an endpoint-id collision. The
// space is ~4×10^8, so a single collision is already unlikely and four in a row
// is not bad luck — it is a broken entropy source or a broken store, and looping
// on either is worse than refusing.
const endpointMintAttempts = 4

// FleetResourceProvisioner provisions a dedicated resident Postgres data-VM as a
// durable P19 resource. It composes FleetProvision (boot + volume + secret) and
// records a fleet_resources claim keyed on (owner_org, claim_key, env).
type FleetResourceProvisioner struct {
	provisioner FleetProvisioner
	resources   repository.FleetResourceRepository
	replicas    repository.ReplicaRepository
	snapshotter VolumeSnapshotter
	// binder stamps the claim's ownership onto the backing app's volumes the
	// moment the claim row exists (D-203). Nil ⇒ a dedicated provision is
	// refused: returning success over an unstamped volume would leave the DDL
	// guard pointing at nothing.
	binder ResourceVolumeBinder
	engine DedicatedEngineConfig
	// naming is the configured zone + region a resource's PERMANENT
	// customer-facing name is minted into (D-190). Unset ⇒ every dedicated
	// provision is refused: a resource born with no servable name, or with one
	// minted under a guessed zone, can never be corrected once its connection
	// string is in a customer's config.
	naming domain.EndpointNaming

	// hosts + hostStaleness back the standard-ha placement gate (slice 1). nil
	// hosts ⇒ every `ha` claim is refused with ErrHAPlacementUnknowable: an
	// invariant that cannot be evaluated is an invariant that is not held, and a
	// resource provisioned under that uncertainty would be sold a promise nothing
	// checked.
	hosts         HAPlacementHosts
	hostStaleness time.Duration

	// facts + affinity + protection back the D-202 attach gate. facts is the
	// worker-liveness ledger (never configuration); affinity resolves which host a
	// resource's claim-owned volumes live on; protection carries the tunable
	// numbers. A nil facts reader makes every durable provision REFUSE — an
	// evaluation that cannot be performed is a protection that is not held, the
	// same stance the HA placement gate takes on an unreadable host inventory.
	facts      ProtectionFactsReader
	affinity   ProtectionAffinityReader
	protection ProtectionConfig

	// now is injected so the heartbeat-staleness windows are testable without
	// sleeping (§30.6). Same seam shape as the mirror worker and the durability
	// collector.
	now func() time.Time

	// pgReady decides whether the provisioned engine ADMITS clients, over and
	// above the app health probe's process-alive + TCP-dial (see engineAdmits). A
	// field rather than a direct call so tests drive both verdicts without a live
	// engine; production is always probePostgresReady. The dedicated tier is
	// postgres only (see ProvisionDedicated's class gate), so the engine-specific
	// probe is exact here in a way it would not be on the general replica health
	// path. Same seam shape as FleetVolumeRestorer.pgReady on purpose — the two
	// readiness gates must never drift.
	pgReady func(ctx context.Context, host string, port int) error
}

// NewFleetResourceProvisioner constructs the use case.
func NewFleetResourceProvisioner(
	provisioner FleetProvisioner,
	resources repository.FleetResourceRepository,
	replicas repository.ReplicaRepository,
	snapshotter VolumeSnapshotter,
	binder ResourceVolumeBinder,
	engine DedicatedEngineConfig,
	naming domain.EndpointNaming,
	hosts HAPlacementHosts,
	hostStaleness time.Duration,
	facts ProtectionFactsReader,
	affinity ProtectionAffinityReader,
	protection ProtectionConfig,
) *FleetResourceProvisioner {
	return &FleetResourceProvisioner{
		provisioner:   provisioner,
		resources:     resources,
		replicas:      replicas,
		snapshotter:   snapshotter,
		binder:        binder,
		engine:        engine,
		naming:        naming,
		hosts:         hosts,
		hostStaleness: hostStaleness,
		facts:         facts,
		affinity:      affinity,
		protection:    protection,
		now:           func() time.Time { return time.Now().UTC() },
		pgReady:       probePostgresReady,
	}
}

// ProvisionDedicatedInput is the wire-agnostic dedicated-resource claim.
type ProvisionDedicatedInput struct {
	OwnerOrg   string
	ClaimKey   string
	Env        string
	Revision   int
	Class      string
	Tier       string
	SystemID   string
	SecretRefs []string
	VaultToken string
	SizeMB     int64
	// AvailabilityClass is the requested third axis: "single" (one member) or "ha"
	// (`standard-ha` — a primary plus a synchronous standby in a different failure
	// domain and the same region).
	//
	// Empty means "single". That is a default in the SAFE direction — the weaker
	// promise — and it exists because the wire cannot express this field yet: the
	// P19 proto carries no availability class, so every call arriving today is a
	// single-member claim and must keep behaving exactly as it did. An `ha` claim
	// can only come from a caller inside this process until that contract is
	// extended, which is a proto change and therefore not this slice's to make.
	AvailabilityClass string
	// Durability is the retention promise claimed for this resource (D-202).
	// ""/"durable" ⇒ durable; "ephemeral" is REFUSED on the dedicated tier, which
	// is durable by construction.
	Durability string
	// Waiver, when non-nil, is the D-202 per-resource AUDITED override of the
	// attach requirement: the resource is provisioned even though a protection
	// component could not attach, and it reports that fact permanently.
	//
	// Typed and wire-agnostic on purpose. The actor is derived server-side from
	// the authenticated principal, never taken from a caller-supplied label — this
	// struct is the seam that keeps that derivation out of the use case, and it is
	// why there is no configuration path to a waiver anywhere. Placement is never
	// waivable.
	Waiver *ProtectionWaiver
}

// ProvisionDedicatedOutput is the claim result: the resource handle + phase.
type ProvisionDedicatedOutput struct {
	Handle string
	Phase  string
}

// RecoveryPointInfo is a resource's newest recovery-point summary.
type RecoveryPointInfo struct {
	ID        string
	ObjectKey string
	Kind      string
	SizeBytes int64
	// RestoredInPlaceOK mirrors the domain field: this point was restored in
	// place and the engine came back admitting clients. NOT a verification drill.
	RestoredInPlaceOK bool
	CreatedAt         time.Time
}

// ResourceStatus is the live status of a resource claim. Conditions carries the
// stable reason tokens below when the resource is not simply healthy.
type ResourceStatus struct {
	Handle            string
	Phase             string
	Tier              string
	Endpoint          string
	SecretRefs        []string
	ConnBudget        int
	Conditions        []string
	LastRecoveryPoint *RecoveryPointInfo
}

// Condition tokens reported on ResourceStatus.Conditions. They are STABLE,
// machine-readable strings — never the raw error text, which is operator-facing
// detail that belongs in the log, not on a tenant-visible API.
const (
	// conditionBackingAppMissing — the resource row points at an app the fleet no
	// longer knows. Its recovery points survive, so this is a restore/rebuild
	// decision, not a dead end; it just cannot be read as health.
	conditionBackingAppMissing = "backing-app-missing"
	// conditionHealthUnavailable — the health of the backing app could not be
	// read at all (a store or transport fault). It says nothing about the data.
	conditionHealthUnavailable = "health-unavailable"
	// conditionEngineNotAdmitting — the backing VM is alive and listening, but the
	// engine refuses (or cannot be proven to accept) a client connection. This is
	// the false-green case: process-alive + TCP-dial pass while every customer
	// connection is rejected (#p19-restore-false-green-health). The refusal detail
	// — SQLSTATE and message — is operator-facing and goes to the log, never here.
	conditionEngineNotAdmitting = "engine-not-admitting"
	// conditionSnapshotFailing — this resource's snapshots are FAILING: the most
	// recent attempt produced no recovery point, and the failure streak has not been
	// broken since. The engine may be perfectly healthy; what has stopped is its
	// PROTECTION, and a resource that cannot be recovered must not read as if
	// nothing were wrong. The count, the last failure time and the operator-facing
	// cause live on the row (fleet_resources.consecutive_snapshot_failures /
	// last_snapshot_error) — this token only says that the condition holds.
	conditionSnapshotFailing = "snapshot-failing"
	// conditionVolumeOwnershipUnstamped — the resource's backing app still holds a
	// volume with no claim owner (D-203). The engine may serve perfectly, but the
	// DDL guard that makes a delete of those bytes RESTRICT points at nothing, so
	// the resource is not the protected thing its `ready` phase claims. Set on the
	// unknown too (a failed check): unprovable ownership is reported as absent
	// ownership, never as present.
	conditionVolumeOwnershipUnstamped = "volume-ownership-unstamped"
)

// healthCondition classifies a failed health probe into a condition token. The
// distinction is load-bearing for an operator: a MISSING app needs a rebuild or
// a restore, while an unreadable one needs nothing but a retry.
func healthCondition(err error) string {
	if errors.Is(err, domain.ErrWorkloadNotFound) || errors.Is(err, domain.ErrFleetAppNotFound) {
		return conditionBackingAppMissing
	}
	return conditionHealthUnavailable
}

// ProvisionDedicated declaratively ensures a dedicated Postgres data-VM for a
// claim. It is idempotent per (owner_org, claim_key, env): the same revision
// returns the current status; a different revision is REJECTED (converge/resize
// is a later slice); a concurrent insert-race returns the winner.
func (uc *FleetResourceProvisioner) ProvisionDedicated(ctx context.Context, in ProvisionDedicatedInput) (ProvisionDedicatedOutput, error) {
	out, err := uc.provisionDedicated(ctx, in)
	// §22 counter at the ONE seam every return path passes through — this use case
	// has a dozen early returns, and per-branch instrumentation is how a later
	// branch silently stops being counted.
	recordExecution("provision_dedicated_resource", outcomeFor(err))
	return out, err
}

func (uc *FleetResourceProvisioner) provisionDedicated(ctx context.Context, in ProvisionDedicatedInput) (ProvisionDedicatedOutput, error) {
	if in.Class != resourceClassPostgres {
		return ProvisionDedicatedOutput{}, domain.ErrResourceClassUnsupported
	}
	if in.Tier != resourceTierDedicated {
		return ProvisionDedicatedOutput{}, domain.ErrResourceTierUnsupported
	}
	if uc.binder == nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("no volume binder wired to stamp claim ownership (D-203): refusing dedicated provision")
	}
	// D-202 — the retention promise is READ from the claim and stored, never
	// inferred from tier afterwards, and the two half-waiver shapes are refused
	// here (one owner for the rule) before anything else is evaluated.
	durability, err := resolveDedicatedDurability(in.Durability)
	if err != nil {
		return ProvisionDedicatedOutput{}, err
	}
	waiver, err := normalizeWaiver(in.Waiver)
	if err != nil {
		return ProvisionDedicatedOutput{}, err
	}
	if in.OwnerOrg == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceOwnerOrgRequired
	}
	ownerUUID, err := uuid.Parse(in.OwnerOrg)
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("parse owner org: %w", err)
	}
	if in.ClaimKey == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceClaimKeyRequired
	}
	// The data engine cannot boot credential-less — fail closed (I32).
	if len(in.SecretRefs) == 0 {
		return ProvisionDedicatedOutput{}, domain.ErrResourceSecretsRequired
	}
	// The engine credentials resolve under the handed per-deployment Vault token.
	if in.VaultToken == "" {
		return ProvisionDedicatedOutput{}, domain.ErrResourceVaultTokenRequired
	}
	if uc.engine.Registry == "" || uc.engine.Repository == "" || uc.engine.Digest == "" {
		return ProvisionDedicatedOutput{}, domain.ErrImageRefIncomplete
	}
	// Checked HERE, before anything is created, and not at mint time: a host with
	// no configured zone/region cannot give this database a name a customer could
	// ever connect to, and the cheapest moment to say so is before a VM boots and
	// a volume is materialized. Fail-closed — there is no default, because a
	// plausible-looking one produces a PERMANENT name nothing will serve (D-190).
	if err := uc.naming.Validate(); err != nil {
		return ProvisionDedicatedOutput{}, err
	}
	// The availability gate (standard-ha slice 1). BEFORE anything is created and
	// before the idempotency lookup: an `ha` claim the fleet cannot place must be
	// refused, not recorded — a claim row naming a tier nothing can build is a
	// promise on the books.
	availability, err := uc.resolveAvailability(ctx, in.AvailabilityClass)
	if err != nil {
		return ProvisionDedicatedOutput{}, err
	}
	revision := in.Revision
	if revision <= 0 {
		revision = 1
	}

	// Idempotency: an existing claim with the same revision is a declarative
	// ensure; a different revision is a converge the current backend rejects.
	existing, err := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env)
	if err == nil {
		if existing.Revision == revision {
			// #p19-handed-token-not-rehandable — this is NOT a bare no-op, and must
			// never be "simplified" back into one. The per-deployment Vault token that
			// resolves this engine's boot secrets is MEMORY-ONLY BY DESIGN (D-125:
			// never a row, rootfs, runtime.json, or log), so a runtime-service restart
			// empties the store and the resource's next (re)boot fails closed with "no
			// handed token" — permanently, because nothing else ever re-hands it. A
			// declarative re-provision is the recovery a caller naturally attempts and
			// the only authenticated moment a fresh token arrives, so it is THE
			// designed post-restart recovery path. A database you lose on reboot is
			// not durable.
			//
			// D-202 attach-on-recover: a claim accepted BEFORE the gate existed
			// carries no enrolment, and this declarative re-provision is the moment
			// the fleet can bring it under protection without anyone asking for a
			// migration. Best-effort and never a refusal — see attachOnRecover.
			uc.attachOnRecover(ctx, existing)
			uc.recoverExisting(ctx, existing, in)
			return ProvisionDedicatedOutput{Handle: existing.ID.String(), Phase: string(existing.Phase)}, nil
		}
		return ProvisionDedicatedOutput{}, domain.ErrResourceConvergeNotSupported
	}
	if !errors.Is(err, domain.ErrResourceNotFound) {
		return ProvisionDedicatedOutput{}, fmt.Errorf("lookup resource claim: %w", err)
	}

	// ── The D-202 attach gate ────────────────────────────────────────────────
	//
	// Creating a durable database is ONE operation: create the engine AND attach
	// its protection. They succeed together or the provision fails. Evaluated HERE
	// — before the engine boots and before a volume materializes — so a refusal
	// leaves NO claim row and NO VM behind; and the result is stamped into the same
	// INSERT that creates the claim, which is what makes an unprotected durable
	// acceptance unrepresentable rather than merely checked.
	//
	// ⚠ AFTER the idempotency lookup, and that ordering is binding. A repeat call
	// for an EXISTING claim returns from inside the lookup branch above, so a fleet
	// whose protection has REGRESSED must still be able to recover the databases it
	// already accepted (the token-re-hand path,
	// #p19-handed-token-not-rehandable). A regressed platform refuses NEW
	// acceptances; it never refuses to serve what it already promised.
	cadenceSeconds, gateErr := uc.attachProtection(ctx, durability, waiver, in.ClaimKey)
	if gateErr != nil {
		return ProvisionDedicatedOutput{}, gateErr
	}

	// Compose FleetProvision: a resident, single-replica, volume-bearing app.
	provOut, err := uc.provisioner.Provision(ctx, uc.dedicatedDescriptor(in))
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("provision dedicated engine: %w", err)
	}
	appHandle, err := uuid.Parse(provOut.Handle)
	if err != nil {
		return ProvisionDedicatedOutput{}, fmt.Errorf("parse app handle: %w", err)
	}

	now := time.Now().UTC()
	res := &domain.FleetResource{
		ID:       uuid.New(),
		OwnerOrg: ownerUUID,
		ClaimKey: in.ClaimKey,
		Env:      in.Env,
		Revision: revision,
		// The data's first incarnation. Stamped explicitly because GORM writes
		// every field it saves and a zero here would be a generation-0 object
		// prefix (refused by the 0021 CHECK).
		Generation: domain.FleetResourceInitialGeneration,
		Class:      resourceClassPostgres,
		Tier:       resourceTierDedicated,
		// Stamped explicitly for the same reason as Generation: GORM writes every
		// field it saves, and '' is refused by the 0022 CHECKs rather than silently
		// stored. The class records what was CLAIMED — an `ha` row does not assert
		// that a standby exists, because membership and replication are later
		// slices; it asserts that the placement invariant was satisfiable when the
		// claim was accepted.
		AvailabilityClass: availability,
		SyncDegradePolicy: domain.SyncDegradePolicyFailClosed,
		Phase:             domain.FleetResourcePhaseProvisioning,
		AppID:             &appHandle,
		SecretRefs:        in.SecretRefs,
		SystemID:          in.SystemID,
		// D-202 — the protection attachment is written in the SAME INSERT that
		// creates the claim. Stamped explicitly for the same reason as Generation
		// and AvailabilityClass: GORM writes every field it saves, and '' is refused
		// by the 0025 CHECKs rather than silently stored.
		Durability:               durability,
		ProtectionCadenceSeconds: cadenceSeconds,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if durability == domain.DurabilityDurable {
		if waiver != nil {
			// A waived acceptance carries the audit and NOT an attached-at stamp:
			// attached-at means the full component set attached, and here it did not.
			// The pair is what makes the row self-describing forever.
			res.ProtectionWaivedBy = waiver.Actor
			res.ProtectionWaiverReason = waiver.Reason
			res.ProtectionWaivedAt = &now
		} else {
			res.ProtectionAttachedAt = &now
		}
	}

	// The endpoint identity is minted HERE — at birth, in the same INSERT that
	// creates the row — and nowhere else. Not in a later "assign endpoints" pass:
	// a resource that exists without its permanent name is a resource something
	// can hand out before it has one. The claim above returns early for an
	// EXISTING resource, so a re-provision can never mint a second name for it.
	var saveErr error
	for attempt := 1; attempt <= endpointMintAttempts; attempt++ {
		endpoint, mintErr := uc.naming.Mint()
		if mintErr != nil {
			return ProvisionDedicatedOutput{}, fmt.Errorf("mint resource endpoint: %w", mintErr)
		}
		endpointID := endpoint.ID()
		res.EndpointID = &endpointID
		res.Region = endpoint.Region()

		saveErr = uc.resources.SaveResource(ctx, res)
		if saveErr == nil {
			// D-203: ownership is stamped the moment the claim exists. A bind failure
			// fails the provision — the claim row survives, and the caller's retry
			// (same revision → recoverExisting) re-binds; returning success over an
			// unstamped volume would leave the DDL guard pointing at nothing.
			if berr := uc.binder.BindToResource(ctx, appHandle, res.ID); berr != nil {
				return ProvisionDedicatedOutput{}, fmt.Errorf("bind volumes to resource claim: %w", berr)
			}
			return ProvisionDedicatedOutput{Handle: res.ID.String(), Phase: string(res.Phase)}, nil
		}
		// The unique index — never this process — decides endpoint uniqueness. A
		// collision means re-mint, not fail: nothing else about the claim is wrong.
		if !errors.Is(saveErr, domain.ErrEndpointTaken) {
			break
		}
		logger.FromContext(ctx).Warn("fleet resource: minted endpoint id was already taken, re-minting",
			"resource_id", res.ID, "endpoint_id", endpointID, "attempt", attempt)
	}
	if errors.Is(saveErr, domain.ErrEndpointTaken) {
		// Four collisions in a ~4×10^8 space is not luck. Refuse rather than loop:
		// the entropy source or the store is wrong, and a database is not worth
		// minting a name for under either.
		return ProvisionDedicatedOutput{}, fmt.Errorf("%w after %d attempts", domain.ErrEndpointMintExhausted, endpointMintAttempts)
	}
	// Lost the insert race on (owner_org, claim_key, env): another concurrent
	// claim won. The FleetApp upsert is idempotent per (component_id, env), so
	// both racers converged on one app — return the winning resource row (and with
	// it the winner's endpoint identity; this call's minted id was never stored).
	if winner, ferr := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env); ferr == nil {
		if winner.AppID != nil {
			if berr := uc.binder.BindToResource(ctx, *winner.AppID, winner.ID); berr != nil {
				return ProvisionDedicatedOutput{}, fmt.Errorf("bind volumes to winning resource claim: %w", berr)
			}
		}
		return ProvisionDedicatedOutput{Handle: winner.ID.String(), Phase: string(winner.Phase)}, nil
	}
	return ProvisionDedicatedOutput{}, fmt.Errorf("persist resource: %w", saveErr)
}

// attachProtection is the D-202 accept gate. It returns the cadence enrolment to
// stamp on the claim (nil when cadence could not attach) and, when the provision
// must be REFUSED, the error naming every component that could not attach.
//
// Three outcomes, and only three:
//
//   - everything attaches ⇒ the resource is enrolled at the configured cadence;
//   - a component cannot attach and NO waiver was supplied ⇒ refused, naming
//     every failed component. Nothing is created;
//   - a component cannot attach and a waiver WAS supplied ⇒ the provision
//     proceeds, attaching whatever CAN attach (a resource that could still be
//     enrolled in a cadence IS enrolled — a waiver forgives the requirement, it
//     does not forgo the protection), and the caller records the audit.
//
// An ephemeral resource has no protection to attach: it is not a promise the
// platform made.
func (uc *FleetResourceProvisioner) attachProtection(ctx context.Context, durability domain.Durability, waiver *ProtectionWaiver, claimKey string) (*int, error) {
	if durability != domain.DurabilityDurable {
		return nil, nil
	}
	eval := uc.evaluateProtection(ctx, uc.acceptScopes(ctx), uc.protection.CadenceSeconds)
	var cadenceSeconds *int
	if eval.Cadence.Attached {
		cs := uc.protection.CadenceSeconds
		cadenceSeconds = &cs
	}
	if err := eval.Err(); err != nil {
		if waiver == nil {
			return nil, err
		}
		logger.FromContext(ctx).Warn("fleet resource: durable provision proceeding under a protection WAIVER — this database is being accepted with protection the fleet cannot attach",
			"claim_key", claimKey, "waived_by", waiver.Actor, "reason", waiver.Reason,
			"cadence_attached", eval.Cadence.Attached, "offsite_attached", eval.Offsite.Attached,
			"unattachable", err)
	}
	return cadenceSeconds, nil
}

// attachOnRecover brings an already-accepted durable claim under protection when
// it is not yet enrolled — the pre-D-202 rows, which exist because the gate did
// not exist when they were created.
//
// It runs on the declarative re-provision because that is the only recurring,
// authenticated moment this control plane touches an existing claim, and a row
// that could be protected should not have to wait for someone to notice it. It is
// deliberately NOT a migration: whether protection can attach is a live fact, and
// a backfill would stamp an enrolment nothing was proven to serve.
//
// ⚠ It NEVER refuses and never blocks. Every failure — the evaluation, the save —
// is logged and swallowed, and recovery proceeds exactly as it did before. This
// path exists to serve a database that already exists; a regressed platform must
// still recover what it already promised (#p19-handed-token-not-rehandable), and
// refusing here would take a customer's live database down to enforce a rule that
// was not in force when it was created.
//
// The scope is the resource's OWN AFFINITY HOST (statusScopes), not the
// all-candidate accept set. At recover the placement is already made and the
// volumes are pinned: the one host holding the bytes is the one host whose worker
// can protect them, and requiring every candidate host's beat would refuse an
// attach that is genuinely available.
func (uc *FleetResourceProvisioner) attachOnRecover(ctx context.Context, res *domain.FleetResource) {
	if res.Durability != domain.DurabilityDurable || res.ProtectionAttachedAt != nil || res.ProtectionWaivedBy != "" {
		return
	}
	eval := uc.evaluateProtection(ctx, uc.statusScopes(ctx, res), uc.protection.CadenceSeconds)
	if err := eval.Err(); err != nil {
		logger.FromContext(ctx).Warn("fleet resource: existing durable claim remains UNPROTECTED — its protection still cannot attach",
			"resource_id", res.ID, "err", err)
		return
	}
	now := uc.now()
	cadenceSeconds := uc.protection.CadenceSeconds
	res.ProtectionCadenceSeconds = &cadenceSeconds
	res.ProtectionAttachedAt = &now
	res.UpdatedAt = now
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		logger.FromContext(ctx).Warn("fleet resource: protection attached on recover but the enrolment could not be persisted",
			"resource_id", res.ID, "err", err)
	}
}

// resolveAvailability validates the requested availability class and, for `ha`,
// REFUSES unless the fleet can currently satisfy the placement invariant: two live
// hosts in DIFFERENT failure domains and the SAME region (design §5.1, D-196
// amendment 2).
//
// ⚠ With one physical machine this always refuses, and that refusal is the
// deliverable — `standard-ha` is refused, never simulated (D-196). A claim
// accepted here still gets ONE member: membership, replication and promotion are
// slices 2-3. What acceptance means is exactly "the invariant is satisfiable",
// never "the promise is held", and nothing downstream may read it as the latter.
//
// The `single` path is untouched in every branch — no host lookup, no new failure
// mode. A gate that could break the tier every existing resource uses in order to
// guard a tier nothing can provision would be a bad trade.
func (uc *FleetResourceProvisioner) resolveAvailability(ctx context.Context, requested string) (domain.AvailabilityClass, error) {
	if requested == "" {
		return domain.AvailabilityClassSingle, nil
	}
	class := domain.AvailabilityClass(requested)
	if !class.IsValid() {
		// Refused, never coerced to `single`: silently downgrading would hand back a
		// resource weaker than the one asked for, with nothing anywhere saying so.
		return "", domain.ErrHAAvailabilityClassInvalid
	}
	if class != domain.AvailabilityClassHA {
		return class, nil
	}
	if uc.hosts == nil {
		return "", domain.ErrHAPlacementUnknowable
	}
	live, err := uc.hosts.ListLive(ctx, uc.hostStaleness)
	if err != nil {
		// An unreadable inventory is not an empty one, but neither can prove the
		// invariant — and the two must not be conflated in the log, because one is a
		// store fault and the other is the honest state of the fleet.
		logger.FromContext(ctx).Error("fleet resource: live host inventory unreadable, refusing an ha claim", "err", err)
		return "", domain.ErrHAPlacementUnknowable
	}
	if err := domain.RequireHAPlacement(live); err != nil {
		return "", err
	}
	return class, nil
}

// dedicatedDescriptor builds the FleetProvision descriptor for a dedicated
// claim: a resident, single-replica, volume-bearing Postgres engine. The first
// provision and the post-restart recovery re-provision BOTH build it here, so
// the two can never drift — a "recovery" that handed a different descriptor
// would silently converge the app to something the claim never asked for. That
// single seam is also why the org namespace below cannot drift: the first
// provision and the recovery re-provision derive the SAME component id, so a
// recovery can never fall back to the old org-blind key and re-collide.
func (uc *FleetResourceProvisioner) dedicatedDescriptor(in ProvisionDedicatedInput) FleetProvisionInput {
	return FleetProvisionInput{
		// The owning org is INSIDE the component id, not just beside it in a column:
		// the ingress host is derived from the component id
		// (sanitizeSlug(component_id)-sanitizeSlug(env), unique-indexed by
		// migrations/0006), so an org-blind 'resource/<claim_key>' collides across
		// organisations on the route even once the app row itself is org-scoped.
		ComponentID:   "resource/" + in.OwnerOrg + "/" + in.ClaimKey,
		Env:           in.Env,
		OwnerOrg:      in.OwnerOrg,
		Registry:      uc.engine.Registry,
		Repository:    uc.engine.Repository,
		Digest:        uc.engine.Digest,
		Port:          residentPGPort,
		WorkloadClass: string(domain.ImageWorkloadClassResident),
		// Declares this app as a data resource, which is what keeps it off the HTTP
		// edge (no ingress route — Postgres is reached at L4, never through Caddy).
		ResourceClass: resourceClassPostgres,
		SecretRefs:    in.SecretRefs,
		VaultToken:    in.VaultToken,
		SystemID:      in.SystemID,
		EnvVars:       map[string]string{"PGDATA": "/data/pgdata"},
		Volumes:       []VolumeSpecInput{{SizeMB: in.SizeMB, MountPath: "/data"}},
		MinReplicas:   1,
		MaxReplicas:   1,
		ScaleToZero:   false,
	}
}

// recoverExisting re-hands the caller's per-deployment Vault token to an already
// claimed resource's backing app and lets the reconciler boot whatever is not
// running (#p19-handed-token-not-rehandable).
//
// It re-runs the SAME composed FleetProvisioner.Provision the first provision
// used rather than inventing a recovery path: that call is an upsert keyed on
// (component_id, env), so for an app that still exists it re-hands the token
// into the in-memory store, re-ensures the SAME volume rows and ingress route,
// and then reconciles once — which is exactly the machinery that already knows
// how to leave a healthy replica alone and replace a dead or missing one, now
// with the token in hand.
//
// Best-effort by contract: the Handle/Phase this path returns is frozen and
// callers poll it, so every failure here is logged and swallowed — the resource
// is never left worse off than before, and the periodic reconcile retries.
func (uc *FleetResourceProvisioner) recoverExisting(ctx context.Context, res *domain.FleetResource, in ProvisionDedicatedInput) {
	if res.Phase == domain.FleetResourcePhaseDecommissioned {
		return // a tombstone was torn down on purpose; never re-boot it
	}
	if res.AppID == nil {
		logger.FromContext(ctx).Warn("fleet resource: re-provision cannot recover a claim with no backing app",
			"resource_id", res.ID)
		return
	}
	// The re-provision below is an upsert on (component_id, env). If the app row
	// is GONE it would mint a NEW app id — and with it a NEW, EMPTY data volume —
	// while this resource row still points at the vanished app: a live but empty
	// engine masquerading as the customer's database. Refuse loudly instead; an
	// app-row rebuild / restore-from-recovery-point is a different operation.
	if _, herr := uc.provisioner.Health(ctx, res.AppID.String()); herr != nil {
		logger.FromContext(ctx).Error("fleet resource: backing app is gone — re-provision cannot recover it, needs an explicit rebuild or restore",
			"resource_id", res.ID, "app_id", res.AppID, "err", herr)
		return
	}
	if _, perr := uc.provisioner.Provision(ctx, uc.dedicatedDescriptor(in)); perr != nil {
		logger.FromContext(ctx).Error("fleet resource: recover dedicated engine failed (token re-hand / re-drive boot)",
			"resource_id", res.ID, "app_id", res.AppID, "err", perr)
		return
	}
	// D-203: re-stamp claim ownership on the way through. Best-effort, matching
	// this method's logged-and-swallowed contract — and it also heals a pre-0024
	// resource whose volumes the backfill could not see because the claim was
	// created after them.
	if uc.binder != nil {
		berr := uc.binder.BindToResource(ctx, *res.AppID, res.ID)
		// The failure is swallowed by contract, so the counter is the only place it
		// is countable: without it a permanently failing re-bind is invisible
		// outside a log line nobody alerts on.
		recordExecution("recover_resource_volume_rebind", outcomeFor(berr))
		if berr != nil {
			logger.FromContext(ctx).Error("fleet resource: re-bind volumes to claim failed",
				"resource_id", res.ID, "app_id", res.AppID, "err", berr)
		}
	}
}

// StatusOf returns the live status of a dedicated resource: the backing app's
// health (healthy → ready), its private endpoint, its connection budget, and its
// newest recovery point.
//
// A health probe that FAILS is reported as a condition, never as an error: this
// call is how an operator (and the portal) sees a resource at all, so a resource
// that is stuck must come back legible rather than as a broken RPC.
func (uc *FleetResourceProvisioner) StatusOf(ctx context.Context, resourceID uuid.UUID) (ResourceStatus, error) {
	res, err := uc.resources.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		return ResourceStatus{}, err
	}
	status := ResourceStatus{
		Handle:     res.ID.String(),
		Phase:      string(res.Phase),
		Tier:       res.Tier,
		Endpoint:   res.Endpoint,
		SecretRefs: res.SecretRefs,
		ConnBudget: uc.engine.ConnBudget,
	}

	if res.Tier == resourceTierDedicated && res.AppID != nil &&
		res.Phase != domain.FleetResourcePhaseDecommissioned {
		// D-203 ownership honesty. A claim whose volumes are not stamped is not
		// the durable thing a `ready` phase promises: nothing in the database
		// refuses a delete of those bytes. The persisted phase is deliberately
		// left alone (same reasoning as the engine-not-admitting branch below —
		// this call reports an observation, it does not rewrite the row), but the
		// reported phase is degraded and the ready transition never runs.
		unstamped := uc.volumeOwnershipUnstamped(ctx, res)
		if unstamped {
			status.Conditions = append(status.Conditions, conditionVolumeOwnershipUnstamped)
			status.Phase = string(domain.FleetResourcePhaseDegraded)
		}
		h, herr := uc.provisioner.Health(ctx, res.AppID.String())
		if herr != nil {
			// A resource whose health cannot be read is a STATE, not an API error.
			// Hard-erroring here made the one case an operator most needs to see —
			// a resource stuck because its backing app is gone — read as a broken
			// endpoint: no phase, no endpoint, no recovery catalog, nothing to act
			// on. The durable phase stands, the condition says why, and the recovery
			// points below still list (they are what a recovery is built from).
			status.Conditions = append(status.Conditions, healthCondition(herr))
			logger.FromContext(ctx).Warn("fleet resource: health probe failed, reporting it as a condition",
				"resource_id", res.ID, "app_id", res.AppID, "err", herr)
		}
		// One replica listing answers both questions below — whether the engine
		// admits a client, and what the private endpoint is — so it is read once
		// here rather than by each of them.
		reps, reperr := uc.replicas.ListByApp(ctx, *res.AppID)
		if reperr != nil {
			logger.FromContext(ctx).Warn("fleet resource: list replicas", "resource_id", res.ID, "app_id", res.AppID, "err", reperr)
		}
		// D-184 — a restore owns the phase while it runs. Auto-advancing to ready
		// here would race the restore's own verification window: the OLD engine can
		// still be healthy the instant before the restore drains it, and the
		// restored one is not proven until the restore says so.
		if !unstamped && herr == nil && h.Healthy && res.Phase != domain.FleetResourcePhaseRestoring {
			// Probed only here, never above: admission is a live network round trip,
			// and its verdict is unused unless the app is already healthy and not
			// mid-restore. Reading it eagerly would put a dial on the path of every
			// status read of an app that is already known to be down.
			aerr := uc.engineAdmits(ctx, *res.AppID, reps, reperr)
			if aerr == nil {
				status.Phase = string(domain.FleetResourcePhaseReady)
				// Keep the durable row honest: advance provisioning → ready once observed.
				if res.Phase != domain.FleetResourcePhaseReady {
					if uerr := uc.resources.UpdateResourcePhase(ctx, res.ID, domain.FleetResourcePhaseReady); uerr != nil {
						logger.FromContext(ctx).Warn("fleet resource: persist ready phase", "resource_id", res.ID, "err", uerr)
					}
				}
			} else {
				// Alive but refusing clients: the resource EXISTS and is impaired, which
				// is exactly what degraded means. Reported, never persisted — a stored
				// ready is left alone here. What a demotion should mean durably (and
				// what would then reconcile it back) is a separate decision; until it is
				// made, this call only tells the truth about the observation it just
				// took, and never rewrites the row on a probe that could itself be the
				// thing that is wrong.
				status.Phase = string(domain.FleetResourcePhaseDegraded)
				status.Conditions = append(status.Conditions, conditionEngineNotAdmitting)
				logger.FromContext(ctx).Warn("fleet resource: backing app is healthy but the engine does not admit clients",
					"resource_id", res.ID, "app_id", res.AppID, "err", aerr)
			}
		}
		if ep := residentEndpointOf(reps); ep != "" {
			status.Endpoint = ep
		}
	}

	// A resource whose snapshots are failing is impaired even when its engine is
	// serving perfectly, so the condition is reported regardless of the health block
	// above. A tombstone is exempt: a torn-down resource's streak is history.
	//
	// The reported PHASE is deliberately left alone. Callers gate on phase (a
	// provision polls it for `ready`), and a failing snapshot must not block
	// anything that succeeds today — so the visibility lands on the condition, which
	// is what an operator and the portal read.
	if res.ConsecutiveSnapshotFailures > 0 && res.Phase != domain.FleetResourcePhaseDecommissioned {
		status.Conditions = append(status.Conditions, conditionSnapshotFailing)
	}

	// D-202 — protection REGRESSION alarms; it never blocks. The conditions below
	// are the customer-visible half of the same evaluation the accept gate reads,
	// which is what keeps the gate and the report from ever telling different
	// stories. They deliberately touch NEITHER the reported nor the persisted
	// phase: a resource whose protection has stopped is still serving, callers gate
	// on phase, and demoting it would break something that works today in order to
	// report something that is already reported here. A tombstone is exempt — a
	// torn-down resource's protection is history.
	status.Conditions = append(status.Conditions, uc.protectionConditions(ctx, res)...)

	if rps, rerr := uc.resources.ListRecoveryPoints(ctx, res.ID); rerr != nil {
		logger.FromContext(ctx).Warn("fleet resource: list recovery points", "resource_id", res.ID, "err", rerr)
	} else if len(rps) > 0 {
		rp := rps[0] // newest first
		status.LastRecoveryPoint = &RecoveryPointInfo{
			ID:                rp.ID.String(),
			ObjectKey:         rp.ObjectKey,
			Kind:              rp.Kind,
			SizeBytes:         rp.SizeBytes,
			RestoredInPlaceOK: rp.RestoredInPlaceOK,
			CreatedAt:         rp.CreatedAt,
		}
	}
	return status, nil
}

// protectionConditions turns the D-202 evaluation over ONE resource's live facts
// into stable condition tokens.
//
// Host-aware at this end too (J3): the resource's protecting host is resolved
// from its OWN claim-owned volumes' affinity, and only that host's cadence worker
// is evaluated. A resource whose bytes are on another host is not protected by a
// worker here, and an unresolvable affinity reports the pessimistic value rather
// than this process's own identity.
func (uc *FleetResourceProvisioner) protectionConditions(ctx context.Context, res *domain.FleetResource) []string {
	if res.Durability != domain.DurabilityDurable || res.Phase == domain.FleetResourcePhaseDecommissioned {
		return nil
	}
	var conditions []string
	// Permanent and first: a waiver is the whole reason the other conditions may
	// legitimately hold, and it must never be inferred from their absence.
	if res.ProtectionWaivedBy != "" {
		conditions = append(conditions, conditionProtectionWaived)
	}
	// The enrolment this status is about is the one on the ROW, never the fleet's
	// current configured default (J3, one computation / two consumers). A resource
	// enrolled at an hour must keep being judged against an hour: re-reading the
	// config here would let a changed (or cleared) default silently re-classify an
	// enrolment nobody touched, reporting a fact about the config as if it were a
	// fact about this database.
	enrolled := 0
	if res.ProtectionCadenceSeconds != nil {
		enrolled = *res.ProtectionCadenceSeconds
	}
	eval := uc.evaluateProtection(ctx, uc.statusScopes(ctx, res), enrolled)
	if !eval.Offsite.Attached {
		conditions = append(conditions, conditionProtectionOffsiteUnavailable)
	}
	switch {
	case res.ProtectionCadenceSeconds == nil:
		// Never enrolled. Its data is captured only by a manual RPC or by teardown —
		// a different thing from an enrolment whose worker has stopped, which is why
		// this is a different token.
		conditions = append(conditions, conditionProtectionCadenceUnattached)
	case !eval.Cadence.Attached:
		logger.FromContext(ctx).Warn("fleet resource: enrolled in a snapshot cadence, but no cadence worker is provably serving it",
			"resource_id", res.ID, "err", eval.Cadence.Err)
		conditions = append(conditions, conditionProtectionCadenceStalled)
	}
	return conditions
}

// volumeOwnershipUnstamped reports whether the resource's backing app still holds
// a volume with no claim owner (D-203).
//
// Fail-closed: a check that errors — or that is not wired at all — answers TRUE,
// because an ownership stamp nobody could confirm is exactly as protective as one
// that is absent. The status RPC itself never fails on this: like every other
// probe here, the unknown becomes a condition, not a broken endpoint.
func (uc *FleetResourceProvisioner) volumeOwnershipUnstamped(ctx context.Context, res *domain.FleetResource) bool {
	if uc.binder == nil {
		logger.FromContext(ctx).Error("fleet resource: no volume-ownership checker wired — reporting claim ownership as unstamped",
			"resource_id", res.ID, "app_id", res.AppID)
		return true
	}
	unstamped, err := uc.binder.HasUnstampedVolumes(ctx, *res.AppID)
	if err != nil {
		logger.FromContext(ctx).Error("fleet resource: claim-ownership check failed — reporting it as unstamped",
			"resource_id", res.ID, "app_id", res.AppID, "err", err)
		return true
	}
	return unstamped
}

// engineAdmits checks that every resident replica of the app lets a client
// through pg_hba to authentication, using the credential-free probe. It takes
// the already-read replica listing (and the error that produced it) so StatusOf
// reads the store once.
//
// This is READINESS, and it is deliberately NOT folded into the app health probe
// that drives it (FleetReplicaRuntime.RefreshHealth stays process-alive + TCP
// dial). That probe is LIVENESS: the reconciler restarts what it calls unhealthy,
// so teaching it about pg_hba would turn a broken-but-alive engine into a restart
// crashloop that destroys the very state an operator needs to repair. Liveness
// decides whether to restart; readiness decides what the customer is told.
//
// Fail-closed throughout, matching FleetVolumeRestorer.engineAdmits: no probe
// wired, a repository error, a replica listed as resident with no guest address,
// and an app with no resident replica at all are ALL failures, because each of
// them means the engine cannot be PROVEN usable — and an unprovable engine must
// never be reported as ready over customer data.
func (uc *FleetResourceProvisioner) engineAdmits(ctx context.Context, appID uuid.UUID, reps []domain.Replica, listErr error) error {
	if uc.pgReady == nil {
		return fmt.Errorf("no readiness probe is wired, so the engine of app %s cannot be confirmed usable", appID)
	}
	if listErr != nil {
		return fmt.Errorf("list replicas of app %s: %w", appID, listErr)
	}
	probed := 0
	for i := range reps {
		r := &reps[i]
		if r.State != domain.ReplicaStateResident {
			continue
		}
		if r.GuestIP == "" || r.Port <= 0 {
			return fmt.Errorf("replica %s is resident but carries no guest address to probe", r.ID)
		}
		if perr := uc.pgReady(ctx, r.GuestIP, r.Port); perr != nil {
			return fmt.Errorf("replica %s: %w", r.ID, perr)
		}
		probed++
	}
	if probed == 0 {
		return fmt.Errorf("app %s has no resident replica to probe", appID)
	}
	return nil
}

// residentEndpointOf returns the private "guest-ip:5432" of the first resident
// replica (same scheme as fleet_replica_runtime), or "" when none is resident.
func residentEndpointOf(replicas []domain.Replica) string {
	for i := range replicas {
		if replicas[i].State == domain.ReplicaStateResident && replicas[i].GuestIP != "" {
			return fmt.Sprintf("%s:%d", replicas[i].GuestIP, residentPGPort)
		}
	}
	return ""
}

// DecommissionDedicated tears down a dedicated resource. A durable tier is torn
// down snapshot-first: a recovery point MUST exist when the teardown proceeds, or
// the decommission fails. Normally that means the final snapshot succeeded and
// produced one; when it captured nothing — or could not run at all because the
// backing file is gone — a PRIOR recovery point satisfies the guarantee and is
// what gets reported. The row becomes a tombstone (recovery points retained).
//
// It RETURNS the final recovery point so the caller can verify the guarantee
// instead of inferring it from a status code. Without that, `final_snapshot=true`
// proved only that a snapshot CALL succeeded — and a call over an app with zero
// volumes succeeds while creating nothing, which is exactly the shape in which a
// resource is destroyed with nothing to restore from. The return is nil only
// when no final snapshot was asked for (an ephemeral tier) or when the resource
// was already a tombstone.
func (uc *FleetResourceProvisioner) DecommissionDedicated(ctx context.Context, resourceID uuid.UUID, finalSnapshot bool) (*domain.FleetResourceRecoveryPoint, error) {
	rp, err := uc.decommissionDedicated(ctx, resourceID, finalSnapshot)
	recordExecution("decommission_dedicated_resource", outcomeFor(err))
	return rp, err
}

func (uc *FleetResourceProvisioner) decommissionDedicated(ctx context.Context, resourceID uuid.UUID, finalSnapshot bool) (*domain.FleetResourceRecoveryPoint, error) {
	res, err := uc.resources.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		return nil, err
	}
	if res.Phase == domain.FleetResourcePhaseDecommissioned {
		return nil, nil // idempotent
	}
	// A durable tier must not be torn down without a recovery point. Only an
	// ephemeral tier/lifecycle may skip the final snapshot — dedicated is durable.
	if !finalSnapshot && res.Tier == resourceTierDedicated {
		return nil, domain.ErrResourceFinalSnapshotRequired
	}
	var final *domain.FleetResourceRecoveryPoint
	if finalSnapshot {
		if uc.snapshotter == nil {
			return nil, domain.ErrResourceFinalSnapshotRequired
		}
		if res.AppID == nil {
			return nil, fmt.Errorf("dedicated resource %s has no backing app to snapshot", res.ID)
		}
		// Snapshot-first: fail the decommission if the snapshot fails.
		//
		// ⚠ With ONE exception, and it is the same exception the catalog fallback
		// below already exists for. A resource whose backing file is GONE fails the
		// snapshot with ErrVolumeBackingFileMissing, and aborting on it made that
		// resource undecommissionable FOREVER — no retry can invent the file, so the
		// call could never reach the fallback that was written precisely for
		// "this call produced nothing". The guarantee is that a recovery point
		// EXISTS, not that this call produced one, so a missing backing file is
		// routed into the same question the empty-points path asks: is there a prior
		// recovery point? If yes the teardown proceeds on it (and reports THAT one,
		// never a point it did not make); if not it is still refused with
		// ErrResourceFinalSnapshotRequired. Every OTHER snapshot error still aborts:
		// those describe a snapshot that COULD have been taken (a failed upload, an
		// unquiesciable guest), and retrying them is the right answer.
		points, serr := uc.snapshotter.SnapshotAppVolumes(ctx, res.ID, *res.AppID)
		backingGone := errors.Is(serr, domain.ErrVolumeBackingFileMissing)
		if serr != nil && !backingGone {
			return nil, fmt.Errorf("final snapshot: %w", serr)
		}
		if backingGone {
			// The snapshotter aborts the whole app on the first failed volume, so
			// whatever it returned alongside the error is a partial answer that must
			// not be mistaken for "this call captured something". Drop it and let the
			// catalog answer.
			points = nil
		}
		// ZERO NEW recovery points is not automatically a refusal — the guarantee
		// is that a recovery point EXISTS, not that this call produced one.
		//
		// The snapshotter walks the app's volumes, so an app carrying none returns
		// ([], nil): the call worked and captured nothing. That happens in two very
		// different situations, and conflating them creates a permanently stuck
		// resource. Consider a teardown that snapshots successfully, deletes the
		// app (cascading its volumes away), and then fails writing the tombstone:
		// the retry finds no volumes, produces no points, and would be refused
		// forever — on a resource that provably HAS a good final recovery point
		// sitting in the store. Recovery points deliberately outlive the tombstone
		// precisely so this is answerable.
		//
		// So: fall back to the catalog. A prior recovery point satisfies the
		// guarantee and the teardown proceeds. None at all means the resource
		// really would be destroyed with nothing to restore from, and that is the
		// refusal worth keeping — refusing costs an operator a question, proceeding
		// costs the data.
		if len(points) == 0 {
			prior, perr := uc.resources.ListRecoveryPoints(ctx, res.ID)
			if perr != nil {
				return nil, fmt.Errorf("resource %s produced no recovery point and its catalog is unreadable: %w", res.ID, perr)
			}
			if len(prior) == 0 {
				if backingGone {
					// Both sentinels: the refusal IS ErrResourceFinalSnapshotRequired
					// (the precondition that held), and the cause the operator has to
					// act on is that the data is gone — neither is inferable from the
					// other, and the boundary reads both.
					return nil, fmt.Errorf("%w: resource %s has NO recovery point on record and its backing file is gone, so tearing it down would leave nothing to restore from: %w",
						domain.ErrResourceFinalSnapshotRequired, res.ID, serr)
				}
				return nil, fmt.Errorf("%w: resource %s produced NO recovery point and has none on record, so tearing it down would leave nothing to restore from",
					domain.ErrResourceFinalSnapshotRequired, res.ID)
			}
			if backingGone {
				logger.FromContext(ctx).Warn("fleet decommission: the volume's backing file is GONE, so no final snapshot could be taken; proceeding on an existing recovery point — the data on that host is NOT in this teardown's recovery point",
					"resource_id", res.ID, "app_id", res.AppID, "missing_backing", serr.Error(),
					"recovery_point_id", prior[0].ID, "recovery_point_object_key", prior[0].ObjectKey,
					"recovery_point_created_at", prior[0].CreatedAt, "prior_recovery_points", len(prior))
			} else {
				logger.FromContext(ctx).Warn("fleet decommission: final snapshot captured nothing; proceeding on an existing recovery point",
					"resource_id", res.ID, "prior_recovery_points", len(prior))
			}
			final = &prior[0]
		} else {
			// The dedicated tier is single-volume by construction (dedicatedDescriptor
			// requests exactly one, and in-place restore refuses anything else), so the
			// first point IS the resource's final recovery point — the same choice the
			// SnapshotResource RPC makes.
			//
			// This MUST stay in the else. Outside it, the empty-points path above
			// indexes points[0] and panics, and it silently overwrites the prior
			// recovery point the fallback just resolved.
			final = &points[0]
		}
	}
	// Mark the teardown INTENT before calling down, because the app seam now
	// REFUSES to tear down an app a live resource claims
	// (domain.ErrAppBacksDurableResource) and this is the legitimate way through:
	// the guard keys on "a live claim", and stamping decommissioned_at is what
	// makes this claim no longer live. It is stamped here — after the
	// snapshot-first gate above — so a refused teardown never marks intent.
	//
	// decommissioned_at (not the phase) is the marker on purpose. Writing the
	// `decommissioned` PHASE first would make the idempotency check at the top of
	// this function short-circuit a RETRY of a teardown that failed on the way
	// down: the resource would read retired forever while its VM kept running on
	// the customer's volume. The timestamp says "teardown began", the phase still
	// says "teardown finished", and a retry re-enters and completes.
	now := time.Now().UTC()
	intentStamped := false
	if res.DecommissionedAt == nil {
		res.DecommissionedAt = &now
		res.UpdatedAt = now
		if err := uc.resources.SaveResource(ctx, res); err != nil {
			return nil, fmt.Errorf("mark resource teardown intent: %w", err)
		}
		intentStamped = true
	}
	// unmarkIntent puts a resource whose teardown FAILED back under the app-seam
	// guard. Without it a resource left mid-teardown is a live database no longer
	// protected from an app-level decommission — exactly the state this change
	// exists to prevent. Best-effort: a failed rollback is logged, and the caller
	// still gets the original teardown error.
	unmarkIntent := func(cause error) {
		if !intentStamped {
			return
		}
		res.DecommissionedAt = nil
		res.UpdatedAt = time.Now().UTC()
		if serr := uc.resources.SaveResource(ctx, res); serr != nil {
			logger.FromContext(ctx).Error("fleet decommission: teardown failed AND its intent stamp could not be rolled back — this resource is not protected by the app-seam guard until it is retried",
				"resource_id", res.ID, "teardown_err", cause, "err", serr)
		}
	}
	if res.AppID != nil {
		if derr := uc.provisioner.Decommission(ctx, res.AppID.String()); derr != nil {
			// An app row that is ALREADY GONE is not a teardown failure — there is
			// nothing left to tear down, and the only work still owed is the tombstone.
			// Without this the resource is stuck forever: DeleteAppVolumes reclaims the
			// volume rows explicitly (0024 removed the cascade) while
			// fleet_resources.app_id carries no FK (0012), so a teardown that deletes the app and then fails
			// writing the tombstone leaves a row pointing at a vanished app that can
			// neither boot (recoverExisting refuses it, correctly — a re-provision
			// would mint a NEW empty volume) nor retire (this call aborted). The
			// snapshot-first guarantee above is UNTOUCHED: reaching here means a
			// recovery point provably exists, and a resource with none is still refused.
			//
			// The tolerance lives HERE and not in FleetProvision.Decommission on
			// purpose: making an unknown handle idempotent success at the P7 seam would
			// widen that shared contract's semantics for every caller of it.
			if !errors.Is(derr, domain.ErrWorkloadNotFound) && !errors.Is(derr, domain.ErrFleetAppNotFound) {
				unmarkIntent(derr)
				return nil, fmt.Errorf("decommission app: %w", derr)
			}
			// Abnormal path — an operator needs to see that this teardown completed
			// over a backing app that had already vanished, and on which recovery
			// point it is leaving the customer.
			reliedOn := "none"
			if final != nil {
				reliedOn = final.ID.String()
			}
			logger.FromContext(ctx).Warn("fleet decommission: backing app was already gone; retiring the resource row on its existing recovery point",
				"resource_id", res.ID, "missing_app_id", res.AppID, "recovery_point_id", reliedOn, "err", derr)
		}
	}
	res.Phase = domain.FleetResourcePhaseDecommissioned
	res.UpdatedAt = time.Now().UTC()
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		// Deliberately NOT rolled back: the backing app is already torn down, so
		// the intent stamp is now simply true, and there is no live data left for
		// the app-seam guard to protect. The retry re-enters (the phase, not the
		// stamp, is what short-circuits it) and re-writes the tombstone.
		return nil, fmt.Errorf("tombstone resource: %w", err)
	}
	return final, nil
}
