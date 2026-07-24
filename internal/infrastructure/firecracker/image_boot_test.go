package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/port/gateway"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

func TestImageBootArgs(t *testing.T) {
	got := imageBootArgs("10.201.0.6", "10.201.0.5")
	want := "console=ttyS0 reboot=k panic=1 pci=off init=/sentiae/init ip=10.201.0.6::10.201.0.5:255.255.255.252::eth0:off"
	if got != want {
		t.Errorf("imageBootArgs =\n %q\nwant\n %q", got, want)
	}
}

func TestDeriveNet(t *testing.T) {
	tests := []struct {
		n                int
		tap, host, guest string
	}{
		{1, "img1", "10.201.0.5", "10.201.0.6"},       // base = 4
		{2, "img2", "10.201.0.9", "10.201.0.10"},      // base = 8
		{63, "img63", "10.201.0.253", "10.201.0.254"}, // base = 252
		{64, "img64", "10.201.1.1", "10.201.1.2"},     // base = 256 → octet3 rolls
	}
	for _, tt := range tests {
		nw := deriveNet(tt.n)
		if nw.tapName != tt.tap || nw.hostIP != tt.host || nw.guestIP != tt.guest {
			t.Errorf("deriveNet(%d) = {tap:%s host:%s guest:%s}, want {tap:%s host:%s guest:%s}",
				tt.n, nw.tapName, nw.hostIP, nw.guestIP, tt.tap, tt.host, tt.guest)
		}
	}
}

func TestDeriveNetUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for n := 1; n <= imgMaxIndex; n++ {
		nw := deriveNet(n)
		if seen[nw.guestIP] {
			t.Fatalf("duplicate guest IP %s at index %d", nw.guestIP, n)
		}
		seen[nw.guestIP] = true
	}
}

func TestParseExitCode(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"0\n", 0},
		{"42", 42},
		{" 7 \n", 7},
		{"-1", -1},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		if got := parseExitCode(tt.in); got != tt.want {
			t.Errorf("parseExitCode(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeResources(t *testing.T) {
	if v, m := normalizeResources(0, 0); v != 1 || m != 512 {
		t.Errorf("defaults = (%d,%d), want (1,512)", v, m)
	}
	if v, m := normalizeResources(4, 2048); v != 4 || m != 2048 {
		t.Errorf("passthrough = (%d,%d), want (4,2048)", v, m)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Resident stop path (#resident-stop-is-vmm-kill)
// ─────────────────────────────────────────────────────────────────────

// stopRecorder is the ordered event log the stop-path fakes share. ORDER is the
// property under test: a guest shutdown issued AFTER the VMM was signalled is as
// broken as no guest shutdown at all, and only an ordered log catches that.
type stopRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *stopRecorder) add(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *stopRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// fakeGuestControl records the SHUTDOWN and returns a scripted outcome. The
// filesystem ops record too: the stop path must never call them.
type fakeGuestControl struct {
	rec    *stopRecorder
	err    error
	delay  time.Duration // a guest that takes time to answer
	onCall func()        // observation hook, run while the call is in flight
}

var _ gateway.GuestControl = (*fakeGuestControl)(nil)

func (f *fakeGuestControl) Shutdown(ctx context.Context, _ string) error {
	f.rec.add("shutdown")
	if f.onCall != nil {
		f.onCall()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("guest control shutdown: %w", err)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			f.rec.add("shutdown-timeout")
			return fmt.Errorf("guest control shutdown: %w", ctx.Err())
		}
	}
	return f.err
}

func (f *fakeGuestControl) SyncFS(context.Context, string) error {
	f.rec.add("syncfs")
	return nil
}

func (f *fakeGuestControl) Freeze(context.Context, string) error {
	f.rec.add("freeze")
	return nil
}

func (f *fakeGuestControl) Renew(context.Context, string) error {
	f.rec.add("renew")
	return nil
}

func (f *fakeGuestControl) Thaw(context.Context, string) error {
	f.rec.add("thaw")
	return nil
}

// fakeProcess stands in for the VMM. exitAfterPolls simulates the guest powering
// itself off (the process disappears after N liveness polls; -1 never does);
// diesOnTerm simulates a VMM that answers SIGTERM.
type fakeProcess struct {
	rec            *stopRecorder
	exitAfterPolls int
	diesOnTerm     bool

	mu    sync.Mutex
	polls int
	dead  bool
	done  chan struct{}
}

var _ vmProcess = (*fakeProcess)(nil)

func newFakeProcess(rec *stopRecorder, exitAfterPolls int, diesOnTerm bool) *fakeProcess {
	return &fakeProcess{
		rec:            rec,
		exitAfterPolls: exitAfterPolls,
		diesOnTerm:     diesOnTerm,
		done:           make(chan struct{}),
	}
}

// die marks the process gone. Caller holds mu.
func (p *fakeProcess) die() {
	if !p.dead {
		p.dead = true
		close(p.done)
	}
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sig == syscall.Signal(0) {
		// Liveness poll — deliberately unrecorded, it is not part of the sequence
		// under test.
		if p.dead {
			return errors.New("os: process already finished")
		}
		p.polls++
		if p.exitAfterPolls >= 0 && p.polls >= p.exitAfterPolls {
			p.die()
			return errors.New("os: process already finished")
		}
		return nil
	}
	if sig == syscall.SIGTERM {
		p.rec.add("sigterm")
		if p.diesOnTerm {
			p.die()
		}
		return nil
	}
	p.rec.add("signal-" + sig.String())
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rec.add("sigkill")
	p.die()
	return nil
}

func (p *fakeProcess) Wait() (*os.ProcessState, error) {
	<-p.done
	return nil, nil
}

// newStopTestBooter builds a booter whose guest channel and VMM handle are fakes
// and whose stop budgets are compressed (the real ones are tens of seconds).
func newStopTestBooter(t *testing.T, gc gateway.GuestControl, proc vmProcess) *ImageBooter {
	t.Helper()
	b := NewImageBooter(
		NewProvider(config.FirecrackerConfig{ChrootBase: t.TempDir()}),
		"10.0.0.1",
		NewGuestControlTokens(),
	)
	b.guestControl = gc
	b.stop = stopTimings{
		guestShutdown: 50 * time.Millisecond,
		powerOff:      30 * time.Millisecond,
		exitPoll:      5 * time.Millisecond,
		sigtermGrace:  30 * time.Millisecond,
	}
	b.findProcess = func(int) (vmProcess, error) { return proc, nil }
	return b
}

func TestDecommissionStopsGuestBeforeSignallingVMM(t *testing.T) {
	tests := []struct {
		name           string
		noControl      bool
		gcErr          error
		gcDelay        time.Duration
		exitAfterPolls int
		diesOnTerm     bool
		cancelCtx      bool
		want           []string
	}{
		{
			name:           "guest shuts down and the vmm exits: no signal at all",
			exitAfterPolls: 1,
			diesOnTerm:     true,
			want:           []string{"shutdown"},
		},
		{
			name:           "vm booted before the control channel existed: signal",
			noControl:      true,
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"sigterm"},
		},
		{
			name:           "no control token for this vm: attempt, then signal",
			gcErr:          domain.ErrGuestControlUnavailable,
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"shutdown", "sigterm"},
		},
		{
			name:           "guest too old to know the verb: attempt, then signal",
			gcErr:          errors.New("guest refused SHUTDOWN: unknown op"),
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"shutdown", "sigterm"},
		},
		{
			name:           "unreachable guest: attempt, then signal",
			gcErr:          errors.New("connect guest control channel: dial: connection refused"),
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"shutdown", "sigterm"},
		},
		{
			name:           "wedged guest: attempt times out, then signal",
			gcDelay:        time.Second,
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"shutdown", "shutdown-timeout", "sigterm"},
		},
		{
			name:           "guest acks but the vmm never powers off: signal, then kill",
			exitAfterPolls: -1,
			want:           []string{"shutdown", "sigterm", "sigkill"},
		},
		{
			name:           "guest that never dies is still torn down",
			gcErr:          errors.New("connect guest control channel: dial: connection refused"),
			exitAfterPolls: -1,
			want:           []string{"shutdown", "sigterm", "sigkill"},
		},
		{
			name:           "cancelled caller still stops the vm",
			cancelCtx:      true,
			exitAfterPolls: -1,
			diesOnTerm:     true,
			want:           []string{"shutdown", "sigterm"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &stopRecorder{}
			proc := newFakeProcess(rec, tt.exitAfterPolls, tt.diesOnTerm)
			b := newStopTestBooter(t, &fakeGuestControl{rec: rec, err: tt.gcErr, delay: tt.gcDelay}, proc)
			if tt.noControl {
				b.guestControl = nil
			}

			dir := t.TempDir()
			socketPath := filepath.Join(dir, "vm.sock")
			rootfsPath := filepath.Join(dir, "rootfs.ext4")
			writeStopTestFile(t, socketPath)
			writeStopTestFile(t, rootfsPath)
			b.controlTokens.Put(socketPath, "vm-token")
			b.Seed([]int{1, 2, 3, 4, 5, 6, 7})

			ctx := context.Background()
			if tt.cancelCtx {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			if err := b.Decommission(ctx, usecase.ImageDecommissionInput{
				PID:        4242,
				SocketPath: socketPath,
				NetIndex:   7,
				RootfsPath: rootfsPath,
			}); err != nil {
				t.Fatalf("Decommission: %v", err)
			}

			if got := rec.got(); !slices.Equal(got, tt.want) {
				t.Fatalf("stop sequence = %v, want %v", got, tt.want)
			}
			// Teardown always completes, whatever the guest did.
			if _, err := os.Stat(rootfsPath); !os.IsNotExist(err) {
				t.Errorf("rootfs still present: %v", err)
			}
			if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
				t.Errorf("socket still present: %v", err)
			}
			if _, ok := b.controlTokens.Get(socketPath); ok {
				t.Error("control token still held after teardown")
			}
			if n, err := b.allocIndex(); err != nil || n != 7 {
				t.Errorf("net index not freed: allocIndex = %d, %v; want 7", n, err)
			}
		})
	}
}

// TestDecommissionHoldsControlTokenUntilShutdown pins the ordering trap: the
// token is what authenticates the guest SHUTDOWN, so deleting it as part of
// cleanup BEFORE the attempt would make every graceful stop fall back to killing
// the VMM. A test that only checks "shutdown was called" cannot see that.
func TestDecommissionHoldsControlTokenUntilShutdown(t *testing.T) {
	rec := &stopRecorder{}
	gc := &fakeGuestControl{rec: rec}
	b := newStopTestBooter(t, gc, newFakeProcess(rec, 1, true))

	socketPath := filepath.Join(t.TempDir(), "vm.sock")
	writeStopTestFile(t, socketPath)
	b.controlTokens.Put(socketPath, "vm-token")

	var tokenAtCall string
	gc.onCall = func() { tokenAtCall, _ = b.controlTokens.Get(socketPath) }

	if err := b.Decommission(context.Background(), usecase.ImageDecommissionInput{
		PID:        4242,
		SocketPath: socketPath,
	}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	if tokenAtCall != "vm-token" {
		t.Fatalf("control token at shutdown = %q, want %q (deleted too early)", tokenAtCall, "vm-token")
	}
	if _, ok := b.controlTokens.Get(socketPath); ok {
		t.Error("control token still held after teardown")
	}
}

// TestNewImageBooterSharesControlTokenStore pins the wiring: a control client
// built over a DIFFERENT store finds no token for any VM, so every graceful stop
// would silently degrade to a kill.
func TestNewImageBooterSharesControlTokenStore(t *testing.T) {
	tokens := NewGuestControlTokens()
	b := NewImageBooter(nil, "10.0.0.1", tokens)

	client, ok := b.guestControl.(*GuestControlClient)
	if !ok {
		t.Fatalf("guestControl = %T, want *GuestControlClient", b.guestControl)
	}
	if client.tokens != tokens {
		t.Error("control client does not share the booter's token store")
	}
}

func TestWaitForProcessExit(t *testing.T) {
	t.Run("returns once the process is gone", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, 2, false)
		if err := waitForProcessExit(context.Background(), proc, time.Second, time.Millisecond); err != nil {
			t.Fatalf("waitForProcessExit: %v", err)
		}
	})
	t.Run("gives up when the process stays alive", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, -1, false)
		if err := waitForProcessExit(context.Background(), proc, 20*time.Millisecond, time.Millisecond); err == nil {
			t.Fatal("waitForProcessExit = nil, want a timeout error")
		}
	})
	t.Run("returns the caller's cancellation rather than waiting", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, -1, false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitForProcessExit(ctx, proc, time.Hour, time.Millisecond)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForProcessExit = %v, want context.Canceled", err)
		}
	})
}

func writeStopTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAllocIndex(t *testing.T) {
	// rt#8 retired per-VM host-port DNAT: only the /30 network index is allocated.
	b := NewImageBooter(nil, "10.0.0.1", NewGuestControlTokens())
	b.Seed([]int{1})

	n, err := b.allocIndex()
	if err != nil || n != 2 {
		t.Fatalf("allocIndex = %d,%v; want 2 (1 seeded used)", n, err)
	}
	b.freeIndex(2)
	if n, _ := b.allocIndex(); n != 2 {
		t.Fatalf("allocIndex after free = %d, want 2", n)
	}
}
