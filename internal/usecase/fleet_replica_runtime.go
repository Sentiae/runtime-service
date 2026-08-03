package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetReplicaRuntime turns a resident replica into a durable, bootable,
// health-checkable, decommissionable microVM on top of the CP3 ImageBooter
// (runtime-fleet CP4 §9#6). It owns only the per-replica boot/teardown/health
// mechanics — placement, scaling, and reconciliation live elsewhere.
type FleetReplicaRuntime struct {
	materializer ImageMaterializer
	booter       ImageBooter
	replicas     repository.ReplicaRepository
	apps         repository.FleetAppRepository
	workDir      string
	advertise    string

	// resolver turns each app.SecretRefs entry into a concrete secret value at
	// boot, scoped to the app's owner org (I28). Nil on a host where no Vault
	// resolver could be built (degrade-not-crash): a secret-less app still boots,
	// but a secret-bearing app fails closed (ErrSecretResolverUnavailable).
	resolver secret.Resolver

	// volumes resolves + attaches the app's persistent data disk at boot (rt#9).
	// Nil leaves the replica stateless (no data disk attached).
	volumes *FleetVolumeManager

	// tokenStore holds each app's handed per-deployment Vault token in memory
	// (D-125). bootSecrets reads it to stamp secret.Principal.Token so the
	// HandedTokenEnvelopeResolver decrypts under the handed token (no host mint).
	// Nil where no handed-token path is wired (secret-bearing apps then fail
	// closed at resolve time, as before).
	tokenStore *FleetSecretTokenStore

	// registryTokenStore holds each app's handed per-deployment registry PULL token
	// in memory (D-124). The materialize step reads it (keyed by app id) and
	// presents it as the registry Basic password when pulling the image. Nil leaves
	// the pull on the shared registry service key (back-compat).
	registryTokenStore *FleetRegistryTokenStore

	// secretSelfTest, when set (APP_FLEET_SECRET_SELFTEST), injects a NON-SECRET
	// marker over the vsock secret channel on secret-ref-less resident boots so
	// the I32 mechanism is verifiable on the live orchestrator path without any
	// real secret. Off by default → behavior-neutral (no /vsock, no push). Mirrors
	// FleetProvision.secretSelfTest for the test/fallback path.
	secretSelfTest bool

	// selfHost is this process's durable fleet host identity
	// (#fleet-reconciler-acts-on-foreign-host-replicas). Every method on this type
	// is a HOST-LOCAL capability — signalling a PID, probing a guest address,
	// destroying a TAP, unlinking a jail — while fleet_replicas is a GLOBAL table.
	// Immutable and constructor-required: a runtime that could be re-scoped could
	// be pointed at another machine's live VMs.
	selfHost uuid.UUID

	// procAlive / dialGuest are the two host-local probes RefreshHealth performs.
	// They are fields so a refusal test can prove NEITHER was invoked on a foreign
	// row — "returned the sentinel" and "did not probe another host's VM" are
	// different claims, and only the second is the invariant. Production always
	// runs the package defaults.
	procAlive func(pid int) bool
	dialGuest func(host string, port int) bool
}

// SetSecretSelfTest enables the gated vsock self-test marker injection on the
// resident replica boot path (Phase 3.3 verification only). Wired from
// APP_FLEET_SECRET_SELFTEST in the container.
func (uc *FleetReplicaRuntime) SetSecretSelfTest(on bool) { uc.secretSelfTest = on }

// SetSecretResolver wires the per-tenant secret resolver (P14). Nil is a valid
// state — a host that could not reach Vault at boot leaves it nil and only
// secret-less apps can boot there (secret-bearing apps fail closed).
func (uc *FleetReplicaRuntime) SetSecretResolver(r secret.Resolver) { uc.resolver = r }

// SetVolumeManager wires the persistent-volume manager so a stateful replica
// attaches its data disk at boot (rt#9). Optional: nil leaves replicas stateless.
func (uc *FleetReplicaRuntime) SetVolumeManager(vm *FleetVolumeManager) { uc.volumes = vm }

// SetTokenStore wires the in-memory handed-token store (D-125). Optional: nil
// keeps the pre-D-125 behavior (the resolver mints, or a secret-bearing app
// fails closed if no resolver).
func (uc *FleetReplicaRuntime) SetTokenStore(ts *FleetSecretTokenStore) { uc.tokenStore = ts }

// SetRegistryTokenStore wires the in-memory handed registry-pull-token store
// (D-124). Optional: nil leaves the image pull on the shared registry service
// key (back-compat).
func (uc *FleetReplicaRuntime) SetRegistryTokenStore(ts *FleetRegistryTokenStore) {
	uc.registryTokenStore = ts
}

// bootSecrets derives the secrets to push to a replica's microVM at boot. It is
// re-evaluated on every BootReplica (crash-recovery, scale) so secrets are
// re-supplied per boot and never persisted plaintext.
//
// For an app with secret_refs it resolves each ref through the per-tenant
// resolver, scoped to the app's owner org (I28), and fails closed on ANY error
// (nil resolver, missing owner org, cross-tenant denial, missing KEK, decrypt
// failure, not-found): the boot aborts rather than run a VM with missing or
// partial secrets (I32). A resolved value is revealed only into the HostSecret
// the caller pushes over vsock — never logged, never persisted.
func (uc *FleetReplicaRuntime) bootSecrets(ctx context.Context, app *domain.FleetApp) ([]HostSecret, error) {
	if len(app.SecretRefs) > 0 {
		// D-125: stamp the app's handed per-deployment token (memory-only) so the
		// HandedTokenEnvelopeResolver decrypts under it instead of the host minting.
		// A missing token with a handed-token resolver fails closed at Resolve
		// (no host-side mint fallback), preserving I32.
		var token string
		if uc.tokenStore != nil {
			if tok, ok := uc.tokenStore.Get(app.ID); ok {
				token = tok
			}
		}
		return resolveBootSecrets(ctx, uc.resolver, app.SecretRefs, app.OwnerOrg, token)
	}
	if uc.secretSelfTest {
		return []HostSecret{{Name: selfTestSecretName, Val: selfTestSecretValue}}, nil
	}
	return nil, nil
}

// resolveBootSecrets resolves each secret_ref through the per-tenant resolver,
// scoped to ownerOrg (I28) and decrypting under the handed per-deployment token
// (D-125). It is THE boot-time P14 resolution path — shared by the resident
// replica boot and the one-shot job boot so neither drifts from the other.
//
// It fails closed on ANY error (nil resolver, missing owner org, cross-tenant
// denial, missing KEK, decrypt failure, not-found): the caller aborts the boot
// rather than run a VM with missing or partial secrets (I32). A resolved value
// is revealed only into the returned HostSecret the caller pushes over vsock —
// never logged, never persisted.
func resolveBootSecrets(ctx context.Context, resolver secret.Resolver, refs []string, ownerOrg, token string) ([]HostSecret, error) {
	if resolver == nil {
		return nil, domain.ErrSecretResolverUnavailable
	}
	if ownerOrg == "" {
		return nil, domain.ErrSecretOwnerOrgMissing
	}
	p := secret.Principal{Service: "runtime-fleet", OrgID: ownerOrg, Token: token}
	secrets := make([]HostSecret, 0, len(refs))
	for _, ref := range refs {
		sv, err := resolver.Resolve(ctx, ref, p)
		if err != nil {
			// ref is a reference (not the secret); safe to surface. The value is
			// never touched here.
			return nil, fmt.Errorf("resolve secret %q: %w", ref, err)
		}
		secrets = append(secrets, HostSecret{Name: fieldName(ref), Val: sv.Reveal()})
	}
	return secrets, nil
}

// fieldName extracts the "<field>" tail of a "<path>#<field>" secret_ref — the
// name the guest binds the secret to. Falls back to the whole ref when there is
// no "#" (a malformed ref the resolver would already have rejected).
func fieldName(ref string) string {
	if i := strings.LastIndex(ref, "#"); i >= 0 && i < len(ref)-1 {
		return ref[i+1:]
	}
	return ref
}

// NewFleetReplicaRuntime constructs the use case. workDir is the per-replica
// materialize staging root (mirrors FleetProvision); advertiseHost is the host
// address published in a resident replica's endpoint URL.
// selfHost is this instance's durable fleet host id — REQUIRED, because every
// verb here acts on a process, a socket and a device that exist on exactly one
// machine.
func NewFleetReplicaRuntime(
	materializer ImageMaterializer,
	booter ImageBooter,
	replicas repository.ReplicaRepository,
	apps repository.FleetAppRepository,
	workDir, advertiseHost string,
	selfHost uuid.UUID,
) (*FleetReplicaRuntime, error) {
	if selfHost == uuid.Nil {
		return nil, fmt.Errorf("%w: a replica runtime needs this instance's fleet host identity before it may boot, probe or tear down anything",
			domain.ErrReplicaHostMismatch)
	}
	return &FleetReplicaRuntime{
		materializer: materializer,
		booter:       booter,
		replicas:     replicas,
		apps:         apps,
		workDir:      workDir,
		advertise:    advertiseHost,
		selfHost:     selfHost,
		procAlive:    func(pid int) bool { return processAlive(pid) },
		dialGuest:    dialTCP,
	}, nil
}

// requireOwnedReplica refuses every host-local action on a replica this instance
// does not own.
//
// A NIL HostID is refused exactly like a foreign one. An unstamped row is not
// "probably mine": nothing about it proves which machine holds the PID it names,
// and every verb guarded by this check would then act on a number that may
// belong to an unrelated process on this host or to a live VM on another. The
// comparison is over UUID VALUES — never pointers, never endpoints, which are
// re-derived per boot and can coincide across hosts.
func (uc *FleetReplicaRuntime) requireOwnedReplica(replica *domain.Replica) error {
	if replica.HostID == nil {
		return fmt.Errorf("%w: replica %s carries no host id, so no host may act on it locally",
			domain.ErrReplicaHostMismatch, replica.ID)
	}
	if *replica.HostID != uc.selfHost {
		return fmt.Errorf("%w: replica %s is placed on another fleet host",
			domain.ErrReplicaHostMismatch, replica.ID)
	}
	return nil
}

// BootReplica materializes the replica's app image and boots it as a resident
// microVM, recording the full Firecracker handle on the replica. It is
// idempotent: an already-resident replica is a no-op.
func (uc *FleetReplicaRuntime) BootReplica(ctx context.Context, replicaID uuid.UUID) error {
	replica, err := uc.replicas.FindByID(ctx, replicaID)
	if err != nil {
		return err
	}
	// BEFORE the resident no-op, the app load, the state write, secret resolution,
	// volume lookup, materialize, lease allocation and boot. Everything past this
	// line is a side effect on THIS machine, and a row placed elsewhere must cost
	// exactly zero of them — including the "harmless" booting-state write, which
	// would tell the owning host's reconciler a boot it never started is in flight.
	if err := uc.requireOwnedReplica(replica); err != nil {
		return err
	}
	if replica.State == domain.ReplicaStateResident {
		return nil
	}

	app, err := uc.apps.FindByID(ctx, replica.AppID)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}

	replica.State = domain.ReplicaStateBooting
	replica.Message = ""
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		return fmt.Errorf("persist booting replica: %w", err)
	}

	// Secrets are re-derived on every boot (never persisted plaintext) so a
	// reconciler re-boot re-injects them (invariant I32). ExpectSecrets tells both
	// the guest (via runtime.json) to listen and the booter to open + push over
	// the vsock channel, failing closed if the push fails. A resolution failure
	// aborts the boot (fail closed) — no VM runs with missing/partial secrets.
	secrets, err := uc.bootSecrets(ctx, app)
	if err != nil {
		return uc.markDead(ctx, replica, fmt.Errorf("resolve secrets: %w", err))
	}
	expectSecrets := len(secrets) > 0

	// Per-boot vsock attestation nonce (D-085 Layer 2): minted once here, written
	// into runtime.json (via matIn) AND presented on the push handshake (via the
	// boot input). Only generated when the boot actually expects secrets.
	var nonce string
	if expectSecrets {
		nonce, err = newBootstrapNonce()
		if err != nil {
			return uc.markDead(ctx, replica, fmt.Errorf("mint bootstrap nonce: %w", err))
		}
	}

	// rt#9 — resolve the app's persistent data volume so the boot attaches its
	// backing file as a 2nd virtio-blk device and the guest mounts it at boot.
	var dataDiskPath, dataMountPath string
	var dataVolume bool
	if uc.volumes != nil {
		vol, ok, verr := uc.volumes.PrimaryVolume(ctx, app.ID)
		if verr != nil {
			return uc.markDead(ctx, replica, fmt.Errorf("resolve volume: %w", verr))
		}
		if ok {
			// D-184 restore stand-off — REFUSE the boot while a restore owns the
			// volume. Desired-replicas=0 alone does not close this: the reconciler
			// ticks every 10s and the scale-to-zero activator scales an app back to
			// one on ANY ingress request, so a wake landing between the restore's
			// drain and its rename would boot a VM holding an fd to the OLD inode
			// while a new backing file is renamed onto the path — silent wrong state.
			if vol.Status == domain.VolumeStatusRestoring {
				return uc.markDead(ctx, replica, fmt.Errorf("volume %s: %w", vol.ID, domain.ErrVolumeRestoreInProgress))
			}
			dataVolume = true
			dataDiskPath = vol.BackingPath
			dataMountPath = vol.MountPath
		}
	}

	// D-124 — read the app's handed registry pull token (memory-only, keyed by app
	// id) so the materialize presents it as the registry Basic password. Missing
	// (pre-cutover / no store) leaves matIn.RegistryPullToken empty → the shared
	// service key is used (back-compat).
	var registryPullToken string
	if uc.registryTokenStore != nil {
		if tok, ok := uc.registryTokenStore.Get(app.ID); ok {
			registryPullToken = tok
		}
	}

	matIn := ImageMaterializeInput{
		Repository:        app.ImageRepository,
		Digest:            app.ImageDigest,
		WorkDir:           filepath.Join(uc.workDir, replica.ID.String()),
		Mode:              string(domain.ImageWorkloadClassResident),
		Port:              app.Port,
		ExpectSecrets:     expectSecrets,
		BootstrapNonce:    nonce,
		DataMountPath:     dataMountPath,
		RegistryPullToken: registryPullToken,
	}
	mat, err := uc.materializer.Materialize(ctx, matIn)
	if err != nil {
		return uc.markDead(ctx, replica, fmt.Errorf("materialize: %w", err))
	}

	res, err := uc.booter.BootResident(ctx, ImageBootInput{
		WorkloadID:     replica.ID,
		OwnerKind:      domain.NetLeaseOwnerReplica,
		RootfsPath:     mat.RootfsPath,
		VCPU:           app.ResourcesVCPU,
		MemoryMB:       int(app.ResourcesMemMB),
		Port:           app.Port,
		ExpectSecrets:  expectSecrets,
		Secrets:        secrets,
		BootstrapNonce: nonce,
		DataDiskPath:   dataDiskPath,
		DataMountPath:  dataMountPath,
	})
	if err != nil {
		return uc.markDead(ctx, replica, fmt.Errorf("boot resident: %w", err))
	}

	pid := res.PID
	replica.PID = &pid
	replica.GuestIP = res.GuestIP
	replica.HostPort = res.HostPort
	replica.NetIndex = res.NetIndex
	replica.TapName = res.TapName
	replica.SocketPath = res.SocketPath
	replica.RootfsPath = mat.RootfsPath
	replica.Port = app.Port
	// rt#8 — the endpoint is now the replica's PRIVATE address (guest IP + app
	// port). Caddy proxies the public host to it; per-VM host-port DNAT is retired.
	replica.Endpoint = fmt.Sprintf("http://%s:%d", res.GuestIP, app.Port)
	replica.State = domain.ReplicaStateResident
	replica.Message = ""
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		// The VM is UP and the row that describes it could not be written. Tear it
		// down so we do not leak an untracked resident — but the teardown's verdict
		// is now load-bearing, not decoration.
		if derr := uc.booter.Decommission(ctx, replicaDecommissionInput(replica)); derr != nil {
			// ⚠ NOTHING IS RECLAIMED HERE. Discarding this error and unlinking the
			// staging tree anyway is exactly the shape that produced the live orphans:
			// a VMM that could not be proven dead, with its rootfs removed out from
			// under it and no row naming it. The staging directory is the last on-disk
			// evidence of what that pid is running, so it is KEPT and both causes are
			// reported — the persist failure that started this, and the teardown that
			// could not finish it.
			logger.FromContext(ctx).Error("fleet replica: the boot could not be persisted AND its VM could not be torn down — the staging tree is retained as the only remaining evidence of the running VMM",
				"replica_id", replica.ID, "pid", res.PID, "persist_err", err, "err", derr)
			return fmt.Errorf("persist resident replica %s failed (%v) and its VM could not be torn down: %w", replica.ID, err, derr)
		}
		// Proven torn down. Decommission unlinks only the rootfs FILE — the staging
		// directory it sits in is this boot's to reclaim, and this boot has failed.
		uc.reclaimStagingDir(ctx, replica.ID)
		return fmt.Errorf("persist resident replica: %w", err)
	}

	// rt#9 — the data volume is now held by this replica (single-writer).
	if dataVolume {
		if aerr := uc.volumes.AttachTo(ctx, app.ID, replica.ID); aerr != nil {
			logger.FromContext(ctx).Warn("fleet replica: attach volume", "replica_id", replica.ID, "err", aerr)
		}
	}
	return nil
}

// DecommissionReplica tears down a replica's microVM and deletes the replica
// row. Idempotent: a missing replica is a no-op.
//
// ⚠ IT IS NO LONGER BEST-EFFORT PAST A BOOTER ERROR. A teardown that could not
// prove the VMM exited used to log the failure and delete the row anyway, which
// is how a running microVM lost every record of itself: the row, the volume
// attachment and the staging tree all went, while the process, its TAP, its jail
// and its /30 stayed. Seven such orphans are on the live fleet host. The booter
// now returns ErrVMTerminationUnproven instead of guessing, and this function's
// job is to PRESERVE the evidence when it does — the replica row and the volume
// attachment are the only handles a retry has.
func (uc *FleetReplicaRuntime) DecommissionReplica(ctx context.Context, replicaID uuid.UUID) error {
	replica, err := uc.replicas.FindByID(ctx, replicaID)
	if err != nil {
		if errors.Is(err, domain.ErrReplicaNotFound) {
			return nil
		}
		return err
	}
	// Before the local handle fields are even inspected: signalling a PID, deleting
	// a TAP or unlinking a jail named by a foreign row are all actions on another
	// host's live VM.
	if err := uc.requireOwnedReplica(replica); err != nil {
		return err
	}
	// NetIndex is part of the admission test, and its absence was a hole: a row
	// whose only artifact is a net coordinate — the lease is taken BEFORE the TAP
	// and the VM exist, so a boot that died in that window records exactly that —
	// skipped the booter entirely and went straight to deleting the row. The lease
	// then outlived every record of its owner, holding a /30, a uid and a jail slot
	// that nothing would ever release. The booter's own never-booted rule (no pid
	// AND no artifacts at all) is what decides that a row genuinely needs no
	// teardown; this seam must not pre-empt it.
	if replica.PID != nil || replica.SocketPath != "" || replica.TapName != "" || replica.NetIndex > 0 {
		if err := uc.booter.Decommission(ctx, replicaDecommissionInput(replica)); err != nil {
			// STOP. Detaching the volume, reclaiming the staging directory or deleting
			// the row here would each erase a fact the retry needs, for a VM that may
			// still be running and still holding the disk.
			logger.FromContext(ctx).Error("fleet replica: teardown did not complete — the replica row, its volume attachment and its staging directory are PRESERVED so the teardown can be retried",
				"replica_id", replica.ID, "err", err)
			return err
		}
	}
	// rt#9 — release the data volume (the backing file survives; only
	// VolumeManager.Delete removes it). A degraded volume stays degraded.
	if uc.volumes != nil {
		if derr := uc.volumes.DetachFrom(ctx, replica.AppID); derr != nil {
			logger.FromContext(ctx).Warn("fleet replica: detach volume", "replica_id", replica.ID, "err", derr)
		}
	}
	// The booter unlinks the rootfs FILE, never the staging directory holding it,
	// so a torn-down replica left an empty directory behind. Reclaimed here rather
	// than left to the sweep so the invariant is "teardown leaves NOTHING" — a
	// property that can be checked right after the call, instead of "nothing that
	// matters, eventually", which cannot. Inodes accumulate too.
	uc.reclaimStagingDir(ctx, replica.ID)
	if err := uc.replicas.Delete(ctx, replica.ID); err != nil {
		return fmt.Errorf("delete replica: %w", err)
	}
	return nil
}

// RefreshHealth probes a resident replica (process alive && guest port
// accepting) and, when it has died, marks it dead. It never restarts — that is
// the reconciler's job (§9#7). Non-resident replicas are reported not-healthy
// without a probe.
func (uc *FleetReplicaRuntime) RefreshHealth(ctx context.Context, replica *domain.Replica) (bool, error) {
	// Before the state is even inspected and before either probe. A PID is only
	// meaningful on the machine that owns it — on this one it names an unrelated
	// process or nothing — so probing it and then writing `dead` onto a globally
	// visible row would report another host's healthy VM as gone, and the reconciler
	// there would replace a replica that never stopped serving.
	if err := uc.requireOwnedReplica(replica); err != nil {
		return false, err
	}
	if replica.State != domain.ReplicaStateResident {
		return false, nil
	}
	alive := replica.PID != nil && uc.procAlive(*replica.PID)
	if alive && replica.GuestIP != "" && replica.Port > 0 {
		alive = uc.dialGuest(replica.GuestIP, replica.Port)
	}
	if !alive {
		replica.State = domain.ReplicaStateDead
		replica.Message = "vm process exited"
		replica.UpdatedAt = time.Now().UTC()
		if err := uc.replicas.Update(ctx, replica); err != nil {
			return false, fmt.Errorf("persist dead replica: %w", err)
		}
		return false, nil
	}
	return true, nil
}

// markDead records a boot/materialize failure on the replica and returns the
// original cause wrapped. It also reclaims the boot's staging directory — see
// reclaimStagingDir: every path that reaches markDead is a boot that failed, and
// a failed boot must not leave its rootfs behind.
func (uc *FleetReplicaRuntime) markDead(ctx context.Context, replica *domain.Replica, cause error) error {
	replica.State = domain.ReplicaStateDead
	replica.Message = cause.Error()
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		logger.FromContext(ctx).Error("fleet replica: persist dead-state failed", "replica_id", replica.ID, "err", err)
	}
	uc.reclaimStagingDir(ctx, replica.ID)
	return cause
}

// reclaimStagingDir removes this replica's materialize staging directory
// (<workDir>/<replicaID>, holding the OCI staging tree and rootfs.ext4).
//
// WHY it exists: nothing else ever reclaimed it. The booter's Decommission
// unlinks only the rootfs FILE of a replica that reached resident, so a boot
// that FAILED left the whole directory behind — and the reconciler retries a
// dead replica every ~10s under a FRESH replica uuid, so one persistently
// failing app mints a new multi-hundred-MB directory per attempt until the host
// fills up (#fleet-image-staging-dirs-no-gc: 338 directories / 23GB observed
// live, fleet host at 100%, every customer VM on it at risk).
//
// WHY it is safe at every call site: it runs only after this boot has failed.
// No VM holds the rootfs — the booter kills whatever it started before returning
// an error, and the persist-failure path decommissions first — and the jailer
// keeps its own hard link to the rootfs inode, so unlinking our name could not
// pull the disk out from under a running VM even if one somehow survived.
// Best-effort by design: a boot failure is already being reported, and a
// removal error must not replace that cause.
func (uc *FleetReplicaRuntime) reclaimStagingDir(ctx context.Context, replicaID uuid.UUID) {
	// An empty workDir or nil id would join to a relative path (the process
	// working directory) — never recurse-delete on a half-configured host.
	if uc.workDir == "" || replicaID == uuid.Nil {
		return
	}
	dir := filepath.Join(uc.workDir, replicaID.String())
	if err := os.RemoveAll(dir); err != nil {
		logger.FromContext(ctx).Warn("fleet replica: reclaim staging dir",
			"replica_id", replicaID, "path", dir, "err", err)
	}
}

// replicaDecommissionInput builds the teardown handle from a replica.
func replicaDecommissionInput(replica *domain.Replica) ImageDecommissionInput {
	pid := 0
	if replica.PID != nil {
		pid = *replica.PID
	}
	return ImageDecommissionInput{
		OwnerKind:  domain.NetLeaseOwnerReplica,
		OwnerID:    replica.ID,
		PID:        pid,
		SocketPath: replica.SocketPath,
		TapName:    replica.TapName,
		NetIndex:   replica.NetIndex,
		GuestIP:    replica.GuestIP,
		HostPort:   replica.HostPort,
		Port:       replica.Port,
		RootfsPath: replica.RootfsPath,
	}
}
