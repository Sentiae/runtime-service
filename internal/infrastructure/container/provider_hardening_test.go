//go:build unit

package container

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// hasFlagValue reports whether args contains the flag immediately followed by
// the given value (e.g. "--user", "65534:65534").
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestHardenedRunArgs_Sandbox asserts the docker run line for untrusted code
// carries every required isolation flag. This is the core security invariant:
// the container must never launch as root, with capabilities, with a writable
// rootfs, without a pids/memory/cpu cap, or with a network by default.
func TestHardenedRunArgs_Sandbox(t *testing.T) {
	p := NewProvider(config.ContainerConfig{})
	boot := usecase.VMBootConfig{
		VMID:        uuid.New(),
		Language:    domain.LanguageJavaScript,
		NetworkMode: domain.NetworkModeIsolated,
	}
	args := p.hardenedRunArgs("sentiae-vm-test", "node:22-slim", boot)

	if !hasFlagValue(args, "--user", untrustedUID) {
		t.Errorf("missing non-root --user %s; args=%v", untrustedUID, args)
	}
	if !hasFlagValue(args, "--cap-drop", "ALL") {
		t.Errorf("missing --cap-drop ALL; args=%v", args)
	}
	if !hasFlagValue(args, "--security-opt", "no-new-privileges") {
		t.Errorf("missing --security-opt no-new-privileges; args=%v", args)
	}
	if !hasFlag(args, "--read-only") {
		t.Errorf("missing --read-only; args=%v", args)
	}
	if !hasFlagValue(args, "--tmpfs", untrustedTmpfs) {
		t.Errorf("missing --tmpfs %s; args=%v", untrustedTmpfs, args)
	}
	if !hasFlag(args, "--pids-limit") {
		t.Errorf("missing --pids-limit; args=%v", args)
	}
	if !hasFlag(args, "--memory") || !hasFlag(args, "--memory-swap") {
		t.Errorf("missing --memory / --memory-swap cap; args=%v", args)
	}
	if !hasFlag(args, "--cpus") {
		t.Errorf("missing --cpus cap; args=%v", args)
	}
	// Default: network isolated.
	if !hasFlagValue(args, "--network", "none") {
		t.Errorf("untrusted container must default to --network none; args=%v", args)
	}
	if hasFlagValue(args, "--network", "host") {
		t.Errorf("untrusted container must NEVER get --network host by default; args=%v", args)
	}
}

// TestHardenedRunArgs_MemCpuDefaults proves a container is never launched
// uncapped: a boot config with zero memory/cpu still yields explicit caps.
func TestHardenedRunArgs_MemCpuDefaults(t *testing.T) {
	p := NewProvider(config.ContainerConfig{})
	boot := usecase.VMBootConfig{VMID: uuid.New(), MemoryMB: 0, VCPU: 0}
	args := p.hardenedRunArgs("c", "node:22-slim", boot)
	if !hasFlag(args, "--memory") || !hasFlag(args, "--memory-swap") || !hasFlag(args, "--cpus") {
		t.Fatalf("zero-resource boot must still cap memory+cpu; args=%v", args)
	}
}

// TestHardenedRunArgs_HostNetworkGated proves host networking is impossible
// without BOTH an explicit operator opt-in AND an explicit host request.
func TestHardenedRunArgs_HostNetworkGated(t *testing.T) {
	hostBoot := usecase.VMBootConfig{VMID: uuid.New(), NetworkMode: domain.NetworkModeHost}

	// Host requested but operator opt-in OFF → still none.
	off := NewProvider(config.ContainerConfig{AllowHostNetwork: false})
	if a := off.hardenedRunArgs("c", "img", hostBoot); !hasFlagValue(a, "--network", "none") {
		t.Errorf("host request without opt-in must collapse to none; args=%v", a)
	}

	// Operator opt-in ON + host requested → host allowed.
	on := NewProvider(config.ContainerConfig{AllowHostNetwork: true})
	if a := on.hardenedRunArgs("c", "img", hostBoot); !hasFlagValue(a, "--network", "host") {
		t.Errorf("opt-in + host request should allow host; args=%v", a)
	}

	// Opt-in ON but NOT a host request (isolated) → none.
	isoBoot := usecase.VMBootConfig{VMID: uuid.New(), NetworkMode: domain.NetworkModeIsolated}
	if a := on.hardenedRunArgs("c", "img", isoBoot); !hasFlagValue(a, "--network", "none") {
		t.Errorf("non-host request must stay none even with opt-in; args=%v", a)
	}
}

// TestHardenedRunArgs_NoStrayRoot is a belt-and-suspenders guard that the
// final image+command tail is intact and no `--privileged` slips in.
func TestHardenedRunArgs_NoStrayRoot(t *testing.T) {
	p := NewProvider(config.ContainerConfig{})
	args := p.hardenedRunArgs("c", "node:22-slim", usecase.VMBootConfig{VMID: uuid.New()})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--privileged") {
		t.Fatalf("must never run --privileged; args=%v", args)
	}
	if args[len(args)-3] != "node:22-slim" || args[len(args)-2] != "sleep" || args[len(args)-1] != "infinity" {
		t.Fatalf("image/command tail malformed; args=%v", args)
	}
}
