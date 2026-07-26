package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// resourceTierShared is the tier the shared logical-database path serves.
const resourceTierShared = "shared"

// sharedReapInterval is how often the TTL reaper polls for expired shared
// resources. It is the poll cadence, NOT the resource TTL.
const sharedReapInterval = time.Minute

// ─────────────────────────────────────────────────────────────────────
// LogicalProvisioner port — provisions/reclaims a logical database + owning role
// on the SEPARATE shared Postgres engine (implemented by testdb.Provisioner).
// ─────────────────────────────────────────────────────────────────────

// LogicalProvisioner materializes a logical database (role + DB cloned from an
// allowlisted seed, PUBLIC revoked) and drops it. Implemented by the shared-tier
// engine's testdb.Provisioner.
type LogicalProvisioner interface {
	ProvisionLogical(ctx context.Context, in LogicalProvisionRequest) (LogicalLease, error)
	DropLogical(ctx context.Context, dbName, roleName string) error
}

// LogicalProvisionRequest is the wire-agnostic logical-database request. Password
// is a plaintext value that lives only for the duration of the call — the port
// implementation must never persist, log, or return it.
type LogicalProvisionRequest struct {
	DBName           string
	RoleName         string
	Password         string
	SeedTemplate     string
	AllowedTemplates []string
}

// LogicalLease is the created logical database's identity (no credential).
type LogicalLease struct {
	DBName   string
	RoleName string
}

// SharedEngineConfig is the resolved shared-tier engine endpoint + policy
// (populated from APP_RESOURCE_SHARED_*).
type SharedEngineConfig struct {
	Host          string
	Port          int
	TTL           time.Duration
	SeedTemplates []string
}

// ─────────────────────────────────────────────────────────────────────
// FleetResourceSharedProvisioner use case (R3, CP4.5 §9 #3, D-183).
// ─────────────────────────────────────────────────────────────────────

// FleetResourceSharedProvisioner provisions a shared-tier Postgres resource as a
// logical database on a separate shared engine, and reaps expired ones. The role
// password is resolved host-side from the claim's secret_refs (P14) under the
// handed Vault token and flows only into CREATE ROLE — never a row, log, or
// return value.
type FleetResourceSharedProvisioner struct {
	logical   LogicalProvisioner
	resources repository.FleetResourceRepository
	cfg       SharedEngineConfig

	// resolver turns the claim's secret_refs into the role password at provision
	// time, scoped to the owner org (I28) and decrypting under the handed Vault
	// token (D-125). Nil → a shared provision fails closed (never role-less).
	resolver secret.Resolver

	reapInterval time.Duration
	stopOnce     sync.Once
	stopCh       chan struct{}
	doneCh       chan struct{}
}

// NewFleetResourceSharedProvisioner constructs the use case. The secret resolver
// is wired separately (SetSecretResolver), mirroring FleetProvision.
func NewFleetResourceSharedProvisioner(
	logical LogicalProvisioner,
	resources repository.FleetResourceRepository,
	cfg SharedEngineConfig,
) *FleetResourceSharedProvisioner {
	return &FleetResourceSharedProvisioner{
		logical:      logical,
		resources:    resources,
		cfg:          cfg,
		reapInterval: sharedReapInterval,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// SetSecretResolver wires the per-tenant secret resolver (P14). Nil is valid —
// a shared provision then fails closed at resolve time (never role-less).
func (uc *FleetResourceSharedProvisioner) SetSecretResolver(r secret.Resolver) { uc.resolver = r }

// ProvisionSharedInput is the wire-agnostic shared-resource claim.
type ProvisionSharedInput struct {
	OwnerOrg     string
	ClaimKey     string
	Env          string
	Revision     int
	Class        string
	Tier         string
	SecretRefs   []string
	VaultToken   string
	SeedTemplate string
}

// ProvisionSharedOutput is the claim result.
type ProvisionSharedOutput struct {
	Handle   string
	Phase    string
	Endpoint string
}

// ProvisionShared declaratively ensures a shared logical database for a claim.
// Idempotent per (owner_org, claim_key, env): same revision returns the current
// status; a different revision is REJECTED; a concurrent insert-race returns the
// winner (and drops the loser's orphan logical database).
func (uc *FleetResourceSharedProvisioner) ProvisionShared(ctx context.Context, in ProvisionSharedInput) (ProvisionSharedOutput, error) {
	if in.Class != resourceClassPostgres {
		return ProvisionSharedOutput{}, domain.ErrResourceClassUnsupported
	}
	if in.Tier != resourceTierShared {
		return ProvisionSharedOutput{}, domain.ErrResourceTierUnsupported
	}
	if in.OwnerOrg == "" {
		return ProvisionSharedOutput{}, domain.ErrResourceOwnerOrgRequired
	}
	ownerUUID, err := uuid.Parse(in.OwnerOrg)
	if err != nil {
		return ProvisionSharedOutput{}, fmt.Errorf("parse owner org: %w", err)
	}
	if in.ClaimKey == "" {
		return ProvisionSharedOutput{}, domain.ErrResourceClaimKeyRequired
	}
	if len(in.SecretRefs) == 0 {
		return ProvisionSharedOutput{}, domain.ErrResourceSecretsRequired
	}
	if in.VaultToken == "" {
		return ProvisionSharedOutput{}, domain.ErrResourceVaultTokenRequired
	}
	revision := in.Revision
	if revision <= 0 {
		revision = 1
	}

	existing, err := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env)
	if err == nil {
		if existing.Revision == revision {
			return ProvisionSharedOutput{Handle: existing.ID.String(), Phase: string(existing.Phase), Endpoint: existing.Endpoint}, nil
		}
		return ProvisionSharedOutput{}, domain.ErrResourceConvergeNotSupported
	}
	if !errors.Is(err, domain.ErrResourceNotFound) {
		return ProvisionSharedOutput{}, fmt.Errorf("lookup resource claim: %w", err)
	}

	// Resolve the role password host-side (P14) under the handed Vault token. The
	// plaintext lives only in this frame and the CREATE ROLE call.
	secrets, err := resolveBootSecrets(ctx, uc.resolver, in.SecretRefs, in.OwnerOrg, in.VaultToken)
	if err != nil {
		return ProvisionSharedOutput{}, fmt.Errorf("resolve shared credentials: %w", err)
	}
	password, err := pickSharedPassword(secrets)
	if err != nil {
		return ProvisionSharedOutput{}, err
	}

	dbName, err := deriveSharedDBName(in.ClaimKey)
	if err != nil {
		return ProvisionSharedOutput{}, err
	}
	roleName, err := deriveSharedRoleName()
	if err != nil {
		return ProvisionSharedOutput{}, err
	}

	lease, err := uc.logical.ProvisionLogical(ctx, LogicalProvisionRequest{
		DBName:           dbName,
		RoleName:         roleName,
		Password:         password,
		SeedTemplate:     in.SeedTemplate,
		AllowedTemplates: uc.cfg.SeedTemplates,
	})
	if err != nil {
		return ProvisionSharedOutput{}, fmt.Errorf("provision logical database: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(uc.cfg.TTL)
	endpoint := fmt.Sprintf("%s:%d", uc.cfg.Host, uc.cfg.Port)
	res := &domain.FleetResource{
		ID:       uuid.New(),
		OwnerOrg: ownerUUID,
		ClaimKey: in.ClaimKey,
		Env:      in.Env,
		Revision: revision,
		// Stamped explicitly (GORM writes every field it saves, and generation 0 is
		// refused by the 0021 CHECK). No endpoint identity is minted here: a shared
		// claim is a logical database on a SHARED engine, so what a customer would
		// connect to is the engine's name, not a name of its own — and inventing one
		// per logical database is a decision the shared tier's gate has not made.
		Generation: domain.FleetResourceInitialGeneration,
		Class:      resourceClassPostgres,
		Tier:       resourceTierShared,
		// Stamped explicitly for the same reason as Generation (GORM writes every
		// field, and '' is refused by the 0022 CHECKs). The shared tier is a logical
		// database on a shared engine: it has no members of its own to replicate, so
		// `single` is the true value here, not a placeholder.
		AvailabilityClass: domain.AvailabilityClassSingle,
		SyncDegradePolicy: domain.SyncDegradePolicyFailClosed,
		Phase:             domain.FleetResourcePhaseReady,
		DBName:            lease.DBName,
		RoleName:          lease.RoleName,
		Endpoint:          endpoint,
		SecretRefs:        in.SecretRefs,
		ExpiresAt:         &expires,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := uc.resources.SaveResource(ctx, res); err != nil {
		// Lost the insert race: drop this call's orphan logical database (the
		// winner created its own) and return the winner.
		if winner, ferr := uc.resources.FindResource(ctx, ownerUUID, in.ClaimKey, in.Env); ferr == nil {
			if derr := uc.logical.DropLogical(ctx, lease.DBName, lease.RoleName); derr != nil {
				logger.FromContext(ctx).Warn("fleet shared resource: drop orphan on race loss", "db_name", lease.DBName, "err", derr)
			}
			return ProvisionSharedOutput{Handle: winner.ID.String(), Phase: string(winner.Phase), Endpoint: winner.Endpoint}, nil
		}
		return ProvisionSharedOutput{}, fmt.Errorf("persist resource: %w", err)
	}
	return ProvisionSharedOutput{Handle: res.ID.String(), Phase: string(res.Phase), Endpoint: endpoint}, nil
}

// Start launches the TTL reaper loop. Safe to call once. The loop exits on ctx
// cancel or Stop().
func (uc *FleetResourceSharedProvisioner) Start(ctx context.Context) {
	go uc.runReaper(ctx)
}

// Stop signals the reaper to exit and waits for it (shutdown group).
func (uc *FleetResourceSharedProvisioner) Stop() {
	uc.stopOnce.Do(func() { close(uc.stopCh) })
	<-uc.doneCh
}

func (uc *FleetResourceSharedProvisioner) runReaper(ctx context.Context) {
	defer close(uc.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("fleet shared resource reaper panicked", "panic", r)
		}
	}()
	t := time.NewTicker(uc.reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-uc.stopCh:
			return
		case <-t.C:
			uc.reapOnce(ctx)
		}
	}
}

// reapOnce drops and tombstones every expired shared resource. A drop failure
// leaves the row live (not tombstoned) so the next tick retries — a tombstoned
// row whose logical DB still exists would leak the database forever.
func (uc *FleetResourceSharedProvisioner) reapOnce(ctx context.Context) {
	expired, err := uc.resources.ListExpiredShared(ctx, time.Now().UTC())
	if err != nil {
		logger.FromContext(ctx).Warn("fleet shared resource reaper: list expired", "err", err)
		return
	}
	for i := range expired {
		r := &expired[i]
		if err := uc.logical.DropLogical(ctx, r.DBName, r.RoleName); err != nil {
			logger.FromContext(ctx).Warn("fleet shared resource reaper: drop logical", "resource_id", r.ID, "err", err)
			continue
		}
		now := time.Now().UTC()
		r.Phase = domain.FleetResourcePhaseDecommissioned
		r.DecommissionedAt = &now
		r.UpdatedAt = now
		if err := uc.resources.SaveResource(ctx, r); err != nil {
			logger.FromContext(ctx).Warn("fleet shared resource reaper: tombstone", "resource_id", r.ID, "err", err)
		}
	}
}

// pickSharedPassword selects the single role password from resolved secrets:
// exactly one secret is unambiguous; otherwise the one named "password" is used.
// Neither → ErrResourceSharedPasswordAmbiguous (fail closed rather than guess).
func pickSharedPassword(secrets []HostSecret) (string, error) {
	if len(secrets) == 1 {
		return secrets[0].Val, nil
	}
	for _, s := range secrets {
		if strings.EqualFold(s.Name, "password") {
			return s.Val, nil
		}
	}
	return "", domain.ErrResourceSharedPasswordAmbiguous
}

// pgIdentMax bounds a Postgres identifier (63 bytes); we stay well under it.
const pgIdentMax = 63

// deriveSharedDBName builds a claim-derived, unique, valid Postgres database
// name: "res_<sanitized-claim>_<rand>". The random suffix keeps it unique; the
// resource row stores the exact name so re-provisions reuse it.
func deriveSharedDBName(claimKey string) (string, error) {
	suffix, err := randHex(6)
	if err != nil {
		return "", err
	}
	san := sanitizeIdent(claimKey)
	prefix := "res_"
	// leave room for "_" + 12-hex suffix under the identifier limit
	maxSan := pgIdentMax - len(prefix) - 1 - len(suffix)
	if len(san) > maxSan {
		san = san[:maxSan]
	}
	return prefix + san + "_" + suffix, nil
}

// deriveSharedRoleName builds a random role name "r_<rand>".
func deriveSharedRoleName() (string, error) {
	h, err := randHex(8)
	if err != nil {
		return "", err
	}
	return "r_" + h, nil
}

// sanitizeIdent lowercases claimKey and replaces every non [a-z0-9_] rune with
// '_' so it is a safe Postgres identifier fragment.
func sanitizeIdent(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// randHex returns n random bytes as a hex string (2n chars). crypto/rand — never
// math/rand for identifiers.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random identifier: %w", err)
	}
	return hex.EncodeToString(b), nil
}
