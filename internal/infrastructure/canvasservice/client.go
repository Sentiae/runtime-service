// Package canvasservice is a narrow HTTP client that runtime-service
// uses to push node-scoped state changes into canvas-service alongside
// the durable Kafka fan-out. It closes §19.1 flow 1E — "a test node
// shows green/red based on its last execution, in real time" — without
// waiting on Kafka consumer lag.
package canvasservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Client posts node-scoped state changes to canvas-service. The client
// is optional — runtime-service falls back to Kafka-only delivery when
// the canvas URL is empty.
type Client struct {
	baseURL    string
	httpClient *http.Client
	// ServiceToken is the inter-service bearer token. Canvas-service's
	// AuthMiddleware accepts it alongside the X-User-ID header. Empty
	// in tests + dev deployments that bypass auth.
	ServiceToken string
	// ServiceUserID is the pseudo-user-id runtime acts as. Required by
	// canvas-service's current auth middleware.
	ServiceUserID string
}

// NewClient builds a Client. Callers that want to disable the push
// path entirely should pass an empty baseURL — the helpers below then
// short-circuit with a logged warning.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// TestResultPayload mirrors the canvas-service NodeReceiversHandler
// testResultRequest. Kept as a separate exported type so callers
// don't have to rebuild the field map.
type TestResultPayload struct {
	RunID       string   `json:"run_id"`
	Status      string   `json:"status"`
	Passing     int      `json:"passing"`
	Failed      int      `json:"failed,omitempty"`
	Skipped     int      `json:"skipped,omitempty"`
	Total       int      `json:"total"`
	CoveragePct *float64 `json:"coverage_pct,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
}

// ApplyTestResult pushes a terminal test-run verdict onto the target
// canvas node. Returns nil on success and on any transport error —
// the Kafka path is authoritative; this push is best-effort.
func (c *Client) ApplyTestResult(ctx context.Context, canvasID, nodeID uuid.UUID, payload TestResultPayload) error {
	if c == nil || c.baseURL == "" {
		return nil
	}
	url := fmt.Sprintf("%s/api/v1/canvases/%s/nodes/%s/test-result", c.baseURL, canvasID, nodeID)
	return c.post(ctx, url, payload)
}

// post marshals + POSTs the payload and swallows non-2xx into a log
// line so the caller isn't forced to unwind on best-effort calls.
func (c *Client) post(ctx context.Context, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("canvasservice: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("canvasservice: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.ServiceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.ServiceToken)
	}
	if c.ServiceUserID != "" {
		req.Header.Set("X-User-ID", c.ServiceUserID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("[canvasservice] POST %s failed: %v", url, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		log.Printf("[canvasservice] POST %s returned %d: %s", url, resp.StatusCode, string(respBody))
		return fmt.Errorf("canvasservice: %s returned %d", url, resp.StatusCode)
	}
	return nil
}
