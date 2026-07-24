// Package guestcontrol holds the host<->guest control-channel protocol shared by
// the host-side client (internal/infrastructure/firecracker) and the in-guest
// server (cmd/image-init), the sibling of the guestsecrets package.
//
// It exists because a P19 data-VM has no live post-boot channel: the vsock
// secret listener is one-shot by design and the HTTP guest-agent belongs to the
// code-exec VM class. Without a channel the host can only quiesce a data volume
// by pausing the VMM, and Firecracker's Pause freezes vCPUs WITHOUT flushing the
// guest kernel's dirty page cache — so a host-side copy of the backing file is
// torn (proven live: a restored pg_hba.conf with the right size and a NUL tail).
// D-185a's answer is to grow image-init into the ratified `pg-guest` agent; this
// package is its wire and its transport-free logic.
//
// It carries no build constraints and no heavy imports so the static linux-only
// image-init binary can depend on it, and so its logic is unit-testable on a
// developer machine without KVM, root, or a real filesystem.
package guestcontrol

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"
)

// The four control ops. This set is closed: an op outside it is refused, never
// guessed at.
const (
	// OpSyncFS asks the guest kernel to flush the data filesystem (syncfs(2)).
	OpSyncFS = "SYNCFS"
	// OpFreeze asks the guest to syncfs and then FIFREEZE the data filesystem,
	// arming the dead-man auto-thaw.
	OpFreeze = "FREEZE"
	// OpThaw asks the guest to FITHAW the data filesystem and disarm the dead-man.
	OpThaw = "THAW"
	// OpShutdown asks the guest to stop the workload child gracefully (SIGINT —
	// Postgres fast shutdown) and wait for it to exit.
	OpShutdown = "SHUTDOWN"
)

// maxFrame caps an accepted frame. A control request is a token plus a verb;
// anything larger is a malformed or hostile peer.
const maxFrame = 64 * 1024

// ErrUnauthorized is the guest's refusal of a request whose control token does
// not match. The op is NOT performed.
var ErrUnauthorized = errors.New("guest control: unauthorized")

// ErrUnknownOp is the guest's refusal of an op outside the closed set above.
var ErrUnknownOp = errors.New("guest control: unknown op")

// ErrNoDataMount is returned when a filesystem op is requested on a guest that
// was booted without a data volume — there is nothing to sync or freeze, and
// silently succeeding would let the host believe it had quiesced something.
var ErrNoDataMount = errors.New("guest control: no data mount configured")

// ErrWorkloadNotRunning is returned when SHUTDOWN arrives before the workload
// child has been started (or after it has already gone).
var ErrWorkloadNotRunning = errors.New("guest control: workload child not running")

// Ops is the guest-side seam the server drives. Its implementation issues real
// syscalls/ioctls and therefore only exists on linux; every branching decision
// AROUND it lives in the server so it can be tested without root.
type Ops interface {
	// SyncFS flushes the data filesystem (syncfs(2)).
	SyncFS(ctx context.Context) error
	// Freeze syncs and then freezes the data filesystem (FIFREEZE).
	Freeze(ctx context.Context) error
	// Thaw thaws the data filesystem (FITHAW). It returns the RAW errno: the
	// server, not the implementation, decides that EINVAL (not frozen) is benign.
	Thaw(ctx context.Context) error
	// Shutdown forwards SIGINT to the workload child and waits up to wait for it
	// to exit. It returns nil only once the child has exited.
	Shutdown(ctx context.Context, wait time.Duration) error
}

// Conn is the minimal duplex stream the server needs. A raw AF_VSOCK fd cannot
// be wrapped in a net.Conn (Go's net package has no vsock support), so the
// server is defined against this instead of net.Conn — which also lets the tests
// drive it over an in-memory pipe.
type Conn interface {
	io.ReadWriteCloser
}

// Listener yields control connections. The guest implements it over its raw
// AF_VSOCK listener fd.
type Listener interface {
	Accept() (Conn, error)
	Close() error
}

// Timer is the subset of *time.Timer the dead-man needs, injected so the tests
// can fire it deterministically instead of sleeping out a real timeout.
type Timer interface {
	Stop() bool
}

// AfterFunc matches time.AfterFunc's shape. The default is realAfterFunc.
type AfterFunc func(d time.Duration, f func()) Timer

func realAfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// WriteMessage writes a [4-byte big-endian length][protobuf] frame — the same
// framing the secret channel and the guest agent already speak, so both ends of
// this channel share one definition of the wire.
func WriteMessage(w io.Writer, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control message: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write control length prefix: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write control payload: %w", err)
	}
	return nil
}

// ReadMessage reads one length-prefixed protobuf frame into msg.
func ReadMessage(r io.Reader, msg proto.Message) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fmt.Errorf("read control length prefix: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxFrame {
		return fmt.Errorf("control frame too large: %d bytes (max %d)", n, maxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read control payload (%d bytes): %w", n, err)
	}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return fmt.Errorf("unmarshal control message: %w", err)
	}
	return nil
}
