// Package http — customer-agent enrolment endpoint. §9.4 A47
// (2026-04-18).
//
// POST /customer-agent/cert accepts a PEM-encoded PKCS#10 CSR from a
// freshly-installed customer-agent and returns a signed certificate
// plus the signing CA cert in PEM form. The request must carry a
// bearer token matching the per-tenant enrolment token — minted by
// the admin portal and distributed out-of-band as part of the
// agent's initial setup.
//
// The CA key is loaded from AGENT_CA_KEY_PATH at request time so
// operators can rotate the key without restarting runtime-service.
// The endpoint fails-closed with 503 when either AGENT_CA_CERT_PATH or
// AGENT_CA_KEY_PATH is unset — enrolment never silently no-ops.
package http

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// CustomerAgentCertHandler signs CSRs submitted by customer-agent.
type CustomerAgentCertHandler struct {
	caCertPath  string
	caKeyPath   string
	tokenSource func() string // returns the currently valid enrolment bearer token
}

// NewCustomerAgentCertHandler constructs the handler. An empty
// caCertPath / caKeyPath leaves the endpoint in 503-fail-closed mode
// so bootstrap flows surface a clear error instead of silently
// signing with an unset CA.
func NewCustomerAgentCertHandler(caCertPath, caKeyPath string, tokenSource func() string) *CustomerAgentCertHandler {
	return &CustomerAgentCertHandler{caCertPath: caCertPath, caKeyPath: caKeyPath, tokenSource: tokenSource}
}

// RegisterRoutes mounts the enrolment endpoint at its canonical path.
func (h *CustomerAgentCertHandler) RegisterRoutes(r chi.Router) {
	r.Post("/customer-agent/cert", h.SignCSR)
}

type signCSRRequest struct {
	CSR string `json:"csr"`
}

type signCSRResponse struct {
	Cert string `json:"cert"`
	CA   string `json:"ca"`
}

// SignCSR decodes the PEM CSR, validates its signature, and issues a
// leaf cert valid for 90 days. Subject Alternative Names are copied
// from the CSR as-is; operators who want to force specific SANs can
// pre-sign the CSR out-of-band.
func (h *CustomerAgentCertHandler) SignCSR(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.caCertPath == "" || h.caKeyPath == "" {
		http.Error(w, "enrolment disabled: AGENT_CA_CERT_PATH/AGENT_CA_KEY_PATH unset", http.StatusServiceUnavailable)
		return
	}

	var req signCSRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	block, _ := pem.Decode([]byte(req.CSR))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		http.Error(w, "csr must be a PEM-encoded CERTIFICATE REQUEST", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		http.Error(w, "parse csr: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := csr.CheckSignature(); err != nil {
		http.Error(w, "csr signature invalid: "+err.Error(), http.StatusBadRequest)
		return
	}

	caCertPEM, err := os.ReadFile(h.caCertPath)
	if err != nil {
		http.Error(w, "read ca cert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		http.Error(w, "ca cert is not PEM", http.StatusInternalServerError)
		return
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		http.Error(w, "parse ca cert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	caKeyPEM, err := os.ReadFile(h.caKeyPath)
	if err != nil {
		http.Error(w, "read ca key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		http.Error(w, "ca key is not PEM", http.StatusInternalServerError)
		return
	}
	caKey, err := parsePrivateKey(caKeyBlock.Bytes)
	if err != nil {
		http.Error(w, "parse ca key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		http.Error(w, "serial: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   csr.Subject.CommonName,
			Organization: csr.Subject.Organization,
		},
		DNSNames:    csr.DNSNames,
		IPAddresses: csr.IPAddresses,
		NotBefore:   time.Now().Add(-1 * time.Minute),
		NotAfter:    time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		http.Error(w, "sign leaf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(signCSRResponse{
		Cert: string(leafPEM),
		CA:   string(caCertPEM),
	})
}

// authorize validates the Authorization header against the configured
// bearer token source. Empty expected token disables the check — for
// dev mode only; production deployments MUST wire a real token source.
func (h *CustomerAgentCertHandler) authorize(r *http.Request) bool {
	if h.tokenSource == nil {
		return true
	}
	expected := h.tokenSource()
	if expected == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(got, "Bearer ") == expected
}

// parsePrivateKey accepts both PKCS8 and PKCS1 blocks. Returns a
// crypto.Signer so the caller can pass it to x509.CreateCertificate.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if signer, ok := k.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, fmt.Errorf("pkcs8 key is not a signer: %T", k)
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, errors.New("unsupported key format (tried PKCS8, PKCS1, EC)")
}
