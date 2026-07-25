package http

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
	h, err := NewCustomerAgentCertHandler(caCertPath, caKeyPath, func() string { return "secret-tok" })
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
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
	h3, err := NewCustomerAgentCertHandler("", "", func() string { return "secret-tok" })
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	req3 := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader(payload))
	req3.Header.Set("Authorization", "Bearer secret-tok")
	w3 := httptest.NewRecorder()
	h3.SignCSR(w3, req3)
	if w3.Code != 503 {
		t.Fatalf("unset CA status=%d want 503", w3.Code)
	}
}

// TestCustomerAgentCertHandler_AuthorizeFailsClosed pins the fail-closed
// contract of the enrolment guard. This route sits outside the JWT group and
// signs CSRs with the agent CA, so absence of a credential must be a denial,
// never a bypass. The nil / empty cases are constructed directly because the
// constructor now refuses to build them — they model a token source rotated
// to empty after boot (the token is read per request).
func TestCustomerAgentCertHandler_AuthorizeFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		tokenSource func() string
		header      string
		want        bool
	}{
		{"nil source denies", nil, "Bearer anything", false},
		{"nil source denies even with no header", nil, "", false},
		{"empty token denies", func() string { return "" }, "Bearer anything", false},
		{"empty token denies empty bearer", func() string { return "" }, "Bearer ", false},
		{"wrong token denies", func() string { return "secret-tok" }, "Bearer wrong", false},
		{"missing header denies", func() string { return "secret-tok" }, "", false},
		{"non-bearer scheme denies", func() string { return "secret-tok" }, "Basic secret-tok", false},
		{"token prefix denies", func() string { return "secret-tok" }, "Bearer secret", false},
		{"correct token allows", func() string { return "secret-tok" }, "Bearer secret-tok", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &CustomerAgentCertHandler{tokenSource: tt.tokenSource}
			req := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if got := h.authorize(req); got != tt.want {
				t.Fatalf("authorize()=%v want %v", got, tt.want)
			}
		})
	}
}

// TestCustomerAgentCertHandler_UnauthorizedResponse checks the denial is a 401
// whose body does not reveal whether an enrolment token is configured.
func TestCustomerAgentCertHandler_UnauthorizedResponse(t *testing.T) {
	for _, src := range []func() string{nil, func() string { return "" }, func() string { return "tok" }} {
		h := &CustomerAgentCertHandler{caCertPath: "/nonexistent", caKeyPath: "/nonexistent", tokenSource: src}
		req := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Authorization", "Bearer nope")
		w := httptest.NewRecorder()
		h.SignCSR(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", w.Code)
		}
		body := w.Body.String()
		for _, leak := range []string{"AGENT_ENROLMENT_TOKEN", "unset", "not configured", "disabled"} {
			if strings.Contains(body, leak) {
				t.Fatalf("401 body leaks configuration state (%q): %s", leak, body)
			}
		}
	}
}

// TestNewCustomerAgentCertHandler_RefusesWithoutToken is the boot-level half of
// the fix: the DI container calls this constructor and log.Fatalf's on error,
// so a CA-configured-but-tokenless deployment refuses to boot instead of
// mounting a signing endpoint that is either open or silently unusable.
func TestNewCustomerAgentCertHandler_RefusesWithoutToken(t *testing.T) {
	tests := []struct {
		name        string
		tokenSource func() string
		wantErr     error
	}{
		{"nil source refuses boot", nil, ErrNoEnrolmentToken},
		{"empty token refuses boot", func() string { return "" }, ErrNoEnrolmentToken},
		{"configured token boots", func() string { return "secret-tok" }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := NewCustomerAgentCertHandler("/ca.crt", "/ca.key", tt.tokenSource)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && h != nil {
				t.Fatalf("handler returned alongside refusal: %+v", h)
			}
			if tt.wantErr == nil && h == nil {
				t.Fatal("no handler returned on valid config")
			}
		})
	}
}

// TestSetupRoutes_NoEnrolmentHandlerNoRoute confirms the mount is conditional:
// with no handler wired, POST /customer-agent/cert is not served at all (404),
// so a tokenless deployment never exposes the signing path even if a future
// caller skips the constructor's boot refusal.
func TestSetupRoutes_NoEnrolmentHandlerNoRoute(t *testing.T) {
	s := &Server{router: chi.NewRouter()}
	s.SetupRoutes()
	req := httptest.NewRequest(http.MethodPost, "/customer-agent/cert", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unmounted enrolment route status=%d want 404", w.Code)
	}
}
