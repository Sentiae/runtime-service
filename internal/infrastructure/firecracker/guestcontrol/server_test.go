//go:build unit

package guestcontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
)

const testToken = "0123456789abcdef0123456789abcdef"

// ─────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────

// fakeOps records every op it is asked to perform, so a test can assert not
// only what a request returned but whether it TOUCHED the guest at all.
type fakeOps struct {
	mu    sync.Mutex
	calls []string

	syncErr     error
	freezeErr   error
	thawErr     error
	shutdownErr error

	lastShutdownWait time.Duration
}

func (o *fakeOps) record(op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, op)
}

func (o *fakeOps) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func (o *fakeOps) SyncFS(context.Context) error {
	o.record(OpSyncFS)
	return o.syncErr
}

func (o *fakeOps) Freeze(context.Context) error {
	o.record(OpFreeze)
	return o.freezeErr
}

func (o *fakeOps) Thaw(context.Context) error {
	o.record(OpThaw)
	return o.thawErr
}

func (o *fakeOps) Shutdown(_ context.Context, wait time.Duration) error {
	o.record(OpShutdown)
	o.mu.Lock()
	o.lastShutdownWait = wait
	o.mu.Unlock()
	return o.shutdownErr
}

// fakeTimer + fakeScheduler replace time.AfterFunc so the dead-man can be fired
// on demand. The suite must never sleep out a real 60s window.
type fakeTimer struct {
	mu      sync.Mutex
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (t *fakeTimer) fire() {
	t.mu.Lock()
	stopped := t.stopped
	f := t.f
	t.mu.Unlock()
	if stopped || f == nil {
		return
	}
	f()
}

type fakeScheduler struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (s *fakeScheduler) afterFunc(d time.Duration, f func()) Timer {
	t := &fakeTimer{d: d, f: f}
	s.mu.Lock()
	s.timers = append(s.timers, t)
	s.mu.Unlock()
	return t
}

func (s *fakeScheduler) last() *fakeTimer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timers) == 0 {
		return nil
	}
	return s.timers[len(s.timers)-1]
}

func (s *fakeScheduler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers)
}

// pipeConn adapts an io.Reader/io.Writer pair to Conn for the framing tests.
type pipeConn struct {
	io.Reader
	io.Writer
}

func (pipeConn) Close() error { return nil }

func newTestServer(t *testing.T, ops Ops, sched *fakeScheduler, log *strings.Builder) *Server {
	t.Helper()
	cfg := Config{Token: testToken, Ops: ops}
	if sched != nil {
		cfg.AfterFunc = sched.afterFunc
	}
	if log != nil {
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(log, format+"\n", args...)
		}
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────
// Construction — fail closed
// ─────────────────────────────────────────────────────────────────────

func TestNewServerFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no token", Config{Ops: &fakeOps{}}},
		{"no ops", Config{Token: testToken}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewServer(tt.cfg); err == nil {
				t.Fatal("NewServer succeeded, want refusal")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Token authentication
// ─────────────────────────────────────────────────────────────────────

func TestHandleTokenAuth(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantOK    bool
		wantCalls []string
	}{
		{"valid token performs the op", testToken, true, []string{OpSyncFS}},
		{"wrong token performs nothing", "ffffffffffffffffffffffffffffffff", false, nil},
		{"absent token performs nothing", "", false, nil},
		{"token prefix performs nothing", testToken[:16], false, nil},
		{"token with suffix performs nothing", testToken + "x", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			var log strings.Builder
			s := newTestServer(t, ops, nil, &log)

			resp := s.Handle(context.Background(), &runtimev1.ControlRequest{
				Token: tt.token,
				Op:    OpSyncFS,
			})

			if resp.GetOk() != tt.wantOK {
				t.Fatalf("ok = %v, want %v (error=%q)", resp.GetOk(), tt.wantOK, resp.GetError())
			}
			if got := ops.got(); len(got) != len(tt.wantCalls) {
				t.Fatalf("ops calls = %v, want %v", got, tt.wantCalls)
			}
			if !tt.wantOK {
				if resp.GetError() != ErrUnauthorized.Error() {
					t.Fatalf("error = %q, want %q", resp.GetError(), ErrUnauthorized.Error())
				}
				if !strings.Contains(log.String(), "REFUSED") {
					t.Fatalf("refusal was not logged; log = %q", log.String())
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Op dispatch
// ─────────────────────────────────────────────────────────────────────

func TestHandleDispatch(t *testing.T) {
	tests := []struct {
		name      string
		op        string
		opErr     error
		wantOK    bool
		wantCalls []string
	}{
		{"syncfs", OpSyncFS, nil, true, []string{OpSyncFS}},
		{"syncfs failure", OpSyncFS, errors.New("boom"), false, []string{OpSyncFS}},
		{"freeze", OpFreeze, nil, true, []string{OpFreeze}},
		{"freeze failure", OpFreeze, errors.New("boom"), false, []string{OpFreeze}},
		{"thaw", OpThaw, nil, true, []string{OpThaw}},
		{"thaw failure", OpThaw, errors.New("boom"), false, []string{OpThaw}},
		{"shutdown", OpShutdown, nil, true, []string{OpShutdown}},
		{"shutdown failure", OpShutdown, errors.New("boom"), false, []string{OpShutdown}},
		{"unknown op", "REBOOT", nil, false, nil},
		{"empty op", "", nil, false, nil},
		{"lowercase op is not the op", "syncfs", nil, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{
				syncErr:     nil,
				freezeErr:   nil,
				thawErr:     nil,
				shutdownErr: nil,
			}
			switch tt.op {
			case OpSyncFS:
				ops.syncErr = tt.opErr
			case OpFreeze:
				ops.freezeErr = tt.opErr
			case OpThaw:
				ops.thawErr = tt.opErr
			case OpShutdown:
				ops.shutdownErr = tt.opErr
			}
			s := newTestServer(t, ops, &fakeScheduler{}, nil)

			resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: tt.op})

			if resp.GetOk() != tt.wantOK {
				t.Fatalf("ok = %v, want %v (error=%q)", resp.GetOk(), tt.wantOK, resp.GetError())
			}
			got := ops.got()
			if len(got) != len(tt.wantCalls) {
				t.Fatalf("ops calls = %v, want %v", got, tt.wantCalls)
			}
			for i := range got {
				if got[i] != tt.wantCalls[i] {
					t.Fatalf("ops calls = %v, want %v", got, tt.wantCalls)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// THAW on an unfrozen filesystem
// ─────────────────────────────────────────────────────────────────────

func TestThawWhenNotFrozenIsBenign(t *testing.T) {
	tests := []struct {
		name   string
		thawFn error
		wantOK bool
	}{
		{"frozen — plain success", nil, true},
		// FITHAW answers EINVAL when the filesystem is not frozen. That is
		// already the state THAW asks for, and the host's crash-recovery path
		// calls THAW unconditionally.
		{"not frozen — EINVAL is success", syscall.EINVAL, true},
		{"wrapped EINVAL is success", fmt.Errorf("FITHAW: %w", syscall.EINVAL), true},
		{"a real failure is still a failure", syscall.EIO, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{thawErr: tt.thawFn}
			s := newTestServer(t, ops, &fakeScheduler{}, nil)

			resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpThaw})
			if resp.GetOk() != tt.wantOK {
				t.Fatalf("ok = %v, want %v (error=%q)", resp.GetOk(), tt.wantOK, resp.GetError())
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Dead-man auto-thaw
// ─────────────────────────────────────────────────────────────────────

func TestDeadManArmedOnFreeze(t *testing.T) {
	ops := &fakeOps{}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	if !resp.GetOk() {
		t.Fatalf("freeze failed: %s", resp.GetError())
	}
	if sched.count() != 1 {
		t.Fatalf("timers armed = %d, want 1", sched.count())
	}
	if got := sched.last().d; got != DefaultDeadMan {
		t.Fatalf("dead-man window = %s, want %s", got, DefaultDeadMan)
	}
}

func TestDeadManNotArmedWhenFreezeFails(t *testing.T) {
	ops := &fakeOps{freezeErr: errors.New("device busy")}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	if resp.GetOk() {
		t.Fatal("freeze reported ok despite a failing op")
	}
	if sched.count() != 0 {
		t.Fatalf("timers armed = %d, want 0 — nothing was frozen", sched.count())
	}
}

func TestDeadManDisarmedByThaw(t *testing.T) {
	ops := &fakeOps{}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	timer := sched.last()

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpThaw})

	// Firing a disarmed timer must be inert: no second thaw, no dead-man log.
	timer.fire()
	if got := ops.got(); len(got) != 2 || got[0] != OpFreeze || got[1] != OpThaw {
		t.Fatalf("ops calls = %v, want [FREEZE THAW]", got)
	}
}

func TestDeadManNotDisarmedWhenThawFails(t *testing.T) {
	ops := &fakeOps{thawErr: syscall.EIO}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	timer := sched.last()

	resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpThaw})
	if resp.GetOk() {
		t.Fatal("thaw reported ok despite EIO")
	}

	// The filesystem may still be frozen, so the dead-man must survive to retry.
	ops.thawErr = nil
	timer.fire()
	if got := ops.got(); len(got) != 3 || got[2] != OpThaw {
		t.Fatalf("ops calls = %v, want a third THAW from the dead-man", got)
	}
}

func TestDeadManFiresAndThaws(t *testing.T) {
	ops := &fakeOps{}
	sched := &fakeScheduler{}
	var log strings.Builder
	s := newTestServer(t, ops, sched, &log)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	sched.last().fire()

	got := ops.got()
	if len(got) != 2 || got[0] != OpFreeze || got[1] != OpThaw {
		t.Fatalf("ops calls = %v, want [FREEZE THAW]", got)
	}
	if !strings.Contains(log.String(), "DEAD-MAN FIRED") {
		t.Fatalf("dead-man firing was not logged loudly; log = %q", log.String())
	}
	if !strings.Contains(log.String(), "HOST-SIDE BUG") {
		t.Fatalf("dead-man log does not name the host as the culprit; log = %q", log.String())
	}
}

func TestDeadManRearmReplacesPreviousTimer(t *testing.T) {
	ops := &fakeOps{}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	first := sched.last()
	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	second := sched.last()

	if first == second {
		t.Fatal("re-arm reused the first timer")
	}
	first.fire() // stopped by the re-arm — must be inert
	if got := ops.got(); len(got) != 2 {
		t.Fatalf("ops calls = %v, want just the two freezes", got)
	}
	second.fire()
	if got := ops.got(); len(got) != 3 || got[2] != OpThaw {
		t.Fatalf("ops calls = %v, want the live timer to thaw", got)
	}
}

// RENEW is the op a host-side copy uses to hold a freeze open past the dead-man
// window. It must extend the window WITHOUT touching the filesystem: a repeat
// FREEZE would hit FIFREEZE's EBUSY on an already-frozen filesystem and re-arm
// nothing, which is why this op exists at all.
func TestRenewExtendsTheDeadManWithoutTouchingTheFilesystem(t *testing.T) {
	ops := &fakeOps{}
	sched := &fakeScheduler{}
	s := newTestServer(t, ops, sched, nil)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
	first := sched.last()

	resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpRenew})
	if !resp.GetOk() {
		t.Fatalf("renew failed: %s", resp.GetError())
	}
	second := sched.last()
	if first == second {
		t.Fatal("renew reused the freeze's timer instead of replacing the window")
	}
	if got := second.d; got != DefaultDeadMan {
		t.Fatalf("renewed window = %s, want %s", got, DefaultDeadMan)
	}
	// The filesystem was touched exactly once, by the freeze.
	if got := ops.got(); len(got) != 1 || got[0] != OpFreeze {
		t.Fatalf("ops calls = %v, want just [FREEZE] — RENEW must issue no syscall", got)
	}
	first.fire() // superseded by the renew — must be inert
	if got := ops.got(); len(got) != 1 {
		t.Fatalf("ops calls = %v, want the replaced timer to be inert", got)
	}
	second.fire()
	if got := ops.got(); len(got) != 2 || got[1] != OpThaw {
		t.Fatalf("ops calls = %v, want the renewed timer to still auto-thaw", got)
	}
}

// A renew that finds no armed dead-man means the freeze this host thought it
// held is GONE — it fired, or was thawed, or never happened. Whatever the host
// is copying under that freeze is void, so this fails loudly with its own
// sentinel instead of handing back a protection that does not exist.
func TestRenewRefusedWhenNoDeadManIsArmed(t *testing.T) {
	tests := []struct {
		name string
		arm  func(*Server, *fakeScheduler)
	}{
		{
			name: "never frozen",
			arm:  func(*Server, *fakeScheduler) {},
		},
		{
			name: "already thawed",
			arm: func(s *Server, _ *fakeScheduler) {
				s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
				s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpThaw})
			},
		},
		{
			name: "the dead-man already fired",
			arm: func(s *Server, sched *fakeScheduler) {
				s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpFreeze})
				sched.last().fire()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			sched := &fakeScheduler{}
			s := newTestServer(t, ops, sched, nil)
			tt.arm(s, sched)
			before := sched.count()

			resp := s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpRenew})
			if resp.GetOk() {
				t.Fatal("renew reported ok with no dead-man armed")
			}
			if resp.GetError() != ErrDeadManNotArmed.Error() {
				t.Fatalf("error = %q, want the distinct %q sentinel so the host can tell it from a transport failure", resp.GetError(), ErrDeadManNotArmed)
			}
			if sched.count() != before {
				t.Fatalf("timers armed = %d, want %d — a refused renew must arm nothing", sched.count(), before)
			}
		})
	}
}

func TestDeadManWindowCanOnlyBeShortened(t *testing.T) {
	tests := []struct {
		name    string
		seconds uint32
		want    time.Duration
	}{
		{"zero means the guest default", 0, DefaultDeadMan},
		{"host may shorten", 5, 5 * time.Second},
		{"exactly the default", uint32(DefaultDeadMan / time.Second), DefaultDeadMan},
		{"host may not lengthen", 3600, DefaultDeadMan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deadManFor(tt.seconds); got != tt.want {
				t.Fatalf("deadManFor(%d) = %s, want %s", tt.seconds, got, tt.want)
			}
		})
	}
}

func TestShutdownWaitDefault(t *testing.T) {
	ops := &fakeOps{}
	s := newTestServer(t, ops, &fakeScheduler{}, nil)

	s.Handle(context.Background(), &runtimev1.ControlRequest{Token: testToken, Op: OpShutdown})
	if ops.lastShutdownWait != DefaultShutdownWait {
		t.Fatalf("shutdown wait = %s, want %s", ops.lastShutdownWait, DefaultShutdownWait)
	}

	s.Handle(context.Background(), &runtimev1.ControlRequest{
		Token: testToken, Op: OpShutdown, ShutdownWaitSeconds: 7,
	})
	if ops.lastShutdownWait != 7*time.Second {
		t.Fatalf("shutdown wait = %s, want 7s", ops.lastShutdownWait)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Wire + connection handling
// ─────────────────────────────────────────────────────────────────────

func TestMessageRoundTrip(t *testing.T) {
	var buf strings.Builder
	req := &runtimev1.ControlRequest{
		Token:               testToken,
		Op:                  OpFreeze,
		DeadmanSeconds:      30,
		ShutdownWaitSeconds: 12,
	}
	if err := WriteMessage(&buf, req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	var got runtimev1.ControlRequest
	if err := ReadMessage(strings.NewReader(buf.String()), &got); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.GetToken() != req.GetToken() || got.GetOp() != req.GetOp() ||
		got.GetDeadmanSeconds() != req.GetDeadmanSeconds() ||
		got.GetShutdownWaitSeconds() != req.GetShutdownWaitSeconds() {
		t.Fatalf("round-trip = %+v, want %+v", &got, req)
	}
}

func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	// A 4-byte length prefix claiming more than maxFrame must be refused before
	// the payload is allocated.
	frame := []byte{0xff, 0xff, 0xff, 0xff}
	var msg runtimev1.ControlRequest
	err := ReadMessage(strings.NewReader(string(frame)), &msg)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a frame-too-large refusal", err)
	}
}

func TestServeConnAnswersOneRequest(t *testing.T) {
	ops := &fakeOps{}
	s := newTestServer(t, ops, &fakeScheduler{}, nil)

	var in, out strings.Builder
	if err := WriteMessage(&in, &runtimev1.ControlRequest{Token: testToken, Op: OpSyncFS}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	s.ServeConn(context.Background(), pipeConn{Reader: strings.NewReader(in.String()), Writer: &out})

	var resp runtimev1.ControlResponse
	if err := ReadMessage(strings.NewReader(out.String()), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.GetOk() {
		t.Fatalf("response not ok: %s", resp.GetError())
	}
	if got := ops.got(); len(got) != 1 || got[0] != OpSyncFS {
		t.Fatalf("ops calls = %v, want [SYNCFS]", got)
	}
}

func TestServeConnFiresShutdownHookAfterReplying(t *testing.T) {
	ops := &fakeOps{}
	var out strings.Builder
	var replyLenAtHook int
	fired := false

	s, err := NewServer(Config{
		Token: testToken,
		Ops:   ops,
		OnShutdownReplied: func() {
			fired = true
			// The hook is what holds the guest's power-off; if it ran before the
			// reply was written the host would get an EOF instead of an ack.
			replyLenAtHook = out.Len()
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var in strings.Builder
	if err := WriteMessage(&in, &runtimev1.ControlRequest{Token: testToken, Op: OpShutdown}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	s.ServeConn(context.Background(), pipeConn{Reader: strings.NewReader(in.String()), Writer: &out})

	if !fired {
		t.Fatal("OnShutdownReplied never fired")
	}
	if replyLenAtHook == 0 || replyLenAtHook != out.Len() {
		t.Fatalf("hook saw %d reply bytes, final reply is %d — hook must run AFTER the write",
			replyLenAtHook, out.Len())
	}
}

func TestServeConnDoesNotFireShutdownHookForOtherOps(t *testing.T) {
	fired := false
	s, err := NewServer(Config{
		Token:             testToken,
		Ops:               &fakeOps{},
		OnShutdownReplied: func() { fired = true },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var in, out strings.Builder
	if err := WriteMessage(&in, &runtimev1.ControlRequest{Token: testToken, Op: OpSyncFS}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	s.ServeConn(context.Background(), pipeConn{Reader: strings.NewReader(in.String()), Writer: &out})

	if fired {
		t.Fatal("OnShutdownReplied fired for a non-shutdown op")
	}
}

// errListener always fails Accept — the accept loop must back off and exit on
// ctx cancellation rather than spin.
type errListener struct{ n int }

func (l *errListener) Accept() (Conn, error) {
	l.n++
	return nil, errors.New("listener is gone")
}

func (l *errListener) Close() error { return nil }

func TestServeStopsOnContextCancel(t *testing.T) {
	s := newTestServer(t, &fakeOps{}, &fakeScheduler{}, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Serve(ctx, &errListener{})
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancellation")
	}
}
