//go:build unit

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestcontrol"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestsecrets"
)

// ─────────────────────────────────────────────────────────────────────
// Token store
// ─────────────────────────────────────────────────────────────────────

func TestGuestControlTokens(t *testing.T) {
	s := NewGuestControlTokens()

	if _, ok := s.Get("/run/a.sock"); ok {
		t.Fatal("empty store returned a token")
	}

	s.Put("/run/a.sock", "tok-a")
	if got, ok := s.Get("/run/a.sock"); !ok || got != "tok-a" {
		t.Fatalf("Get = (%q,%v), want (tok-a,true)", got, ok)
	}

	// A boot with no control channel must leave NO entry, so the client fails
	// loud instead of dialing a listener that was never armed.
	s.Put("/run/b.sock", "")
	if _, ok := s.Get("/run/b.sock"); ok {
		t.Fatal("empty token was stored")
	}
	s.Put("", "tok-c")
	if _, ok := s.Get(""); ok {
		t.Fatal("empty socket path was stored")
	}

	s.Put("/run/a.sock", "tok-a2")
	if got, _ := s.Get("/run/a.sock"); got != "tok-a2" {
		t.Fatalf("Get after replace = %q, want tok-a2", got)
	}

	s.Delete("/run/a.sock")
	if _, ok := s.Get("/run/a.sock"); ok {
		t.Fatal("token survived Delete")
	}
	s.Delete("/run/a.sock") // idempotent
}

func TestNewControlTokenIsFreshAndLong(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		tok, err := newControlToken()
		if err != nil {
			t.Fatalf("newControlToken: %v", err)
		}
		if len(tok) != controlTokenBytes*2 {
			t.Fatalf("token length = %d, want %d hex chars", len(tok), controlTokenBytes*2)
		}
		if seen[tok] {
			t.Fatal("newControlToken repeated itself")
		}
		seen[tok] = true
	}
}

// ─────────────────────────────────────────────────────────────────────
// Fail-loud when the VM has no control channel
// ─────────────────────────────────────────────────────────────────────

func TestGuestControlClientFailsLoudWithoutAToken(t *testing.T) {
	c := NewGuestControlClient(NewGuestControlTokens())
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"syncfs", func() error { return c.SyncFS(ctx, "/run/unknown.sock") }},
		{"freeze", func() error { return c.Freeze(ctx, "/run/unknown.sock") }},
		{"thaw", func() error { return c.Thaw(ctx, "/run/unknown.sock") }},
		{"shutdown", func() error { return c.Shutdown(ctx, "/run/unknown.sock") }},
		{"empty socket path", func() error { return c.SyncFS(ctx, "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, domain.ErrGuestControlUnavailable) {
				t.Fatalf("err = %v, want ErrGuestControlUnavailable", err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// End-to-end over a fake guest (no microVM): the client's real dial +
// Firecracker CONNECT handshake + request/response encode/decode.
// ─────────────────────────────────────────────────────────────────────

// fakeGuest listens on a VM's vsock UDS, answers Firecracker's
// "CONNECT <port>" handshake, and serves one control exchange per connection.
type fakeGuest struct {
	mu       sync.Mutex
	requests []*runtimev1.ControlRequest
	ports    []string

	respond func(*runtimev1.ControlRequest) *runtimev1.ControlResponse
}

func (g *fakeGuest) got() []*runtimev1.ControlRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*runtimev1.ControlRequest(nil), g.requests...)
}

func (g *fakeGuest) connectPorts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.ports...)
}

func (g *fakeGuest) serve(conn net.Conn) {
	defer conn.Close()
	line, err := readLine(conn)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.ports = append(g.ports, strings.TrimSpace(line))
	g.mu.Unlock()
	if _, err := conn.Write([]byte("OK 1024\n")); err != nil {
		return
	}

	var req runtimev1.ControlRequest
	if err := guestcontrol.ReadMessage(conn, &req); err != nil {
		return
	}
	g.mu.Lock()
	g.requests = append(g.requests, &req)
	g.mu.Unlock()

	_ = guestcontrol.WriteMessage(conn, g.respond(&req))
}

// startFakeGuest binds the VM's vsock UDS and returns the host-view socket path
// the client addresses the VM by. /tmp (not t.TempDir) because a macOS temp dir
// plus ".vsock" can exceed the AF_UNIX path limit.
func startFakeGuest(t *testing.T, g *fakeGuest) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gc-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "vm.sock")
	ln, err := net.Listen("unix", socketPath+".vsock")
	if err != nil {
		t.Fatalf("listen vsock uds: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		defer func() { _ = recover() }()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			g.serve(conn)
		}
	}()
	return socketPath
}

func TestGuestControlClientRoundTrip(t *testing.T) {
	tests := []struct {
		name           string
		call           func(*GuestControlClient, context.Context, string) error
		wantOp         string
		wantDeadman    uint32
		wantShutdownWt uint32
	}{
		{"syncfs", (*GuestControlClient).SyncFS, guestcontrol.OpSyncFS, 0, 0},
		{"freeze carries the dead-man window", (*GuestControlClient).Freeze, guestcontrol.OpFreeze,
			uint32(guestcontrol.DefaultDeadMan / time.Second), 0},
		{"thaw", (*GuestControlClient).Thaw, guestcontrol.OpThaw, 0, 0},
		{"shutdown", (*GuestControlClient).Shutdown, guestcontrol.OpShutdown, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGuest{respond: func(*runtimev1.ControlRequest) *runtimev1.ControlResponse {
				return &runtimev1.ControlResponse{Ok: true}
			}}
			socketPath := startFakeGuest(t, g)

			tokens := NewGuestControlTokens()
			tokens.Put(socketPath, "tok-xyz")
			c := NewGuestControlClient(tokens)

			if err := tt.call(c, context.Background(), socketPath); err != nil {
				t.Fatalf("%s: %v", tt.wantOp, err)
			}

			reqs := g.got()
			if len(reqs) != 1 {
				t.Fatalf("guest saw %d requests, want 1", len(reqs))
			}
			req := reqs[0]
			if req.GetOp() != tt.wantOp {
				t.Fatalf("op = %q, want %q", req.GetOp(), tt.wantOp)
			}
			if req.GetToken() != "tok-xyz" {
				t.Fatalf("token = %q, want the stored token", req.GetToken())
			}
			if req.GetDeadmanSeconds() != tt.wantDeadman {
				t.Fatalf("deadman_seconds = %d, want %d", req.GetDeadmanSeconds(), tt.wantDeadman)
			}
			if req.GetShutdownWaitSeconds() != tt.wantShutdownWt {
				t.Fatalf("shutdown_wait_seconds = %d, want %d", req.GetShutdownWaitSeconds(), tt.wantShutdownWt)
			}

			wantConnect := fmt.Sprintf("CONNECT %d", guestsecrets.ControlPort)
			if ports := g.connectPorts(); len(ports) != 1 || ports[0] != wantConnect {
				t.Fatalf("handshake = %v, want [%q]", ports, wantConnect)
			}
		})
	}
}

func TestGuestControlClientSurfacesGuestRefusal(t *testing.T) {
	g := &fakeGuest{respond: func(*runtimev1.ControlRequest) *runtimev1.ControlResponse {
		return &runtimev1.ControlResponse{Ok: false, Error: guestcontrol.ErrUnauthorized.Error()}
	}}
	socketPath := startFakeGuest(t, g)

	tokens := NewGuestControlTokens()
	tokens.Put(socketPath, "tok-wrong")
	c := NewGuestControlClient(tokens)

	err := c.Freeze(context.Background(), socketPath)
	if err == nil {
		t.Fatal("client reported success for a refused freeze")
	}
	if !strings.Contains(err.Error(), guestcontrol.ErrUnauthorized.Error()) {
		t.Fatalf("err = %v, want the guest's refusal reason", err)
	}
}

// TestGuestControlClientAgainstRealServer wires the host client straight to the
// in-guest server implementation, so one test proves both ends agree on the
// wire, the token check, and the verdict encoding.
func TestGuestControlClientAgainstRealServer(t *testing.T) {
	ops := &recordingOps{}
	srv, err := guestcontrol.NewServer(guestcontrol.Config{Token: "tok-real", Ops: ops})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	g := &fakeGuest{}
	g.respond = func(req *runtimev1.ControlRequest) *runtimev1.ControlResponse {
		return srv.Handle(context.Background(), req)
	}
	socketPath := startFakeGuest(t, g)

	tokens := NewGuestControlTokens()
	c := NewGuestControlClient(tokens)

	// Wrong token: the guest refuses AND performs nothing.
	tokens.Put(socketPath, "tok-forged")
	if err := c.SyncFS(context.Background(), socketPath); err == nil {
		t.Fatal("forged token was accepted")
	}
	if got := ops.got(); len(got) != 0 {
		t.Fatalf("ops ran for a forged token: %v", got)
	}

	// Right token: the op runs.
	tokens.Put(socketPath, "tok-real")
	if err := c.SyncFS(context.Background(), socketPath); err != nil {
		t.Fatalf("SyncFS: %v", err)
	}
	if got := ops.got(); len(got) != 1 || got[0] != guestcontrol.OpSyncFS {
		t.Fatalf("ops = %v, want [SYNCFS]", got)
	}
}

// recordingOps is a no-op guestcontrol.Ops that records what it was asked to do.
type recordingOps struct {
	mu    sync.Mutex
	calls []string
}

func (o *recordingOps) record(op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, op)
}

func (o *recordingOps) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *recordingOps) SyncFS(context.Context) error { o.record(guestcontrol.OpSyncFS); return nil }
func (o *recordingOps) Freeze(context.Context) error { o.record(guestcontrol.OpFreeze); return nil }
func (o *recordingOps) Thaw(context.Context) error   { o.record(guestcontrol.OpThaw); return nil }
func (o *recordingOps) Shutdown(context.Context, time.Duration) error {
	o.record(guestcontrol.OpShutdown)
	return nil
}
