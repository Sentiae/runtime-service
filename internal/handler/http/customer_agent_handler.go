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
//
// Authorization fails closed in both directions: the route refuses to
// mount at all without a configured enrolment token (boot error, see
// ErrNoEnrolmentToken), and a request presenting no/empty/wrong token is
// denied at request time. This route is mounted OUTSIDE the JWT-protected
// group, so its bearer token is the only thing standing between an
// anonymous caller and a certificate signed by the agent CA.
package http

import (
	"crypto"
	"crypto/rand"
	"crypto/subtle"
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

// ErrNoEnrolmentToken reports that the enrolment endpoint was asked to
// mount without a usable enrolment token. It is a boot error, not a
// request error: this endpoint issues certificates from the agent CA,
// so mounting it with no credential would hand a signed identity to any
// unauthenticated caller (it lives outside the JWT-protected group).
var ErrNoEnrolmentToken = errors.New("customer-agent enrolment endpoint refused: AGENT_ENROLMENT_TOKEN is unset — this endpoint signs CSRs with the agent CA and is mounted outside the JWT group, so it must not run without its bearer token; set the token or unset AGENT_CA_CERT_PATH/AGENT_CA_KEY_PATH to leave enrolment disabled")

// NewCustomerAgentCertHandler constructs the handler. An empty
// caCertPath / caKeyPath leaves the endpoint in 503-fail-closed mode
// so bootstrap flows surface a clear error instead of silently
// signing with an unset CA.
//
// A nil tokenSource, or one that yields an empty token at construction,
// is ErrNoEnrolmentToken — the caller (DI) must refuse to boot rather
// than mount an unauthenticated signing endpoint. Mirrors
// MustPermissionChecker: the misconfiguration surfaces at boot, not as
// a mystery 401 on a route that looks healthy.
func NewCustomerAgentCertHandler(caCertPath, caKeyPath string, tokenSource func() string) (*CustomerAgentCertHandler, error) {
	if tokenSource == nil || tokenSource() == "" {
		return nil, ErrNoEnrolmentToken
	}
	return &CustomerAgentCertHandler{caCertPath: caCertPath, caKeyPath: caKeyPath, tokenSource: tokenSource}, nil
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
		// Deliberately uniform: the same message whether the token is
		// absent, empty, or wrong. A caller must not learn from the
		// response whether enrolment has a credential configured.
		http.Error(w, "unauthorized: a valid enrolment bearer token is required", http.StatusUnauthorized)
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
// bearer token source. Absence of a credential is NOT permission: a nil
// source, or a source that has since been rotated to empty, denies every
// request. The token is read per-request so operators can rotate it
// without a restart, which means the boot-time check in the constructor
// cannot be the only gate.
//
// The comparison is constant-time — this endpoint signs certificates
// with the agent CA, so a timing oracle on the token is a path to a
// forged identity.
func (h *CustomerAgentCertHandler) authorize(r *http.Request) bool {
	if h.tokenSource == nil {
		return false
	}
	expected := h.tokenSource()
	if expected == "" {
		return false
	}
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") {
		return false
	}
	presented := strings.TrimPrefix(got, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
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
