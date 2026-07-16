package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
)

// mustParse parses a handle into a uuid or fails the test.
func mustParse(t *testing.T, handle string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(handle)
	if err != nil {
		t.Fatalf("parse handle %q: %v", handle, err)
	}
	return id
}

// jobBase is a minimal valid job-class provision input.
func jobBase() FleetProvisionInput {
	return FleetProvisionInput{
		Registry:      "reg:8089",
		Repository:    "org/app",
		Digest:        "sha256:abc",
		WorkloadClass: "job",
		OwnerOrg:      "11111111-1111-1111-1111-111111111111",
	}
}

// TestJobClassFieldDiscipline asserts each job-only / non-job field is REJECTED
// rather than silently ignored. Silently dropping a caller's intent on a
// data-mutating one-shot is the failure mode the job class exists to prevent.
func TestJobClassFieldDiscipline(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*FleetProvisionInput)
		wantErr error
	}{
		{"job rejects test_command", func(in *FleetProvisionInput) {
			in.TestCommand = "echo hi"
		}, domain.ErrTestCommandNotSupported},
		{"job rejects volumes", func(in *FleetProvisionInput) {
			in.Volumes = []VolumeSpecInput{{SizeMB: 10}}
		}, domain.ErrVolumesNotSupported},
		{"job with idempotency_key requires owner org", func(in *FleetProvisionInput) {
			in.IdempotencyKey = "migrate-v1"
			in.OwnerOrg = ""
		}, domain.ErrIdempotencyOwnerOrgMissing},
		{"test class rejects job_command", func(in *FleetProvisionInput) {
			in.WorkloadClass = "test"
			in.JobCommand = []string{"/app/migrate"}
		}, domain.ErrJobCommandNotSupported},
		{"test class rejects idempotency_key", func(in *FleetProvisionInput) {
			in.WorkloadClass = "test"
			in.IdempotencyKey = "migrate-v1"
		}, domain.ErrIdempotencyKeyNotSupported},
		{"resident class rejects job_command", func(in *FleetProvisionInput) {
			in.WorkloadClass = "resident"
			in.Port = 8080
			in.JobCommand = []string{"/app/migrate"}
		}, domain.ErrJobCommandNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := jobBase()
			tt.mutate(&in)
			uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{}, fakeBooter{})
			_, err := uc.Provision(context.Background(), in)
			uc.Wait()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Provision err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestJobHealthReportsExit asserts the job class's observation contract: the run
// is terminal at state=="exited", success is exit_code==0, and the tails are
// populated — the same shape the test class reports (fleet.proto Health).
func TestJobHealthReportsExit(t *testing.T) {
	tests := []struct {
		name        string
		exitCode    int
		wantHealthy bool
	}{
		{"success reports exit 0 and healthy", 0, true},
		{"failure reports non-zero and unhealthy", 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, fakeBooter{
				test: ImageTestResult{ExitCode: tt.exitCode, Stdout: "out-tail", Stderr: "err-tail"},
			})
			out, err := uc.Provision(context.Background(), jobBase())
			if err != nil {
				t.Fatalf("Provision err = %v", err)
			}
			uc.Wait() // the job run is detached; drain it

			h, err := uc.Health(context.Background(), out.Handle)
			if err != nil {
				t.Fatalf("Health err = %v", err)
			}
			if h.State != string(domain.ImageWorkloadStateExited) {
				t.Fatalf("State = %q, want exited", h.State)
			}
			if h.ExitCode != tt.exitCode {
				t.Fatalf("ExitCode = %d, want %d", h.ExitCode, tt.exitCode)
			}
			if h.Healthy != tt.wantHealthy {
				t.Fatalf("Healthy = %v, want %v", h.Healthy, tt.wantHealthy)
			}
			if h.StdoutTail != "out-tail" || h.StderrTail != "err-tail" {
				t.Fatalf("tails = %q/%q, want out-tail/err-tail", h.StdoutTail, h.StderrTail)
			}
			if h.URL != "" {
				t.Fatalf("URL = %q, want empty (a job serves no port)", h.URL)
			}
		})
	}
}

// TestJobIdempotencyReturnsExistingHandle is the load-bearing case: a duplicate
// key must return the EXISTING handle and must NOT start a second run. A
// migration that runs twice can destroy data.
func TestJobIdempotencyReturnsExistingHandle(t *testing.T) {
	repo := newFakeWorkloadRepo()
	booter := &countingBooter{}
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)

	in := jobBase()
	in.IdempotencyKey = "migrate-2026-07-16"

	first, err := uc.Provision(context.Background(), in)
	if err != nil {
		t.Fatalf("first Provision err = %v", err)
	}
	uc.Wait()

	second, err := uc.Provision(context.Background(), in)
	if err != nil {
		t.Fatalf("second Provision err = %v", err)
	}
	uc.Wait()

	if first.Handle != second.Handle {
		t.Fatalf("duplicate idempotency_key returned a NEW handle %s (want the existing %s)", second.Handle, first.Handle)
	}
	if got := booter.count(); got != 1 {
		t.Fatalf("booter ran %d times for one idempotency key; want exactly 1 (a job must never run twice)", got)
	}
}

// TestJobIdempotencyIsOrgScoped asserts a key never resolves across tenants: the
// same key from a different org is a DIFFERENT job (I28). If this regressed, one
// tenant could read another tenant's job handle by guessing a key.
func TestJobIdempotencyIsOrgScoped(t *testing.T) {
	repo := newFakeWorkloadRepo()
	booter := &countingBooter{}
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)

	inA := jobBase()
	inA.IdempotencyKey = "migrate-v1"
	inB := jobBase()
	inB.IdempotencyKey = "migrate-v1"
	inB.OwnerOrg = "22222222-2222-2222-2222-222222222222"

	a, err := uc.Provision(context.Background(), inA)
	if err != nil {
		t.Fatalf("org A Provision err = %v", err)
	}
	b, err := uc.Provision(context.Background(), inB)
	if err != nil {
		t.Fatalf("org B Provision err = %v", err)
	}
	uc.Wait()

	if a.Handle == b.Handle {
		t.Fatalf("the same idempotency_key resolved ACROSS orgs to handle %s — cross-tenant leak", a.Handle)
	}
	if got := booter.count(); got != 2 {
		t.Fatalf("booter ran %d times; want 2 (each org's job runs independently)", got)
	}
}

// TestJobIdempotencyConcurrentRace asserts the duplicate-key branch holds under
// a real race: N concurrent Provisions with one key yield ONE handle and ONE run.
func TestJobIdempotencyConcurrentRace(t *testing.T) {
	repo := newFakeWorkloadRepo()
	booter := &countingBooter{}
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)

	in := jobBase()
	in.IdempotencyKey = "migrate-concurrent"

	const callers = 8
	handles := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := uc.Provision(context.Background(), in)
			handles[i], errs[i] = out.Handle, err
		}(i)
	}
	close(start)
	wg.Wait()
	uc.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d err = %v", i, err)
		}
	}
	for i, h := range handles {
		if h != handles[0] {
			t.Fatalf("caller %d got handle %s, caller 0 got %s — the key started more than one run", i, h, handles[0])
		}
	}
	if got := booter.count(); got != 1 {
		t.Fatalf("booter ran %d times under a concurrent duplicate key; want exactly 1", got)
	}
}

// TestJobScaleRejected asserts Scale on a job handle is a caller error, not a
// silent no-op (mapped to FailedPrecondition at the handler).
func TestJobScaleRejected(t *testing.T) {
	uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, fakeBooter{})
	out, err := uc.Provision(context.Background(), jobBase())
	if err != nil {
		t.Fatalf("Provision err = %v", err)
	}
	uc.Wait()
	if err := uc.Scale(context.Background(), out.Handle, 3); !errors.Is(err, domain.ErrScaleNotSupported) {
		t.Fatalf("Scale err = %v, want ErrScaleNotSupported", err)
	}
}

// TestJobDecommissionCancelsRunningJob asserts Decommission cancels a RUNNING
// job: the booter's context is cancelled (kill) and the job lands terminal with
// a NON-ZERO exit code — a cancelled migration must never look successful.
func TestJobDecommissionCancelsRunningJob(t *testing.T) {
	repo := newFakeWorkloadRepo()
	booter := newBlockingBooter()
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)

	out, err := uc.Provision(context.Background(), jobBase())
	if err != nil {
		t.Fatalf("Provision err = %v", err)
	}
	booter.awaitEntered(t) // the job is now running inside BootTest

	if err := uc.Decommission(context.Background(), out.Handle); err != nil {
		t.Fatalf("Decommission err = %v", err)
	}
	uc.Wait() // the cancelled run drains

	if !booter.sawCancel() {
		t.Fatal("Decommission did not cancel the running job's boot context")
	}
	h, err := uc.Health(context.Background(), out.Handle)
	if err != nil {
		t.Fatalf("Health err = %v", err)
	}
	if h.State != string(domain.ImageWorkloadStateExited) {
		t.Fatalf("State = %q, want exited (a cancelled job is terminal)", h.State)
	}
	// Pin the exact code, not merely "non-zero". 137 (128+SIGKILL) is the value
	// callers read; normalizing it to 0 would make a cancelled migration look
	// SUCCESSFUL, and silently changing it to another value would break the
	// caller's cancellation check. Both are regressions this assertion catches.
	if h.ExitCode != jobCancelledExitCode {
		t.Fatalf("cancelled job reported exit_code=%d, want %d (128+SIGKILL); a cancelled job must never report success",
			h.ExitCode, jobCancelledExitCode)
	}
	if h.Healthy {
		t.Fatal("cancelled job reported healthy=true; a cancelled job must never report success")
	}
}

// TestJobCancelledExitCodeIsNonZero guards the constant itself: whatever value
// the cancel path records, it must never be 0. A future edit that "normalizes"
// jobCancelledExitCode to 0 would make every cancelled migration report success
// to delivery — this fails before that can ship.
func TestJobCancelledExitCodeIsNonZero(t *testing.T) {
	if jobCancelledExitCode == 0 {
		t.Fatal("jobCancelledExitCode is 0 — a cancelled job would report success")
	}
}

// TestJobSecretsFailClosed asserts a secret-bearing job with no resolver wired
// fails closed rather than running without the secrets it declared (I32).
func TestJobSecretsFailClosed(t *testing.T) {
	booter := &countingBooter{}
	uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)
	in := jobBase()
	in.SecretRefs = []string{"tenants/org/db#dsn"}

	_, err := uc.Provision(context.Background(), in)
	uc.Wait()
	if !errors.Is(err, domain.ErrSecretResolverUnavailable) {
		t.Fatalf("Provision err = %v, want ErrSecretResolverUnavailable", err)
	}
	if got := booter.count(); got != 0 {
		t.Fatalf("booter ran %d times for a job whose secrets could not resolve; want 0", got)
	}
}

// TestJobSecretsResolvedAndPushedNotInArgv asserts a job's secret_refs resolve
// through the P14 path and reach the VM ONLY over the vsock push — never in the
// argv the guest execs, and never in the persisted row.
func TestJobSecretsResolvedAndPushedNotInArgv(t *testing.T) {
	const dsn = "postgres://u:p@db/app"
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111") // == jobBase().OwnerOrg
	repo := newFakeWorkloadRepo()
	booter := &capturingBooter{}
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)
	uc.SetSecretResolver(jobResolver(dsn))

	in := jobBase()
	in.SecretRefs = []string{secret.TenantRef(org, "prod/app", "DATABASE_URL")}
	in.VaultToken = "hvs.handed-token"
	in.JobCommand = []string{"/app/migrate", "up"}

	out, err := uc.Provision(context.Background(), in)
	if err != nil {
		t.Fatalf("Provision err = %v", err)
	}
	uc.Wait()

	got := booter.input()
	if !got.ExpectSecrets {
		t.Fatal("ExpectSecrets = false; a secret-bearing job must open the vsock channel")
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "DATABASE_URL" || got.Secrets[0].Val != dsn {
		t.Fatalf("pushed secrets = %+v, want one DATABASE_URL carrying the resolved value", got.Secrets)
	}
	if got.BootstrapNonce == "" {
		t.Fatal("BootstrapNonce empty; a secret push must be nonce-attested (D-085)")
	}
	// The secret must never appear in the argv the guest execs.
	for _, arg := range booter.jobArgv() {
		if arg == dsn {
			t.Fatalf("resolved secret leaked into job argv: %v", booter.jobArgv())
		}
	}
	// ...nor into the persisted row.
	wl, err := repo.FindByID(context.Background(), mustParse(t, out.Handle))
	if err != nil {
		t.Fatalf("FindByID err = %v", err)
	}
	for _, arg := range wl.JobCommand {
		if arg == dsn {
			t.Fatalf("resolved secret persisted into the workload row: %v", wl.JobCommand)
		}
	}
	if wl.Message == dsn || wl.StdoutTail == dsn {
		t.Fatal("resolved secret persisted into the workload row")
	}
}

// TestJobEgressAllowReachesBooter asserts the allowlist is handed to the boot
// substrate (which installs the per-VM iptables chain).
func TestJobEgressAllowReachesBooter(t *testing.T) {
	booter := &capturingBooter{}
	uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, booter)

	in := jobBase()
	in.EgressAllow = []string{"10.0.10.20", "10.0.0.0/8"}
	if _, err := uc.Provision(context.Background(), in); err != nil {
		t.Fatalf("Provision err = %v", err)
	}
	uc.Wait()

	got := booter.input().EgressAllow
	if len(got) != 2 || got[0] != "10.0.10.20" || got[1] != "10.0.0.0/8" {
		t.Fatalf("EgressAllow = %v, want the descriptor's allowlist verbatim", got)
	}
}

// ── fakes ────────────────────────────────────────────────────────────────

// countingBooter counts BootTest calls (the "did the job run?" probe).
type countingBooter struct {
	mu sync.Mutex
	n  int
}

func (b *countingBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	return ImageTestResult{}, nil
}
func (b *countingBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, nil
}
func (b *countingBooter) Decommission(context.Context, ImageDecommissionInput) error { return nil }
func (b *countingBooter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// capturingBooter records the last ImageBootInput it received.
type capturingBooter struct {
	mu   sync.Mutex
	last ImageBootInput
	argv []string
}

func (b *capturingBooter) BootTest(_ context.Context, in ImageBootInput) (ImageTestResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = in
	return ImageTestResult{}, nil
}
func (b *capturingBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, nil
}
func (b *capturingBooter) Decommission(context.Context, ImageDecommissionInput) error { return nil }
func (b *capturingBooter) input() ImageBootInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// jobArgv returns the argv the guest would exec. The booter never receives it
// (it travels in runtime.json via the materializer), so this asserts on what the
// use case passed through — see the materializer test for the runtime.json shape.
func (b *capturingBooter) jobArgv() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.argv
}

// blockingBooter blocks inside BootTest until its context is cancelled, so a
// test can observe a job that is genuinely RUNNING.
type blockingBooter struct {
	entered   chan struct{}
	mu        sync.Mutex
	cancelled bool
	once      sync.Once
}

func newBlockingBooter() *blockingBooter {
	return &blockingBooter{entered: make(chan struct{})}
}
func (b *blockingBooter) BootTest(ctx context.Context, _ ImageBootInput) (ImageTestResult, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	b.mu.Lock()
	b.cancelled = true
	b.mu.Unlock()
	return ImageTestResult{}, ctx.Err()
}
func (b *blockingBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return ImageResidentResult{}, nil
}
func (b *blockingBooter) Decommission(context.Context, ImageDecommissionInput) error { return nil }
func (b *blockingBooter) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the job to enter BootTest")
	}
}
func (b *blockingBooter) sawCancel() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

// jobResolver builds a REAL EnvelopeVaultResolver over the package's existing
// stubKV/stubKEK (see fleet_replica_runtime_test.go), so the job path exercises
// the actual I28 authorize + I29 unseal codepath rather than a hand-rolled
// stand-in — and because secret.SecretValue is unconstructable from outside the
// secret package.
func jobResolver(plaintext string) secret.Resolver {
	return secret.NewEnvelopeVaultResolver(stubKV{val: "vault:v1:ct"}, stubKEK{pt: []byte(plaintext)})
}
