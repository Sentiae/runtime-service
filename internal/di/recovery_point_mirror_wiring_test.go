//go:build unit

package di

import (
	"errors"
	"io"
	"testing"

	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/pkg/config"
)

// ─────────────────────────────────────────────────────────────────────
// D-200's gate: the second-domain copy runs on the CONTROL PLANE and nowhere else.
//
// ⚠ WHY THIS IS TESTED AT THE WIRING AND NOT ONLY IN THE WORKER. The failure this
// guards against is not a logic bug — the worker is correct wherever it runs. It is
// that a fleet host, which is TENANT-ADJACENT and holds zero standing Vault
// capability by D-125's design, ends up carrying an off-chassis all-tenant
// object-store credential. That is a WIRING fact, and a wiring miss is precisely
// what unit tests of the surrounding code never see (the platform's own recurring
// trap: a config-name miss failing silently into the permissive branch).
// ─────────────────────────────────────────────────────────────────────

// stubArtifactStore stands in for the PRIMARY store. Nothing here is ever called:
// the builder only needs it to be non-nil, and no test in this file performs any
// object I/O (writing a probe object into the real second domain would be
// undeletable for 30 days).
type stubArtifactStore struct{}

func (stubArtifactStore) Put(string, io.Reader) error       { return errors.New("stub") }
func (stubArtifactStore) Get(string) (io.ReadCloser, error) { return nil, errors.New("stub") }
func (stubArtifactStore) Exists(string) (bool, error)       { return false, errors.New("stub") }
func (stubArtifactStore) VerifyHash(string) error           { return errors.New("stub") }

// stubFleetResourceRepo is a NON-NIL ledger. It embeds the interface rather than
// implementing ~20 methods: nothing calls it here, and what these tests need is for
// the builder to have every ingredient it would need to SUCCEED, so that the only
// thing left refusing is the gate under test.
type stubFleetResourceRepo struct {
	repository.FleetResourceRepository
}

// TestOnlyTheControlPlaneMirrors pins the predicate to the SAME seam
// registerSelfHost is gated on: executor_type=firecracker means fleet host.
func TestOnlyTheControlPlaneMirrors(t *testing.T) {
	tests := []struct {
		name         string
		executorType string
		want         bool
	}{
		{
			name:         "a firecracker fleet host does NOT mirror",
			executorType: "firecracker",
			// It self-registers as a fleet host and runs customer VMs. After D-200 it
			// holds no off-chassis credential at all, so the copy must not even be
			// attempted here — disabled BY DESIGN, not disabled by a 403.
			want: false,
		},
		{
			name:         "the mesh instance (container executor) mirrors",
			executorType: "container",
			// The single control-plane instance: it already holds the ledger and the
			// primary object store, and is already the all-tenant TCB.
			want: true,
		},
		{
			name:         "the simulated executor is also a control-plane-shaped instance",
			executorType: "simulated",
			want:         true,
		},
		{
			name:         "an unset executor type is NOT treated as a fleet host",
			executorType: "",
			// The default branch must be the one that boots the mirror, never the one
			// that silently skips it: a typo'd executor name that landed on "fleet host"
			// would disable the mirror while looking configured.
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isControlPlaneInstance(tt.executorType); got != tt.want {
				t.Errorf("isControlPlaneInstance(%q) = %v, want %v", tt.executorType, got, tt.want)
			}
		})
	}
}

// TestFleetHostNeverBuildsTheMirrorWorker drives the real builder, because the
// predicate being right is worth nothing if the builder consults it after reading
// the credential. A fleet host must return nil BEFORE any of that — and would
// return nil even if a credential had somehow been placed in its environment.
func TestFleetHostNeverBuildsTheMirrorWorker(t *testing.T) {
	c := &Container{}
	// A PRIMARY store is present, so the only thing left that can refuse is the gate
	// itself. Without this the test would pass for the wrong reason (no store to read
	// blobs out of) and would keep passing with the gate deleted.
	c.snapshotArtifactStore = stubArtifactStore{}
	c.FleetResourceRepo = stubFleetResourceRepo{}
	cfg := &config.Config{}
	cfg.App.ExecutorType = "firecracker"
	// Fully configured second domain, deliberately: the assertion is that the GATE
	// refuses, not that the host merely lacks the values.
	cfg.SecondDomain = config.SecondDomainConfig{
		Enabled:   true,
		Endpoint:  "https://example.r2.cloudflarestorage.com",
		Bucket:    "sentiae-recovery-points",
		AccessKey: "fake-access-key-for-this-test-only",
		SecretKey: "fake-secret-key-for-this-test-only",
		Region:    "auto",
	}

	if w := c.newRecoveryPointMirrorWorker(cfg); w != nil {
		t.Fatal("a Firecracker fleet host built a second-domain mirror — D-200 exists precisely so this host never holds or uses an off-chassis object-store credential")
	}
}

// TestControlPlaneWithoutACredentialStaysHonest proves the miss is a nil worker and
// not a half-built one. A non-nil worker that cannot reach the second domain would
// stamp failures forever while looking wired; nil leaves every recovery point
// recorded primary_only, which is the truth.
func TestControlPlaneWithoutACredentialStaysHonest(t *testing.T) {
	tests := []struct {
		name string
		sd   config.SecondDomainConfig
	}{
		{
			name: "disabled",
			sd:   config.SecondDomainConfig{Enabled: false},
		},
		{
			name: "enabled but credential-less",
			sd:   config.SecondDomainConfig{Enabled: true, Endpoint: "https://x", Bucket: "b", Region: "auto"},
		},
		{
			name: "enabled but no bucket",
			sd:   config.SecondDomainConfig{Enabled: true, Endpoint: "https://x", AccessKey: "a", SecretKey: "s", Region: "auto"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Container{}
			// Same reason as above: with no primary store every case would refuse for a
			// reason other than the one under test.
			c.snapshotArtifactStore = stubArtifactStore{}
			c.FleetResourceRepo = stubFleetResourceRepo{}
			cfg := &config.Config{}
			cfg.App.ExecutorType = "container"
			cfg.SecondDomain = tt.sd
			if w := c.newRecoveryPointMirrorWorker(cfg); w != nil {
				t.Fatal("a worker was built without a complete second domain — it would fail on every recovery point while reading as wired")
			}
		})
	}
}
