// Package firecracker_customer speaks to a customer-hosted
// Firecracker HTTP control plane over mTLS. Closes §9.4 (C15) — the
// previous stub on vm_router.go short-circuited for every call; this
// package carries the real transport so the router can proxy Boot,
// Terminate, Pause, Resume, Snapshot, and Restore to the customer's
// own fleet.
//
// All configuration is path-based (cert, key, ca) so the service
// never handles cert bytes in memory outside of load time. Paths are
// read from env vars in NewClientFromEnv so Kubernetes / Nomad
// operators can mount secrets as files.
package firecracker_customer

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Config carries the paths the adapter needs to construct a mTLS
// http.Client. All three fields are required in production.
//
// Defaults land in NewClientFromEnv; tests pass Config directly.
type Config struct {
	// CertPath is the customer-issued client certificate in PEM.
	CertPath string
	// KeyPath is the matching private key in PEM.
	KeyPath string
	// CAPath bundles the customer's CA chain so the server's TLS
	// presentation can be verified.
	CAPath string
	// Timeout caps every outbound call. Zero → 30s.
	Timeout time.Duration
	// InsecureSkipVerify is *never* set by NewClientFromEnv and is
	// here only for local-dev tests using a self-signed test cert.
	InsecureSkipVerify bool
}

// envCertPath / envKeyPath / envCAPath document the env vars the task
// spec pins. Exported so tests can assert behaviour without duplicating
// the literals.
const (
	EnvCertPath = "APP_FIRECRACKER_CUSTOMER_CERT"
	EnvKeyPath  = "APP_FIRECRACKER_CUSTOMER_KEY"
	EnvCAPath   = "APP_FIRECRACKER_CUSTOMER_CA"
	EnvTimeout  = "APP_FIRECRACKER_CUSTOMER_TIMEOUT_SECONDS"
)

// NewClientFromEnv reads the three configuration env vars and
// returns a configured mTLS http.Client. Returns an error if any
// required env var is missing or the certificates fail to load.
func NewClientFromEnv() (*Client, error) {
	cfg := Config{
		CertPath: os.Getenv(EnvCertPath),
		KeyPath:  os.Getenv(EnvKeyPath),
		CAPath:   os.Getenv(EnvCAPath),
	}
	if raw := os.Getenv(EnvTimeout); raw != "" {
		d, err := time.ParseDuration(raw + "s")
		if err == nil && d > 0 {
			cfg.Timeout = d
		}
	}
	return NewClient(cfg)
}

// NewClient builds an mTLS http.Client from a Config and wraps it in
// the Client struct.
func NewClient(cfg Config) (*Client, error) {
	httpClient, err := buildMTLSHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{http: httpClient}, nil
}

// buildMTLSHTTPClient is factored so tests can drive the cert-loading
// path with fake PEM files without instantiating a full Client.
func buildMTLSHTTPClient(cfg Config) (*http.Client, error) {
	if cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("firecracker_customer: client cert + key paths required (set %s/%s)", EnvCertPath, EnvKeyPath)
	}
	if cfg.CAPath == "" {
		return nil, fmt.Errorf("firecracker_customer: CA path required (set %s)", EnvCAPath)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("firecracker_customer: load cert/key: %w", err)
	}
	caBytes, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("firecracker_customer: read ca: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("firecracker_customer: ca bundle contained no PEM certificates")
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caPool,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   timeout,
	}, nil
}
