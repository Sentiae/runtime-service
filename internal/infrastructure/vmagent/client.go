// Package vmagent — HTTP client for the customer-hosted vm-agent
// control-plane gateway (§9.4 AgentDeploymentTarget). This is the
// third deployment mode supported by VMRouterProvider:
//
//	sentiae_hosted   → local Firecracker provider
//	customer_hosted  → firecracker_customer HTTP adapter
//	agent            → this vmagent package
//
// The distinction from customer_hosted is intent: customer_hosted is a
// customer that runs the full Firecracker control API themselves;
// agent mode sits in front of their Firecracker (or other microVM
// runtime) and provides a thinner, auth-token-gated interface
// runtime-service can dial out to from an airgapped-enterprise
// posture. The contract is minimal:
//
//	POST  {base}/boot           -- start a VM, return socket/pid/ip
//	DELETE {base}/vms/{pid}      -- terminate (query param: socket)
//	POST  {base}/vms/{socket}/pause
//	POST  {base}/vms/{socket}/resume
//	POST  {base}/vms/{socket}/snapshots
//	POST  {base}/vms/{socket}/snapshots/restore
//	GET   {base}/vms/{ip}/metrics
//	DELETE {base}/snapshots
//
// Each call carries a Bearer token (provided by DeploymentTarget) so
// the agent can authenticate runtime-service without mutual TLS.
// Failures map to typed errors so upstream retry policy can react.
package vmagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// ErrAgentUnreachable signals a transport-level failure (DNS, TCP,
// TLS). Upstream retry logic should treat this as transient.
var ErrAgentUnreachable = errors.New("vmagent: agent unreachable")

// ErrAgentRejected signals a 4xx from the agent — the request itself
// is malformed or unauthorized. Not retryable.
var ErrAgentRejected = errors.New("vmagent: agent rejected request")

// ErrAgentInternal signals a 5xx from the agent. Retryable per the
// dispatcher's backoff policy.
var ErrAgentInternal = errors.New("vmagent: agent internal error")

// TokenProvider returns the bearer token for a given agent endpoint.
// Agents are identified by their base URL so one provider can serve
// multiple fleets; DI wires this from the secret store.
type TokenProvider interface {
	TokenFor(apiURL string) (string, error)
}

// StaticTokenProvider is the simplest TokenProvider — one token for
// every URL. Production wires a secret-store-backed provider; tests
// and single-tenant dev paths use this one.
type StaticTokenProvider struct {
	Token string
}

// TokenFor implements TokenProvider.
func (p StaticTokenProvider) TokenFor(_ string) (string, error) {
	if p.Token == "" {
		return "", fmt.Errorf("vmagent: no static token configured")
	}
	return p.Token, nil
}

// Client is the HTTP adapter. It implements the same
// usecase.CustomerFirecrackerClient surface as the customer firecracker
// client, so the VMRouter can treat both modes uniformly.
type Client struct {
	http   *http.Client
	tokens TokenProvider
}

// NewClient wires the client with sane defaults. Callers can pass a
// preconfigured http.Client to customise TLS; nil falls back to a
// conservative 60s timeout.
func NewClient(httpClient *http.Client, tokens TokenProvider) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if tokens == nil {
		tokens = StaticTokenProvider{}
	}
	return &Client{http: httpClient, tokens: tokens}
}

// Boot issues POST {apiURL}/boot and decodes into a VMBootResult.
func (c *Client) Boot(ctx context.Context, apiURL string, config usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	body, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("vmagent: marshal boot config: %w", err)
	}
	var out usecase.VMBootResult
	if err := c.do(ctx, http.MethodPost, joinURL(apiURL, "/boot"), apiURL, bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Terminate stops the VM and releases its resources.
func (c *Client) Terminate(ctx context.Context, apiURL, socketPath string, pid int) error {
	url := joinURL(apiURL, fmt.Sprintf("/vms/%d", pid))
	url = appendQuery(url, "socket", socketPath)
	return c.do(ctx, http.MethodDelete, url, apiURL, nil, nil)
}

// Pause issues a pause signal to the named VM socket.
func (c *Client) Pause(ctx context.Context, apiURL, socketPath string) error {
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/pause"), apiURL, nil, nil)
}

// Resume issues a resume signal to the named VM socket.
func (c *Client) Resume(ctx context.Context, apiURL, socketPath string) error {
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/resume"), apiURL, nil, nil)
}

// CreateSnapshot snapshots the VM and returns the recorded paths.
func (c *Client) CreateSnapshot(ctx context.Context, apiURL, socketPath string, snapshotID uuid.UUID) (*usecase.SnapshotResult, error) {
	body, err := json.Marshal(map[string]any{"snapshot_id": snapshotID.String()})
	if err != nil {
		return nil, fmt.Errorf("vmagent: marshal snapshot request: %w", err)
	}
	var out usecase.SnapshotResult
	if err := c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/snapshots"), apiURL, bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreSnapshot restores the VM from a previously captured pair.
func (c *Client) RestoreSnapshot(ctx context.Context, apiURL, socketPath, memPath, statePath string) error {
	body, err := json.Marshal(map[string]any{
		"memory_file_path": memPath,
		"state_file_path":  statePath,
	})
	if err != nil {
		return fmt.Errorf("vmagent: marshal restore request: %w", err)
	}
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/snapshots/restore"), apiURL, bytes.NewReader(body), nil)
}

// CollectMetrics fetches periodic usage metrics from a running VM.
func (c *Client) CollectMetrics(ctx context.Context, apiURL, ip string) (*usecase.VMMetrics, error) {
	var out usecase.VMMetrics
	if err := c.do(ctx, http.MethodGet, joinURL(apiURL, "/vms/"+ip+"/metrics"), apiURL, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSnapshotFiles asks the agent to remove the snapshot files on
// its local filesystem. Synchronous; kept on the client interface so
// runtime-service's cleanup loops don't have to know which mode a VM
// was booted in.
func (c *Client) DeleteSnapshotFiles(apiURL, memPath, statePath string) error {
	body, err := json.Marshal(map[string]any{
		"memory_file_path": memPath,
		"state_file_path":  statePath,
	})
	if err != nil {
		return fmt.Errorf("vmagent: marshal delete request: %w", err)
	}
	// No request ctx available on this legacy surface — fall back to a
	// short timeout so a wedged agent doesn't block the caller.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodDelete, joinURL(apiURL, "/snapshots"), apiURL, bytes.NewReader(body), nil)
}

// do is the shared request dispatch. It sets the bearer token, routes
// the status code into a typed error, and decodes the response body
// when out is non-nil.
func (c *Client) do(ctx context.Context, method, url, apiURL string, body io.Reader, out any) error {
	token, err := c.tokens.TokenFor(apiURL)
	if err != nil {
		return fmt.Errorf("vmagent: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("vmagent: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrAgentUnreachable, err.Error())
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			return nil
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("vmagent: read body: %w", err)
		}
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("vmagent: decode body: %w", err)
		}
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: %s: %s", ErrAgentRejected, resp.Status, strings.TrimSpace(string(raw)))
	case resp.StatusCode >= 500:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: %s: %s", ErrAgentInternal, resp.Status, strings.TrimSpace(string(raw)))
	default:
		return fmt.Errorf("vmagent: unexpected status %d", resp.StatusCode)
	}
}

// Compile-time check: Client satisfies the same surface the router
// expects for customer-hosted targets.
var _ usecase.CustomerFirecrackerClient = (*Client)(nil)

// joinURL concatenates a base URL and path ensuring exactly one slash.
func joinURL(base, path string) string {
	if base == "" {
		return path
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// appendQuery adds ?k=v or &k=v depending on what's already in the URL.
func appendQuery(url, key, value string) string {
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + key + "=" + value
}
