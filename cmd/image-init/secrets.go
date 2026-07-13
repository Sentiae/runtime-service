// Secret channel receiver for image-init (invariant I32). When the descriptor
// sets expect_secrets, PID 1 opens an AF_VSOCK listener, accepts ONE host push,
// verifies the pusher's per-boot bootstrap nonce (D-085 Layer 2), writes each
// secret into a RAM-only tmpfs (mode 0600), and Acks. Secret plaintext is
// written ONLY to the tmpfs file — it is NEVER staged into the workload env
// (D-085 Layer 3: no /proc/<pid>/environ or core-dump exposure). Any failure is
// fail-closed: the workload never execs — exit_code 78 (EX_CONFIG) is recorded
// and the VM powers off.
//
//go:build linux

package main

import (
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestsecrets"
)

const (
	// exConfig is EX_CONFIG (sysexits.h): a configuration/environment error —
	// used when the secret channel is expected but cannot be satisfied.
	exConfig = 78
	// secretAcceptTimeout bounds how long we wait for the host to connect.
	secretAcceptTimeout = 30 * time.Second
	// secretIOTimeout bounds the framed read/write once connected.
	secretIOTimeout = 10 * time.Second
	// secretMaxFrame caps an accepted frame (bundle) size.
	secretMaxFrame = 1 << 20 // 1 MiB
)

// receiveSecrets performs the full guest-side vsock secret handshake, writing
// each received secret to a tmpfs-0600 file. It carries NO value into the
// workload env. It NEVER returns on failure — failSecrets records the error and
// powers the VM off (fail-closed). The expected per-boot bootstrap nonce is read
// from the (host-authored) runtime spec; a push whose nonce does not match is
// rejected fail-closed (D-085 Layer 2 attestation).
func receiveSecrets(spec *runtimeSpec) {
	// A boot that expects secrets MUST carry a host-minted nonce; an empty
	// expected nonce means a misconfigured/spoofed spec — fail closed rather
	// than accept an unauthenticated push.
	if spec.BootstrapNonce == "" {
		failSecrets("expected secrets but runtime.json carries no bootstrap nonce")
	}

	if err := mountSecretTmpfs(); err != nil {
		failSecrets("mount secrets tmpfs: " + err.Error())
	}

	lfd, err := listenVsock(guestsecrets.SecretPort)
	if err != nil {
		failSecrets("listen vsock: " + err.Error())
	}
	defer unix.Close(lfd)

	cfd, err := acceptVsock(lfd, secretAcceptTimeout)
	if err != nil {
		failSecrets("accept vsock: " + err.Error())
	}
	defer unix.Close(cfd)
	setRecvTimeout(cfd, secretIOTimeout)

	var bundle runtimev1.SecretBundle
	if err := readFrameProto(cfd, &bundle); err != nil {
		failSecrets("read secret bundle: " + err.Error())
	}

	// Attest the pusher: only the legitimate orchestrator knows the per-boot
	// nonce the host wrote into runtime.json. Constant-time compare avoids
	// leaking it via timing. A mismatch (spoof/replay/race) fails closed — we do
	// NOT accept a second push (accept-ONE semantics).
	if subtle.ConstantTimeCompare([]byte(bundle.GetBootstrapNonce()), []byte(spec.BootstrapNonce)) != 1 {
		failSecrets("secret push rejected: bootstrap nonce mismatch")
	}

	if len(bundle.GetItems()) == 0 {
		// expect_secrets was set but the bundle carried nothing — fail closed.
		failSecrets("secret bundle empty (expected at least one secret)")
	}

	for _, it := range bundle.GetItems() {
		name := it.GetName()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			failSecrets("invalid secret name")
		}
		path := filepath.Join(guestsecrets.MountDir, name)
		if err := os.WriteFile(path, []byte(it.GetValue()), 0o600); err != nil {
			failSecrets("write secret file: " + err.Error())
		}
	}

	if err := writeFrameProto(cfd, &runtimev1.Ack{Ok: true}); err != nil {
		failSecrets("send ack: " + err.Error())
	}
}

// hardenAgainstDumps is a defense-in-depth belt (D-085 Layer 3): it disables
// core dumps for this process tree and marks PID 1 non-dumpable so a
// secret-bearing core dump or ptrace of image-init is impossible.
//
//   - RLIMIT_CORE=0 is inherited across fork+exec, so it also covers the
//     workload child and its descendants — no process can write a core file.
//   - PR_SET_DUMPABLE=0 protects THIS process (PID 1), which briefly holds
//     secret plaintext in memory while writing the tmpfs files. The kernel resets
//     dumpable to 1 across the child's execve, so it does NOT persist into the
//     workload; RLIMIT_CORE=0 is what covers the child. Applying PR_SET_DUMPABLE
//     to the child before exec is not achievable through os/exec (no prctl hook),
//     so RLIMIT_CORE=0 is the child's guarantee.
//
// Best-effort: a failure here does not block boot.
func hardenAgainstDumps() {
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// mountSecretTmpfs mounts a fresh RAM-only tmpfs at the secrets dir (mode 0700)
// so secret plaintext never touches the rootfs.
func mountSecretTmpfs() error {
	if err := os.MkdirAll(guestsecrets.MountDir, 0o700); err != nil {
		return err
	}
	return unix.Mount("tmpfs", guestsecrets.MountDir, "tmpfs", 0, "mode=0700")
}

// listenVsock opens an AF_VSOCK stream listener bound to VMADDR_CID_ANY:port.
func listenVsock(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
	}
	return fd, nil
}

// acceptVsock accepts one connection, bounded by timeout.
func acceptVsock(lfd int, timeout time.Duration) (int, error) {
	type res struct {
		fd  int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		nfd, _, err := unix.Accept(lfd)
		ch <- res{fd: nfd, err: err}
	}()
	select {
	case r := <-ch:
		return r.fd, r.err
	case <-time.After(timeout):
		return -1, fmt.Errorf("timed out after %s waiting for host push", timeout)
	}
}

// setRecvTimeout best-effort bounds blocking reads on the connection fd.
func setRecvTimeout(fd int, d time.Duration) {
	tv := unix.NsecToTimeval(d.Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

// readFrameProto reads a [4-byte big-endian length][protobuf] frame from fd and
// unmarshals it into msg (matches the host vmcomm framing).
func readFrameProto(fd int, msg proto.Message) error {
	var lenBuf [4]byte
	if err := readFull(fd, lenBuf[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > secretMaxFrame {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	payload := make([]byte, n)
	if err := readFull(fd, payload); err != nil {
		return err
	}
	return proto.Unmarshal(payload, msg)
}

// writeFrameProto marshals msg and writes it with the 4-byte length prefix.
func writeFrameProto(fd int, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if err := writeFull(fd, lenBuf[:]); err != nil {
		return err
	}
	return writeFull(fd, data)
}

// readFull reads len(buf) bytes from fd, retrying short reads / EINTR.
func readFull(fd int, buf []byte) error {
	off := 0
	for off < len(buf) {
		n, err := unix.Read(fd, buf[off:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			return fmt.Errorf("unexpected EOF (read %d of %d)", off, len(buf))
		}
		off += n
	}
	return nil
}

// writeFull writes all of buf to fd, retrying short writes / EINTR.
func writeFull(fd int, buf []byte) error {
	off := 0
	for off < len(buf) {
		n, err := unix.Write(fd, buf[off:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		off += n
	}
	return nil
}

// failSecrets records a fail-closed secret-channel error and powers off. It does
// not return (syncAndPowerOff halts the VM).
func failSecrets(msg string) {
	_ = os.MkdirAll(outDir, 0o755)
	_ = os.WriteFile(filepath.Join(outDir, "stderr.txt"), []byte("image-init: secret channel: "+msg), 0o644)
	_ = os.WriteFile(filepath.Join(outDir, "exit_code.txt"), []byte(fmt.Sprintf("%d", exConfig)), 0o644)
	syncAndPowerOff()
}
