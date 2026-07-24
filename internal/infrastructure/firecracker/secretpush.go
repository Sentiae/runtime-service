package firecracker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/guestsecrets"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/vmcomm"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// secretPushTimeout bounds the whole host->guest secret handshake (dial + CONNECT
// retries + bundle write + Ack read). The guest listener must come up within it
// or the boot fails closed.
const secretPushTimeout = 30 * time.Second

// pushSecrets delivers the secret bundle to a booting guest over Firecracker's
// host->guest vsock (invariant I32). It dials the VM's vsock UDS, performs the
// Firecracker "CONNECT <port>" -> "OK" handshake to the guest's AF_VSOCK
// listener, sends a length-prefixed SecretBundle, and waits for the guest Ack.
//
// The marshaled plaintext buffer is owned here and zeroed after the push so no
// secret lingers in host memory beyond the send. Any failure returns an error —
// the caller kills the VM (fail-closed): a secret workload must never run
// without its channel.
// controlToken (D-185a) rides along inside this already-sealed push: it is the
// credential the guest will require on every post-boot control request. Empty
// for a boot with no control channel.
func pushSecrets(ctx context.Context, socketPath string, items []usecase.HostSecret, nonce, controlToken string, timeout time.Duration) error {
	udsPath := socketPath + ".vsock"
	if timeout <= 0 {
		timeout = secretPushTimeout
	}
	deadline := time.Now().Add(timeout)

	// Present the per-boot bootstrap nonce (D-085 Layer 2) the guest wrote into
	// runtime.json; the guest rejects the bundle fail-closed if it does not match,
	// defeating a host-side spoof/replay of the push.
	bundle := &runtimev1.SecretBundle{
		Items:          make([]*runtimev1.SecretItem, 0, len(items)),
		BootstrapNonce: nonce,
		ControlToken:   controlToken,
	}
	for i := range items {
		bundle.Items = append(bundle.Items, &runtimev1.SecretItem{
			Name:  items[i].Name,
			Value: items[i].Val,
		})
	}
	data, err := proto.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("marshal secret bundle: %w", err)
	}
	// Wipe the plaintext wire buffer on every return path — the only secret
	// copy this function owns.
	defer func() {
		for i := range data {
			data[i] = 0
		}
	}()

	conn, err := connectGuestVsock(ctx, udsPath, guestsecrets.SecretPort, deadline)
	if err != nil {
		return err
	}
	defer conn.Close()

	if d, ok := ctxOrDeadline(ctx, deadline); ok {
		_ = conn.SetDeadline(d)
	}

	if err := vmcomm.WriteFrame(conn, data); err != nil {
		return fmt.Errorf("send secret bundle: %w", err)
	}

	var ack runtimev1.Ack
	if err := vmcomm.RecvMessage(conn, &ack); err != nil {
		return fmt.Errorf("read secret ack: %w", err)
	}
	if !ack.GetOk() {
		return fmt.Errorf("guest rejected secret bundle: %s", ack.GetError())
	}
	return nil
}

// connectGuestVsock dials the Firecracker vsock UDS and performs the host->guest
// "CONNECT <port>\n" -> "OK ...\n" handshake, retrying until the guest listener
// is up or the deadline passes. It returns the connected stream (raw conn, no
// buffering: the OK line is read one byte at a time so no guest bytes are
// consumed into a reader buffer).
func connectGuestVsock(ctx context.Context, udsPath string, port int, deadline time.Time) (net.Conn, error) {
	connectMsg := fmt.Sprintf("CONNECT %d\n", port)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := net.DialTimeout("unix", udsPath, 2*time.Second)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(connectMsg)); err != nil {
			conn.Close()
			continue
		}
		line, err := readLine(conn)
		if err != nil {
			conn.Close()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "OK") {
			_ = conn.SetDeadline(time.Time{})
			return conn, nil
		}
		// Not yet OK (guest listener not up) — retry with a fresh connection.
		conn.Close()
		time.Sleep(300 * time.Millisecond)
	}
	return nil, fmt.Errorf("vsock secret handshake to %s timed out", udsPath)
}

// readLine reads a single '\n'-terminated line one byte at a time so the
// connection buffer is left untouched for the subsequent framed protobuf read.
func readLine(conn net.Conn) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for i := 0; i < 256; i++ {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			return sb.String(), err
		}
	}
	return sb.String(), fmt.Errorf("handshake response line too long")
}

// ctxOrDeadline returns the earlier of the context deadline and the fixed
// deadline, so the framed read/write cannot block past either.
func ctxOrDeadline(ctx context.Context, deadline time.Time) (time.Time, bool) {
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		return cd, true
	}
	return deadline, true
}
