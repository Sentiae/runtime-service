// Post-boot control channel for image-init (D-185a). A RESIDENT guest arms a
// PERSISTENT AF_VSOCK listener after the one-shot secret phase and serves
// SYNCFS / FREEZE / THAW / SHUTDOWN until the VM dies.
//
// Why: Firecracker's Pause stops vCPUs but does NOT flush the guest kernel's
// dirty page cache, so a host-side copy of the data volume taken under Pause
// alone is torn — proven live, twice, by a restored pg_hba.conf with the right
// size and a NUL-filled tail. Only the guest can flush the guest.
//
// This file owns exactly the parts that need real syscalls: the vsock listener,
// the fd-backed connection, and the syncfs/FIFREEZE/FITHAW/kill implementations.
// Every branching decision (token auth, op dispatch, the dead-man auto-thaw)
// lives in the transport-free guestcontrol package so it is testable off-host.
//
//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestcontrol"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestsecrets"
)

const (
	// FIFREEZE / FITHAW are _IOWR('X', 119|120, int). golang.org/x/sys/unix does
	// not export them, so they are spelled out here (linux/amd64 — the only
	// architecture the guest kernel + rootfs are built for).
	fiFreeze = 0xc0045877
	fiThaw   = 0xc0045878

	// controlBacklog lets a retrying host queue behind an in-flight request
	// instead of getting a connection refused.
	controlBacklog = 4

	// controlIOTimeout bounds a blocking read/write on an accepted control
	// connection so a host that dies mid-request cannot wedge the accept loop.
	// It must exceed the guest's own SHUTDOWN wait, which is answered on this
	// same connection.
	controlIOTimeout = guestcontrol.DefaultShutdownWait + 30*time.Second
)

// residentOps implements guestcontrol.Ops against the real guest.
type residentOps struct {
	// dataMount is the in-guest mount point of the persistent data volume. Empty
	// when the workload has no volume, which makes every filesystem op fail loud.
	dataMount string

	// childPID is the workload child, published by runResident AFTER it starts.
	// Zero means "not started yet" — the listener is deliberately armed first so
	// there is no window where the host can connect and be silently ignored.
	childPID atomic.Int64

	// childExited is closed by the PID-1 reaper when the direct child exits. The
	// control server must not wait4 for itself: only one waiter can reap a child,
	// and the reaper is already it.
	childExited <-chan struct{}

	// shutdownAsked records that a control SHUTDOWN drove the exit, so the reaper
	// knows to let the reply flush before it powers the VM off.
	shutdownAsked atomic.Bool
}

var _ guestcontrol.Ops = (*residentOps)(nil)

// SyncFS flushes the data filesystem's dirty pages to the virtio-blk device.
func (o *residentOps) SyncFS(context.Context) error {
	return o.withMountFD(func(fd int) error { return unix.Syncfs(fd) })
}

// Freeze syncs and then freezes the data filesystem. FIFREEZE alone would be
// enough (the kernel syncs as part of the freeze), but the explicit syncfs first
// keeps the freeze window — during which every guest writer blocks — short.
func (o *residentOps) Freeze(context.Context) error {
	return o.withMountFD(func(fd int) error {
		if err := unix.Syncfs(fd); err != nil {
			return fmt.Errorf("syncfs: %w", err)
		}
		if err := unix.IoctlSetInt(fd, fiFreeze, 0); err != nil {
			return fmt.Errorf("FIFREEZE: %w", err)
		}
		return nil
	})
}

// Thaw thaws the data filesystem. It returns the RAW errno on purpose: EINVAL
// means "was not frozen", and guestcontrol — not this file — decides that is a
// benign success.
func (o *residentOps) Thaw(context.Context) error {
	return o.withMountFD(func(fd int) error { return unix.IoctlSetInt(fd, fiThaw, 0) })
}

// Shutdown forwards SIGINT to the workload child (the engine image's STOPSIGNAL
// — Postgres fast shutdown) and waits for the reaper to observe its exit.
func (o *residentOps) Shutdown(ctx context.Context, wait time.Duration) error {
	pid := int(o.childPID.Load())
	if pid <= 0 {
		return guestcontrol.ErrWorkloadNotRunning
	}
	o.shutdownAsked.Store(true)
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		return fmt.Errorf("signal workload pid %d: %w", pid, err)
	}
	select {
	case <-o.childExited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return fmt.Errorf("workload pid %d did not exit within %s of SIGINT", pid, wait)
	}
}

// withMountFD opens the data mount point and runs fn against its fd. Every
// filesystem ioctl here is issued on the mount point, not the block device.
func (o *residentOps) withMountFD(fn func(fd int) error) error {
	if o.dataMount == "" {
		return guestcontrol.ErrNoDataMount
	}
	fd, err := unix.Open(o.dataMount, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open data mount %s: %w", o.dataMount, err)
	}
	defer unix.Close(fd)
	return fn(fd)
}

// startControlServer arms the persistent control listener and serves it in the
// background. It returns nil when no channel could be armed — a control channel
// is a durability aid, not an admission gate, and refusing to boot a customer's
// database because a vsock bind failed would be the worse outcome. The host is
// the enforcer: its Freeze fails loud, and the snapshot path fails with it.
func startControlServer(ctx context.Context, token string, ops *residentOps, logf func(string, ...any), onShutdownReplied func()) {
	if token == "" {
		logf("image-init: no control token in the secret bundle — post-boot control channel DISABLED")
		return
	}

	srv, err := guestcontrol.NewServer(guestcontrol.Config{
		Token:             token,
		Ops:               ops,
		Logf:              logf,
		OnShutdownReplied: onShutdownReplied,
	})
	if err != nil {
		logf("image-init: control channel not armed: %v", err)
		return
	}

	lfd, err := listenVsockBacklog(guestsecrets.ControlPort, controlBacklog)
	if err != nil {
		logf("image-init: control channel not armed: listen vsock port %d: %v", guestsecrets.ControlPort, err)
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "image-init: panic in control server: %v\n", r)
			}
			_ = unix.Close(lfd)
		}()
		srv.Serve(ctx, &vsockListener{fd: lfd})
	}()
	logf("image-init: control channel armed on vsock port %d", guestsecrets.ControlPort)
}

// listenVsockBacklog is listenVsock with a real backlog: the control listener is
// long-lived and may be reconnected to many times, unlike the accept-one secret
// listener.
func listenVsockBacklog(port uint32, backlog int) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
	}
	return fd, nil
}

// vsockListener adapts a raw AF_VSOCK listener fd to guestcontrol.Listener. Go's
// net package has no vsock support, so there is no net.Listener to reuse.
type vsockListener struct{ fd int }

func (l *vsockListener) Accept() (guestcontrol.Conn, error) {
	for {
		nfd, _, err := unix.Accept(l.fd)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Bound the blocking read/write: a host that dies mid-request must not
		// hold the (sequential) control channel forever.
		tv := unix.NsecToTimeval(controlIOTimeout.Nanoseconds())
		_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
		return &fdConn{fd: nfd}, nil
	}
}

func (l *vsockListener) Close() error { return unix.Close(l.fd) }

// fdConn is an io.ReadWriteCloser over a raw socket fd.
type fdConn struct{ fd int }

func (c *fdConn) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(c.fd, p)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 && len(p) > 0 {
			// A stream socket returning 0 is EOF; io.ReadFull needs it spelled.
			return 0, io.EOF
		}
		return n, nil
	}
}

func (c *fdConn) Write(p []byte) (int, error) {
	off := 0
	for off < len(p) {
		n, err := unix.Write(c.fd, p[off:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return off, err
		}
		off += n
	}
	return off, nil
}

func (c *fdConn) Close() error { return unix.Close(c.fd) }
