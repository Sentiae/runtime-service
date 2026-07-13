package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteRuntimeJSONThreadsBootstrapNonce asserts the per-boot vsock
// attestation nonce (D-085 Layer 2) the host mints is written into runtime.json
// so the guest can require the pusher to present it. The nonce is NOT a secret
// (it authenticates the pusher, not confidentiality) — it is the one value that
// makes the host<->guest push unspoofable.
func TestWriteRuntimeJSONThreadsBootstrapNonce(t *testing.T) {
	staging := t.TempDir()
	const nonce = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	req := MaterializeRequest{
		WorkDir:        staging,
		Mode:           "resident",
		Port:           8080,
		ExpectSecrets:  true,
		BootstrapNonce: nonce,
	}
	cfg := ImageConfig{Entrypoint: []string{"/app/server"}, WorkingDir: "/app"}

	if err := writeRuntimeJSON(staging, req, cfg); err != nil {
		t.Fatalf("writeRuntimeJSON: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(staging, "sentiae", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}
	var spec runtimeSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("unmarshal runtime spec: %v", err)
	}
	if spec.BootstrapNonce != nonce {
		t.Fatalf("runtime.json bootstrap_nonce = %q, want %q", spec.BootstrapNonce, nonce)
	}
	if !spec.ExpectSecrets {
		t.Fatal("expect_secrets = false, want true")
	}
}

// TestWriteRuntimeJSONOmitsNonceWhenNoSecrets asserts a secret-less boot writes
// no nonce (the field is omitempty) — the handshake change is invisible to boots
// that expect no secrets.
func TestWriteRuntimeJSONOmitsNonceWhenNoSecrets(t *testing.T) {
	staging := t.TempDir()
	req := MaterializeRequest{WorkDir: staging, Mode: "resident", Port: 8080}
	cfg := ImageConfig{Entrypoint: []string{"/app/server"}, WorkingDir: "/app"}

	if err := writeRuntimeJSON(staging, req, cfg); err != nil {
		t.Fatalf("writeRuntimeJSON: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(staging, "sentiae", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}
	var spec runtimeSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("unmarshal runtime spec: %v", err)
	}
	if spec.BootstrapNonce != "" {
		t.Fatalf("bootstrap_nonce = %q on a secret-less boot, want empty", spec.BootstrapNonce)
	}
}
