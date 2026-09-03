package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/sentiae/platform-kit/nodebroker"
)

// redeemTimeout bounds the whole redemption: dial, write, and read the answer.
// It matches bindTimeout for the same reason — a stuck broker must surface as a
// failed redemption, not as a container that hangs until the run times out.
const redeemTimeout = 5 * time.Second

// redeemOutputFormat is the ONE line `node-sidecar redeem` writes. It carries
// the status, the refusal code and whether an answer was found — and NEVER the
// value: this command exists to be run inside a node-shaped container whose
// stdout the runtime reads and logs, so a value here would be a secret in a
// container log.
const redeemOutputFormat = "redeem: status=%d code=%q found=%t\n"

// runRedeem is `node-sidecar redeem`: read ONE nodebroker.Request from the
// attached stdin stream and POST it over the broker's unix socket, exactly as a
// node SDK does.
//
// It exists so the boot probe can exercise the half of the secret path that
// only the NODE can see. Dir-search and connect(2) through the read-only
// subpath mount are decided by the accessor's uid class, so a dial from the
// runtime process measures the wrong class on both inodes; only a container
// launched on the node's own line proves the socket is reachable.
//
// The request is on stdin and nowhere else — not argv (visible in
// `docker inspect` and /proc/<pid>/cmdline), not the environment (visible in
// `docker inspect` and `docker exec env`) — because it carries a handle.
func runRedeem(stdin io.Reader, stdout io.Writer, socket string, timeout time.Duration) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, bindingMaxBytes))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("no request on stdin")
	}
	var req nodebroker.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		// The document is never echoed: a decode failure names the failure, not
		// the bytes, because those bytes carry a handle.
		return fmt.Errorf("request is not a JSON document")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://broker/v1/secret", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var answer nodebroker.Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, bindingMaxBytes)).Decode(&answer); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, redeemOutputFormat, resp.StatusCode, answer.Code, answer.Found); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}
