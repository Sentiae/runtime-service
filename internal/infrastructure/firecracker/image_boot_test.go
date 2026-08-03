package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
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

// fakeProcess stands in for the VMM.
//
// exitAfterPolls is the number of signal-0 probes it survives before it
// disappears (-1 = never). Probe #1 is the ADMISSION probe proveTerminated makes
// before anything else, so a value of 1 models a process that was already gone
// when teardown started.
//
// waits counts Wait() calls and MUST stay zero: Wait is not exit proof (the boot
// path may already own it, and after a service restart the VMM is not our child),
// and a teardown that trusted it would delete a live VM's jail.
type fakeProcess struct {
	rec            *stopRecorder
	exitAfterPolls int
	diesOnTerm     bool
	// probeErr replaces the signal-0 answer with a NON-absence error (EPERM is
	// the real-world case: the process exists and we may not touch it).
	probeErr error
	// waitErr is what a service-restart Wait() would return (ECHILD).
	waitErr error

	mu    sync.Mutex
	polls int
	waits int
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
			return os.ErrProcessDone
		}
		if p.probeErr != nil {
			return p.probeErr
		}
		p.polls++
		if p.exitAfterPolls >= 0 && p.polls >= p.exitAfterPolls {
			p.die()
			return os.ErrProcessDone
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
	if sig == syscall.SIGKILL {
		p.rec.add("sigkill")
		return nil
	}
	p.rec.add("signal-" + sig.String())
	return nil
}

// killAfterSignal makes the process answer SIGKILL by dying, which is what a
// normal VMM does.
func (p *fakeProcess) killAfterSignal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.die()
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rec.add("sigkill")
	p.die()
	return nil
}

func (p *fakeProcess) Wait() (*os.ProcessState, error) {
	p.mu.Lock()
	p.waits++
	err := p.waitErr
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	<-p.done
	return nil, nil
}

func (p *fakeProcess) waitCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waits
}

// fakeInspector is a processInspector that answers whatever the test scripts.
// Its ZERO-argument constructor answers PROVINGLY: every fact matches, so a test
// that wants a mismatch has to state which one, and a future field added to the
// proof cannot silently start passing.
type fakeInspector struct {
	commVal string
	argv    []string
	pgidVal int
	jailPid int
	commErr error
	argvErr error
	pgidErr error
	jailErr error
	// calls counts every fact read, so a refusal test can prove the proof ran at
	// all rather than short-circuiting somewhere unexpected.
	calls int
}

func provingInspector(pid, slot int, ownerID uuid.UUID) *fakeInspector {
	return &fakeInspector{
		commVal: jailExecName,
		argv: []string{
			"/usr/bin/firecracker",
			"--id", strconv.Itoa(slot),
			"--api-sock", "/run/" + ownerID.String() + ".sock",
		},
		pgidVal: pid,
		jailPid: pid,
	}
}

func (f *fakeInspector) comm(int) (string, error) {
	f.calls++
	return f.commVal, f.commErr
}
func (f *fakeInspector) cmdline(int) ([]string, error) {
	f.calls++
	return f.argv, f.argvErr
}
func (f *fakeInspector) pgid(int) (int, error) {
	f.calls++
	return f.pgidVal, f.pgidErr
}
func (f *fakeInspector) jailPID(string) (int, error) {
	f.calls++
	return f.jailPid, f.jailErr
}

// stopFixture is one teardown under test: a booter over fakes, the durable
// handle it is asked to tear down, and the artifacts that must survive a
// refusal.
type stopFixture struct {
	b     *ImageBooter
	proc  *fakeProcess
	insp  *fakeInspector
	alloc *fakeNetAlloc
	rec   *stopRecorder
	in    usecase.ImageDecommissionInput
	// ownerID is the SEEDED lease owner, kept separately because a test may
	// corrupt in.OwnerID to break the identity proof — the lease assertion must
	// still look the real lease up.
	ownerID uuid.UUID
	lease   domain.NetLease
	socket  string
	rootfs  string
	jail    string
}

const stopTestPID = 4242

// newStopFixture builds a teardown whose identity proof PASSES by default, so
// each test states only the one fact it is breaking.
func newStopFixture(t *testing.T, gc gateway.GuestControl, proc *fakeProcess) *stopFixture {
	t.Helper()
	chrootBase := t.TempDir()
	b := NewImageBooter(
		NewProvider(config.FirecrackerConfig{ChrootBase: chrootBase}),
		"10.0.0.1",
		NewGuestControlTokens(),
		newFakeNetAlloc(),
	)
	b.guestControl = gc
	b.stop = stopTimings{
		guestShutdown: 50 * time.Millisecond,
		powerOff:      30 * time.Millisecond,
		exitPoll:      5 * time.Millisecond,
		sigtermGrace:  30 * time.Millisecond,
		sigkillGrace:  30 * time.Millisecond,
	}
	b.findProcess = func(int) (vmProcess, error) { return proc, nil }

	ownerID := uuid.New()
	alloc := b.alloc.(*fakeNetAlloc)
	lease, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, ownerID)
	if err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	// The socket basename IS the owner id — that is what the proof checks, and
	// what startVM actually writes.
	jail := filepath.Join(chrootBase, jailExecName, strconv.Itoa(lease.LocalSlot))
	socketDir := filepath.Join(jail, "root", "run")
	if mkErr := os.MkdirAll(socketDir, 0o750); mkErr != nil {
		t.Fatalf("mkdir jail: %v", mkErr)
	}
	socket := filepath.Join(socketDir, ownerID.String()+".sock")
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeStopTestFile(t, socket)
	writeStopTestFile(t, rootfs)
	b.controlTokens.Put(socket, "vm-token")

	insp := provingInspector(stopTestPID, lease.LocalSlot, ownerID)
	b.inspect = insp

	return &stopFixture{
		b: b, proc: proc, insp: insp, alloc: alloc, rec: proc.rec,
		in: usecase.ImageDecommissionInput{
			OwnerKind:  domain.NetLeaseOwnerReplica,
			OwnerID:    ownerID,
			PID:        stopTestPID,
			SocketPath: socket,
			NetIndex:   lease.NetIndex,
			RootfsPath: rootfs,
		},
		ownerID: ownerID, lease: lease, socket: socket, rootfs: rootfs, jail: jail,
	}
}

// assertEverythingPreserved is the refusal invariant: a teardown that cannot
// prove the VM is gone must leave EVERY resource that VM could still be using.
// Releasing any one of them is what hands the next boot a live VM's address, uid
// or chroot.
func (f *stopFixture) assertEverythingPreserved(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.rootfs); err != nil {
		t.Errorf("rootfs was removed on an unproven teardown: %v", err)
	}
	if _, err := os.Stat(f.socket); err != nil {
		t.Errorf("socket was removed on an unproven teardown: %v", err)
	}
	if _, err := os.Stat(f.jail); err != nil {
		t.Errorf("jail dir was removed on an unproven teardown: %v", err)
	}
	if !f.alloc.held(domain.NetLeaseOwnerReplica, f.ownerID) {
		t.Error("addressing lease was released on an unproven teardown — the next boot can now be handed this live VM's /30, uid and chroot")
	}
	if _, ok := f.b.controlTokens.Get(f.socket); !ok {
		t.Error("control token was dropped on an unproven teardown")
	}
}

func (f *stopFixture) assertEverythingReclaimed(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.rootfs); !os.IsNotExist(err) {
		t.Errorf("rootfs still present: %v", err)
	}
	if _, err := os.Stat(f.socket); !os.IsNotExist(err) {
		t.Errorf("socket still present: %v", err)
	}
	if _, err := os.Stat(f.jail); !os.IsNotExist(err) {
		t.Errorf("jail dir still present: %v", err)
	}
	if _, ok := f.b.controlTokens.Get(f.socket); ok {
		t.Error("control token still held after teardown")
	}
	if f.alloc.held(domain.NetLeaseOwnerReplica, f.ownerID) {
		t.Errorf("addressing lease still held after teardown (slot %d permanently burned)", f.lease.LocalSlot)
	}
}

// TestDecommissionStopsGuestBeforeSignallingVMM pins the ORDER of the stop
// ladder — guest first, then TERM, then KILL — and, for every branch where
// absence is observed, that the teardown then completes.
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
			name: "already gone before teardown starts: no guest call, no signal",
			// Probe #1 is the admission probe, so this process is absent from the
			// outset — and absence alone is complete proof, with no identity check.
			exitAfterPolls: 1,
			want:           nil,
		},
		{
			name:           "guest shuts down and the vmm exits: no signal at all",
			exitAfterPolls: 2,
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
			// The cancelled caller must NOT abandon the fallback: an RPC deadline is
			// not a reason to leave a live VM behind a released lease.
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
			f := newStopFixture(t, &fakeGuestControl{rec: rec, err: tt.gcErr, delay: tt.gcDelay}, proc)
			if tt.noControl {
				f.b.guestControl = nil
			}

			ctx := context.Background()
			if tt.cancelCtx {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			if err := f.b.Decommission(ctx, f.in); err != nil {
				t.Fatalf("Decommission: %v", err)
			}
			if got := rec.got(); !slices.Equal(got, tt.want) {
				t.Fatalf("stop sequence = %v, want %v", got, tt.want)
			}
			f.assertEverythingReclaimed(t)
			if n := proc.waitCalls(); n != 0 {
				t.Errorf("Wait() called %d times — it is never exit proof", n)
			}
		})
	}
}

// TestDecommissionWaitsOutThePowerOffWindowAfterAShutdownError.
//
// A failed Shutdown CALL does not mean the guest did not receive it: a timeout, a
// lost ack, and a reply that arrived late are indistinguishable from here, and in
// every one of them the guest may be part-way through the clean stop this path
// exists to allow — Postgres running its fast-shutdown checkpoint, image-init
// syncing and issuing its power-off. Signalling at that moment crash-stops a
// customer's database mid-flush, which is the exact failure the control channel
// was added to remove. So the power-off window is waited out on BOTH branches.
func TestDecommissionWaitsOutThePowerOffWindowAfterAShutdownError(t *testing.T) {
	rec := &stopRecorder{}
	// The guest errors on the call but powers itself off during the window: probe
	// #1 is admission (alive), and it disappears on the next poll.
	proc := newFakeProcess(rec, 2, true)
	f := newStopFixture(t, &fakeGuestControl{rec: rec, err: errors.New("guest control shutdown: context deadline exceeded")}, proc)

	if err := f.b.Decommission(context.Background(), f.in); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	want := []string{"shutdown"}
	if got := rec.got(); !slices.Equal(got, want) {
		t.Fatalf("stop sequence = %v, want %v — no signal may be sent while the guest is still powering off", got, want)
	}
	f.assertEverythingReclaimed(t)
}

// TestDecommissionKillLadderProvesExit walks the full TERM→KILL ladder for a
// guest that ignores everything until SIGKILL.
func TestDecommissionKillLadderProvesExit(t *testing.T) {
	rec := &stopRecorder{}
	proc := newFakeProcess(rec, -1, false)
	f := newStopFixture(t, &fakeGuestControl{rec: rec, err: errors.New("unreachable")}, proc)
	// A normal VMM answers SIGKILL by dying; the fake needs telling.
	go func() {
		for {
			if slices.Contains(rec.got(), "sigkill") {
				proc.killAfterSignal()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := f.b.Decommission(context.Background(), f.in); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	want := []string{"shutdown", "sigterm", "sigkill"}
	if got := rec.got(); !slices.Equal(got, want) {
		t.Fatalf("stop sequence = %v, want %v", got, want)
	}
	f.assertEverythingReclaimed(t)
}

// TestDecommissionRefusesWhenTheVMSurvivesSIGKILL is the defect this whole item
// exists for. The old path sent SIGKILL, assumed it worked, and deleted the TAP,
// the lease, the jail and the rootfs — producing a running microVM with no
// record of itself. Seven of those are on the live fleet host.
func TestDecommissionRefusesWhenTheVMSurvivesSIGKILL(t *testing.T) {
	rec := &stopRecorder{}
	proc := newFakeProcess(rec, -1, false)
	f := newStopFixture(t, &fakeGuestControl{rec: rec, err: errors.New("unreachable")}, proc)

	err := f.b.Decommission(context.Background(), f.in)
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("Decommission = %v, want ErrVMTerminationUnproven", err)
	}
	want := []string{"shutdown", "sigterm", "sigkill"}
	if got := rec.got(); !slices.Equal(got, want) {
		t.Fatalf("stop sequence = %v, want %v", got, want)
	}
	f.assertEverythingPreserved(t)
	if n := proc.waitCalls(); n != 0 {
		t.Errorf("Wait() called %d times — it is never exit proof", n)
	}
}

// TestDecommissionRefusesOnIdentityMismatch: a recorded pid is a NUMBER, and pids
// are reused. Each row below breaks exactly ONE fact of the proof, and each must
// refuse BEFORE any signal and before any cleanup.
func TestDecommissionRefusesOnIdentityMismatch(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(f *stopFixture)
	}{
		{"comm is not firecracker", func(f *stopFixture) { f.insp.commVal = "postgres" }},
		{"comm unreadable", func(f *stopFixture) { f.insp.commErr = errors.New("no such process") }},
		{"argv names a different jail slot", func(f *stopFixture) {
			f.insp.argv = []string{"/usr/bin/firecracker", "--id", "999", "--api-sock", "/run/" + f.in.OwnerID.String() + ".sock"}
		}},
		{"argv names a different owner's socket", func(f *stopFixture) {
			f.insp.argv = []string{"/usr/bin/firecracker", "--id", strconv.Itoa(f.lease.LocalSlot), "--api-sock", "/run/" + uuid.NewString() + ".sock"}
		}},
		{"argv unreadable", func(f *stopFixture) { f.insp.argvErr = errors.New("permission denied") }},
		{"process is not its own group leader", func(f *stopFixture) { f.insp.pgidVal = 1 }},
		{"process group unreadable", func(f *stopFixture) { f.insp.pgidErr = errors.New("no such process") }},
		{"jail pidfile records a different pid", func(f *stopFixture) { f.insp.jailPid = 9999 }},
		{"jail pidfile unreadable", func(f *stopFixture) { f.insp.jailErr = errors.New("no such file") }},
		{"row records no net index, so no slot can be derived", func(f *stopFixture) { f.in.NetIndex = 0 }},
		{"row's socket is not this owner's", func(f *stopFixture) { f.in.SocketPath = "/tmp/somebody-else.sock" }},
		{"row records no owner id", func(f *stopFixture) { f.in.OwnerID = uuid.Nil }},
		{"row records an unrecognized owner kind", func(f *stopFixture) { f.in.OwnerKind = "not-a-kind" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &stopRecorder{}
			// -1: alive throughout, so only the identity proof can stop the teardown.
			proc := newFakeProcess(rec, -1, true)
			f := newStopFixture(t, &fakeGuestControl{rec: rec}, proc)
			tt.break_(f)

			err := f.b.Decommission(context.Background(), f.in)
			if !errors.Is(err, domain.ErrVMTerminationUnproven) {
				t.Fatalf("Decommission = %v, want ErrVMTerminationUnproven", err)
			}
			if got := rec.got(); len(got) != 0 {
				t.Fatalf("a refused teardown signalled or called the guest: %v", got)
			}
			f.assertEverythingPreserved(t)
		})
	}
}

// TestDecommissionRefusesWhenTheProbeCannotAnswer: EPERM means "the process is
// there and I may not touch it", which is the opposite of absence. Reading it as
// "gone" is how a live VM's resources get released.
func TestDecommissionRefusesWhenTheProbeCannotAnswer(t *testing.T) {
	rec := &stopRecorder{}
	proc := newFakeProcess(rec, -1, true)
	proc.probeErr = syscall.EPERM
	f := newStopFixture(t, &fakeGuestControl{rec: rec}, proc)

	err := f.b.Decommission(context.Background(), f.in)
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("Decommission = %v, want ErrVMTerminationUnproven", err)
	}
	f.assertEverythingPreserved(t)
}

// TestDecommissionAfterServiceRestart: the VMM is no longer this process's child,
// so Wait() answers ECHILD immediately. The teardown must still succeed through
// signal-0 disappearance — and must never have called Wait at all.
func TestDecommissionAfterServiceRestart(t *testing.T) {
	rec := &stopRecorder{}
	proc := newFakeProcess(rec, 2, true)
	proc.waitErr = syscall.ECHILD
	f := newStopFixture(t, &fakeGuestControl{rec: rec}, proc)

	if err := f.b.Decommission(context.Background(), f.in); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	if n := proc.waitCalls(); n != 0 {
		t.Fatalf("Wait() called %d times — an ECHILD would have read as 'exited' for a live process", n)
	}
	f.assertEverythingReclaimed(t)
}

// TestDecommissionNeverBootedRow: a scheduled replica that never booted has no
// pid and no artifacts. There is nothing a live VM could be holding, so the
// teardown proceeds — the refusal is about EVIDENCE, not about pessimism.
func TestDecommissionNeverBootedRow(t *testing.T) {
	rec := &stopRecorder{}
	b := NewImageBooter(NewProvider(config.FirecrackerConfig{ChrootBase: t.TempDir()}), "10.0.0.1",
		NewGuestControlTokens(), newFakeNetAlloc())
	b.guestControl = &fakeGuestControl{rec: rec}
	b.findProcess = func(int) (vmProcess, error) {
		t.Fatal("looked up a process for a row that never booted")
		return nil, nil
	}

	if err := b.Decommission(context.Background(), usecase.ImageDecommissionInput{
		OwnerKind: domain.NetLeaseOwnerReplica,
		OwnerID:   uuid.New(),
	}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("a never-booted row produced host actions: %v", got)
	}
}

// TestDecommissionRefusesArtifactsWithoutPID: artifacts but no pid means the VM
// may well be running under a number the row failed to record. Guessing a victim
// from the socket name — or killing by process name — is how an unrelated
// process gets killed.
func TestDecommissionRefusesArtifactsWithoutPID(t *testing.T) {
	b := NewImageBooter(NewProvider(config.FirecrackerConfig{ChrootBase: t.TempDir()}), "10.0.0.1",
		NewGuestControlTokens(), newFakeNetAlloc())
	alloc := b.alloc.(*fakeNetAlloc)
	ownerID := uuid.New()
	if _, err := alloc.Acquire(context.Background(), domain.NetLeaseOwnerReplica, ownerID); err != nil {
		t.Fatal(err)
	}
	err := b.Decommission(context.Background(), usecase.ImageDecommissionInput{
		OwnerKind: domain.NetLeaseOwnerReplica,
		OwnerID:   ownerID,
		TapName:   "img7",
		NetIndex:  7,
	})
	if !errors.Is(err, domain.ErrVMTerminationUnproven) {
		t.Fatalf("Decommission = %v, want ErrVMTerminationUnproven", err)
	}
	if !alloc.held(domain.NetLeaseOwnerReplica, ownerID) {
		t.Error("lease released for a row whose VM could still be running")
	}
}

// TestDecommissionReturnsLeaseReleaseFailure: after PROVEN death the lease
// release is required. Its failure is returned, and the jail and rootfs are kept
// so a retry can finish — a slot held with every identifying artifact deleted is
// only recoverable by hand.
func TestDecommissionReturnsLeaseReleaseFailure(t *testing.T) {
	rec := &stopRecorder{}
	proc := newFakeProcess(rec, 1, true)
	f := newStopFixture(t, &fakeGuestControl{rec: rec}, proc)
	f.alloc.releaseErr = errors.New("ledger unavailable")

	err := f.b.Decommission(context.Background(), f.in)
	if err == nil {
		t.Fatal("Decommission = nil, want the lease-release error")
	}
	if _, serr := os.Stat(f.rootfs); serr != nil {
		t.Errorf("rootfs removed despite a failed lease release: %v", serr)
	}
	if _, serr := os.Stat(f.jail); serr != nil {
		t.Errorf("jail removed despite a failed lease release: %v", serr)
	}
}

// TestDecommissionHoldsControlTokenUntilShutdown pins the ordering trap: the
// token is what authenticates the guest SHUTDOWN, so deleting it as part of
// cleanup BEFORE the attempt would make every graceful stop fall back to killing
// the VMM. A test that only checks "shutdown was called" cannot see that.
func TestDecommissionHoldsControlTokenUntilShutdown(t *testing.T) {
	rec := &stopRecorder{}
	gc := &fakeGuestControl{rec: rec}
	proc := newFakeProcess(rec, 2, true)
	f := newStopFixture(t, gc, proc)

	var tokenAtCall string
	gc.onCall = func() { tokenAtCall, _ = f.b.controlTokens.Get(f.socket) }

	if err := f.b.Decommission(context.Background(), f.in); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	if tokenAtCall != "vm-token" {
		t.Fatalf("control token at shutdown = %q, want %q (deleted too early)", tokenAtCall, "vm-token")
	}
	if _, ok := f.b.controlTokens.Get(f.socket); ok {
		t.Error("control token still held after teardown")
	}
}

// TestNewImageBooterSharesControlTokenStore pins the wiring: a control client
// built over a DIFFERENT store finds no token for any VM, so every graceful stop
// would silently degrade to a kill.
func TestNewImageBooterSharesControlTokenStore(t *testing.T) {
	tokens := NewGuestControlTokens()
	b := NewImageBooter(nil, "10.0.0.1", tokens, newFakeNetAlloc())

	client, ok := b.guestControl.(*GuestControlClient)
	if !ok {
		t.Fatalf("guestControl = %T, want *GuestControlClient", b.guestControl)
	}
	if client.tokens != tokens {
		t.Error("control client does not share the booter's token store")
	}
}

func TestWaitProcessGone(t *testing.T) {
	t.Run("proves absence once the process is gone", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, 2, false)
		gone, err := waitProcessGone(context.Background(), proc, time.Second, time.Millisecond)
		if err != nil || !gone {
			t.Fatalf("waitProcessGone = (%v,%v), want (true,nil)", gone, err)
		}
	})
	t.Run("does not claim absence for a process that stays alive", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, -1, false)
		gone, err := waitProcessGone(context.Background(), proc, 20*time.Millisecond, time.Millisecond)
		if gone || err != nil {
			t.Fatalf("waitProcessGone = (%v,%v), want (false,nil)", gone, err)
		}
	})
	t.Run("a probe that cannot answer is not absence", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, -1, false)
		proc.probeErr = syscall.EPERM
		gone, err := waitProcessGone(context.Background(), proc, 20*time.Millisecond, time.Millisecond)
		if gone || err == nil {
			t.Fatalf("waitProcessGone = (%v,%v), want (false, an error)", gone, err)
		}
	})
	t.Run("a cancelled context ends the wait without claiming absence", func(t *testing.T) {
		proc := newFakeProcess(&stopRecorder{}, -1, false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		gone, err := waitProcessGone(ctx, proc, time.Hour, time.Millisecond)
		if gone || err != nil {
			t.Fatalf("waitProcessGone = (%v,%v), want (false,nil)", gone, err)
		}
	})
}

func writeStopTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSetupNetRefusesWithoutAllocator pins the no-fallback rule: a booter with no
// addressing plane must refuse to boot, never compute an address locally. The old
// in-memory allocator IS the vulnerability this plane replaces, so "degrade to
// local allocation" must not exist as a code path.
func TestSetupNetRefusesWithoutAllocator(t *testing.T) {
	b := NewImageBooter(NewProvider(config.FirecrackerConfig{ChrootBase: t.TempDir()}), "10.0.0.1", nil, nil)
	_, _, err := b.setupNet(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("setupNet without an allocator = %v, want ErrNetPlaneUnreconciled", err)
	}
}

// TestSetupNetRefusesWhenLeaseRefused pins that a refused lease refuses the BOOT.
// A booter that swallowed an allocation error and continued would run a VM with no
// fenced address, uid or chroot at all.
func TestSetupNetRefusesWhenLeaseRefused(t *testing.T) {
	alloc := newFakeNetAlloc()
	alloc.acquireErr = domain.ErrNetLeaseExhausted
	b := NewImageBooter(NewProvider(config.FirecrackerConfig{ChrootBase: t.TempDir()}), "10.0.0.1", nil, alloc)
	_, _, err := b.setupNet(context.Background(), domain.NetLeaseOwnerReplica, uuid.New())
	if !errors.Is(err, domain.ErrNetLeaseExhausted) {
		t.Fatalf("setupNet with a refused lease = %v, want ErrNetLeaseExhausted", err)
	}
}

// The old TestDecommissionReleasesLeaseWhenTeardownFails asserted the opposite of
// the rule this item establishes — that a teardown which could NOT stop the VM
// still released its lease — so it is deleted rather than adjusted. Its inverse
// is TestDecommissionRefusesWhenTheVMSurvivesSIGKILL above: an unproven
// termination retains the lease, because the slot it fences is an address, a uid
// and a chroot the still-running VM is using.

// fakeNetAlloc is an in-memory NetLeaseAllocator for the adapter's tests. It
// derives real coordinates through domain.DeriveNetLease so a test can never
// assert on addressing the production path would not produce.
type fakeNetAlloc struct {
	mu         sync.Mutex
	leases     map[string]domain.NetLease
	nextSlot   int
	acquireErr error
	releaseErr error
}

var _ usecase.NetLeaseAllocator = (*fakeNetAlloc)(nil)

func newFakeNetAlloc() *fakeNetAlloc {
	return &fakeNetAlloc{leases: map[string]domain.NetLease{}, nextSlot: 1}
}

func (f *fakeNetAlloc) Acquire(_ context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (domain.NetLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return domain.NetLease{}, f.acquireErr
	}
	key := string(kind) + ":" + ownerID.String()
	if held, ok := f.leases[key]; ok {
		return held, nil
	}
	lease, err := domain.DeriveNetLease(0, f.nextSlot, 100000)
	if err != nil {
		return domain.NetLease{}, err
	}
	f.nextSlot++
	lease.ID = uuid.New()
	lease.OwnerKind, lease.OwnerID = kind, ownerID
	f.leases[key] = lease
	return lease, nil
}

func (f *fakeNetAlloc) Release(_ context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return f.releaseErr
	}
	delete(f.leases, string(kind)+":"+ownerID.String())
	return nil
}

func (f *fakeNetAlloc) held(kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.leases[string(kind)+":"+ownerID.String()]
	return ok
}
