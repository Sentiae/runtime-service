// Command node-sidecar is the trusted half of ONE node invocation.
//
// It is the only process that ever holds a resolved secret VALUE inside the
// sandbox topology, and the only process an egress-declaring node can reach the
// network through. It runs from the runtime's OWN image (so there is no second
// supply chain to trust), with the same hardened flags a hostile bundle gets,
// on the invocation's --internal bridge — and it is handed its binding on an
// attached stdin stream, never on argv, never in the environment, never in a
// file (§3.7, D-4).
//
// Two commands:
//
//	node-sidecar         the long-running sidecar: health, control socket, then
//	                     the broker (iff the binding carries secrets) and the
//	                     egress proxy (iff the binding carries egress).
//	node-sidecar bind    copies ONE binding document from stdin into the running
//	                     sidecar's private control socket and waits for its ack.
//	                     This is what `docker exec -i` runs.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// The sidecar's env keys. Every one of them has a DEFAULT (R-26): the
	// launch line sets no -e beyond the hardened flags' six and a sidecar
	// inherits nothing from the runtime, so an absent value is the normal case.
	// A value that is PRESENT and unusable is fatal, because silently falling
	// back would bind the health endpoint or the run directory somewhere the
	// runtime is not looking.
	envHealthListen = "APP_HEALTH_LISTEN"
	envTunnelMax    = "APP_TUNNEL_MAX"
	envRunDir       = "APP_RUN_DIR"

	defaultHealthListen = "127.0.0.1:3129"
	defaultTunnelMax    = 130 * time.Second
	defaultRunDir       = "/run/sentiae-inv"

	// controlSocketPath is on the sidecar's OWN tmpfs (--read-only + --tmpfs
	// /tmp), so the binding never crosses a shared volume: the node's mount is
	// the run directory, and the control socket is not in it.
	controlSocketPath = "/tmp/control.sock"

	// brokerSocketName is the socket the NODE dials, inside the shared run
	// directory it mounts read-only.
	brokerSocketName = "broker.sock"

	// proxyPort is the invocation-side port the egress proxy listens on. It is
	// bound to the sidecar's address on the invocation bridge and NEVER to
	// 0.0.0.0: the sidecar also sits on the uplink, and a wildcard bind would
	// publish the proxy there.
	proxyPort = 3128

	// bindCommand is the argv[1] `docker exec` uses to deliver the binding.
	bindCommand = "bind"
)

// options is the sidecar's whole configuration.
type options struct {
	HealthListen  string
	RunDir        string
	ControlSocket string
	TunnelMax     time.Duration
	ProxyPort     int
}

// defaultOptions is the configuration a sidecar launched by the runtime gets.
func defaultOptions() options {
	return options{
		HealthListen:  defaultHealthListen,
		RunDir:        defaultRunDir,
		ControlSocket: controlSocketPath,
		TunnelMax:     defaultTunnelMax,
		ProxyPort:     proxyPort,
	}
}

// optionsFromEnv overlays the environment onto the defaults. A key that is set
// but unusable is an error — never a silent fallback.
func optionsFromEnv(lookup func(string) (string, bool)) (options, error) {
	opt := defaultOptions()
	if v, ok := lookup(envHealthListen); ok {
		if _, _, err := net.SplitHostPort(v); err != nil {
			return options{}, fmt.Errorf("node sidecar: %s=%q is not host:port: %w", envHealthListen, v, err)
		}
		opt.HealthListen = v
	}
	if v, ok := lookup(envTunnelMax); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return options{}, fmt.Errorf("node sidecar: %s=%q is not a positive duration", envTunnelMax, v)
		}
		opt.TunnelMax = d
	}
	if v, ok := lookup(envRunDir); ok {
		if !filepath.IsAbs(v) {
			return options{}, fmt.Errorf("node sidecar: %s=%q is not an absolute path", envRunDir, v)
		}
		opt.RunDir = v
	}
	return opt, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == bindCommand {
		if err := runBind(os.Stdin, controlSocketPath, bindTimeout); err != nil {
			log.Fatalf("node sidecar: bind: %v", err)
		}
		return
	}

	opt, err := optionsFromEnv(os.LookupEnv)
	if err != nil {
		log.Fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := newSidecar(opt, newSidecarLogger(os.Stdout)).run(ctx); err != nil {
		log.Fatalf("node sidecar: %v", err)
	}
}
