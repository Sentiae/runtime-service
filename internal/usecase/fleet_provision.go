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
	Mode        string // "test" | "resident"
	TestCommand string
	Port        int
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
	WorkloadID     uuid.UUID
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
// Fail-loud booter (non-firecracker executor).
// ─────────────────────────────────────────────────────────────────────

// FailLoudImageBooter is wired when the executor is not firecracker. Every call
// fails with ErrImageBootUnavailable so the image-boot path is never silently
// faked on a host without KVM.
type FailLoudImageBooter struct{}

func (FailLoudImageBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, domain.ErrImageBootUnavailable
}
func (FailLoudImageBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, domain.ErrImageBootUnavailable
}
func (FailLoudImageBooter) Decommission(context.Context, ImageDecommissionInput) error {
	return domain.ErrImageBootUnavailable
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
	ComponentID    string
	Env            string
	OwnerOrg       string
	Registry       string
	Repository     string
	Digest         string
	ChangeID       string
	VCPU           int
	MemoryMB       int
	EnvVars        map[string]string
	SecretRefs     []string
	Port           int
	WorkloadClass  string
	TestCommand    string
	TimeoutSeconds int64
	Volumes        []VolumeSpecInput
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

	baseCtx context.Context
	wg      sync.WaitGroup
}

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
	// The test class is a single-shot ephemeral boot with no durable data path, so
	// it rejects volumes rather than silently dropping them (mirrors secret_refs).
	if class == domain.ImageWorkloadClassTest && len(in.Volumes) > 0 {
		return FleetProvisionOutput{}, domain.ErrVolumesNotSupported
	}
	if in.Registry == "" || in.Repository == "" || in.Digest == "" {
		return FleetProvisionOutput{}, domain.ErrImageRefIncomplete
	}
	if class == domain.ImageWorkloadClassResident && in.Port <= 0 {
		return FleetProvisionOutput{}, domain.ErrResidentPortRequired
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
	// Not a known app: the handle must be a test-class / fallback workload. Confirm
	// it exists (fail closed on an unknown handle) — the row carries no owner org,
	// so an existing one is org-less (uuid.Nil → gate skipped, as Provision does).
	if _, err := uc.repo.FindByID(ctx, id); err != nil {
		return uuid.Nil, err
	}
	return uuid.Nil, nil
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
	case domain.ImageWorkloadClassTest:
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
