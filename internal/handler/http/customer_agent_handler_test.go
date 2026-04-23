package http

import (
	"bytes"
	"crypto/ed25519"
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
)

// TestCustomerAgentCertHandler_SignsCSR verifies the full CSR →
// signed cert round-trip through the handler, including bearer token
// enforcement and the failure modes when the CA files are missing.
func TestCustomerAgentCertHandler_SignsCSR(t *testing.T) {
	dir := t.TempDir()

	// Seed a toy CA on disk.
	_, caKey, _ := ed25519.GenerateKey(rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sentiae-test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCertPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caCertPath, caCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER})
	caKeyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// Build an agent CSR.
	_, agentKey, _ := ed25519.GenerateKey(rand.Reader)
	csrTpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "agent.example.com"},
		SignatureAlgorithm: x509.PureEd25519,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTpl, agentKey)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// Happy path with bearer token.
	h := NewCustomerAgentCertHandler(caCertPath, caKeyPath, func() string { return "secret-tok" })
	payload, _ := json.Marshal(signCSRRequest{CSR: string(csrPEM)})
	req := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer secret-tok")
	w := httptest.NewRecorder()
	h.SignCSR(w, req)
	if w.Code != 200 {
		t.Fatalf("happy path status=%d body=%s", w.Code, w.Body.String())
	}
	var out signCSRResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	block, _ := pem.Decode([]byte(out.Cert))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse signed cert: %v", err)
	}
	if leaf.Subject.CommonName != "agent.example.com" {
		t.Fatalf("leaf CN=%s want agent.example.com", leaf.Subject.CommonName)
	}

	// Wrong bearer — 401.
	req2 := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader(payload))
	req2.Header.Set("Authorization", "Bearer wrong")
	w2 := httptest.NewRecorder()
	h.SignCSR(w2, req2)
	if w2.Code != 401 {
		t.Fatalf("wrong bearer status=%d want 401", w2.Code)
	}

	// CA paths unset — 503 even with valid bearer.
	h3 := NewCustomerAgentCertHandler("", "", func() string { return "secret-tok" })
	req3 := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader(payload))
	req3.Header.Set("Authorization", "Bearer secret-tok")
	w3 := httptest.NewRecorder()
	h3.SignCSR(w3, req3)
	if w3.Code != 503 {
		t.Fatalf("unset CA status=%d want 503", w3.Code)
	}
}
