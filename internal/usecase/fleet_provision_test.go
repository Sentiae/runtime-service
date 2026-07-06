package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

type fakeWorkloadRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.ImageWorkload
}

func newFakeWorkloadRepo() *fakeWorkloadRepo {
	return &fakeWorkloadRepo{store: map[uuid.UUID]*domain.ImageWorkload{}}
}
func (f *fakeWorkloadRepo) Create(_ context.Context, w *domain.ImageWorkload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *w
	f.store[w.ID] = &cp
	return nil
}
func (f *fakeWorkloadRepo) Update(_ context.Context, w *domain.ImageWorkload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *w
	f.store[w.ID] = &cp
	return nil
}
func (f *fakeWorkloadRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.ImageWorkload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.store[id]
	if !ok {
		return nil, domain.ErrWorkloadNotFound
	}
	cp := *w
	return &cp, nil
}
func (f *fakeWorkloadRepo) FindActive(_ context.Context) ([]domain.ImageWorkload, error) {
	return nil, nil
}
func (f *fakeWorkloadRepo) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, id)
	return nil
}

type fakeMaterializer struct {
	rootfs string
	err    error
}

func (f fakeMaterializer) Materialize(context.Context, ImageMaterializeInput) (ImageMaterializeOutput, error) {
	return ImageMaterializeOutput{RootfsPath: f.rootfs}, f.err
}

type fakeBooter struct {
	test     ImageTestResult
	resident ImageResidentResult
	testErr  error
	resErr   error
}

func (f fakeBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return f.test, f.testErr
}
func (f fakeBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	return f.resident, f.resErr
}
func (f fakeBooter) Decommission(context.Context, ImageDecommissionInput) error { return nil }

func newUC(repo *fakeWorkloadRepo, m ImageMaterializer, b ImageBooter) *FleetProvision {
	return NewFleetProvision(context.Background(), repo, m, b, "/tmp/imgwork", "10.0.0.9")
}

func TestFleetProvisionValidation(t *testing.T) {
	base := FleetProvisionInput{
		Registry:      "reg:8089",
		Repository:    "org/app",
		Digest:        "sha256:abc",
		WorkloadClass: "test",
	}
	tests := []struct {
		name    string
		mutate  func(*FleetProvisionInput)
		wantErr error
	}{
		{"unsupported class", func(in *FleetProvisionInput) { in.WorkloadClass = "batch" }, domain.ErrUnsupportedClass},
		{"secrets set", func(in *FleetProvisionInput) { in.SecretRefs = []string{"db"} }, domain.ErrSecretsNotSupported},
		{"missing registry", func(in *FleetProvisionInput) { in.Registry = "" }, domain.ErrImageRefIncomplete},
		{"missing repository", func(in *FleetProvisionInput) { in.Repository = "" }, domain.ErrImageRefIncomplete},
		{"missing digest", func(in *FleetProvisionInput) { in.Digest = "" }, domain.ErrImageRefIncomplete},
		{"resident no port", func(in *FleetProvisionInput) { in.WorkloadClass = "resident"; in.Port = 0 }, domain.ErrResidentPortRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mutate(&in)
			uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{}, fakeBooter{})
			_, err := uc.Provision(context.Background(), in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Provision err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestFleetProvisionTestClassAsync(t *testing.T) {
	repo := newFakeWorkloadRepo()
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, fakeBooter{
		test: ImageTestResult{ExitCode: 0, Stdout: "ok", Stderr: ""},
	})
	out, err := uc.Provision(context.Background(), FleetProvisionInput{
		Registry:      "reg:8089",
		Repository:    "org/app",
		Digest:        "sha256:abc",
		WorkloadClass: "test",
		TestCommand:   "run-tests",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if out.Handle == "" || out.URL != "" {
		t.Fatalf("test provision out = %+v; want handle set, url empty", out)
	}
	uc.Wait() // drain the detached run

	id, _ := uuid.Parse(out.Handle)
	wl, _ := repo.FindByID(context.Background(), id)
	if wl.State != domain.ImageWorkloadStateExited {
		t.Errorf("state = %s, want exited", wl.State)
	}
	if wl.ExitCode == nil || *wl.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", wl.ExitCode)
	}

	h, err := uc.Health(context.Background(), out.Handle)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Healthy || h.State != "exited" {
		t.Errorf("health = %+v, want healthy exited", h)
	}
}

func TestFleetProvisionResidentSync(t *testing.T) {
	repo := newFakeWorkloadRepo()
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, fakeBooter{
		resident: ImageResidentResult{PID: 4242, GuestIP: "10.201.0.6", HostPort: 20000, NetIndex: 1, TapName: "img1", SocketPath: "/tmp/s.sock"},
	})
	out, err := uc.Provision(context.Background(), FleetProvisionInput{
		Registry:      "reg:8089",
		Repository:    "org/app",
		Digest:        "sha256:abc",
		WorkloadClass: "resident",
		Port:          8080,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if out.URL != "http://10.0.0.9:20000" {
		t.Errorf("url = %q, want http://10.0.0.9:20000", out.URL)
	}
	id, _ := uuid.Parse(out.Handle)
	wl, _ := repo.FindByID(context.Background(), id)
	if wl.State != domain.ImageWorkloadStateRunning || wl.HostPort != 20000 || wl.NetIndex != 1 {
		t.Errorf("resident workload persisted wrong: %+v", wl)
	}
}

func TestFleetProvisionFailLoudBooter(t *testing.T) {
	repo := newFakeWorkloadRepo()
	uc := newUC(repo, fakeMaterializer{rootfs: "/tmp/rootfs.ext4"}, FailLoudImageBooter{})
	_, err := uc.Provision(context.Background(), FleetProvisionInput{
		Registry:      "reg:8089",
		Repository:    "org/app",
		Digest:        "sha256:abc",
		WorkloadClass: "resident",
		Port:          8080,
	})
	if !errors.Is(err, domain.ErrImageBootUnavailable) {
		t.Fatalf("resident boot err = %v, want ErrImageBootUnavailable", err)
	}
}

func TestFleetHealthNotFound(t *testing.T) {
	uc := newUC(newFakeWorkloadRepo(), fakeMaterializer{}, fakeBooter{})
	if _, err := uc.Health(context.Background(), uuid.New().String()); !errors.Is(err, domain.ErrWorkloadNotFound) {
		t.Fatalf("Health err = %v, want ErrWorkloadNotFound", err)
	}
	if _, err := uc.Health(context.Background(), "not-a-uuid"); !errors.Is(err, domain.ErrWorkloadNotFound) {
		t.Fatalf("Health bad-handle err = %v, want ErrWorkloadNotFound", err)
	}
}
