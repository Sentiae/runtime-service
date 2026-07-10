package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
)

// rtReplicaRepo is a stateful in-memory ReplicaRepository for the replica
// runtime tests. Only the methods FleetReplicaRuntime calls carry behavior.
type rtReplicaRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.Replica
}

func newRTReplicaRepo() *rtReplicaRepo {
	return &rtReplicaRepo{store: map[uuid.UUID]*domain.Replica{}}
}
func (f *rtReplicaRepo) Create(_ context.Context, r *domain.Replica) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.store[r.ID] = &cp
	return nil
}
func (f *rtReplicaRepo) Update(_ context.Context, r *domain.Replica) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.store[r.ID] = &cp
	return nil
}
func (f *rtReplicaRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.store[id]
	if !ok {
		return nil, domain.ErrReplicaNotFound
	}
	cp := *r
	return &cp, nil
}
func (f *rtReplicaRepo) ListByApp(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *rtReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *rtReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (f *rtReplicaRepo) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, id)
	return nil
}

// rtAppRepo serves one app by id.
type rtAppRepo struct {
	app *domain.FleetApp
}

func (f *rtAppRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	if f.app != nil && f.app.ID == id {
		return f.app, nil
	}
	return nil, domain.ErrFleetAppNotFound
}
func (f *rtAppRepo) Create(context.Context, *domain.FleetApp) error { return nil }
func (f *rtAppRepo) Update(context.Context, *domain.FleetApp) error { return nil }
func (f *rtAppRepo) FindByComponentEnv(context.Context, string, string) (*domain.FleetApp, error) {
	return nil, domain.ErrFleetAppNotFound
}
func (f *rtAppRepo) List(context.Context) ([]domain.FleetApp, error) {
	if f.app != nil {
		return []domain.FleetApp{*f.app}, nil
	}
	return nil, nil
}
func (f *rtAppRepo) Delete(context.Context, uuid.UUID) error { return nil }

// recordingBooter records BootResident + Decommission calls.
type recordingBooter struct {
	resident    ImageResidentResult
	resErr      error
	bootInput   *ImageBootInput
	decommInput *ImageDecommissionInput
	decommN     int
}

func (b *recordingBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, nil
}
func (b *recordingBooter) BootResident(_ context.Context, in ImageBootInput) (ImageResidentResult, error) {
	cp := in
	b.bootInput = &cp
	return b.resident, b.resErr
}
func (b *recordingBooter) Decommission(_ context.Context, in ImageDecommissionInput) error {
	b.decommN++
	cp := in
	b.decommInput = &cp
	return nil
}

func newTestApp() *domain.FleetApp {
	return &domain.FleetApp{
		ID:              uuid.New(),
		ImageRepository: "org/app",
		ImageDigest:     "sha256:abc",
		Port:            8080,
		ResourcesVCPU:   2,
		ResourcesMemMB:  1024,
	}
}

func newTestReplica(appID uuid.UUID) *domain.Replica {
	return &domain.Replica{
		ID:    uuid.New(),
		AppID: appID,
		State: domain.ReplicaStateScheduled,
	}
}

func TestBootReplica_Success(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	booter := &recordingBooter{resident: ImageResidentResult{
		PID: 4242, GuestIP: "10.0.0.5", HostPort: 20001, NetIndex: 3,
		TapName: "imgtap3", SocketPath: "/run/fc-3.sock",
	}}
	uc := NewFleetReplicaRuntime(
		fakeMaterializer{rootfs: "/work/rep/rootfs.ext4"},
		booter,
		replicas,
		&rtAppRepo{app: app},
		"/tmp/imgwork",
		"10.0.0.9",
	)

	if err := uc.BootReplica(context.Background(), rep.ID); err != nil {
		t.Fatalf("BootReplica: %v", err)
	}
	got, _ := replicas.FindByID(context.Background(), rep.ID)
	if got.State != domain.ReplicaStateResident {
		t.Fatalf("state = %q, want resident", got.State)
	}
	if got.PID == nil || *got.PID != 4242 {
		t.Fatalf("pid = %v, want 4242", got.PID)
	}
	if got.GuestIP != "10.0.0.5" || got.HostPort != 20001 || got.NetIndex != 3 {
		t.Fatalf("handle not stored: %+v", got)
	}
	if got.TapName != "imgtap3" || got.SocketPath != "/run/fc-3.sock" {
		t.Fatalf("teardown handle not stored: %+v", got)
	}
	if got.RootfsPath != "/work/rep/rootfs.ext4" || got.Port != 8080 {
		t.Fatalf("rootfs/port not stored: %+v", got)
	}
	if got.Endpoint != "http://10.0.0.9:20001" {
		t.Fatalf("endpoint = %q", got.Endpoint)
	}
}

// TestBootReplica_SecretSelfTest verifies the P3.3 vsock self-test marker is
// threaded onto the live resident-orchestrator boot for the SECRET-REF-LESS
// case only: with the flag on and no real secret_refs the boot carries
// ExpectSecrets + the injected marker; off, the boot is behavior-neutral (no
// ExpectSecrets, no secrets). Apps WITH real secret_refs go through the resolver
// path instead (TestBootReplica_SecretResolution).
func TestBootReplica_SecretSelfTest(t *testing.T) {
	tests := []struct {
		name       string
		selfTest   bool
		secretRefs []string
		wantExpect bool
		wantMarker bool
	}{
		{"flag off → neutral", false, nil, false, false},
		{"flag on, no refs → marker", true, nil, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp()
			app.SecretRefs = tt.secretRefs
			rep := newTestReplica(app.ID)
			replicas := newRTReplicaRepo()
			_ = replicas.Create(context.Background(), rep)

			booter := &recordingBooter{resident: ImageResidentResult{PID: 1, GuestIP: "10.0.0.5", HostPort: 20001}}
			uc := NewFleetReplicaRuntime(
				fakeMaterializer{rootfs: "/work/rep/rootfs.ext4"},
				booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9",
			)
			uc.SetSecretSelfTest(tt.selfTest)

			if err := uc.BootReplica(context.Background(), rep.ID); err != nil {
				t.Fatalf("BootReplica: %v", err)
			}
			if booter.bootInput == nil {
				t.Fatal("BootResident not called")
			}
			if booter.bootInput.ExpectSecrets != tt.wantExpect {
				t.Fatalf("ExpectSecrets = %v, want %v", booter.bootInput.ExpectSecrets, tt.wantExpect)
			}
			hasMarker := false
			for _, s := range booter.bootInput.Secrets {
				if s.Name == selfTestSecretName && s.Val == selfTestSecretValue {
					hasMarker = true
				}
			}
			if hasMarker != tt.wantMarker {
				t.Fatalf("marker present = %v, want %v (secrets=%+v)", hasMarker, tt.wantMarker, booter.bootInput.Secrets)
			}
		})
	}
}

func TestBootReplica_Idempotent(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	rep.State = domain.ReplicaStateResident
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	booter := &recordingBooter{}
	uc := NewFleetReplicaRuntime(fakeMaterializer{}, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")

	if err := uc.BootReplica(context.Background(), rep.ID); err != nil {
		t.Fatalf("BootReplica idempotent: %v", err)
	}
}

func TestBootReplica_BootFailure_MarksDead(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	bootErr := errors.New("kvm exploded")
	booter := &recordingBooter{resErr: bootErr}
	uc := NewFleetReplicaRuntime(fakeMaterializer{rootfs: "/work/rep/rootfs.ext4"}, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")

	err := uc.BootReplica(context.Background(), rep.ID)
	if !errors.Is(err, bootErr) {
		t.Fatalf("err = %v, want wrap of bootErr", err)
	}
	got, _ := replicas.FindByID(context.Background(), rep.ID)
	if got.State != domain.ReplicaStateDead {
		t.Fatalf("state = %q, want dead", got.State)
	}
	if got.Message == "" {
		t.Fatalf("dead replica should carry a message")
	}
}

func TestBootReplica_MaterializeFailure_MarksDead(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	matErr := errors.New("pull failed")
	uc := NewFleetReplicaRuntime(fakeMaterializer{err: matErr}, &recordingBooter{}, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")

	err := uc.BootReplica(context.Background(), rep.ID)
	if !errors.Is(err, matErr) {
		t.Fatalf("err = %v, want wrap of matErr", err)
	}
	got, _ := replicas.FindByID(context.Background(), rep.ID)
	if got.State != domain.ReplicaStateDead {
		t.Fatalf("state = %q, want dead", got.State)
	}
}

func TestDecommissionReplica_TearsDownAndDeletes(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	pid := 4242
	rep.State = domain.ReplicaStateResident
	rep.PID = &pid
	rep.SocketPath = "/run/fc-3.sock"
	rep.TapName = "imgtap3"
	rep.NetIndex = 3
	rep.GuestIP = "10.0.0.5"
	rep.HostPort = 20001
	rep.Port = 8080
	rep.RootfsPath = "/work/rep/rootfs.ext4"
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	booter := &recordingBooter{}
	uc := NewFleetReplicaRuntime(fakeMaterializer{}, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")

	if err := uc.DecommissionReplica(context.Background(), rep.ID); err != nil {
		t.Fatalf("DecommissionReplica: %v", err)
	}
	if booter.decommN != 1 {
		t.Fatalf("Decommission called %d times, want 1", booter.decommN)
	}
	if booter.decommInput == nil || booter.decommInput.PID != 4242 ||
		booter.decommInput.SocketPath != "/run/fc-3.sock" || booter.decommInput.TapName != "imgtap3" ||
		booter.decommInput.NetIndex != 3 || booter.decommInput.Port != 8080 {
		t.Fatalf("Decommission got wrong handle: %+v", booter.decommInput)
	}
	if _, err := replicas.FindByID(context.Background(), rep.ID); !errors.Is(err, domain.ErrReplicaNotFound) {
		t.Fatalf("replica row should be deleted, got %v", err)
	}
}

// stubKV + stubKEK implement the (unexported) vaultGetter + tenantKEK surfaces
// secret.NewEnvelopeVaultResolver depends on, so these tests drive a REAL
// EnvelopeVaultResolver end to end (its SecretValue is otherwise
// unconstructable from outside the secret package). This exercises the actual
// I28 authorize + I29 unseal codepath, not a hand-rolled stand-in.
type stubKV struct {
	val string
	err error
}

func (s stubKV) GetSecret(context.Context, string, string) (string, error) { return s.val, s.err }

type stubKEK struct {
	pt  []byte
	err error
}

func (s stubKEK) Decrypt(context.Context, uuid.UUID, string) ([]byte, error) { return s.pt, s.err }

// TestBootReplica_SecretResolution proves the P3.4 fail-closed contract: a
// resident app with secret_refs resolves each ref through the per-tenant
// resolver (scoped to its owner org) and pushes the revealed value; ANY resolve
// failure aborts the boot BEFORE BootResident — no HostSecret with an empty or
// foreign value ever reaches the booter.
func TestBootReplica_SecretResolution(t *testing.T) {
	org := uuid.New()
	ref := secret.TenantRef(org, "prod/app", "db_password") // tenants/<org>/prod/app#db_password

	// boot builds a fresh replica + runtime, wires the resolver, boots once, and
	// returns the booter (to inspect what — if anything — reached BootResident).
	boot := func(app *domain.FleetApp, resolver secret.Resolver) (*recordingBooter, error) {
		rep := newTestReplica(app.ID)
		replicas := newRTReplicaRepo()
		_ = replicas.Create(context.Background(), rep)
		booter := &recordingBooter{resident: ImageResidentResult{PID: 1, GuestIP: "10.0.0.5", HostPort: 20001}}
		uc := NewFleetReplicaRuntime(
			fakeMaterializer{rootfs: "/work/rep/rootfs.ext4"},
			booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9",
		)
		if resolver != nil {
			uc.SetSecretResolver(resolver)
		}
		return booter, uc.BootReplica(context.Background(), rep.ID)
	}

	okResolver := func() secret.Resolver {
		return secret.NewEnvelopeVaultResolver(stubKV{val: "vault:v1:ct"}, stubKEK{pt: []byte("s3cr3t")})
	}

	t.Run("real refs, nil resolver → fail closed", func(t *testing.T) {
		app := newTestApp()
		app.OwnerOrg = org.String()
		app.SecretRefs = []string{ref}
		booter, err := boot(app, nil)
		if !errors.Is(err, domain.ErrSecretResolverUnavailable) {
			t.Fatalf("err = %v, want ErrSecretResolverUnavailable", err)
		}
		if booter.bootInput != nil {
			t.Fatal("BootResident must not be called — no VM may run without its secrets")
		}
	})

	t.Run("refs, empty owner org → fail closed", func(t *testing.T) {
		app := newTestApp()
		app.OwnerOrg = ""
		app.SecretRefs = []string{ref}
		booter, err := boot(app, okResolver())
		if !errors.Is(err, domain.ErrSecretOwnerOrgMissing) {
			t.Fatalf("err = %v, want ErrSecretOwnerOrgMissing", err)
		}
		if booter.bootInput != nil {
			t.Fatal("BootResident must not be called with no org to scope to")
		}
	})

	t.Run("cross-tenant ref → fail closed", func(t *testing.T) {
		app := newTestApp()
		app.OwnerOrg = uuid.New().String() // owner org != ref org
		app.SecretRefs = []string{ref}
		booter, err := boot(app, okResolver())
		if !errors.Is(err, secret.ErrCrossTenantSecret) {
			t.Fatalf("err = %v, want ErrCrossTenantSecret", err)
		}
		if booter.bootInput != nil {
			t.Fatal("BootResident must not be called for a cross-tenant ref")
		}
	})

	t.Run("resolver error → fail closed", func(t *testing.T) {
		app := newTestApp()
		app.OwnerOrg = org.String()
		app.SecretRefs = []string{ref}
		bad := secret.NewEnvelopeVaultResolver(stubKV{val: "vault:v1:ct"}, stubKEK{err: errors.New("kek missing")})
		booter, err := boot(app, bad)
		if err == nil {
			t.Fatal("want error when the KEK decrypt fails")
		}
		if booter.bootInput != nil {
			t.Fatal("BootResident must not be called on a resolve failure")
		}
	})

	t.Run("resolves + pushes the secret", func(t *testing.T) {
		app := newTestApp()
		app.OwnerOrg = org.String()
		app.SecretRefs = []string{ref}
		booter, err := boot(app, okResolver())
		if err != nil {
			t.Fatalf("BootReplica: %v", err)
		}
		if booter.bootInput == nil || !booter.bootInput.ExpectSecrets {
			t.Fatalf("ExpectSecrets should be set, got %+v", booter.bootInput)
		}
		if len(booter.bootInput.Secrets) != 1 {
			t.Fatalf("want 1 pushed secret, got %d", len(booter.bootInput.Secrets))
		}
		got := booter.bootInput.Secrets[0]
		if got.Name != "db_password" {
			t.Fatalf("secret name = %q, want db_password", got.Name)
		}
		if got.Val != "s3cr3t" {
			t.Fatalf("secret val = %q, want the resolved plaintext", got.Val)
		}
	})
}

func TestDecommissionReplica_MissingIsNoop(t *testing.T) {
	replicas := newRTReplicaRepo()
	booter := &recordingBooter{}
	uc := NewFleetReplicaRuntime(fakeMaterializer{}, booter, replicas, &rtAppRepo{}, "/tmp/imgwork", "10.0.0.9")

	if err := uc.DecommissionReplica(context.Background(), uuid.New()); err != nil {
		t.Fatalf("missing replica should be no-op, got %v", err)
	}
	if booter.decommN != 0 {
		t.Fatalf("Decommission should not be called for a missing replica")
	}
}
