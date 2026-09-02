//go:build integration

package container

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// T4.12 — TestTwoOrgsCannotReach is the isolation proof, and it needs a REAL
// daemon: two organizations' invocations run at the same time, each with its
// own --internal bridge and its own sidecar aliased `proxy`, and neither can
// reach the other's proxy — not by name (the alias resolves only on its own
// bridge) and not by address (the bridges are disjoint and internal).
//
// AUTHORED HERE, RUN BY THE LEAD (D-041, §9). It is driven from inside the
// deployed runtime container, which is the only place that has all three of the
// docker socket, the runs volume mounted at the configured path, and the image
// that carries /app/node-sidecar:
//
//	docker exec sentiae-runtime-service sh -c 'cd /src && \
//	  SENTIAE_P4_SIDECAR_IMAGE=$(docker inspect $(hostname) --format {{.Image}}) \
//	  go test -tags integration -count=1 -run TestTwoOrgsCannotReach ./internal/infrastructure/container/'
//
// MUTATION (the control, executable): SENTIAE_P4_ONE_BRIDGE=1 puts both
// invocations on ONE bridge. The reachability probe must then SUCCEED — which
// is what proves the probe can observe reachability at all, and therefore that
// the negative result in the normal run is isolation and not a broken probe.
func TestTwoOrgsCannotReach(t *testing.T) {
	image := os.Getenv("SENTIAE_P4_SIDECAR_IMAGE")
	if image == "" {
		t.Skip("SENTIAE_P4_SIDECAR_IMAGE is unset: this test needs the deployed runtime image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}

	cfg := config.NodeRunnerConfig{
		RegistryHost:        "10.0.10.20:8443",
		RegistryUser:        "registry-client",
		RunsVolume:          envOr("SENTIAE_P4_RUNS_VOLUME", "sentiae-node-runs"),
		RunsDir:             envOr("SENTIAE_P4_RUNS_DIR", "/var/lib/sentiae/node-runs"),
		UplinkNetwork:       envOr("SENTIAE_P4_UPLINK", "sentiae-node-egress-uplink"),
		InvocationCIDR:      "10.202.0.0/16", // a range of its own, so a live fleet is untouched
		InvocationPrefixLen: 29,
		SidecarReadyTimeout: 15 * time.Second,
		PullTimeout:         time.Minute,
		TunnelMax:           30 * time.Second,
	}
	if err := os.MkdirAll(cfg.RunsDir, 0o777); err != nil {
		t.Skipf("runs dir %s is not writable here: %v", cfg.RunsDir, err)
	}

	pool, err := usecase.NewSubnetPool(cfg.InvocationCIDR, cfg.InvocationPrefixLen)
	if err != nil {
		t.Fatalf("subnet pool: %v", err)
	}
	manager, err := NewSidecarManager(cfg, pool)
	if err != nil {
		t.Fatalf("sidecar manager: %v", err)
	}
	manager.image = image

	ctx := context.Background()
	oneBridge := os.Getenv("SENTIAE_P4_ONE_BRIDGE") == "1"

	subnetA, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire subnet A: %v", err)
	}
	subnetB := subnetA
	if !oneBridge {
		if subnetB, err = pool.Acquire(); err != nil {
			t.Fatalf("acquire subnet B: %v", err)
		}
	}

	orgA := openOrg(t, ctx, manager, "acme", subnetA)
	orgB := openOrg(t, ctx, manager, "globex", subnetB)

	networkA := "sentiae-inv-" + orgA
	networkB := "sentiae-inv-" + orgB
	if oneBridge {
		// The mutation: attach B's sidecar to A's bridge so the two really can
		// see each other, and the probe below must observe it.
		mustDocker(t, "network", "connect", "--alias", "proxy-b", networkA, "sentiae-sc-"+orgB)
		networkB = networkA
	}
	// Read the address ON A NAMED NETWORK: a sidecar holds one address per
	// network it joined (its bridge AND the uplink), and picking "the first" out
	// of a map would be a coin flip.
	addressB := containerAddressOn(t, "sentiae-sc-"+orgB, networkB)
	if addressB == "" {
		t.Fatal("could not read the second sidecar's address")
	}

	// The probe runs ON A's BRIDGE, exactly where the hostile node would be.
	reachable := probeReach(t, networkA, image, addressB)

	if oneBridge {
		if !reachable {
			t.Fatalf("MUTATION: on ONE bridge %s:3128 must be reachable — the probe cannot observe reachability, so the isolation result below would be meaningless", addressB)
		}
		t.Logf("mutation confirmed: on one bridge, %s:3128 is reachable", addressB)
		return
	}
	if reachable {
		t.Fatalf("ISOLATION BREACH: a node on %s reached the other organization's proxy at %s:3128", networkA, addressB)
	}

	// And the alias must resolve to A's OWN sidecar only.
	resolved := probeResolve(t, networkA, image)
	ownAddress := containerAddressOn(t, "sentiae-sc-"+orgA, networkA)
	if resolved == "" || !strings.Contains(resolved, ownAddress) {
		t.Fatalf("the proxy alias on %s resolved to %q, want the invocation's own sidecar %s",
			networkA, resolved, ownAddress)
	}
	if strings.Contains(resolved, addressB) {
		t.Fatalf("the proxy alias resolved to the OTHER organization's sidecar: %q", resolved)
	}
}

// openOrg stands up one organization's egress invocation and registers its
// teardown.
func openOrg(t *testing.T, ctx context.Context, manager *SidecarManager, org, subnet string) string {
	t.Helper()
	invocation := "inv-" + uuid.NewString()
	run := uuid.New()
	_, err := manager.Open(ctx, usecase.SidecarOpen{
		InvocationID: invocation,
		RunID:        run,
		Node:         org,
		Binding: usecase.SidecarBinding{
			Invocation: invocation,
			Run:        run.String(),
			Node:       org,
			Secrets:    map[string]usecase.SecretAnswer{},
			Egress: &usecase.EgressBinding{
				Patterns: []string{"httpbin.org"},
				Token:    strings.Repeat("a", 64),
				Subnet:   subnet,
			},
		},
	})
	if err != nil {
		t.Fatalf("open %s sidecar: %v", org, err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background(), invocation) })
	return invocation
}

// probeReach answers whether a container ON THIS BRIDGE can open a TCP
// connection to the given address's proxy port.
func probeReach(t *testing.T, network, image, address string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "run", "--rm", "--network", network,
		"--entrypoint", "/bin/sh", image, "-c",
		"nc -z -w 2 "+address+" 3128 && echo REACHABLE || echo UNREACHABLE").CombinedOutput()
	return strings.Contains(string(out), "REACHABLE") && !strings.Contains(string(out), "UNREACHABLE")
}

// probeResolve answers what the `proxy` alias resolves to on this bridge.
func probeResolve(t *testing.T, network, image string) string {
	t.Helper()
	out, _ := exec.Command("docker", "run", "--rm", "--network", network,
		"--entrypoint", "/bin/sh", image, "-c", "getent hosts proxy || true").CombinedOutput()
	return strings.TrimSpace(string(out))
}

func containerAddressOn(t *testing.T, name, network string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", name,
		"--format", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`).Output()
	if err != nil {
		t.Fatalf("inspect %s on %s: %v", name, network, err)
	}
	return strings.TrimSpace(string(out))
}

func mustDocker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v (%s)", strings.Join(args, " "), err, out)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
