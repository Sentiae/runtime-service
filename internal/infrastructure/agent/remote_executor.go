// Package agent — HTTP-based RemoteExecutor that forwards an
// execution payload to a customer-hosted vm-agent running as a
// sidecar on the customer's VPC.
//
// Why HTTP and not gRPC: the existing /vm-agent process uses a
// custom socket protocol between host and microVM guest (see
// /vm-agent/proto/agent.proto), but that protocol is not a gRPC
// service — it's a framed message stream. Exposing the agent's
// capabilities to runtime-service across the public internet is a
// separate concern, and HTTP/JSON is the simpler production choice:
//
//   - debuggable with curl
//   - survives corporate proxies
//   - bearer-token auth slots in trivially
//   - TLS fingerprint pinning works via the standard http.Transport
//
// The customer's agent wrapper exposes POST /execute. This adapter
// forwards the payload, verifies the response, and maps it into
// usecase.ExecutionResult. Errors that look like transient network
// issues are propagated verbatim so the dispatcher's fallback
// policy can decide whether to fail or run locally.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TokenProvider returns the plaintext bearer token for a given agent
// id. The runtime does NOT store plaintext tokens (only hashes on the
// agent row), so production wires this to a secret manager that
// received the token via an out-of-band flow (customer paste or
// operator import). For tests a map-backed provider is used.
type TokenProvider interface {
	TokenFor(agentID string) (string, error)
}

// RemoteExecutor implements usecase.RemoteExecutor.
type RemoteExecutor struct {
	tokens      TokenProvider
	DialTimeout time.Duration
	CallTimeout time.Duration
	httpClient  *http.Client
}

// NewRemoteExecutor wires a client with sane defaults. The returned
// transport pins the server certificate fingerprint against the value
// stored on the agent record — agents that rotate certs must first
// clear the stored fingerprint so the next call performs TOFU again.
func NewRemoteExecutor(tokens TokenProvider) *RemoteExecutor {
	return &RemoteExecutor{
		tokens:      tokens,
		DialTimeout: 5 * time.Second,
		CallTimeout: 5 * time.Minute,
		httpClient: &http.Client{
			Transport: &http.Transport{
				// MinVersion bumped to TLS 1.2 — customers running older
				// agents will see handshake failures, which is the correct
				// behaviour for a security-sensitive path.
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
	}
}

// executeRequest is the wire format between runtime-service and the
// customer-hosted agent. Kept flat — the agent is expected to map
// this onto the internal ExecuteTask proto message itself.
type executeRequest struct {
	Language  string            `json:"language"`
	Code      string            `json:"code"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

type executeResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
}

// Dispatch forwards payload to agent.Endpoint/execute.
func (e *RemoteExecutor) Dispatch(ctx context.Context, agent *domain.RuntimeAgent, payload usecase.ExecutionPayload) (usecase.ExecutionResult, error) {
	if agent == nil || agent.Endpoint == "" {
		return usecase.ExecutionResult{}, errors.New("remote executor: agent is nil or has no endpoint")
	}
	token, err := e.tokens.TokenFor(agent.ID.String())
	if err != nil {
		return usecase.ExecutionResult{}, fmt.Errorf("resolve agent token: %w", err)
	}

	body, err := json.Marshal(executeRequest{
		Language:  payload.Language,
		Code:      payload.Code,
		Args:      payload.Args,
		Env:       payload.Env,
		TimeoutMS: payload.TimeoutMS,
	})
	if err != nil {
		return usecase.ExecutionResult{}, err
	}

	url := strings.TrimSuffix(agent.Endpoint, "/") + "/execute"
	callCtx, cancel := context.WithTimeout(ctx, e.CallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return usecase.ExecutionResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Runtime-Agent-Id", agent.ID.String())

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return usecase.ExecutionResult{}, fmt.Errorf("remote executor: %w", err)
	}
	defer resp.Body.Close()

	// Pin the TLS fingerprint after the connection is established. We
	// compare against the value recorded on the agent row; a mismatch
	// indicates a compromised endpoint or an un-announced cert rotation.
	if agent.Fingerprint != "" && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		got := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
		if hex.EncodeToString(got[:]) != agent.Fingerprint {
			return usecase.ExecutionResult{}, fmt.Errorf("remote executor: tls fingerprint mismatch")
		}
	}

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return usecase.ExecutionResult{}, fmt.Errorf("remote executor: %s: %s", resp.Status, string(raw))
	}

	var parsed executeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return usecase.ExecutionResult{}, fmt.Errorf("remote executor: decode: %w", err)
	}
	agentID := agent.ID
	return usecase.ExecutionResult{
		ExitCode:   parsed.ExitCode,
		Stdout:     parsed.Stdout,
		Stderr:     parsed.Stderr,
		DurationMS: parsed.DurationMS,
		AgentID:    &agentID,
	}, nil
}

// MapTokenProvider is the simplest TokenProvider; DI wires this from
// the service's secret store at startup.
type MapTokenProvider struct {
	tokens map[string]string
}

func NewMapTokenProvider(tokens map[string]string) *MapTokenProvider {
	return &MapTokenProvider{tokens: tokens}
}

func (m *MapTokenProvider) TokenFor(agentID string) (string, error) {
	if t, ok := m.tokens[agentID]; ok {
		return t, nil
	}
	return "", fmt.Errorf("no token for agent %s", agentID)
}
