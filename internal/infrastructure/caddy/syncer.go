// Package caddy adapts the usecase.IngressSyncer port to a co-located Caddy
// server driven over its admin JSON API (rt#8, D-079: the fleet owns ingress).
// Each Sync builds the FULL Caddy config and POSTs it to <admin>/load, which
// replaces the running config atomically — idempotent and self-healing across
// reconcile ticks. TLS for platform subdomains uses Caddy's internal (local CA)
// issuer so no public ACME is needed on the homelab.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// defaultActivatorDial is the loopback dial target Caddy uses to reach the
// runtime-service scale-to-zero activator when a route has no live upstream.
const defaultActivatorDial = "127.0.0.1:8090"

// fleetAccessLogName is the Caddy named-log the fleet server routes its access
// logs to (#fleet-scale-to-zero-activity-feed, D-122). The activity feed tails
// the file this log writes to.
const fleetAccessLogName = "fleet_access"

// Syncer pushes the desired ingress route set to a local Caddy admin endpoint.
type Syncer struct {
	admin string
	// activator is the "host:port" Caddy dials for the scale-to-zero wake path
	// (rt#11): a route with zero live upstreams reverse-proxies to this loopback
	// activator instead of serving a dead 503.
	activator string
	// accessLogPath is the host file the fleet server writes JSON access logs to
	// (rt#11/D-122). Empty → no logging block is emitted (access logging off).
	accessLogPath string
	http          *http.Client
}

var _ usecase.IngressSyncer = (*Syncer)(nil)

// NewSyncer constructs a Syncer targeting the Caddy admin base URL (e.g.
// http://127.0.0.1:2019). activatorDial is the loopback "host:port" of the
// runtime-service scale-to-zero activator (empty → 127.0.0.1:8090).
// accessLogPath is the host file the fleet server writes JSON access logs to
// (empty → access logging disabled). The 5s client timeout bounds a hung admin
// endpoint so the reconcile tick never blocks.
func NewSyncer(admin, activatorDial, accessLogPath string) *Syncer {
	if activatorDial == "" {
		activatorDial = defaultActivatorDial
	}
	return &Syncer{
		admin:         admin,
		activator:     activatorDial,
		accessLogPath: accessLogPath,
		http:          &http.Client{Timeout: 5 * time.Second},
	}
}

// Sync builds the full Caddy config from routes and loads it atomically. A
// failure (unreachable admin, non-2xx) is returned wrapped for the caller to
// log-and-continue — the reconciler must never crash on an ingress hiccup.
func (s *Syncer) Sync(ctx context.Context, routes []usecase.IngressRoute) error {
	cfg := buildConfig(routes, s.activator, s.accessLogPath)
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal caddy config: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.admin+"/load", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build caddy load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("post caddy config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("caddy load returned %d: %s", resp.StatusCode, buf.String())
	}
	return nil
}

// buildConfig assembles the full Caddy JSON: one HTTP server "fleet" on :443
// (TLS) + :80 (auto HTTP→HTTPS redirect), one route per IngressRoute, and a TLS
// automation policy issuing internal (local CA) certs for the platform
// subdomains. When accessLogPath is non-empty the fleet server also emits JSON
// access logs to that file (each entry carries request.host) via a named log —
// what the activity feed tails (#fleet-scale-to-zero-activity-feed, D-122).
// Returned as a map because it is a wire document (JSON), not a domain type.
func buildConfig(routes []usecase.IngressRoute, activatorDial, accessLogPath string) map[string]any {
	httpRoutes := make([]any, 0, len(routes))
	platformHosts := make([]any, 0, len(routes))
	for _, r := range routes {
		httpRoutes = append(httpRoutes, buildRoute(r, activatorDial))
		if r.Host != "" {
			platformHosts = append(platformHosts, r.Host)
		}
	}

	fleetServer := map[string]any{
		"listen": []any{":443", ":80"},
		"routes": httpRoutes,
	}
	// rt#11/D-122 — route this server's access logs to the named "fleet_access"
	// log (defined under top-level "logging" below). The hot request path is
	// untouched: this only mirrors each handled request to the log file.
	if accessLogPath != "" {
		fleetServer["logs"] = map[string]any{"default_logger_name": fleetAccessLogName}
	}

	apps := map[string]any{
		"http": map[string]any{
			"servers": map[string]any{
				"fleet": fleetServer,
			},
		},
	}

	// Issue internal (local CA) certs only for the platform subdomains. Custom
	// domains are left to Caddy's default automation (public ACME) — a later
	// increment wires their real issuance.
	if len(platformHosts) > 0 {
		apps["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": []any{
					map[string]any{
						"subjects": platformHosts,
						"issuers":  []any{map[string]any{"module": "internal"}},
					},
				},
			},
		}
	}

	cfg := map[string]any{"apps": apps}

	// rt#11/D-122 — top-level logging: a JSON access log written to accessLogPath,
	// scoped (via include) to only the fleet server's access records so the file
	// stays parseable one-request-per-line for the activity feed's tailer.
	if accessLogPath != "" {
		cfg["logging"] = map[string]any{
			"logs": map[string]any{
				fleetAccessLogName: map[string]any{
					"writer": map[string]any{
						"output":   "file",
						"filename": accessLogPath,
					},
					"encoder": map[string]any{"format": "json"},
					"include": []any{"http.log.access." + fleetAccessLogName},
				},
			},
		}
	}

	return cfg
}

// buildRoute builds one Caddy route: a host matcher over [Host, CustomDomain]
// and the handler chain from buildRouteHandler.
func buildRoute(r usecase.IngressRoute, activatorDial string) map[string]any {
	hosts := make([]any, 0, 2)
	if r.Host != "" {
		hosts = append(hosts, r.Host)
	}
	if r.CustomDomain != "" {
		hosts = append(hosts, r.CustomDomain)
	}
	return map[string]any{
		"match":  []any{map[string]any{"host": hosts}},
		"handle": buildRouteHandler(r, activatorDial),
	}
}

// buildRouteHandler centralizes the per-route handler chain. With upstreams it is
// a single reverse_proxy over each "ip:port" dial target. With ZERO upstreams the
// route has no live replica (scaled to zero / not yet booted): it rewrites the
// request to /_activate, tags it with the requested host, and reverse-proxies to
// the runtime-service activator (rt#11), which cold-wakes the app and streams the
// original request through. The next reconcile tick republishes the route with
// the live upstream, bypassing the activator.
func buildRouteHandler(r usecase.IngressRoute, activatorDial string) []any {
	if len(r.Upstreams) == 0 {
		return []any{
			map[string]any{
				"handler": "rewrite",
				"uri":     "/_activate{http.request.uri}",
			},
			map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": activatorDial}},
				"headers": map[string]any{
					"request": map[string]any{
						"set": map[string]any{
							"X-Fleet-Host": []any{"{http.request.host}"},
						},
					},
				},
			},
		}
	}
	ups := make([]any, 0, len(r.Upstreams))
	for _, u := range r.Upstreams {
		ups = append(ups, map[string]any{"dial": u})
	}
	return []any{
		map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": ups,
		},
	}
}
