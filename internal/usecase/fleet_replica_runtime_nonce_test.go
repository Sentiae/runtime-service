package usecase

import (
	"context"
	"encoding/hex"
	"testing"
)

// capturingMaterializer records the ImageMaterializeInput so a test can assert
// what the boot wrote into runtime.json (the guest's expected nonce).
type capturingMaterializer struct {
	rootfs string
	last   *ImageMaterializeInput
}

func (m *capturingMaterializer) Materialize(_ context.Context, in ImageMaterializeInput) (ImageMaterializeOutput, error) {
	cp := in
	m.last = &cp
	return ImageMaterializeOutput{RootfsPath: m.rootfs}, nil
}

// TestBootReplica_BootstrapNonceThreadedToBothLegs locks the D-085 Layer-2
// property: when a boot expects secrets, one fresh, unpredictable per-boot nonce
// is written into runtime.json (materialize input) AND presented on the vsock
// push (boot input) — and the two are the SAME value. A guest that requires the
// runtime.json nonce therefore only accepts the legitimate push.
func TestBootReplica_BootstrapNonceThreadedToBothLegs(t *testing.T) {
	app := newTestApp()
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	mat := &capturingMaterializer{rootfs: "/work/rep/rootfs.ext4"}
	booter := &recordingBooter{resident: ImageResidentResult{PID: 1, GuestIP: "10.0.0.5", HostPort: 20001}}
	uc := newTestReplicaRuntime(t, mat, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")
	uc.SetSecretSelfTest(true) // ExpectSecrets without needing a resolver

	if err := uc.BootReplica(context.Background(), rep.ID); err != nil {
		t.Fatalf("BootReplica: %v", err)
	}
	if mat.last == nil || booter.bootInput == nil {
		t.Fatal("materialize or boot not called")
	}
	if !booter.bootInput.ExpectSecrets {
		t.Fatal("ExpectSecrets = false, want true (self-test marker)")
	}

	nonce := mat.last.BootstrapNonce
	if nonce == "" {
		t.Fatal("runtime.json nonce empty — guest could not attest the pusher")
	}
	if booter.bootInput.BootstrapNonce != nonce {
		t.Fatalf("push nonce %q != runtime.json nonce %q — guest would reject the legit push",
			booter.bootInput.BootstrapNonce, nonce)
	}
	// Fresh + unpredictable: 32 crypto/rand bytes hex-encoded.
	if raw, err := hex.DecodeString(nonce); err != nil || len(raw) != bootstrapNonceBytes {
		t.Fatalf("nonce %q not %d-byte hex (err=%v)", nonce, bootstrapNonceBytes, err)
	}
}

// TestBootReplica_NoNonceWhenNoSecrets asserts a boot with no secrets carries no
// nonce on either leg (the handshake change is invisible to secret-less boots).
func TestBootReplica_NoNonceWhenNoSecrets(t *testing.T) {
	app := newTestApp()
	app.SecretRefs = nil
	rep := newTestReplica(app.ID)
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), rep)

	mat := &capturingMaterializer{rootfs: "/work/rep/rootfs.ext4"}
	booter := &recordingBooter{resident: ImageResidentResult{PID: 1, GuestIP: "10.0.0.5", HostPort: 20001}}
	uc := newTestReplicaRuntime(t, mat, booter, replicas, &rtAppRepo{app: app}, "/tmp/imgwork", "10.0.0.9")

	if err := uc.BootReplica(context.Background(), rep.ID); err != nil {
		t.Fatalf("BootReplica: %v", err)
	}
	if mat.last == nil || booter.bootInput == nil {
		t.Fatal("materialize or boot not called")
	}
	if booter.bootInput.ExpectSecrets {
		t.Fatal("ExpectSecrets = true, want false (no secrets)")
	}
	if mat.last.BootstrapNonce != "" || booter.bootInput.BootstrapNonce != "" {
		t.Fatalf("nonce present on a secret-less boot: matIn=%q bootIn=%q",
			mat.last.BootstrapNonce, booter.bootInput.BootstrapNonce)
	}
}

// TestNewBootstrapNonceIsFreshPerCall asserts the nonce is per-boot fresh (two
// successive mints differ) and correctly sized — a predictable/repeated nonce
// would let a spoofer replay a push.
func TestNewBootstrapNonceIsFreshPerCall(t *testing.T) {
	a, err := newBootstrapNonce()
	if err != nil {
		t.Fatalf("newBootstrapNonce: %v", err)
	}
	b, err := newBootstrapNonce()
	if err != nil {
		t.Fatalf("newBootstrapNonce: %v", err)
	}
	if a == b {
		t.Fatal("two nonces are equal — not per-boot fresh")
	}
	for _, n := range []string{a, b} {
		raw, derr := hex.DecodeString(n)
		if derr != nil || len(raw) != bootstrapNonceBytes {
			t.Fatalf("nonce %q not %d-byte hex (err=%v)", n, bootstrapNonceBytes, derr)
		}
	}
}
