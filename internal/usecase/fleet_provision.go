package usecase

import (
	"context"
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

// ImageBootInput is the common boot request.
type ImageBootInput struct {
	WorkloadID     uuid.UUID
	RootfsPath     string
	VCPU           int
	MemoryMB       int
	Port           int
	TimeoutSeconds int
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

// FleetProvisionInput is the wire-agnostic provision request.
type FleetProvisionInput struct {
	ComponentID    string
	Env            string
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

	baseCtx context.Context
	wg      sync.WaitGroup
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
	if len(in.SecretRefs) > 0 {
		return FleetProvisionOutput{}, domain.ErrSecretsNotSupported
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

	matIn := ImageMaterializeInput{
		Registry:    in.Registry,
		Repository:  in.Repository,
		Digest:      in.Digest,
		ChangeID:    in.ChangeID,
		WorkDir:     filepath.Join(uc.workDir, wl.ID.String()),
		EnvVars:     in.EnvVars,
		Mode:        string(class),
		TestCommand: in.TestCommand,
		Port:        in.Port,
	}

	if class == domain.ImageWorkloadClassTest {
		uc.startTestRun(wl, matIn, in.VCPU, in.MemoryMB, int(in.TimeoutSeconds))
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
	url, err := uc.runResident(ctx, wl, matIn, in.VCPU, in.MemoryMB)
	if err != nil {
		uc.markFailed(ctx, wl, err)
		return FleetProvisionOutput{}, err
	}
	return FleetProvisionOutput{Handle: wl.ID.String(), URL: url}, nil
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
func (uc *FleetProvision) startTestRun(wl *domain.ImageWorkload, matIn ImageMaterializeInput, vcpu, memMB, timeoutSec int) {
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
func (uc *FleetProvision) runResident(ctx context.Context, wl *domain.ImageWorkload, matIn ImageMaterializeInput, vcpu, memMB int) (string, error) {
	mat, err := uc.materializer.Materialize(ctx, matIn)
	if err != nil {
		return "", fmt.Errorf("materialize: %w", err)
	}
	wl.RootfsPath = mat.RootfsPath
	wl.UpdatedAt = time.Now().UTC()
	_ = uc.repo.Update(ctx, wl)

	res, err := uc.booter.BootResident(ctx, ImageBootInput{
		WorkloadID: wl.ID,
		RootfsPath: mat.RootfsPath,
		VCPU:       vcpu,
		MemoryMB:   memMB,
		Port:       wl.Port,
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
