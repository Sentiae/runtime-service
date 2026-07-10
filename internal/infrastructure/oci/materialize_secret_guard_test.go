package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRuntimeJSONHasNoSecretChannel locks the invariant that the materialized
// rootfs runtime.json carries ONLY non-secret entrypoint env. secret_refs (P14)
// is the sole secret channel and it is blocked before boot; runtime.json at rest
// must never become a secret-bearing channel. Guard test only — no new behavior.
func TestRuntimeJSONHasNoSecretChannel(t *testing.T) {
	// runtimeSpec is what lands on disk. It must expose no secret-VALUE-bearing
	// field: there is no inject-as-env secret path in the image-boot codepath.
	// A boolean flag (e.g. ExpectSecrets — the vsock-listener toggle) carries no
	// value and is permitted; only string/slice/map secret fields would be a
	// plaintext-at-rest channel and are forbidden.
	specType := reflect.TypeOf(runtimeSpec{})
	for i := 0; i < specType.NumField(); i++ {
		f := specType.Field(i)
		name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
		if strings.Contains(name, "secret") && isValueBearing(f.Type) {
			t.Fatalf("runtimeSpec exposes a secret-value-bearing field %q — runtime.json must carry no secret channel", f.Name)
		}
	}

	// MaterializeRequest is the descriptor input. env_vars must be plain descriptor
	// env only; there must be no secret-value field feeding the materialize path.
	reqType := reflect.TypeOf(MaterializeRequest{})
	for i := 0; i < reqType.NumField(); i++ {
		f := reqType.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "secret") && isValueBearing(f.Type) {
			t.Fatalf("MaterializeRequest exposes a secret-value field %q — secrets must not flow through env_vars", f.Name)
		}
	}
}

// isValueBearing reports whether t could carry a secret PLAINTEXT (string, byte
// slice, or a collection thereof). A bool/int flag cannot, so a boolean toggle
// like ExpectSecrets is not a secret channel.
func isValueBearing(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map, reflect.Struct, reflect.Ptr, reflect.Interface:
		return true
	default:
		return false
	}
}

// TestWriteRuntimeJSONEnvIsNonSecretOnly asserts the writer materializes env from
// image Env + descriptor env_vars only (later wins), never a secret binding, and
// pins the at-rest file mode to 0600.
func TestWriteRuntimeJSONEnvIsNonSecretOnly(t *testing.T) {
	staging := t.TempDir()
	req := MaterializeRequest{
		WorkDir: staging,
		EnvVars: map[string]string{"FEATURE_FLAG": "on", "PORT": "8080"},
		Mode:    "resident",
		Port:    8080,
	}
	cfg := ImageConfig{
		Entrypoint: []string{"/app/server"},
		Env:        []string{"PATH=/usr/bin"},
		WorkingDir: "/app",
	}

	if err := writeRuntimeJSON(staging, req, cfg); err != nil {
		t.Fatalf("writeRuntimeJSON: %v", err)
	}

	path := filepath.Join(staging, "sentiae", "runtime.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat runtime.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("runtime.json mode = %o, want 0600 (env at rest must be owner-only)", perm)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}

	var spec runtimeSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("unmarshal runtime spec: %v", err)
	}

	want := []string{"PATH=/usr/bin", "FEATURE_FLAG=on", "PORT=8080"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("spec.Env = %v, want %v (image env + sorted descriptor env, no secret injection)", spec.Env, want)
	}
}
