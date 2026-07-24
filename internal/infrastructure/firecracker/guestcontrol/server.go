package guestcontrol

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
)

const (
	// DefaultDeadMan is how long the guest will stay frozen without hearing a
	// THAW before it thaws itself. A frozen filesystem blocks EVERY writer in the
	// guest, so a host that dies mid-snapshot would otherwise wedge the workload
	// until someone power-cycles it. The host may shorten this per request; it
	// may not lengthen it.
	DefaultDeadMan = 60 * time.Second

	// DefaultShutdownWait bounds how long the guest waits for the workload child
	// to exit after the forwarded SIGINT. Postgres fast shutdown has to finish a
	// checkpoint, so this is generous.
	DefaultShutdownWait = 60 * time.Second

	// acceptBackoff throttles the accept loop after an accept error so a
	// permanently broken listener cannot spin a guest vCPU.
	acceptBackoff = 200 * time.Millisecond
)

// Config constructs a Server. Token and Ops are mandatory.
type Config struct {
	// Token is the per-VM control token the host delivered in the boot-time
	// sealed secret push. Every request must present it.
	Token string
	// Ops performs the actual syscalls/ioctls.
	Ops Ops
	// Logf writes to the guest console. Optional; nil discards.
	Logf func(format string, args ...any)
	// AfterFunc schedules the dead-man. Optional; defaults to time.AfterFunc.
	AfterFunc AfterFunc
	// OnShutdownReplied fires after a SHUTDOWN reply has been written to the
	// wire. The guest uses it to hold its power-off until the host has its
	// answer — without it the PID-1 reaper races the reply and the host sees an
	// EOF instead of an ack.
	OnShutdownReplied func()
}

// Server serves the persistent post-boot control channel inside a resident
// guest: one request, one response, connection closed, repeat, until the VM
// dies. It owns the dead-man auto-thaw, which is the safety property of the
// whole channel.
type Server struct {
	token             string
	ops               Ops
	logf              func(format string, args ...any)
	afterFunc         AfterFunc
	onShutdownReplied func()

	mu      sync.Mutex
	deadMan Timer
}

// NewServer validates the config and returns a Server. It fails closed: a
// missing token or missing ops would mean an unauthenticated or inert control
// channel, and either is worse than no channel at all.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("guest control: refusing to serve without a control token")
	}
	if cfg.Ops == nil {
		return nil, errors.New("guest control: refusing to serve without ops")
	}
	s := &Server{
		token:             cfg.Token,
		ops:               cfg.Ops,
		logf:              cfg.Logf,
		afterFunc:         cfg.AfterFunc,
		onShutdownReplied: cfg.OnShutdownReplied,
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.afterFunc == nil {
		s.afterFunc = realAfterFunc
	}
	return s, nil
}

// Serve accepts control connections until ctx is cancelled or the listener is
// closed. Connections are handled SEQUENTIALLY on purpose: freeze/thaw is
// ordered state and there is exactly one legitimate caller (the host that holds
// the token), so concurrency here would only buy interleavings to reason about.
func (s *Server) Serve(ctx context.Context, ln Listener) {
	defer func() {
		if r := recover(); r != nil {
			s.logf("guest-control: panic in accept loop: %v", r)
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logf("guest-control: accept: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(acceptBackoff):
			}
			continue
		}
		s.ServeConn(ctx, conn)
	}
}

// ServeConn reads exactly one request off conn, executes it, writes the
// response, and closes the connection. Exported so the guest (and the tests)
// can drive a single exchange.
func (s *Server) ServeConn(ctx context.Context, conn Conn) {
	defer conn.Close()

	var req runtimev1.ControlRequest
	if err := ReadMessage(conn, &req); err != nil {
		s.logf("guest-control: read request: %v", err)
		return
	}

	resp := s.Handle(ctx, &req)
	if err := WriteMessage(conn, resp); err != nil {
		s.logf("guest-control: write response for %q: %v", req.GetOp(), err)
	}
	if req.GetOp() == OpShutdown && s.onShutdownReplied != nil {
		s.onShutdownReplied()
	}
}

// Handle authenticates and dispatches one request. It NEVER performs an op for
// an unauthenticated request — the token check is the first thing it does and
// the refusal returns before any Ops call.
func (s *Server) Handle(ctx context.Context, req *runtimev1.ControlRequest) *runtimev1.ControlResponse {
	// Constant-time so a peer cannot recover the token byte-by-byte from timing.
	if subtle.ConstantTimeCompare([]byte(req.GetToken()), []byte(s.token)) != 1 {
		s.logf("guest-control: REFUSED op %q — control token mismatch", req.GetOp())
		return errResp(ErrUnauthorized)
	}

	switch req.GetOp() {
	case OpSyncFS:
		return resultResp(s.ops.SyncFS(ctx))
	case OpFreeze:
		return resultResp(s.freeze(ctx, deadManFor(req.GetDeadmanSeconds())))
	case OpRenew:
		return resultResp(s.renew(ctx, deadManFor(req.GetDeadmanSeconds())))
	case OpThaw:
		return resultResp(s.thaw(ctx))
	case OpShutdown:
		return resultResp(s.ops.Shutdown(ctx, shutdownWaitFor(req.GetShutdownWaitSeconds())))
	default:
		s.logf("guest-control: REFUSED unknown op %q", req.GetOp())
		return errResp(ErrUnknownOp)
	}
}

// freeze freezes the data filesystem and arms the dead-man. The dead-man is
// armed only on success: arming it for a freeze that did not happen would fire
// a pointless thaw and log a host bug that is not one.
func (s *Server) freeze(ctx context.Context, deadMan time.Duration) error {
	if err := s.ops.Freeze(ctx); err != nil {
		return fmt.Errorf("freeze data filesystem: %w", err)
	}
	s.armDeadMan(ctx, deadMan)
	return nil
}

// renew extends the dead-man window WITHOUT touching the filesystem, and only
// while a dead-man is actually armed.
//
// s.deadMan != nil is the guest's own record of "the host holds a freeze I am
// protecting": disarmDeadMan nils it on THAW, and the auto-thaw closure nils it
// before thawing. So a nil timer means the freeze is gone, and the host — which
// may be halfway through copying the backing file — has to learn that
// immediately rather than be handed a fresh window over an unfrozen filesystem.
// It is refused with ErrDeadManNotArmed so the host can tell that apart from a
// transport failure.
//
// Ops is untouched on purpose: this op is pure dead-man bookkeeping, which is
// why it works on an already-frozen filesystem where a repeat FIFREEZE (EBUSY)
// cannot.
func (s *Server) renew(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadMan == nil {
		return ErrDeadManNotArmed
	}
	s.armDeadManLocked(ctx, d)
	return nil
}

// thaw thaws the data filesystem and disarms the dead-man. FITHAW on a
// filesystem that is not frozen returns EINVAL; that is "already thawed", which
// is exactly the state THAW asks for, so it is a benign success. Reporting it as
// an error would make the host's crash-recovery path (thaw everything, then
// proceed) fail for the one reason it must tolerate.
func (s *Server) thaw(ctx context.Context) error {
	err := s.ops.Thaw(ctx)
	if err != nil && !errors.Is(err, syscall.EINVAL) {
		// Deliberately leave the dead-man armed: the filesystem may still be
		// frozen, and this failed thaw is exactly the case it exists to cover.
		return fmt.Errorf("thaw data filesystem: %w", err)
	}
	s.disarmDeadMan()
	return nil
}

func (s *Server) armDeadMan(ctx context.Context, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armDeadManLocked(ctx, d)
}

// armDeadManLocked is armDeadMan's body, callable from a path that already holds
// the mutex (renew, which must decide and re-arm without a gap another op could
// slip through).
//
// If the previous timer had ALREADY fired, Stop returns false and its closure
// still runs: it nils s.deadMan and thaws. The next renew then finds no dead-man
// and fails, so the copy is aborted and discarded — a snapshot taken across that
// thaw is never kept. That is the correct outcome anyway: a timer that fired
// means the freeze window genuinely elapsed, i.e. the host was already too late.
func (s *Server) armDeadManLocked(ctx context.Context, d time.Duration) {
	if s.deadMan != nil {
		// A second FREEZE (or a RENEW) replaces the previous window rather than
		// stacking one.
		s.deadMan.Stop()
	}
	s.deadMan = s.afterFunc(d, func() {
		defer func() {
			if r := recover(); r != nil {
				s.logf("guest-control: panic in dead-man auto-thaw: %v", r)
			}
		}()
		s.mu.Lock()
		s.deadMan = nil
		s.mu.Unlock()
		// Loud on purpose: reaching here means the host never sent its THAW, i.e.
		// it crashed or leaked a freeze. The guest just saved itself from a
		// permanently wedged filesystem, and someone should go read the host logs.
		s.logf("guest-control: DEAD-MAN FIRED after %s with no THAW — auto-thawing the data filesystem. This is a HOST-SIDE BUG: a freeze was never released.", d)
		if err := s.ops.Thaw(ctx); err != nil && !errors.Is(err, syscall.EINVAL) {
			s.logf("guest-control: dead-man auto-thaw FAILED: %v — the data filesystem may still be frozen", err)
		}
	})
}

func (s *Server) disarmDeadMan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadMan != nil {
		s.deadMan.Stop()
		s.deadMan = nil
	}
}

// deadManFor clamps the requested window: the host may SHORTEN the guest's
// self-protection, never extend it.
func deadManFor(seconds uint32) time.Duration {
	d := time.Duration(seconds) * time.Second
	if d <= 0 || d > DefaultDeadMan {
		return DefaultDeadMan
	}
	return d
}

func shutdownWaitFor(seconds uint32) time.Duration {
	d := time.Duration(seconds) * time.Second
	if d <= 0 {
		return DefaultShutdownWait
	}
	return d
}

func resultResp(err error) *runtimev1.ControlResponse {
	if err != nil {
		return errResp(err)
	}
	return &runtimev1.ControlResponse{Ok: true}
}

func errResp(err error) *runtimev1.ControlResponse {
	return &runtimev1.ControlResponse{Ok: false, Error: err.Error()}
}
