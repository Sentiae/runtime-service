package firecracker_customer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// Client wraps a preconfigured mTLS http.Client and implements
// usecase.CustomerFirecrackerClient by issuing HTTP calls to the
// customer-hosted Firecracker control plane. §9.4 (C15).
//
// The customer API shape mirrors Sentiae's internal control plane:
//
//	POST  {base}/vms                 -- boot
//	DELETE {base}/vms/{pid}          -- terminate
//	POST  {base}/vms/{socket}/pause
//	POST  {base}/vms/{socket}/resume
//	POST  {base}/vms/{socket}/snapshots
//	POST  {base}/vms/{socket}/snapshots/restore
//	GET   {base}/vms/{ip}/metrics
//	DELETE {base}/snapshots          -- delete mem/state files
//
// The client is transport-agnostic beyond mTLS; the caller supplies
// the customer's base URL per-call via apiURL so one client can serve
// multiple customer fleets.
type Client struct {
	http *http.Client
}

// HTTPClient exposes the underlying http.Client for tests that want
// to assert TLS configuration was applied correctly.
func (c *Client) HTTPClient() *http.Client {
	return c.http
}

// Boot issues POST {apiURL}/vms with the boot config and decodes the
// response into a VMBootResult. 5xx and transport errors surface to
// the VMRouter which is responsible for deciding whether to fail the
// caller or retry.
func (c *Client) Boot(ctx context.Context, apiURL string, config usecase.VMBootConfig) (*usecase.VMBootResult, error) {
	body, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("firecracker_customer: marshal boot config: %w", err)
	}
	var out usecase.VMBootResult
	if err := c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms"), bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Terminate deletes the VM row matching pid + socket. The socket is
// passed as a query param so the server can correlate without us
// exposing file-system paths in the URL path.
func (c *Client) Terminate(ctx context.Context, apiURL, socketPath string, pid int) error {
	url := joinURL(apiURL, fmt.Sprintf("/vms/%d", pid))
	url = appendQuery(url, "socket", socketPath)
	return c.do(ctx, http.MethodDelete, url, nil, nil)
}

// Pause issues a pause signal to the named VM socket.
func (c *Client) Pause(ctx context.Context, apiURL, socketPath string) error {
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/pause"), nil, nil)
}

// Resume issues a resume signal to the named VM socket.
func (c *Client) Resume(ctx context.Context, apiURL, socketPath string) error {
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/resume"), nil, nil)
}

// CreateSnapshot asks the customer API to snapshot the VM and
// returns the mem/state file paths it chose.
func (c *Client) CreateSnapshot(ctx context.Context, apiURL, socketPath string, snapshotID uuid.UUID) (*usecase.SnapshotResult, error) {
	body, err := json.Marshal(map[string]any{"snapshot_id": snapshotID.String()})
	if err != nil {
		return nil, fmt.Errorf("firecracker_customer: marshal snapshot request: %w", err)
	}
	var out usecase.SnapshotResult
	if err := c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/snapshots"), bytes.NewReader(body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreSnapshot restores a VM from a previously captured snapshot.
func (c *Client) RestoreSnapshot(ctx context.Context, apiURL, socketPath, memPath, statePath string) error {
	body, err := json.Marshal(map[string]any{
		"memory_file_path": memPath,
		"state_file_path":  statePath,
	})
	if err != nil {
		return fmt.Errorf("firecracker_customer: marshal restore request: %w", err)
	}
	return c.do(ctx, http.MethodPost, joinURL(apiURL, "/vms/"+socketPath+"/snapshots/restore"), bytes.NewReader(body), nil)
}

// CollectMetrics pulls the live VM metrics for the given IP.
func (c *Client) CollectMetrics(ctx context.Context, apiURL, ip string) (*usecase.VMMetrics, error) {
	var out usecase.VMMetrics
	if err := c.do(ctx, http.MethodGet, joinURL(apiURL, "/vms/"+ip+"/metrics"), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSnapshotFiles is the synchronous file-cleanup call. Unlike
// the other methods it does not take a context — the interface
// matches the local VMProvider which has the same signature.
func (c *Client) DeleteSnapshotFiles(apiURL, memPath, statePath string) error {
	body, err := json.Marshal(map[string]any{
		"memory_file_path": memPath,
		"state_file_path":  statePath,
	})
	if err != nil {
		return fmt.Errorf("firecracker_customer: marshal delete request: %w", err)
	}
	return c.do(context.Background(), http.MethodDelete, joinURL(apiURL, "/snapshots"), bytes.NewReader(body), nil)
}

// do is the single transport seam; every public method routes
// through it so retries / instrumentation can be added centrally.
func (c *Client) do(ctx context.Context, method, url string, body io.Reader, decodeInto any) error {
	if c == nil || c.http == nil {
		return fmt.Errorf("firecracker_customer: client not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("firecracker_customer: build request %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker_customer: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker_customer: %s %s returned %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	if decodeInto == nil {
		// Drain body so keep-alives survive.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(decodeInto)
}

// joinURL is a small URL-joiner that skips the pitfalls of
// path.Join (which collapses "https://").
func joinURL(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return base + suffix
}

// appendQuery appends "?k=v" (or "&k=v") to url. Keeps the client
// free of a url.URL parse on the hot path.
func appendQuery(url, key, value string) string {
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + key + "=" + value
}

// Compile-time assertion that Client satisfies the usecase contract.
var _ usecase.CustomerFirecrackerClient = (*Client)(nil)
