//go:build unit

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentiae/platform-kit/nodebroker"
)

// TestRedeem_NodeHalfOverTheSocket drives the half of the secret path only the
// NODE can see: a POST over the broker's unix socket, from the process the boot
// probe launches inside a node-shaped container. It runs against a REAL sidecar
// broker, delivered through the real bind path, because a stub would prove
// nothing about the socket.
//
// The output line is the whole contract with the runtime: status, refusal code
// and found — and never a value. This command's stdout is read and logged by
// the runtime, so a value here would be a secret in a container log.
//
// CONTROL (hygiene): add answer.Value to the Fprintf in runRedeem — the canary
// assertion at the bottom goes red.
// CONTROL (one-shot): key the broker's `redeemed` map by name instead of by
// handle — the second redemption answers 200 and the 409 assertion goes red.
// CONTROL (fail-closed): swallow the client.Do error and print a line anyway —
// the missing-socket row stops returning an error and goes red.
func TestRedeem_NodeHalfOverTheSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := canaryBinding(false)
	run := startSidecar(t, ctx, cancel, b, freePort(t))
	socket := filepath.Join(run.runDir, brokerSocketName)

	request, err := json.Marshal(nodebroker.Request{
		Handle:     canaryHandle,
		Invocation: b.Invocation,
		Name:       "greeting_suffix",
		Node:       "greet",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var first bytes.Buffer
	if err := runRedeem(bytes.NewReader(request), &first, socket, 5*time.Second); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if want := "redeem: status=200 code=\"\" found=true\n"; first.String() != want {
		t.Fatalf("first redemption:\n got %q\nwant %q", first.String(), want)
	}

	// The handle is one-shot, which is what makes the value safe to serve at all.
	var second bytes.Buffer
	if err := runRedeem(bytes.NewReader(request), &second, socket, 5*time.Second); err != nil {
		t.Fatalf("second redeem: %v", err)
	}
	if want := "redeem: status=409 code=\"handle_consumed\" found=false\n"; second.String() != want {
		t.Fatalf("second redemption:\n got %q\nwant %q", second.String(), want)
	}

	// A socket nobody serves is an ERROR, never a line: the boot probe reads the
	// exit status as well as the output, and a redemption that could not happen
	// must never look like one that did.
	var missing bytes.Buffer
	missingSocket := filepath.Join(run.runDir, "absent.sock")
	dialErr := runRedeem(bytes.NewReader(request), &missing, missingSocket, time.Second)
	if dialErr == nil {
		t.Fatal("redeeming against a socket nobody serves must fail")
	}
	if !strings.Contains(dialErr.Error(), "dial "+missingSocket) {
		t.Fatalf("the dial failure must name the socket: %v", dialErr)
	}
	if missing.Len() != 0 {
		t.Fatalf("a failed redemption wrote a line anyway: %q", missing.String())
	}

	for _, surface := range []struct{ name, text string }{
		{"first", first.String()},
		{"second", second.String()},
		{"missing socket", missing.String()},
		{"the dial error", dialErr.Error()},
	} {
		for _, forbidden := range []string{canaryValue, canaryHandle, "handle:"} {
			if strings.Contains(surface.text, forbidden) {
				t.Fatalf("redeem's %s output contains %q: %s", surface.name, forbidden, surface.text)
			}
		}
	}
}
