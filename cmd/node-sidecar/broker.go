package main

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"

	"github.com/sentiae/platform-kit/nodebroker"
)

// startBroker serves the secret broker on the unix socket the NODE mounts
// read-only. The broker itself is platform-kit/nodebroker — the same package
// the node SDKs and nodesmoke redeem against, so the refusal codes exist in
// exactly one place and this sidecar cannot drift into a permissive stub (D-1).
//
// It runs HERE, inside the per-invocation sidecar, and not in the runtime
// process: a secret value then never leaves the one container that is
// unreachable from the node except through a one-shot handle over this socket
// (E2/⟨SOL-1⟩).
func startBroker(runDir string, b binding) (net.Listener, *http.Server, error) {
	handles := make(map[string]string, len(b.Secrets))
	answers := make(map[string]nodebroker.Answer, len(b.Secrets))
	for name, a := range b.Secrets {
		handles[name] = a.Handle
		answers[name] = nodebroker.Answer{Found: a.Found, Value: a.Value}
	}

	socket := filepath.Join(runDir, brokerSocketName)
	lis, err := nodebroker.Listen(socket)
	if err != nil {
		return nil, nil, fmt.Errorf("broker socket %s: %w", socket, err)
	}
	// The invocation id is bound here, so a handle stolen from one invocation
	// cannot be redeemed against another sidecar's broker.
	return lis, nodebroker.Serve(lis, nodebroker.New(b.Invocation, handles, answers)), nil
}
