package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AgentRunRequest mirrors the guest-agent's runRequest (cmd/guest-agent/main.go).
// The host POSTs this to the in-guest agent's /run endpoint. Field tags match the
// agent exactly so the JSON contract is identical across cold and warm execution.
type AgentRunRequest struct {
	Language  string `json:"language"`
	Code      string `json:"code"`
	Stdin     string `json:"stdin"`
	TimeoutMS int    `json:"timeout_ms"`
}

// AgentRunResult mirrors the guest-agent's runResponse. Error is set when the
// agent could not run the program (decode failure, unsupported language, timeout).
type AgentRunResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// AgentClient talks to the in-guest execution agent over plain HTTP. The agent
// listens on <guestIP>:8000 (reachable on the per-VM /30 for warm VMs, or via
// the clone's host-routed 10.200.N.2 address). It is a thin transport: callers
// pass a fully-formed "host:port" endpoint and the client builds the URL.
type AgentClient struct {
	http *http.Client
}

// NewAgentClient builds an AgentClient with the given per-request timeout. A
// zero timeout disables the client-level deadline (the agent enforces its own
// run timeout via AgentRunRequest.TimeoutMS).
func NewAgentClient(timeout time.Duration) *AgentClient {
	return &AgentClient{http: &http.Client{Timeout: timeout}}
}

// RunCode POSTs the request to http://<endpoint>/run and decodes the result.
// endpoint is "host:port" (e.g. "10.200.3.2:8000"). A non-2xx response is an
// error carrying the body; a 2xx response with AgentRunResult.Error set is the
// agent reporting an execution-level failure (returned to the caller as-is).
func (c *AgentClient) RunCode(ctx context.Context, endpoint string, req AgentRunRequest) (AgentRunResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return AgentRunResult{}, fmt.Errorf("marshal run request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+endpoint+"/run", bytes.NewReader(body))
	if err != nil {
		return AgentRunResult{}, fmt.Errorf("create run request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return AgentRunResult{}, fmt.Errorf("post run request to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return AgentRunResult{}, fmt.Errorf("agent /run returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result AgentRunResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return AgentRunResult{}, fmt.Errorf("decode run response: %w", err)
	}
	return result, nil
}

// WaitReady polls GET http://<endpoint>/healthz until it returns 200 or the
// timeout elapses. The warm-boot, snapshot-restore, and clone paths all gate on
// this before sending the first /run. Honors ctx cancellation.
func (c *AgentClient) WaitReady(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	url := "http://" + endpoint + "/healthz"
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for agent readiness at %s", endpoint)
		case <-ticker.C:
			if c.probe(ctx, url) {
				return nil
			}
		}
	}
}

// probe issues a single GET and reports whether the agent answered 200.
func (c *AgentClient) probe(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
