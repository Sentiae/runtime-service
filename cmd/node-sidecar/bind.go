package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// bindTimeout bounds the whole delivery: connect, write, and wait for the ack.
// It is comfortably inside the runtime's sidecar readiness budget, so a stuck
// sidecar surfaces as a bind failure rather than as a readiness timeout.
const bindTimeout = 5 * time.Second

// runBind is `node-sidecar bind`: copy ONE binding document from the attached
// stdin stream into the running sidecar's private control socket and wait for
// its ack.
//
// This process is the ONLY channel the binding travels on. It is not an
// argument (visible in `docker inspect` and /proc/<pid>/cmdline), not an
// environment variable (visible in `docker inspect` and `docker exec env`), and
// not a file (readable by anything that can reach the container's filesystem).
// The document is never written anywhere by this process and never echoed —
// including on the failure paths, which name the failure and not the bytes.
func runBind(stdin io.Reader, socket string, timeout time.Duration) error {
	doc, err := io.ReadAll(io.LimitReader(stdin, bindingMaxBytes))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if len(doc) == 0 {
		return errors.New("no binding on stdin")
	}

	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", socket, time.Until(deadline))
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(doc); err != nil {
		return fmt.Errorf("write binding: %w", err)
	}
	// Half-close so the sidecar's read reaches EOF; the ack still comes back on
	// the read half. Without this the sidecar would block on a read that never
	// ends and the exec would time out with the binding delivered but unapplied.
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			return fmt.Errorf("close write half: %w", err)
		}
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}
	if strings.TrimSpace(line) != strings.TrimSpace(ackOK) {
		return errors.New(strings.TrimSpace(line))
	}
	return nil
}
