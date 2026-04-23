package firecracker_customer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// generateTestCert writes a throwaway self-signed ECDSA cert + key
// pair so the config loader can be exercised without baking real
// PEM fixtures into the repo. §9.4 (C15).
func generateTestCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign cert: %v", err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	_ = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	_ = keyOut.Close()
	return certPath, keyPath
}

func TestNewClient_LoadsMTLSMaterialFromPaths(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, "client")
	// CA bundle uses the same self-signed cert as both client and CA.
	caPath := certPath

	client, err := NewClient(Config{
		CertPath: certPath,
		KeyPath:  keyPath,
		CAPath:   caPath,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	httpClient := client.HTTPClient()
	if httpClient == nil {
		t.Fatal("HTTPClient() returned nil")
	}
	if httpClient.Timeout != 5*time.Second {
		t.Fatalf("timeout not applied: %v", httpClient.Timeout)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatal("expected *http.Transport with TLS config")
	}
	if len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("expected 1 client certificate, got %d", len(transport.TLSClientConfig.Certificates))
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected non-nil RootCAs pool")
	}
	if transport.TLSClientConfig.MinVersion != 0 && transport.TLSClientConfig.MinVersion < 0x0303 {
		t.Fatalf("MinVersion < TLS 1.2: %x", transport.TLSClientConfig.MinVersion)
	}
}

func TestNewClient_MissingEnvVarsErrors(t *testing.T) {
	_, err := NewClient(Config{})
	if err == nil {
		t.Fatal("expected error for empty Config")
	}
}

func TestNewClientFromEnv_ReadsPaths(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateTestCert(t, dir, "from-env")
	t.Setenv(EnvCertPath, certPath)
	t.Setenv(EnvKeyPath, keyPath)
	t.Setenv(EnvCAPath, certPath)
	t.Setenv(EnvTimeout, "7")

	client, err := NewClientFromEnv()
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if client.HTTPClient().Timeout != 7*time.Second {
		t.Fatalf("timeout env not applied: %v", client.HTTPClient().Timeout)
	}
}

// TestClient_BootDispatchesOverHTTP exercises the request shape on a
// non-TLS httptest.Server — we swap the client's http.Client with the
// default so we don't need to wire a trust chain for the inner test.
// The TLS plumbing is covered by TestNewClient_LoadsMTLSMaterialFromPaths.
func TestClient_BootDispatchesOverHTTP(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var cfg usecase.VMBootConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(usecase.VMBootResult{PID: 42, IPAddress: "10.0.0.1"})
	}))
	defer server.Close()

	client := &Client{http: server.Client()}
	result, err := client.Boot(context.Background(), server.URL, usecase.VMBootConfig{VMID: uuid.New()})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if gotPath != "/vms" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if result.PID != 42 {
		t.Fatalf("unexpected PID: %d", result.PID)
	}
}

func TestClient_TerminateFailsOn5xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &Client{http: server.Client()}
	if err := client.Terminate(context.Background(), server.URL, "/tmp/sock", 7); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestClient_NotConfiguredNilSafety(t *testing.T) {
	// A zero-value Client (no http) must not panic; every method
	// should return an error instead so operators see a clear
	// message when env vars weren't set.
	var c Client
	if err := c.Pause(context.Background(), "http://x", "sock"); err == nil {
		t.Fatal("expected error from uninitialized client")
	}
}
