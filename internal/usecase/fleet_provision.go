package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// bootstrapNonceBytes is the length of the per-boot vsock attestation nonce
// (D-085 Layer 2). 32 bytes of crypto/rand is unpredictable and collision-free.
const bootstrapNonceBytes = 32

// newBootstrapNonce mints a per-boot, cryptographically-random nonce the guest
// requires the secret pusher to present before accepting the bundle. It is NOT a
// secret (it authenticates the pusher, not confidentiality) but MUST be fresh +
// unpredictable every boot — crypto/rand, never math/rand.
func newBootstrapNonce() (string, error) {
	b := make([]byte, bootstrapNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate bootstrap nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ─────────────────────────────────────────────────────────────────────
// Ports the FleetProvision use case depends on (implemented under
// internal/infrastructure — the materializer in oci/, the booter in
// firecracker/).
// ─────────────────────────────────────────────────────────────────────

// ImageMaterializer pulls a compiled OCI image and lays it down as an ext4
// rootfs the booter can hand to Firecracker.
type ImageMaterializer interface {
	Materialize(ctx context.Context, in ImageMaterializeInput) (ImageMaterializeOutput, error)
}

// ImageMaterializeInput is the wire-agnostic materialize request.
type ImageMaterializeInput struct {
	Registry    string
	Repository  string
	Digest      string
	ChangeID    string
	WorkDir     string
	EnvVars     map[string]string
	Mode        string // "test" | "resident" | "job"
	TestCommand string
	// JobCommand is the job class's argv-exact entrypoint override, carried as a
	// list end-to-end (never a joined string) so the guest execs it directly.
	JobCommand []string
	Port       int
	// ExpectSecrets tells the guest (via runtime.json) to open the vsock secret
	// listener at boot and block on a host push before exec (invariant I32).
	ExpectSecrets bool
	// BootstrapNonce is the per-boot vsock attestation nonce (D-085 Layer 2),
	// written into runtime.json so the guest can require the pusher to present it.
	// Same value must be supplied to the booter's push (see ImageBootInput). Empty
	// when the boot expects no secrets.
	BootstrapNonce string
	// DataMountPath is the in-guest mount point for the persistent data volume
	// (2nd virtio-blk /dev/vdb). Empty when the workload has no volume; written to
	// runtime.json so image-init mounts /dev/vdb there at boot.
	DataMountPath string
	// RegistryPullToken is the per-deployment, org-scoped registry PULL token
	// (D-124) the materializer presents as the registry Basic-auth password when
	// pulling the image, replacing the shared read-any-org service key. Empty →
	// fall back to the shared service key (back-compat). A live credential: NEVER
	// written to the rootfs, runtime.json, or a log.
	RegistryPullToken string
}

// ImageMaterializeOutput is the materialize result.
type ImageMaterializeOutput struct {
	RootfsPath string
}

// ImageBooter boots a materialized rootfs as a Firecracker microVM.
type ImageBooter interface {
	BootTest(ctx context.Context, in ImageBootInput) (ImageTestResult, error)
	BootResident(ctx context.Context, in ImageBootInput) (ImageResidentResult, error)
	Decommission(ctx context.Context, in ImageDecommissionInput) error
}

// HostSecret is one plaintext secret the host pushes to the guest over vsock at
// boot. Val is a plain string at this boot layer; the real resolver's
// secret.SecretValue is unwrapped by the caller only at push time (Phase 3.4).
// It NEVER touches the host rootfs, logs, or DB (invariant I32).
type HostSecret struct {
	Name string
	Val  string
}

// ImageBootInput is the common boot request.
type ImageBootInput struct {
	WorkloadID uuid.UUID
	// OwnerKind names which control-plane table WorkloadID points at, and it is
	// REQUIRED: the booter acquires this VM's addressing lease keyed by
	// (OwnerKind, WorkloadID), and both fleet_replicas and image_workloads boot
	// into the SAME index space, so an owner reference without its table is
	// ambiguous. An empty value refuses the boot rather than defaulting — a lease
	// filed under the wrong kind could be reclaimed while its VM is still running.
	OwnerKind      domain.NetLeaseOwnerKind
	RootfsPath     string
	VCPU           int
	MemoryMB       int
	Port           int
	TimeoutSeconds int
	// ExpectSecrets makes the booter attach a /vsock device and push Secrets to
	// the guest after start but before the ready/power-off wait. A push failure
	// fails the boot closed (the VM is killed) — a secret workload must not run
	// without its channel.
	ExpectSecrets bool
	Secrets       []HostSecret
	// BootstrapNonce is the per-boot vsock attestation nonce (D-085 Layer 2)
	// echoed to the guest in the push handshake; it MUST equal the nonce written
	// into runtime.json (ImageMaterializeInput.BootstrapNonce). Empty when the
	// boot expects no secrets.
	BootstrapNonce string
	// DataDiskPath is the host path of the ext4 backing file attached to the guest
	// as a 2nd virtio-blk device (/dev/vdb). Empty when the workload has no volume.
	DataDiskPath string
	// DataMountPath is the in-guest mount point for the data disk (informational at
	// the boot layer; the guest reads it from runtime.json).
	DataMountPath string
	// EgressAllow is the job class's network egress allowlist (IPs/CIDRs/hostnames,
	// resolved once at boot). Non-empty makes the booter install a per-VM iptables
	// chain on the workload's TAP that ACCEPTs only these destinations and DROPs
	// everything else, torn down with the TAP. Empty installs nothing (the class's
	// default subnet rules apply) — so the test/resident paths are unaffected.
	EgressAllow []string
}

// ImageTestResult is the outcome of a single-shot test boot.
type ImageTestResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// ImageResidentResult is the outcome of a resident boot.
type ImageResidentResult struct {
	PID        int
	GuestIP    string
	HostPort   int
	NetIndex   int
	TapName    string
	SocketPath string
}

// ImageDecommissionInput tears down a resident workload.
type ImageDecommissionInput struct {
	// OwnerKind + OwnerID identify the control-plane row this VM belongs to, which
	// is what the booter releases the addressing lease by. They are required for
	// the RELEASE to happen: a teardown that cannot name its owner leaves the
	// lease held, which permanently burns the slot (fail-closed — never reused —
	// until the next boot-time reconcile).
	OwnerKind  domain.NetLeaseOwnerKind
	OwnerID    uuid.UUID
	PID        int
	SocketPath string
	TapName    string
	NetIndex   int
	GuestIP    string
	HostPort   int
	Port       int
	RootfsPath string
}

// ─────────────────────────────────────────────────────────────────────
// Fail-loud booter (non-firecracker executor / unreconciled net plane).
// ─────────────────────────────────────────────────────────────────────

// FailLoudImageBooter refuses to BOOT. It is wired when the executor is not
// firecracker (no KVM — the image-boot path must never be silently faked) and when
// the host's microVM addressing plane could not be reconciled at startup (a host
// that cannot prove which addresses are held must not hand any out).
//
// Teardown is the deliberate asymmetry. When Teardown is set, Decommission is
// DELEGATED to the real booter: refusing to boot protects data, whereas refusing
// to tear down protects nothing and strands a customer's resource — a running VM,
// its /30, its lease, its rootfs — with no way to release it. Off a firecracker
// host there is nothing real to delegate to, so Teardown stays nil there and
// Decommission refuses like everything else.
type FailLoudImageBooter struct {
	// Reason is the error every boot fails with. Nil means
	// domain.ErrImageBootUnavailable (the no-KVM case), so the zero value keeps
	// its original meaning.
	Reason error
	// Teardown is the real booter this shim delegates Decommission to, or nil.
	Teardown ImageBooter
}

var _ ImageBooter = FailLoudImageBooter{}

// reason resolves the refusal error.
func (b FailLoudImageBooter) reason() error {
	if b.Reason != nil {
		return b.Reason
	}
	return domain.ErrImageBootUnavailable
}

func (b FailLoudImageBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, b.reason()
}
func (b FailLoudImageBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, b.reason()
}
func (b FailLoudImageBooter) Decommission(ctx context.Context, in ImageDecommissionInput) error {
	if b.Teardown != nil {
		return b.Teardown.Decommission(ctx, in)
	}
	return b.reason()
}

// ─────────────────────────────────────────────────────────────────────
// Net-plane-guarded booter (the refusal that clears itself).
// ─────────────────────────────────────────────────────────────────────

// NetPlaneVerifier re-derives whether this host's microVM addressing plane can
// safely hand out an address. Nil error means it can.
type NetPlaneVerifier func(ctx context.Context) error

// NetPlaneGuardedImageBooter refuses BOOTS while the host's addressing plane
// cannot be proven — and re-derives that verdict ON EVERY BOOT.
//
// ⚠ WHY IT IS NOT FailLoudImageBooter WITH A STORED REASON. The plane's verdict
// used to be computed once at startup and frozen into the booter seam, so a host
// that hit a transient violation kept refusing every boot after the cause was gone
// and only `systemctl restart` cleared it (observed live on the second fleet host:
// 10+ minutes of refusals citing a replica id that no longer existed in any table).
// Fail-closed is right; LATCHING is not — it turns a self-healing refusal into a
// stuck host. A stored "degraded" flag would be worse still, because nothing in
// this service ever clears one. So the verdict is DERIVED per boot and never kept.
//
// Teardown keeps FailLoudImageBooter's asymmetry: it is DELEGATED unconditionally,
// never gated on the verdict. Refusing to boot protects customer data; refusing to
// tear down protects nothing and strands a running VM with its /30, its lease and
// its rootfs — and teardown is also how the operator removes the very row a
// refusal is about.
type NetPlaneGuardedImageBooter struct {
	// Real is the booter every permitted call is delegated to.
	Real ImageBooter
	// Verify re-derives the verdict. A nil Verify refuses every boot: a guard with
	// no check is a wiring mistake, and the permissive reading of one is how
	// fail-closed code silently becomes fail-open.
	Verify NetPlaneVerifier
}

var _ ImageBooter = NetPlaneGuardedImageBooter{}

// admit re-derives the verdict and reports it on the gauge, so the self-heal is
// watchable rather than merely claimed.
func (b NetPlaneGuardedImageBooter) admit(ctx context.Context) error {
	if b.Verify == nil || b.Real == nil {
		PublishNetPlaneReconciled(false)
		return fmt.Errorf("%w: this host's addressing plane has no verifier wired, so it can prove nothing about which addresses it holds",
			domain.ErrNetPlaneUnreconciled)
	}
	err := b.Verify(ctx)
	PublishNetPlaneReconciled(err == nil)
	if err != nil {
		logger.FromContext(ctx).Error("fleet net plane: refusing a boot — the plane cannot be proven right now", "err", err)
	}
	return err
}

func (b NetPlaneGuardedImageBooter) BootTest(ctx context.Context, in ImageBootInput) (ImageTestResult, error) {
	if err := b.admit(ctx); err != nil {
		return ImageTestResult{}, err
	}
	return b.Real.BootTest(ctx, in)
}

func (b NetPlaneGuardedImageBooter) BootResident(ctx context.Context, in ImageBootInput) (ImageResidentResult, error) {
	if err := b.admit(ctx); err != nil {
		return ImageResidentResult{}, err
	}
	return b.Real.BootResident(ctx, in)
}

func (b NetPlaneGuardedImageBooter) Decommission(ctx context.Context, in ImageDecommissionInput) error {
	if b.Real == nil {
		return domain.ErrImageBootUnavailable
	}
	return b.Real.Decommission(ctx, in)
}

// ─────────────────────────────────────────────────────────────────────
// FleetProvision use case.
// ─────────────────────────────────────────────────────────────────────

// stdoutTailBytes bounds the stdout/stderr tail stored + returned (spec: <=8KB).
const stdoutTailBytes = 8 * 1024

// selfTestSecretName / selfTestSecretValue are the NON-SECRET marker injected by
// the gated self-test (APP_FLEET_SECRET_SELFTEST) so a secret-ref-less deploy
// exercises the full host->guest vsock path end-to-end without any real secret
// resolution. Orthogonal to the real ErrSecretsNotSupported gate; removed/gated
// at Phase 3.4 when real secret resolution lands.
const (
	selfTestSecretName  = "__selftest__"
	selfTestSecretValue = "FLEET-VSOCK-SELFTEST-OK"
)

// FleetProvisionInput is the wire-agnostic provision request.
type FleetProvisionInput struct {
	ComponentID   string
	Env           string
	OwnerOrg      string
	Registry      string
	Repository    string
	Digest        string
	ChangeID      string
	VCPU          int
	MemoryMB      int
	EnvVars       map[string]string
	SecretRefs    []string
	Port          int
	WorkloadClass string
	// SystemID binds the workload to a P21 fleet network (CP4.5 §9 #5, D-164). It
	// is the opaque scope key delivery resolves from catalog. Non-empty requires an
	// ACTIVE network for (SystemID, Env) on a host that can prove its enforcement
	// posture — otherwise the provision is REFUSED, never auto-networked. Empty
	// means no membership: the workload reaches no fleet peer, which is exactly the
	// pre-#5 behavior, so back-compat and fail-closed are the same path.
	SystemID       string
	TestCommand    string
	TimeoutSeconds int64
	Volumes        []VolumeSpecInput
	// JobCommand is the job class's argv-exact entrypoint override (empty → the
	// image's own entrypoint). Carried as a list end-to-end and never joined.
	JobCommand []string
	// IdempotencyKey makes a job at-most-once: a duplicate returns the existing
	// handle instead of starting a second run. Scoped to OwnerOrg (I28).
	IdempotencyKey string
	// EgressAllow is the job's boot-time network egress allowlist.
	EgressAllow []string
	// Scale-to-zero desired state (rt#11, D-082). 0-defaults preserve today's
	// behavior: ScaleToZero=false, MinReplicas=0, MaxReplicas defaults to 1.
	ScaleToZero    bool
	IdleTTLSeconds int
	MinReplicas    int
	MaxReplicas    int
	// VaultToken is the per-deployment Vault secret-broker token delivery minted
	// and handed to the fleet (D-125, resident class only). It is carried
	// MEMORY-ONLY: stashed in the in-memory FleetSecretTokenStore keyed by app,
	// used by the HandedTokenEnvelopeResolver at boot, renewed for the deployment
	// lifetime, and revoked on Decommission. It is NEVER written to the fleet_apps
	// row, rootfs, runtime.json, or any log.
	VaultToken string
	// RegistryPullToken is the per-deployment, org-scoped OCI registry PULL token
	// delivery minted and handed to the fleet (D-124, fleet class only). It is
	// carried MEMORY-ONLY: on the orchestrator path it is stashed in the in-memory
	// FleetRegistryTokenStore keyed by app (the reconciler-driven materialize reads
	// it there, since the reconciler reloads the app from the DB and the token is
	// never a column); on the fallback/test path it flows straight into the
	// materialize input. The materializer presents it as the registry Basic password
	// when pulling the image, else falls back to the shared service key (back-compat).
	// It is NEVER written to the fleet_apps row, rootfs, runtime.json, or any log.
	RegistryPullToken string
	// ResourceClass names the P19 data-resource class this app IS (today only
	// "postgres") when the descriptor comes from the resource control plane
	// (fleet_resource_provision.go dedicatedDescriptor). Empty means an ordinary
	// application workload.
	//
	// It is INTERNAL by construction: no proto field feeds it, so the P7 descriptor
	// delivery sends can neither set nor clear it. It exists because a data engine
	// must not be given an HTTP ingress route — Postgres speaks a different protocol
	// on the wire, so the route would only publish a public hostname, a gateway
	// certificate and a wake key for a customer's database that could never serve a
	// single request through it. It is not persisted: it is consulted at provision,
	// which is the only moment a route is created.
	ResourceClass string
}

// FleetProvisionOutput is the provision result.
type FleetProvisionOutput struct {
	Handle string
	URL    string
}

// FleetHealthOutput is the health of a workload.
type FleetHealthOutput struct {
	State      string
	Healthy    bool
	ExitCode   int
	Message    string
	StdoutTail string
	StderrTail string
	URL        string
}

// FleetProvision provisions image-boot workloads (runtime-fleet CP3). Test-class
// runs execute asynchronously so Provision returns the handle immediately;
// resident-class boots synchronously so the URL is known on return.
type FleetProvision struct {
	repo         repository.ImageWorkloadRepository
	materializer ImageMaterializer
	booter       ImageBooter
	workDir      string
	advertise    string

	// orchestrator is the CP4 durable app→replicas path for the resident class.
	// When nil (non-firecracker / unwired builds) resident provisioning falls
	// back to the CP3 single-workload runResident path so behavior is preserved.
	orchestrator *FleetOrchestrator

	// secretSelfTest, when set (APP_FLEET_SECRET_SELFTEST), injects a NON-SECRET
	// marker over the vsock secret channel on secret-ref-less provisions so the
	// I32 mechanism is verifiable without any real secret. Off by default →
	// behavior-neutral (no /vsock, no push, guest skips the receive).
	secretSelfTest bool

	// resolver turns a job's secret_refs into concrete values at boot, scoped to
	// the job's owner org (I28) and decrypting under the handed per-deployment
	// Vault token (D-125). This is the SAME P14 resolver the resident class uses
	// (see fleet_replica_runtime.go). Nil where Vault could not be reached at
	// boot: a secret-less job still runs, a secret-bearing one fails closed.
	resolver secret.Resolver

	// jobCancels holds the cancel func of every in-flight job run, keyed by
	// handle, so Decommission can cancel a RUNNING job (kill + remove). Entries
	// are removed by the run goroutine when it finishes, so a completed job's
	// Decommission is a plain no-op.
	jobCancels sync.Map // uuid.UUID → context.CancelFunc

	baseCtx context.Context
	wg      sync.WaitGroup
}

// SetSecretResolver wires the per-tenant secret resolver (P14) into the job
// path. Nil is valid — a host that could not reach Vault only runs secret-less
// jobs; a secret-bearing one fails closed at resolve time, never silently
// secret-less. Mirrors FleetReplicaRuntime.SetSecretResolver.
func (uc *FleetProvision) SetSecretResolver(r secret.Resolver) { uc.resolver = r }

// SetSecretSelfTest enables the gated vsock self-test marker injection (Phase
// 3.3 verification only). Wired from APP_FLEET_SECRET_SELFTEST in the container.
func (uc *FleetProvision) SetSecretSelfTest(on bool) { uc.secretSelfTest = on }

// selfTestSecrets returns the injected marker secret when the self-test flag is
// on AND the provision carries no real secret_refs (the real gate still rejects
// those upstream). It returns nil otherwise, keeping the default path untouched.
func (uc *FleetProvision) selfTestSecrets(in FleetProvisionInput) []HostSecret {
	if !uc.secretSelfTest || len(in.SecretRefs) > 0 {
		return nil
	}
	return []HostSecret{{Name: selfTestSecretName, Val: selfTestSecretValue}}
}

// SetOrchestrator wires the CP4 reconciler-backed app model. Resident-class
// provision/health/scale/decommission then route through it; the test class is
// unaffected.
func (uc *FleetProvision) SetOrchestrator(orch *FleetOrchestrator) { uc.orchestrator = orch }

// NewFleetProvision constructs the use case. baseCtx is the service root context
// used for detached test-run goroutines (they must outlive the Provision RPC).
func NewFleetProvision(
	baseCtx context.Context,
	repo repository.ImageWorkloadRepository,
	materializer ImageMaterializer,
	booter ImageBooter,
	workDir, advertiseHost string,
) *FleetProvision {
	return &FleetProvision{
		repo:         repo,
		materializer: materializer,
		booter:       booter,
		workDir:      workDir,
		advertise:    advertiseHost,
		baseCtx:      baseCtx,
	}
}

// Wait blocks until in-flight detached test runs finish (called on shutdown).
func (uc *FleetProvision) Wait() { uc.wg.Wait() }

// Provision validates the descriptor, persists a booting workload, and boots it.
func (uc *FleetProvision) Provision(ctx context.Context, in FleetProvisionInput) (FleetProvisionOutput, error) {
	class := domain.ImageWorkloadClass(in.WorkloadClass)
	if !class.IsValid() {
		return FleetProvisionOutput{}, domain.ErrUnsupportedClass
	}
	// The resident class resolves + delivers secret_refs per boot (via the
	// orchestrator → replica runtime → resolver, invariant I32). The test class
	// has no resolver wired, so it rejects secret_refs rather than silently
	// booting a workload without the secrets it declared.
	if class == domain.ImageWorkloadClassTest && len(in.SecretRefs) > 0 {
		return FleetProvisionOutput{}, domain.ErrSecretsNotSupported
	}
	// Neither one-shot class has a durable data path, so both reject volumes
	// rather than silently dropping them (mirrors secret_refs).
	if class.IsOneShot() && len(in.Volumes) > 0 {
		return FleetProvisionOutput{}, domain.ErrVolumesNotSupported
	}
	// Job-class field discipline. Each of these is REJECTED rather than ignored,
	// matching how the test class treats secret_refs/volumes: silently dropping a
	// caller's intent on a data-mutating one-shot is exactly the failure mode the
	// job class exists to prevent.
	if class == domain.ImageWorkloadClassJob && in.TestCommand != "" {
		return FleetProvisionOutput{}, domain.ErrTestCommandNotSupported
	}
	if class != domain.ImageWorkloadClassJob && len(in.JobCommand) > 0 {
		return FleetProvisionOutput{}, domain.ErrJobCommandNotSupported
	}
	if class != domain.ImageWorkloadClassJob && in.IdempotencyKey != "" {
		return FleetProvisionOutput{}, domain.ErrIdempotencyKeyNotSupported
	}
	// A key with no attested org has no tenant scope to be unique within, and the
	// index is (owner_org, idempotency_key) — fail closed rather than let a key
	// collide or resolve across tenants (I28).
	if class == domain.ImageWorkloadClassJob && in.IdempotencyKey != "" && in.OwnerOrg == "" {
		return FleetProvisionOutput{}, domain.ErrIdempotencyOwnerOrgMissing
	}
	// CP4.5 §9 #5 — egress_allow is for EXTERNAL destinations only. An entry naming
	// the fleet's own subnet (or a supernet of it) is rejected at the seam: inter-VM
	// reach is governed by network policies alone. This is the EXPLICIT half of a
	// two-layer guard; the structural half is the chain topology (SNT-XVM is
	// terminal for inter-VM flows and evaluated before SNT-EGRESS), which holds
	// even if this check is bypassed.
	if err := ValidateEgressAllow(in.EgressAllow); err != nil {
		return FleetProvisionOutput{}, err
	}
	if in.Registry == "" || in.Repository == "" || in.Digest == "" {
		return FleetProvisionOutput{}, domain.ErrImageRefIncomplete
	}
	if class == domain.ImageWorkloadClassResident && in.Port <= 0 {
		return FleetProvisionOutput{}, domain.ErrResidentPortRequired
	}

	if class == domain.ImageWorkloadClassJob {
		return uc.provisionJob(ctx, in)
	}

	now := time.Now().UTC()
	wl := &domain.ImageWorkload{
		ID:              uuid.New(),
		ComponentID:     in.ComponentID,
		Env:             in.Env,
		ImageRepository: in.Repository,
		ImageDigest:     in.Digest,
		Class:           class,
		State:           domain.ImageWorkloadStateBooting,
		Port:            in.Port,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := uc.repo.Create(ctx, wl); err != nil {
		return FleetProvisionOutput{}, fmt.Errorf("persist workload: %w", err)
	}

	secrets := uc.selfTestSecrets(in)
	var nonce string
	if len(secrets) > 0 {
		var nerr error
		if nonce, nerr = newBootstrapNonce(); nerr != nil {
			return FleetProvisionOutput{}, nerr
		}
	}
	matIn := ImageMaterializeInput{
		Registry:       in.Registry,
		Repository:     in.Repository,
		Digest:         in.Digest,
		ChangeID:       in.ChangeID,
		WorkDir:        filepath.Join(uc.workDir, wl.ID.String()),
		EnvVars:        in.EnvVars,
		Mode:           string(class),
		TestCommand:    in.TestCommand,
		Port:           in.Port,
		ExpectSecrets:  len(secrets) > 0,
		BootstrapNonce: nonce,
		// D-124: the fallback/test materialize path takes the pull token straight
		// from the provision input (the orchestrator resident path uses the token
		// store instead — see runResident-vs-orchestrator below). Empty on the test
		// leg → the materializer falls back to the shared service key (back-compat).
		RegistryPullToken: in.RegistryPullToken,
	}

	if class == domain.ImageWorkloadClassTest {
		uc.startTestRun(wl, matIn, in.VCPU, in.MemoryMB, int(in.TimeoutSeconds), secrets)
		return FleetProvisionOutput{Handle: wl.ID.String()}, nil
	}

	// Resident: the CP4 orchestrator owns a durable app→replicas model driven by
	// the reconciler. The CP3 single-workload row persisted above is not used for
	// the resident class when the orchestrator is wired — drop it.
	if uc.orchestrator != nil {
		if delErr := uc.repo.Delete(ctx, wl.ID); delErr != nil {
			logger.FromContext(ctx).Warn("fleet: drop placeholder workload row", "workload_id", wl.ID, "err", delErr)
		}
		handle, url, err := uc.orchestrator.ProvisionApp(ctx, in)
		if err != nil {
			return FleetProvisionOutput{}, err
		}
		return FleetProvisionOutput{Handle: handle, URL: url}, nil
	}

	// Fallback (no orchestrator wired): materialize + boot one workload
	// synchronously so the URL is known on return.
	url, err := uc.runResident(ctx, wl, matIn, in.VCPU, in.MemoryMB, secrets)
	if err != nil {
		uc.markFailed(ctx, wl, err)
		return FleetProvisionOutput{}, err
	}
	return FleetProvisionOutput{Handle: wl.ID.String(), URL: url}, nil
}

// provisionJob starts a one-shot job: the test class's boot path plus resolved
// secrets, an egress allowlist, and at-most-once idempotency. Provision returns
// the handle immediately; the run itself is detached (like the test class), so a
// long migration never blocks the RPC.
//
// Idempotency is enforced by the DB, not by a check-then-act: a pre-check races
// (two callers both see "no run" and both boot), and a migration that runs twice
// can destroy data. The (owner_org, idempotency_key) unique index is the actual
// guarantee — the pre-check below is only a fast path, and the duplicate-key
// branch is what makes a concurrent duplicate safe.
func (uc *FleetProvision) provisionJob(ctx context.Context, in FleetProvisionInput) (FleetProvisionOutput, error) {
	if in.IdempotencyKey != "" {
		existing, err := uc.repo.FindByIdempotencyKey(ctx, in.OwnerOrg, in.IdempotencyKey)
		if err == nil {
			logger.FromContext(ctx).Info("fleet job: idempotency key already ran — returning existing handle",
				"workload_id", existing.ID, "component_id", in.ComponentID)
			return FleetProvisionOutput{Handle: existing.ID.String()}, nil
		}
		if !errors.Is(err, domain.ErrWorkloadNotFound) {
			return FleetProvisionOutput{}, fmt.Errorf("lookup idempotency key: %w", err)
		}
	}

	now := time.Now().UTC()
	wl := &domain.ImageWorkload{
		ID:              uuid.New(),
		ComponentID:     in.ComponentID,
		Env:             in.Env,
		OwnerOrg:        in.OwnerOrg,
		ImageRepository: in.Repository,
		ImageDigest:     in.Digest,
		Class:           domain.ImageWorkloadClassJob,
		State:           domain.ImageWorkloadStateBooting,
		JobCommand:      in.JobCommand,
		EgressAllow:     in.EgressAllow,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if in.IdempotencyKey != "" {
		key := in.IdempotencyKey
		wl.IdempotencyKey = &key
	}

	if err := uc.repo.Create(ctx, wl); err != nil {
		// Lost the race: a concurrent Provision inserted this exact
		// (owner_org, idempotency_key) first. Return THEIR handle — never boot a
		// second VM for a key that is already running.
		if in.IdempotencyKey != "" && uc.repo.IsDuplicateKey(err) {
			existing, ferr := uc.repo.FindByIdempotencyKey(ctx, in.OwnerOrg, in.IdempotencyKey)
			if ferr != nil {
				return FleetProvisionOutput{}, fmt.Errorf("resolve raced idempotency key: %w", ferr)
			}
			logger.FromContext(ctx).Info("fleet job: idempotency key raced — returning existing handle",
				"workload_id", existing.ID, "component_id", in.ComponentID)
			return FleetProvisionOutput{Handle: existing.ID.String()}, nil
		}
		return FleetProvisionOutput{}, fmt.Errorf("persist job workload: %w", err)
	}

	// Resolve secrets through the SAME P14 boot path the resident class uses. A
	// job legitimately needs them (a migrator needs its DSN), unlike the test
	// class. Fails closed: the job never runs with missing/partial secrets (I32).
	// Values live only in this slice and the vsock push — never a row, log, or argv.
	var secrets []HostSecret
	if len(in.SecretRefs) > 0 {
		resolved, err := resolveBootSecrets(ctx, uc.resolver, in.SecretRefs, in.OwnerOrg, in.VaultToken)
		if err != nil {
			uc.markFailed(ctx, wl, fmt.Errorf("resolve secrets: %w", err))
			return FleetProvisionOutput{}, err
		}
		secrets = resolved
	} else {
		secrets = uc.selfTestSecrets(in)
	}

	var nonce string
	if len(secrets) > 0 {
		var nerr error
		if nonce, nerr = newBootstrapNonce(); nerr != nil {
			uc.markFailed(ctx, wl, nerr)
			return FleetProvisionOutput{}, nerr
		}
	}

	matIn := ImageMaterializeInput{
		Registry:          in.Registry,
		Repository:        in.Repository,
		Digest:            in.Digest,
		ChangeID:          in.ChangeID,
		WorkDir:           filepath.Join(uc.workDir, wl.ID.String()),
		EnvVars:           in.EnvVars,
		Mode:              string(domain.ImageWorkloadClassJob),
		JobCommand:        in.JobCommand,
		ExpectSecrets:     len(secrets) > 0,
		BootstrapNonce:    nonce,
		RegistryPullToken: in.RegistryPullToken,
	}
	uc.startJobRun(wl, matIn, in.VCPU, in.MemoryMB, int(in.TimeoutSeconds), secrets, in.EgressAllow)
	return FleetProvisionOutput{Handle: wl.ID.String()}, nil
}

// startJobRun launches the detached job boot and registers its cancel func so
// Decommission can kill a running job. Provision has already returned the handle
// by the time this runs.
func (uc *FleetProvision) startJobRun(wl *domain.ImageWorkload, matIn ImageMaterializeInput, vcpu, memMB, timeoutSec int, secrets []HostSecret, egressAllow []string) {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	// The run context outlives the Provision RPC. Its deadline covers materialize
	// (image pull) + the run; the booter separately enforces timeoutSec as the
	// real substrate deadline on the VM itself (it kills the VM at that mark), so
	// this outer budget only bounds the surrounding host work.
	ctx, cancel := context.WithTimeout(uc.baseCtx, time.Duration(timeoutSec)*time.Second+10*time.Minute)
	uc.jobCancels.Store(wl.ID, cancel)

	uc.wg.Add(1)
	go func() {
		defer uc.wg.Done()
		defer cancel()
		defer uc.jobCancels.Delete(wl.ID)
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(uc.baseCtx).Error("fleet job run panicked", "workload_id", wl.ID, "panic", r)
			}
		}()

		mat, err := uc.materializer.Materialize(ctx, matIn)
		if err != nil {
			uc.finishCancelledOrFailed(ctx, wl, fmt.Errorf("materialize: %w", err))
			return
		}
		wl.RootfsPath = mat.RootfsPath
		wl.State = domain.ImageWorkloadStateRunning
		wl.UpdatedAt = time.Now().UTC()
		_ = uc.repo.Update(ctx, wl)

		res, err := uc.booter.BootTest(ctx, ImageBootInput{
			WorkloadID:     wl.ID,
			OwnerKind:      domain.NetLeaseOwnerWorkload,
			RootfsPath:     mat.RootfsPath,
			VCPU:           vcpu,
			MemoryMB:       memMB,
			TimeoutSeconds: timeoutSec,
			ExpectSecrets:  matIn.ExpectSecrets,
			Secrets:        secrets,
			BootstrapNonce: matIn.BootstrapNonce,
			EgressAllow:    egressAllow,
		})
		if err != nil {
			uc.finishCancelledOrFailed(ctx, wl, fmt.Errorf("boot job: %w", err))
			return
		}

		code := res.ExitCode
		wl.ExitCode = &code
		wl.StdoutTail = tail(res.Stdout, stdoutTailBytes)
		wl.StderrTail = tail(res.Stderr, stdoutTailBytes)
		wl.State = domain.ImageWorkloadStateExited
		if res.TimedOut {
			wl.Message = "job timed out"
		}
		wl.UpdatedAt = time.Now().UTC()
		// The run context is cancelled/expired at this point only in the cancel
		// path (handled above), so persist the terminal result on the base context
		// — a job whose result is lost is a job that "never ran" to the caller.
		if err := uc.repo.Update(uc.baseCtx, wl); err != nil {
			logger.FromContext(ctx).Error("fleet job: persist result failed", "workload_id", wl.ID, "err", err)
		}
	}()
}

// jobCancelledExitCode is the exit code recorded for a job cancelled via
// Decommission. It is deliberately NON-ZERO (128+SIGKILL, the standard
// convention): a cancelled migration must never report exit_code 0, which the
// caller reads as "the job succeeded".
const jobCancelledExitCode = 137

// finishCancelledOrFailed records a job's terminal state. A run killed by
// Decommission lands as exited/137 (terminal + unsuccessful) rather than
// "failed", since cancellation is an operator action, not a fault. Everything
// else is a genuine failure.
func (uc *FleetProvision) finishCancelledOrFailed(ctx context.Context, wl *domain.ImageWorkload, cause error) {
	if !errors.Is(ctx.Err(), context.Canceled) {
		uc.markFailed(ctx, wl, cause)
		return
	}
	code := jobCancelledExitCode
	wl.ExitCode = &code
	wl.State = domain.ImageWorkloadStateExited
	wl.Message = "job cancelled"
	wl.UpdatedAt = time.Now().UTC()
	// ctx is cancelled — persist on the base context or the write is dropped.
	if err := uc.repo.Update(uc.baseCtx, wl); err != nil {
		logger.FromContext(uc.baseCtx).Error("fleet job: persist cancelled-state failed", "workload_id", wl.ID, "err", err)
	}
}

// OwnerOrgForHandle resolves the owning org of a fleet handle for the by-handle
// caller-org check (#fleet-handle-ops-org-check, D-083): Health/Decommission/
// Scale act on an unguessable handle, so a leaked one must not let a foreign
// caller act on another org's app. It fails closed — an unparseable or unknown
// handle returns ErrWorkloadNotFound (the same not-found shape the other methods
// yield). A handle with no owning org (a test-class workload — which stores none
// — or an app whose OwnerOrg is empty) returns uuid.Nil, mirroring Provision's
// empty-owner_org pass-through so the caller skips the org gate.
func (uc *FleetProvision) OwnerOrgForHandle(ctx context.Context, handle string) (uuid.UUID, error) {
	id, err := uuid.Parse(handle)
	if err != nil {
		return uuid.Nil, domain.ErrWorkloadNotFound
	}
	if uc.orchestrator != nil {
		org, isApp, oerr := uc.orchestrator.OwnerOrgForApp(ctx, id)
		if oerr != nil {
			return uuid.Nil, oerr
		}
		if isApp {
			return parseOwnerOrg(org)
		}
	}
	// Not a known app: the handle must be a job / test-class / fallback workload.
	// Confirm it exists (fail closed on an unknown handle). A JOB stores its
	// attested owner org, so the by-handle gate applies to it exactly as it does
	// to an app — a leaked job handle must not let a foreign caller read another
	// org's job output or cancel its migration. A test-class row carries no owner
	// org and stays org-less (uuid.Nil → gate skipped, as Provision does).
	wl, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return uuid.Nil, err
	}
	return parseOwnerOrg(wl.OwnerOrg)
}

// parseOwnerOrg maps a stored owner-org string to a uuid. Empty → uuid.Nil (no
// org to gate on). A non-empty value is a uuid validated at provision time, so a
// parse failure here is corrupt state and fails closed with an error.
func parseOwnerOrg(org string) (uuid.UUID, error) {
	if org == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(org)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse owner org: %w", err)
	}
	return id, nil
}

// Scale sets the desired replica count for a resident app (routed to the CP4
// orchestrator). A handle that is not a known app returns ErrWorkloadNotFound.
func (uc *FleetProvision) Scale(ctx context.Context, handle string, replicas int) error {
	id, err := uuid.Parse(handle)
	if err != nil {
		return domain.ErrWorkloadNotFound
	}
	// A one-shot job has no replica count to set — reject rather than no-op, so a
	// caller scaling a job learns it asked the wrong question. Checked before the
	// orchestrator lookup: a job is never an app, and the orchestrator would
	// otherwise report it simply not-found.
	if wl, ferr := uc.repo.FindByID(ctx, id); ferr == nil && wl.Class == domain.ImageWorkloadClassJob {
		return domain.ErrScaleNotSupported
	}
	if uc.orchestrator == nil {
		return domain.ErrWorkloadNotFound
	}
	isApp, serr := uc.orchestrator.ScaleApp(ctx, id, replicas)
	if serr != nil {
		return serr
	}
	if !isApp {
		return domain.ErrWorkloadNotFound
	}
	return nil
}

// startTestRun launches the detached test boot. Provision has already returned
// the handle by the time this runs.
func (uc *FleetProvision) startTestRun(wl *domain.ImageWorkload, matIn ImageMaterializeInput, vcpu, memMB, timeoutSec int, secrets []HostSecret) {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	uc.wg.Add(1)
	go func() {
		defer uc.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(uc.baseCtx).Error("fleet test run panicked", "workload_id", wl.ID, "panic", r)
			}
		}()

		ctx, cancel := context.WithTimeout(uc.baseCtx, time.Duration(timeoutSec)*time.Second+10*time.Minute)
		defer cancel()

		mat, err := uc.materializer.Materialize(ctx, matIn)
		if err != nil {
			uc.markFailed(ctx, wl, fmt.Errorf("materialize: %w", err))
			return
		}
		wl.RootfsPath = mat.RootfsPath
		wl.State = domain.ImageWorkloadStateRunning
		wl.UpdatedAt = time.Now().UTC()
		_ = uc.repo.Update(ctx, wl)

		res, err := uc.booter.BootTest(ctx, ImageBootInput{
			WorkloadID:     wl.ID,
			OwnerKind:      domain.NetLeaseOwnerWorkload,
			RootfsPath:     mat.RootfsPath,
			VCPU:           vcpu,
			MemoryMB:       memMB,
			TimeoutSeconds: timeoutSec,
			ExpectSecrets:  matIn.ExpectSecrets,
			Secrets:        secrets,
			BootstrapNonce: matIn.BootstrapNonce,
		})
		if err != nil {
			uc.markFailed(ctx, wl, fmt.Errorf("boot test: %w", err))
			return
		}

		code := res.ExitCode
		wl.ExitCode = &code
		wl.StdoutTail = tail(res.Stdout, stdoutTailBytes)
		wl.StderrTail = tail(res.Stderr, stdoutTailBytes)
		wl.State = domain.ImageWorkloadStateExited
		if res.TimedOut {
			wl.Message = "test run timed out"
		}
		wl.UpdatedAt = time.Now().UTC()
		if err := uc.repo.Update(ctx, wl); err != nil {
			logger.FromContext(ctx).Error("fleet test run: persist result failed", "workload_id", wl.ID, "err", err)
		}
	}()
}

// runResident materializes + boots a resident workload synchronously.
func (uc *FleetProvision) runResident(ctx context.Context, wl *domain.ImageWorkload, matIn ImageMaterializeInput, vcpu, memMB int, secrets []HostSecret) (string, error) {
	mat, err := uc.materializer.Materialize(ctx, matIn)
	if err != nil {
		return "", fmt.Errorf("materialize: %w", err)
	}
	wl.RootfsPath = mat.RootfsPath
	wl.UpdatedAt = time.Now().UTC()
	_ = uc.repo.Update(ctx, wl)

	res, err := uc.booter.BootResident(ctx, ImageBootInput{
		WorkloadID:     wl.ID,
		OwnerKind:      domain.NetLeaseOwnerWorkload,
		RootfsPath:     mat.RootfsPath,
		VCPU:           vcpu,
		MemoryMB:       memMB,
		Port:           wl.Port,
		ExpectSecrets:  matIn.ExpectSecrets,
		Secrets:        secrets,
		BootstrapNonce: matIn.BootstrapNonce,
	})
	if err != nil {
		return "", fmt.Errorf("boot resident: %w", err)
	}

	pid := res.PID
	url := fmt.Sprintf("http://%s:%d", uc.advertise, res.HostPort)
	wl.PID = &pid
	wl.GuestIP = res.GuestIP
	wl.HostPort = res.HostPort
	wl.NetIndex = res.NetIndex
	wl.TapName = res.TapName
	wl.SocketPath = res.SocketPath
	wl.URL = url
	wl.State = domain.ImageWorkloadStateRunning
	wl.UpdatedAt = time.Now().UTC()
	if err := uc.repo.Update(ctx, wl); err != nil {
		// The VM is up; decommission it so we don't leak an untracked workload.
		_ = uc.booter.Decommission(ctx, decommissionInput(wl))
		return "", fmt.Errorf("persist resident workload: %w", err)
	}
	return url, nil
}

// Health returns the current health of a workload. Resident health is a live
// TCP dial of the guest port; test health is exited && exit_code==0.
func (uc *FleetProvision) Health(ctx context.Context, handle string) (FleetHealthOutput, error) {
	id, err := uuid.Parse(handle)
	if err != nil {
		return FleetHealthOutput{}, domain.ErrWorkloadNotFound
	}
	if uc.orchestrator != nil {
		out, isApp, herr := uc.orchestrator.HealthApp(ctx, id)
		if herr != nil {
			return FleetHealthOutput{}, herr
		}
		if isApp {
			return out, nil
		}
	}
	wl, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return FleetHealthOutput{}, err
	}

	out := FleetHealthOutput{
		State:      string(wl.State),
		Message:    wl.Message,
		StdoutTail: wl.StdoutTail,
		StderrTail: wl.StderrTail,
		URL:        wl.URL,
	}
	if wl.ExitCode != nil {
		out.ExitCode = *wl.ExitCode
	}

	switch wl.Class {
	case domain.ImageWorkloadClassResident:
		if wl.State == domain.ImageWorkloadStateRunning && wl.PID != nil && !processAlive(*wl.PID) {
			// The VM process died after boot — surface it instead of a stale
			// "running". The console tail is the only crash evidence.
			wl.State = domain.ImageWorkloadStateFailed
			wl.Message = "vm process exited"
			if wl.RootfsPath != "" {
				if tail := fileTail(filepath.Join(filepath.Dir(wl.RootfsPath), "console.log"), 8192); tail != "" {
					wl.StderrTail = tail
				}
			}
			// Tear down the leaked net plumbing (TAP/DNAT/port/index) — a dead
			// resident otherwise blocks its index for every later workload.
			deadWl := *wl
			deadWl.PID = nil // process already gone; skip the kill path
			if err := uc.booter.Decommission(ctx, decommissionInput(&deadWl)); err != nil {
				logger.FromContext(ctx).Warn("fleet: dead-resident teardown", "workload_id", wl.ID, "err", err)
			}
			if err := uc.repo.Update(ctx, wl); err != nil {
				logger.FromContext(ctx).Warn("fleet: persist dead-resident state", "err", err)
			}
			out.State = string(wl.State)
			out.Message = wl.Message
			out.StderrTail = wl.StderrTail
			return out, nil
		}
		if wl.State == domain.ImageWorkloadStateRunning && wl.GuestIP != "" && wl.Port > 0 {
			out.Healthy = dialTCP(wl.GuestIP, wl.Port)
		}
	case domain.ImageWorkloadClassTest, domain.ImageWorkloadClassJob:
		// One-shot observation contract (identical for both classes, per the
		// frozen Health shape): terminal == exited, success == exit_code 0.
		out.Healthy = wl.State == domain.ImageWorkloadStateExited && wl.ExitCode != nil && *wl.ExitCode == 0
	}
	return out, nil
}

// Decommission tears down a workload and marks it exited.
func (uc *FleetProvision) Decommission(ctx context.Context, handle string) error {
	id, err := uuid.Parse(handle)
	if err != nil {
		return domain.ErrWorkloadNotFound
	}
	if uc.orchestrator != nil {
		isApp, derr := uc.orchestrator.DecommissionApp(ctx, id)
		if derr != nil {
			return derr
		}
		if isApp {
			return nil
		}
	}
	wl, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return err
	}
	// A job is cancelled, not torn down: cancelling its run context makes the
	// booter kill the VM and reverse its own net/rootfs plumbing. The run
	// goroutine owns the terminal state write (exited/137) — writing it here too
	// would race it. An already-finished job has no cancel entry, so this is a
	// no-op and Decommission stays idempotent.
	if wl.Class == domain.ImageWorkloadClassJob {
		if v, ok := uc.jobCancels.Load(id); ok {
			logger.FromContext(ctx).Info("fleet job: cancelling running job", "workload_id", id)
			v.(context.CancelFunc)()
		}
		return nil
	}
	if wl.Class == domain.ImageWorkloadClassResident && wl.State == domain.ImageWorkloadStateRunning {
		if err := uc.booter.Decommission(ctx, decommissionInput(wl)); err != nil && !errors.Is(err, domain.ErrImageBootUnavailable) {
			logger.FromContext(ctx).Error("fleet decommission teardown failed", "workload_id", wl.ID, "err", err)
		}
	}
	wl.State = domain.ImageWorkloadStateExited
	wl.UpdatedAt = time.Now().UTC()
	if err := uc.repo.Update(ctx, wl); err != nil {
		return fmt.Errorf("persist decommission: %w", err)
	}
	return nil
}

func (uc *FleetProvision) markFailed(ctx context.Context, wl *domain.ImageWorkload, cause error) {
	logger.FromContext(ctx).Error("fleet workload failed", "workload_id", wl.ID, "err", cause)
	wl.State = domain.ImageWorkloadStateFailed
	wl.Message = cause.Error()
	wl.UpdatedAt = time.Now().UTC()
	if err := uc.repo.Update(ctx, wl); err != nil {
		logger.FromContext(ctx).Error("fleet: persist failed-state failed", "workload_id", wl.ID, "err", err)
	}
}

func decommissionInput(wl *domain.ImageWorkload) ImageDecommissionInput {
	pid := 0
	if wl.PID != nil {
		pid = *wl.PID
	}
	return ImageDecommissionInput{
		OwnerKind:  domain.NetLeaseOwnerWorkload,
		OwnerID:    wl.ID,
		PID:        pid,
		SocketPath: wl.SocketPath,
		TapName:    wl.TapName,
		NetIndex:   wl.NetIndex,
		GuestIP:    wl.GuestIP,
		HostPort:   wl.HostPort,
		Port:       wl.Port,
		RootfsPath: wl.RootfsPath,
	}
}

// dialTCP reports whether host:port accepts a TCP connection within 2s.
func dialTCP(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// processAlive reports whether a PID still exists (signal 0 probe). Overridable
// in tests.
var processAlive = func(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// fileTail returns up to the last n bytes of the file at path ("" on any error).
func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	if st.Size() > n {
		if _, err := f.Seek(st.Size()-n, 0); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(b)
}
