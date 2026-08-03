package usecase

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/repository"
)

// The host-authority fence (#fleet-reconciler-acts-on-foreign-host-replicas)
// made this instance's fleet host id a CONSTRUCTOR requirement of every
// host-authoritative use case. These helpers give the tests one self host and
// one foreign host, and fail the test on a constructor error rather than letting
// a nil use case surface later as a confusing panic.

var (
	// testSelfHost is the host every fake in these tests is "running on".
	testSelfHost = uuid.MustParse("f1915bca-8c97-5816-a0d5-4e57afecf393")
	// testForeignHost is the OTHER fleet host — the one whose rows must be
	// visible as facts and untouchable as actions.
	testForeignHost = uuid.MustParse("940445b5-a7fa-4f22-b48a-8d04a31bbd7a")
)

// hostPtr is the addressable-copy helper the domain's nullable host columns need.
func hostPtr(id uuid.UUID) *uuid.UUID { return &id }

func newTestVolumeManager(t *testing.T, volumes repository.VolumeRepository, backend VolumeBackend,
	dir string, resources repository.FleetResourceRepository) *FleetVolumeManager {
	t.Helper()
	m, err := NewFleetVolumeManager(volumes, backend, dir, resources, testSelfHost)
	if err != nil {
		t.Fatalf("NewFleetVolumeManager: %v", err)
	}
	return m
}

func newTestReplicaRuntime(t *testing.T, materializer ImageMaterializer, booter ImageBooter,
	replicas repository.ReplicaRepository, apps repository.FleetAppRepository,
	workDir, advertiseHost string) *FleetReplicaRuntime {
	t.Helper()
	uc, err := NewFleetReplicaRuntime(materializer, booter, replicas, apps, workDir, advertiseHost, testSelfHost)
	if err != nil {
		t.Fatalf("NewFleetReplicaRuntime: %v", err)
	}
	return uc
}

func newTestOrchestrator(t *testing.T, apps repository.FleetAppRepository, replicas repository.ReplicaRepository,
	scheduler *FleetScheduler, runtime *FleetReplicaRuntime, resources repository.FleetResourceRepository) *FleetOrchestrator {
	t.Helper()
	uc, err := NewFleetOrchestrator(apps, replicas, scheduler, runtime, resources, testSelfHost)
	if err != nil {
		t.Fatalf("NewFleetOrchestrator: %v", err)
	}
	return uc
}

// TestHostAuthoritativeConstructors_RefuseWithoutIdentity proves the identity is
// a REQUIREMENT and not a defaulted field. A nil-host constructor that returned a
// usable value would be the whole fence's fail-open: the component would exist,
// be reachable, and answer every ownership question with "not mine" (or, if made
// permissive, with "mine") on a fleet-wide table.
func TestHostAuthoritativeConstructors_RefuseWithoutIdentity(t *testing.T) {
	if _, err := NewFleetVolumeManager(newVolRepoFake(), &recordingBackend{}, "/vol", nil, uuid.Nil); err == nil {
		t.Fatal("NewFleetVolumeManager accepted a nil self host")
	}
	if _, err := NewFleetReplicaRuntime(fakeMaterializer{}, &recordingBooter{}, nil, nil, "/work", "10.0.0.9", uuid.Nil); err == nil {
		t.Fatal("NewFleetReplicaRuntime accepted a nil self host")
	}
	if _, err := NewFleetOrchestrator(nil, nil, nil, nil, nil, uuid.Nil); err == nil {
		t.Fatal("NewFleetOrchestrator accepted a nil self host")
	}
	if _, err := NewFleetVolumeSnapshotter(nil, nil, nil, nil, nil, uuid.Nil); err == nil {
		t.Fatal("NewFleetVolumeSnapshotter accepted a nil self host")
	}
	if _, err := NewFleetVolumeRestorer(context.Background(), nil, nil, nil, nil, nil, nil, uuid.Nil); err == nil {
		t.Fatal("NewFleetVolumeRestorer accepted a nil self host")
	}
}
