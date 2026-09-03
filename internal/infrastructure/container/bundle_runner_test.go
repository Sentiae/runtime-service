//go:build unit

package container

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// T2.1 — the untrusted-sandbox flag list is PINNED, element for element. Every
// hostile container this service launches is hardened by exactly this list, so
// a flag silently dropped here is a sandbox silently opened everywhere.
//
// Control: delete "--cap-drop", "ALL" from hardenedFlags ⇒ this fails on both
// the length and the element.
func TestHardenedFlags_Unchanged(t *testing.T) {
	want := []string{
		"--user", "65534:65534",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,size=256m,mode=1777",
		"--pids-limit", "256",
		"--memory", "64m",
		"--memory-swap", "64m",
		"--cpus", "1",
		"-e", "HOME=/tmp",
		"-e", "TMPDIR=/tmp",
		"-e", "XDG_CACHE_HOME=/tmp/.cache",
		"-e", "NPM_CONFIG_CACHE=/tmp/.npm",
		"-e", "GOCACHE=/tmp/.cache/go-build",
		"-e", "GOPATH=/tmp/go",
	}
	if len(want) != 29 {
		t.Fatalf("the pinned literal itself has %d elements, want 29", len(want))
	}

	got := hardenedFlags(64, 1)
	if len(got) != len(want) {
		t.Fatalf("hardenedFlags returned %d elements, want %d:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hardenedFlags[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// The memory cap and its swap twin move together — equal values are what
	// disables swap, so a container can never page its way past the cap.
	scaled := hardenedFlags(512, 2)
	if scaled[12] != "512m" || scaled[14] != "512m" || scaled[16] != "2" {
		t.Fatalf("hardenedFlags(512, 2) memory/swap/cpu = %q/%q/%q", scaled[12], scaled[14], scaled[16])
	}
}

// T2.2 — the bundle launch line: both labels, the read-only broker mount only
// when there are secrets, the network the invocation was given, and NO trailing
// command (the image's own CMD is what must run).
//
// Control: drop ",readonly" from the mount ⇒ the mount assertion fails.
// Control: omit "--label", "sentiae.node.run="+… ⇒ the run-label assertion
// fails and every SweepRun would silently leave the container behind.
func TestBundleRunArgs(t *testing.T) {
	runID := uuid.New()
	base := usecase.BundleLaunch{
		RunID:        runID,
		InvocationID: "inv-1234",
		Image:        "10.0.10.20:8443/acme/hello.node@sha256:" + strings.Repeat("aa", 32),
		MemoryMB:     64,
		TimeoutSec:   5,
	}

	t.Run("no secrets and no egress", func(t *testing.T) {
		args := bundleRunArgs(base, "sentiae-node-runs")
		assertPrefix(t, args, []string{
			"run", "--rm", "-i",
			"--name", "sentiae-node-inv-1234",
			"--label", "sentiae.node.invocation=inv-1234",
			"--label", "sentiae.node.run=" + runID.String(),
		})
		if !hasFlagValue(args, "--network", "none") {
			t.Fatalf("a node with no egress must run --network none; args=%v", args)
		}
		if hasFlag(args, "--mount") {
			t.Fatalf("a node with no secrets must mount nothing; args=%v", args)
		}
		if args[len(args)-1] != base.Image {
			t.Fatalf("the image must be LAST — no trailing command; args=%v", args)
		}
	})

	t.Run("secrets and egress", func(t *testing.T) {
		launch := base
		launch.BrokerSubpath = "inv-1234"
		launch.Network = "sentiae-inv-inv-1234"
		args := bundleRunArgs(launch, "sentiae-node-runs")

		wantMount := "type=volume,source=sentiae-node-runs,target=/run/sentiae,readonly,volume-subpath=inv-1234"
		if !hasFlagValue(args, "--mount", wantMount) {
			t.Fatalf("mount not verbatim; want %q; args=%v", wantMount, args)
		}
		if !hasFlagValue(args, "--network", "sentiae-inv-inv-1234") {
			t.Fatalf("an egress invocation must join its own bridge; args=%v", args)
		}
		if args[len(args)-1] != launch.Image {
			t.Fatalf("the image must be LAST — no trailing command; args=%v", args)
		}
	})

	t.Run("hardening is not optional", func(t *testing.T) {
		args := bundleRunArgs(base, "sentiae-node-runs")
		for _, flag := range []string{"--user", "--cap-drop", "--read-only", "--pids-limit", "--memory", "--cpus"} {
			if !hasFlag(args, flag) {
				t.Fatalf("bundle launch is missing %s; args=%v", flag, args)
			}
		}
	})
}

// TestProbeRedeemArgs_IsTheNodeLine pins the boot probe's redemption container
// as THE NODE LINE. The probe exists to measure dir-search and connect(2) from
// the node's uid class — a runtime-side dial measures the wrong class on both
// inodes — so a probe container that differs from the real node launch is a
// probe that proves the wrong thing. It is DERIVED from bundleRunArgs for
// exactly that reason, and this asserts the derivation element for element.
//
// The two deliberate differences: --entrypoint, because the runtime image's own
// ENTRYPOINT is the server and the redeeming binary is the sidecar's (R-23);
// and the `redeem` subcommand, which is the only thing that may follow the
// image.
//
// CONTROL (drift): write the probe's argv by hand instead of deriving it from
// bundleRunArgs — any later change to the node line (a dropped hardening flag,
// a mount that stops being readonly) stops reaching the probe and this
// equality goes red on the next such change.
// CONTROL (uid class): drop "--user", "65534:65534" from hardenedFlags — the
// probe would run as root, could search any directory, and the equality here
// goes red.
func TestProbeRedeemArgs_IsTheNodeLine(t *testing.T) {
	runID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	image := "sha256:036daa5efeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	launch := usecase.BundleLaunch{
		RunID:         runID,
		InvocationID:  "inv-1234",
		Image:         image,
		BrokerSubpath: "inv-1234",
	}

	want := []string{
		"run", "--rm", "-i",
		"--name", "sentiae-node-inv-1234",
		"--label", "sentiae.node.invocation=inv-1234",
		"--label", "sentiae.node.run=" + runID.String(),
		"--user", "65534:65534",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,size=256m,mode=1777",
		"--pids-limit", "256",
		"--memory", "256m",
		"--memory-swap", "256m",
		"--cpus", "1",
		"-e", "HOME=/tmp",
		"-e", "TMPDIR=/tmp",
		"-e", "XDG_CACHE_HOME=/tmp/.cache",
		"-e", "NPM_CONFIG_CACHE=/tmp/.npm",
		"-e", "GOCACHE=/tmp/.cache/go-build",
		"-e", "GOPATH=/tmp/go",
		"--mount", "type=volume,source=sentiae-node-runs,target=/run/sentiae,readonly,volume-subpath=inv-1234",
		"--network", "none",
		"--entrypoint", "/app/node-sidecar",
		image,
		"redeem",
	}

	got := probeRedeemArgs(launch, "sentiae-node-runs")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probe redeem argv:\n got %q\nwant %q", got, want)
	}
	if got[len(got)-1] != "redeem" || got[len(got)-2] != image {
		t.Fatalf("the image must be followed by `redeem` and nothing else, got %q", got[len(got)-3:])
	}
}

func assertPrefix(t *testing.T, args, want []string) {
	t.Helper()
	if len(args) < len(want) {
		t.Fatalf("args shorter than the expected prefix: %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}
}

// fakeDocker records every invocation and answers from a scripted table.
type fakeDocker struct {
	calls   [][]string
	configs []string
	fail    map[string]string // first arg → stderr to fail with
}

func (f *fakeDocker) run(_ context.Context, env []string, _ []byte, args ...string) (string, string, int, error) {
	f.calls = append(f.calls, args)
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			f.configs = append(f.configs, strings.TrimPrefix(kv, "DOCKER_CONFIG="))
		}
	}
	if msg, ok := f.fail[args[0]]; ok {
		return "", msg, 1, nil
	}
	return "", "", 0, nil
}

func (f *fakeDocker) verbs() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c[0])
	}
	return out
}

func testRunner(t *testing.T, docker *fakeDocker) *BundleRunner {
	t.Helper()
	return &BundleRunner{
		cfg: config.NodeRunnerConfig{
			RegistryHost: "10.0.10.20:8443",
			RegistryUser: "registry-client",
			RunsVolume:   "sentiae-node-runs",
			RunsDir:      t.TempDir(),
			PullTimeout:  30 * time.Second,
		},
		password: "service-api-key",
		self:     "sentiae-runtime-service",
		docker:   docker.run,
	}
}

// T2.3 — a bundle reference without a digest is REFUSED, never resolved. A tag
// can be moved after a graph is compiled, so pulling one would run bytes nobody
// pinned.
//
// Control: drop the "@sha256:" check ⇒ the tagged reference is pulled.
func TestPull_RefusesTagRef(t *testing.T) {
	docker := &fakeDocker{}
	runner := testRunner(t, docker)

	err := runner.Pull(context.Background(), "10.0.10.20:8443/acme/hello.node:1.0.9-go")
	if !errors.Is(err, domain.ErrBundlePullFailed) {
		t.Fatalf("Pull(tag) error = %v, want ErrBundlePullFailed", err)
	}
	if len(docker.calls) != 0 {
		t.Fatalf("a refused pull still ran docker: %v", docker.verbs())
	}

	// Anchor: the digest-pinned form IS pulled, inside the credential envelope.
	digest := "10.0.10.20:8443/acme/hello.node@sha256:" + strings.Repeat("aa", 32)
	if err := runner.Pull(context.Background(), digest); err != nil {
		t.Fatalf("Pull(digest): %v", err)
	}
	if got := docker.verbs(); len(got) != 3 || got[0] != "login" || got[1] != "pull" || got[2] != "logout" {
		t.Fatalf("docker verbs = %v, want [login pull logout]", got)
	}
}

// T2.3b — the registry credential exists only for the duration of one
// operation. `docker login` writes the service API key into DOCKER_CONFIG, so
// the directory is logged out of and REMOVED on the success path and on the
// failure path alike; anything else leaves the key on disk for whatever else
// runs in this container.
//
// Control: skip the os.RemoveAll on the failure path (return before the defer's
// cleanup) ⇒ the "failed pull" case finds the directory still on disk.
func TestRegistryAuthEnvelope_LeavesNothing(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		docker := &fakeDocker{}
		runner := testRunner(t, docker)
		digest := "10.0.10.20:8443/acme/hello.node@sha256:" + strings.Repeat("bb", 32)
		if err := runner.Pull(context.Background(), digest); err != nil {
			t.Fatalf("Pull: %v", err)
		}
		assertEnvelopeSwept(t, docker, []string{"login", "pull", "logout"})
	})

	t.Run("the pull fails", func(t *testing.T) {
		docker := &fakeDocker{fail: map[string]string{"pull": "manifest unknown"}}
		runner := testRunner(t, docker)
		digest := "10.0.10.20:8443/acme/hello.node@sha256:" + strings.Repeat("cc", 32)
		err := runner.Pull(context.Background(), digest)
		if !errors.Is(err, domain.ErrBundlePullFailed) {
			t.Fatalf("Pull error = %v, want ErrBundlePullFailed", err)
		}
		if !strings.Contains(err.Error(), "manifest unknown") {
			t.Fatalf("Pull error = %q, want the daemon's own message", err)
		}
		assertEnvelopeSwept(t, docker, []string{"login", "pull", "logout"})
	})

	t.Run("the login fails", func(t *testing.T) {
		docker := &fakeDocker{fail: map[string]string{"login": "unauthorized: authentication required"}}
		runner := testRunner(t, docker)
		err := runner.Probe(context.Background())
		want := "node runner: docker login 10.0.10.20:8443 failed: unauthorized: authentication required"
		if err == nil || err.Error() != want {
			t.Fatalf("Probe error = %v, want %q", err, want)
		}
		// Even a failed login wrote a config directory; it is swept anyway.
		assertEnvelopeSwept(t, docker, []string{"login", "logout"})
	})
}

func assertEnvelopeSwept(t *testing.T, docker *fakeDocker, wantVerbs []string) {
	t.Helper()
	if got := docker.verbs(); len(got) != len(wantVerbs) {
		t.Fatalf("docker verbs = %v, want %v", got, wantVerbs)
	} else {
		for i := range wantVerbs {
			if got[i] != wantVerbs[i] {
				t.Fatalf("docker verbs = %v, want %v", got, wantVerbs)
			}
		}
	}
	if len(docker.configs) == 0 {
		t.Fatal("no DOCKER_CONFIG was set — the credential would land in the shared store")
	}
	dir := docker.configs[0]
	for _, c := range docker.configs {
		if c != dir {
			t.Fatalf("the operation used two credential directories (%q, %q)", dir, c)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the registry credential directory %s survived (stat err = %v)", dir, err)
	}
}
