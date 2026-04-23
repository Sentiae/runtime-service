package vmagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMockAgent spins up an httptest.Server that records each request
// and can be configured per-path for success/failure status codes.
type mockAgent struct {
	server        *httptest.Server
	tokenExpected string
	responses     map[string]mockResponse
	requests      []*http.Request
	bodies        map[string][]byte
}

type mockResponse struct {
	status int
	body   string
}

func newMockAgent(t *testing.T, token string, responses map[string]mockResponse) *mockAgent {
	t.Helper()
	m := &mockAgent{
		tokenExpected: token,
		responses:     responses,
		bodies:        make(map[string][]byte),
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad token"}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		m.bodies[key] = b
		m.requests = append(m.requests, r.Clone(r.Context()))
		if resp, ok := m.responses[key]; ok {
			w.WriteHeader(resp.status)
			if resp.body != "" {
				_, _ = w.Write([]byte(resp.body))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(m.server.Close)
	return m
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestClient_Boot_Success(t *testing.T) {
	t.Parallel()

	mock := newMockAgent(t, "tok-1", map[string]mockResponse{
		"POST /boot": {
			status: http.StatusOK,
			body: `{
				"PID": 4242,
				"SocketPath": "/tmp/vm1.sock",
				"IPAddress": "10.0.0.2",
				"BootTimeMS": 120
			}`,
		},
	})

	c := NewClient(mock.server.Client(), StaticTokenProvider{Token: "tok-1"})
	res, err := c.Boot(context.Background(), mock.server.URL, usecase.VMBootConfig{
		VMID:     uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		VCPU:     2,
		MemoryMB: 512,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 4242, res.PID)
	assert.Equal(t, "/tmp/vm1.sock", res.SocketPath)
	assert.Equal(t, "10.0.0.2", res.IPAddress)

	// Verify the body was sent — VMBootConfig uses default json field
	// names (capitalised) because the struct has no explicit tags.
	body := mock.bodies["POST /boot"]
	assert.Contains(t, string(body), `"VCPU":2`)
	assert.Contains(t, string(body), `"MemoryMB":512`)
}

func TestClient_Terminate_IncludesSocketInQuery(t *testing.T) {
	t.Parallel()

	mock := newMockAgent(t, "tok-1", map[string]mockResponse{
		"DELETE /vms/7777": {status: http.StatusNoContent},
	})
	c := NewClient(mock.server.Client(), StaticTokenProvider{Token: "tok-1"})

	err := c.Terminate(context.Background(), mock.server.URL, "/tmp/vm.sock", 7777)
	require.NoError(t, err)
	require.NotEmpty(t, mock.requests)
	last := mock.requests[len(mock.requests)-1]
	assert.Equal(t, "/tmp/vm.sock", last.URL.Query().Get("socket"))
}

func TestClient_MissingToken_FailsClosed(t *testing.T) {
	t.Parallel()

	mock := newMockAgent(t, "tok-1", nil)
	c := NewClient(mock.server.Client(), StaticTokenProvider{})
	_, err := c.Boot(context.Background(), mock.server.URL, usecase.VMBootConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no static token configured")
}

func TestClient_4xxMapsToRejected(t *testing.T) {
	t.Parallel()

	mock := newMockAgent(t, "tok-1", map[string]mockResponse{
		"POST /boot": {status: http.StatusBadRequest, body: `{"error":"bad config"}`},
	})
	c := NewClient(mock.server.Client(), StaticTokenProvider{Token: "tok-1"})
	_, err := c.Boot(context.Background(), mock.server.URL, usecase.VMBootConfig{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentRejected)
}

func TestClient_5xxMapsToInternal(t *testing.T) {
	t.Parallel()

	mock := newMockAgent(t, "tok-1", map[string]mockResponse{
		"POST /boot": {status: http.StatusInternalServerError, body: `{"error":"boom"}`},
	})
	c := NewClient(mock.server.Client(), StaticTokenProvider{Token: "tok-1"})
	_, err := c.Boot(context.Background(), mock.server.URL, usecase.VMBootConfig{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentInternal)
}

func TestClient_TransportErrorMapsToUnreachable(t *testing.T) {
	t.Parallel()

	c := NewClient(nil, StaticTokenProvider{Token: "x"})
	// Hit a reserved TEST-NET-1 address that should immediately fail to
	// dial; we don't want to depend on DNS behaviour in CI.
	_, err := c.Boot(context.Background(), "http://127.0.0.1:1/", usecase.VMBootConfig{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAgentUnreachable), "expected ErrAgentUnreachable, got %v", err)
}

func TestClient_CreateSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	snapID := uuid.New()
	mock := newMockAgent(t, "tok-1", map[string]mockResponse{
		"POST /vms/my.sock/snapshots": {
			status: http.StatusOK,
			body:   `{"MemoryFilePath":"/tmp/mem","StateFilePath":"/tmp/state"}`,
		},
	})
	c := NewClient(mock.server.Client(), StaticTokenProvider{Token: "tok-1"})
	res, err := c.CreateSnapshot(context.Background(), mock.server.URL, "my.sock", snapID)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/mem", res.MemoryFilePath)
	assert.Equal(t, "/tmp/state", res.StateFilePath)

	// Body should carry the snapshot id.
	var payload map[string]string
	require.NoError(t, json.Unmarshal(mock.bodies["POST /vms/my.sock/snapshots"], &payload))
	assert.Equal(t, snapID.String(), payload["snapshot_id"])
}

func TestClient_JoinURL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "http://a/path", joinURL("http://a", "path"))
	assert.Equal(t, "http://a/path", joinURL("http://a/", "/path"))
	assert.Equal(t, "http://a/path", joinURL("http://a", "/path"))
}

func TestClient_AppendQuery(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "http://a?k=v", appendQuery("http://a", "k", "v"))
	assert.Equal(t, "http://a?x=1&k=v", appendQuery("http://a?x=1", "k", "v"))
	assert.True(t, strings.Contains(appendQuery("http://a?x=1", "k", "v"), "&k=v"))
}
