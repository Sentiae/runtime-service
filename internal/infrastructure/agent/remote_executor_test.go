package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

func TestRemoteExecutor_ForwardsPayloadAndReturnsResult(t *testing.T) {
	var gotAuth string
	var gotBody executeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(executeResponse{
			ExitCode:   0,
			Stdout:     "hello",
			DurationMS: 42,
		})
	}))
	defer srv.Close()

	agentID := uuid.New()
	agentRow := &domain.RuntimeAgent{ID: agentID, Endpoint: srv.URL}
	tokens := NewMapTokenProvider(map[string]string{agentID.String(): "secret-123"})

	exec := NewRemoteExecutor(tokens)
	res, err := exec.Dispatch(context.Background(), agentRow, usecase.ExecutionPayload{
		Language: "go",
		Code:     "package main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.AgentID == nil || *res.AgentID != agentID {
		t.Fatalf("expected agent id %s in result, got %+v", agentID, res.AgentID)
	}
	if gotAuth != "Bearer secret-123" {
		t.Fatalf("expected bearer token passthrough, got %q", gotAuth)
	}
	if gotBody.Language != "go" {
		t.Fatalf("expected language forwarded, got %q", gotBody.Language)
	}
}

func TestRemoteExecutor_4xxErrorPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.Copy(w, strings.NewReader("no access"))
	}))
	defer srv.Close()

	agentID := uuid.New()
	agentRow := &domain.RuntimeAgent{ID: agentID, Endpoint: srv.URL}
	tokens := NewMapTokenProvider(map[string]string{agentID.String(): "k"})
	exec := NewRemoteExecutor(tokens)

	_, err := exec.Dispatch(context.Background(), agentRow, usecase.ExecutionPayload{})
	if err == nil {
		t.Fatal("expected error for 4xx response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 in error, got %v", err)
	}
}

func TestRemoteExecutor_MissingTokenErrors(t *testing.T) {
	agentRow := &domain.RuntimeAgent{ID: uuid.New(), Endpoint: "http://x"}
	exec := NewRemoteExecutor(NewMapTokenProvider(map[string]string{}))
	_, err := exec.Dispatch(context.Background(), agentRow, usecase.ExecutionPayload{})
	if err == nil {
		t.Fatal("expected missing-token error")
	}
}
