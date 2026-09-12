package config

import (
	"strings"
	"testing"
	"time"
)

// nodeRunnerHost is the deployed registry endpoint the compose environment
// supplies; any non-empty value satisfies the refusal, and this is the real one.
const nodeRunnerHost = "10.0.10.20:8443"

// T1.7 — the node runner's registry is required exactly where it is used.
//
// The refusal is CONDITIONAL on the executor, and both halves matter. On the
// container executor the runtime pulls node bundles, so an unset registry means
// it would pull from nowhere — or, if a default existed, from whatever answers
// at an address nobody chose. On the firecracker executor there is no node
// runner at all, and refusing there would take down a fleet host over a setting
// it has no use for.
//
// Control (the refusal): drop the `cfg.NodeRunner.RegistryHost == ""` check in
// Load ⇒ the "container executor without a registry" row loads clean, and the
// boot that should have refused proceeds with an empty registry host.
// Control (the condition): make the check unconditional ⇒ the "firecracker
// executor without a registry" row fails, i.e. the fleet host stops booting.
func TestNodeRunnerConfig(t *testing.T) {
	const wantMsg = "load config: node runner registry host is required (APP_NODE_RUNNER_REGISTRY_HOST)"

	tests := []struct {
		name         string
		executorType string // "" ⇒ leave unset, exercising the default (container)
		registryHost string // "" ⇒ leave unset
		wantErr      string
	}{
		{name: "container executor without a registry refuses", executorType: "container", wantErr: wantMsg},
		{name: "default executor without a registry refuses", wantErr: wantMsg},
		{name: "container executor with a registry loads", executorType: "container", registryHost: nodeRunnerHost},
		{name: "firecracker executor without a registry loads", executorType: "firecracker"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withOwnerCredentials(t)
			if tt.executorType != "" {
				t.Setenv("APP_EXECUTOR_TYPE", tt.executorType)
			}
			if tt.registryHost != "" {
				t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", tt.registryHost)
			}

			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() succeeded; want refusal %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("Load() error = %q, want exactly %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg.NodeRunner.RegistryHost != tt.registryHost {
				t.Fatalf("RegistryHost = %q, want %q", cfg.NodeRunner.RegistryHost, tt.registryHost)
			}
		})
	}
}

// The defaults are the deployed topology, and every one of them is read by an
// adapter that has no other source for it. A silently-changed default here is a
// sidecar on the wrong network or a bundle pulled from the wrong host.
//
// Control: change any default in Load ⇒ its row fails.
func TestNodeRunnerConfig_Defaults(t *testing.T) {
	withOwnerCredentials(t)
	t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", nodeRunnerHost)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	nr := cfg.NodeRunner

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"registry_user", nr.RegistryUser, "registry-client"},
		{"runs_volume", nr.RunsVolume, "sentiae-node-runs"},
		{"runs_dir", nr.RunsDir, "/var/lib/sentiae/node-runs"},
		{"uplink_network", nr.UplinkNetwork, "sentiae-node-egress-uplink"},
		{"invocation_cidr", nr.InvocationCIDR, "10.201.0.0/16"},
		{"invocation_prefix_len", nr.InvocationPrefixLen, 29},
		{"sidecar_ready_timeout", nr.SidecarReadyTimeout, 5 * time.Second},
		{"pull_timeout", nr.PullTimeout, 120 * time.Second},
		{"tunnel_max", nr.TunnelMax, 130 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("node_runner.%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

// Every field is reachable from the environment under its APP_NODE_RUNNER_*
// name. A struct field the deployment cannot set is indistinguishable from a
// knob that does not work.
//
// ⚠ Measured, not assumed: removing a {"node_runner.x", "APP_NODE_RUNNER_X"}
// pair from BindEnvs does NOT make this test red — platform-kit's loader maps
// APP_<SECTION>_<FIELD> onto the key path automatically, so the explicit pairs
// are belt-and-braces (kept because every other section in this file lists its
// bindings, §32). The discriminating control is therefore the mapstructure key
// itself: rename `tunnel_max` to `tunnelmax` ⇒ the env name no longer maps and
// the row fails with `tunnel_max = 0s`.
func TestNodeRunnerConfig_EnvBindings(t *testing.T) {
	withOwnerCredentials(t)
	t.Setenv("APP_NODE_RUNNER_REGISTRY_HOST", "registry.example:8443")
	t.Setenv("APP_NODE_RUNNER_REGISTRY_USER", "puller")
	t.Setenv("APP_NODE_RUNNER_RUNS_VOLUME", "vol")
	t.Setenv("APP_NODE_RUNNER_RUNS_DIR", "/runs")
	t.Setenv("APP_NODE_RUNNER_UPLINK_NETWORK", "uplink")
	t.Setenv("APP_NODE_RUNNER_INVOCATION_CIDR", "10.99.0.0/16")
	t.Setenv("APP_NODE_RUNNER_INVOCATION_PREFIX_LEN", "30")
	t.Setenv("APP_NODE_RUNNER_SIDECAR_READY_TIMEOUT", "7s")
	t.Setenv("APP_NODE_RUNNER_PULL_TIMEOUT", "90s")
	t.Setenv("APP_NODE_RUNNER_TUNNEL_MAX", "200s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	nr := cfg.NodeRunner

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"registry_host", nr.RegistryHost, "registry.example:8443"},
		{"registry_user", nr.RegistryUser, "puller"},
		{"runs_volume", nr.RunsVolume, "vol"},
		{"runs_dir", nr.RunsDir, "/runs"},
		{"uplink_network", nr.UplinkNetwork, "uplink"},
		{"invocation_cidr", nr.InvocationCIDR, "10.99.0.0/16"},
		{"invocation_prefix_len", nr.InvocationPrefixLen, 30},
		{"sidecar_ready_timeout", nr.SidecarReadyTimeout, 7 * time.Second},
		{"pull_timeout", nr.PullTimeout, 90 * time.Second},
		{"tunnel_max", nr.TunnelMax, 200 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("node_runner.%s = %v, want %v (env binding not wired)", c.field, c.got, c.want)
		}
	}
}

// The refusal text is quoted verbatim in the spec and read by an operator at a
// failed boot; it names the ONE environment variable that fixes it.
func TestNodeRunnerConfig_RefusalNamesTheEnvVar(t *testing.T) {
	t.Setenv("APP_EXECUTOR_TYPE", "container")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded without a node runner registry host")
	}
	if !strings.Contains(err.Error(), "APP_NODE_RUNNER_REGISTRY_HOST") {
		t.Fatalf("refusal %q does not name the environment variable that fixes it", err.Error())
	}
}
