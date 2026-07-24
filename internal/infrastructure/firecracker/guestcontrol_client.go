package firecracker

import (
	"context"
	"fmt"
	"net"
	"time"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestcontrol"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestsecrets"
	"github.com/sentiae/runtime-service/internal/port/gateway"
)

const (
	// controlCallTimeout bounds a filesystem control op (dial + CONNECT
	// handshake + request + response). syncfs on a large dirty page cache is the
	// slow case.
	controlCallTimeout = 30 * time.Second

	// controlShutdownTimeout bounds a SHUTDOWN. It must exceed the guest's own
	// wait for the workload child (guestcontrol.DefaultShutdownWait) or the host
	// would give up while the guest is still correctly waiting on a Postgres
	// fast-shutdown checkpoint.
	controlShutdownTimeout = guestcontrol.DefaultShutdownWait + 30*time.Second
)

// GuestControlClient speaks the post-boot control channel to a resident guest
// over the same host->guest vsock the secret push uses (D-185a): dial the VM's
// vsock UDS, perform Firecracker's "CONNECT <port>" handshake against the
// guest's AF_VSOCK listener, send one request, read one response.
type GuestControlClient struct {
	tokens *GuestControlTokens
	// deadMan is the freeze window the host asks the guest to enforce. The guest
	// clamps it to its own default, so this can only ever shorten the window.
	deadMan time.Duration
}

var _ gateway.GuestControl = (*GuestControlClient)(nil)

// NewGuestControlClient constructs a client over the store the booter fills at
// boot. Both must be the same instance — a client with its own store would find
// no token for any VM.
func NewGuestControlClient(tokens *GuestControlTokens) *GuestControlClient {
	return &GuestControlClient{
		tokens:  tokens,
		deadMan: guestcontrol.DefaultDeadMan,
	}
}

// SyncFS asks the guest to flush its data filesystem.
func (c *GuestControlClient) SyncFS(ctx context.Context, socketPath string) error {
	return c.call(ctx, socketPath, &runtimev1.ControlRequest{Op: guestcontrol.OpSyncFS}, controlCallTimeout)
}

// Freeze asks the guest to flush and freeze its data filesystem, carrying the
// dead-man window so a host that dies before Thaw cannot wedge the guest.
func (c *GuestControlClient) Freeze(ctx context.Context, socketPath string) error {
	return c.call(ctx, socketPath, &runtimev1.ControlRequest{
		Op:             guestcontrol.OpFreeze,
		DeadmanSeconds: uint32(c.deadMan / time.Second),
	}, controlCallTimeout)
}

// Renew extends the dead-man window of a freeze this host already holds. It
// carries the same window as Freeze (the guest clamps it either way) and does
// not touch the guest filesystem, which is what lets it run against a filesystem
// that is already frozen — a repeat Freeze there would fail with EBUSY and, worse,
// would not re-arm anything.
//
// A guest with no dead-man armed refuses it (guestcontrol.ErrDeadManNotArmed):
// the freeze is gone, and the caller's in-flight work under it is void.
func (c *GuestControlClient) Renew(ctx context.Context, socketPath string) error {
	return c.call(ctx, socketPath, &runtimev1.ControlRequest{
		Op:             guestcontrol.OpRenew,
		DeadmanSeconds: uint32(c.deadMan / time.Second),
	}, controlCallTimeout)
}

// Thaw releases a Freeze. Safe to call unconditionally: an unfrozen guest
// answers ok.
func (c *GuestControlClient) Thaw(ctx context.Context, socketPath string) error {
	return c.call(ctx, socketPath, &runtimev1.ControlRequest{Op: guestcontrol.OpThaw}, controlCallTimeout)
}

// Shutdown asks the guest to stop the workload gracefully and returns once the
// workload child has exited.
func (c *GuestControlClient) Shutdown(ctx context.Context, socketPath string) error {
	return c.call(ctx, socketPath, &runtimev1.ControlRequest{Op: guestcontrol.OpShutdown}, controlShutdownTimeout)
}

// call performs one request/response exchange. The token is looked up here (not
// passed by the caller) so no use case ever handles it.
func (c *GuestControlClient) call(ctx context.Context, socketPath string, req *runtimev1.ControlRequest, timeout time.Duration) error {
	if c.tokens == nil || socketPath == "" {
		return fmt.Errorf("guest control %s: %w", req.GetOp(), domain.ErrGuestControlUnavailable)
	}
	token, ok := c.tokens.Get(socketPath)
	if !ok {
		// No token means this VM never armed a control channel (non-resident
		// class, or a boot with no sealed push). Fail loud — see the sentinel.
		return fmt.Errorf("guest control %s: %w", req.GetOp(), domain.ErrGuestControlUnavailable)
	}
	req.Token = token

	// The CALLER's deadline wins when it is the tighter one. Without this the dial
	// loop below runs to its own fixed deadline no matter what the caller asked
	// for, which is why a caller that budgets 10s for several retried attempts got
	// exactly one 30s attempt (fleet_volume_snapshot.thawWithRetries).
	deadline, _ := ctxOrDeadline(ctx, time.Now().Add(timeout))

	conn, err := c.dial(ctx, socketPath, deadline)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(deadline)
	return exchange(conn, req)
}

// dial reuses the secret pusher's vsock dial + Firecracker CONNECT handshake —
// same UDS convention (socketPath + ".vsock"), same retry-until-deadline shape,
// different port. The deadline bounds the whole retry loop (a per-attempt dial
// or handshake can still overshoot it by its own fixed timeout).
func (c *GuestControlClient) dial(ctx context.Context, socketPath string, deadline time.Time) (net.Conn, error) {
	conn, err := connectGuestVsock(ctx, socketPath+".vsock", guestsecrets.ControlPort, deadline)
	if err != nil {
		return nil, fmt.Errorf("connect guest control channel: %w", err)
	}
	return conn, nil
}

// exchange writes the request and reads the guest's verdict. Split out from call
// so the wire round-trip is testable without a microVM.
func exchange(conn net.Conn, req *runtimev1.ControlRequest) error {
	if err := guestcontrol.WriteMessage(conn, req); err != nil {
		return fmt.Errorf("send guest control %s: %w", req.GetOp(), err)
	}
	var resp runtimev1.ControlResponse
	if err := guestcontrol.ReadMessage(conn, &resp); err != nil {
		return fmt.Errorf("read guest control %s response: %w", req.GetOp(), err)
	}
	if !resp.GetOk() {
		return fmt.Errorf("guest refused %s: %s", req.GetOp(), resp.GetError())
	}
	return nil
}
