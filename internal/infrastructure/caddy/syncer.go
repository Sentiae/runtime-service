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

// Syncer pushes the desired ingress route set to a local Caddy admin endpoint.
type Syncer struct {
	admin string
	http  *http.Client
}

var _ usecase.IngressSyncer = (*Syncer)(nil)

// NewSyncer constructs a Syncer targeting the Caddy admin base URL (e.g.
// http://127.0.0.1:2019). The 5s client timeout bounds a hung admin endpoint so
// the reconcile tick never blocks.
func NewSyncer(admin string) *Syncer {
	return &Syncer{
		admin: admin,
		http:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Sync builds the full Caddy config from routes and loads it atomically. A
// failure (unreachable admin, non-2xx) is returned wrapped for the caller to
// log-and-continue — the reconciler must never crash on an ingress hiccup.
func (s *Syncer) Sync(ctx context.Context, routes []usecase.IngressRoute) error {
	cfg := buildConfig(routes)
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
// subdomains. Returned as a map because it is a wire document (JSON), not a
// domain type.
func buildConfig(routes []usecase.IngressRoute) map[string]any {
	httpRoutes := make([]any, 0, len(routes))
	platformHosts := make([]any, 0, len(routes))
	for _, r := range routes {
		httpRoutes = append(httpRoutes, buildRoute(r))
		if r.Host != "" {
			platformHosts = append(platformHosts, r.Host)
		}
	}

	apps := map[string]any{
		"http": map[string]any{
			"servers": map[string]any{
				"fleet": map[string]any{
					"listen": []any{":443", ":80"},
					"routes": httpRoutes,
				},
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

	return map[string]any{"apps": apps}
}

// buildRoute builds one Caddy route: a host matcher over [Host, CustomDomain]
// and a single handler from buildRouteHandler.
func buildRoute(r usecase.IngressRoute) map[string]any {
	hosts := make([]any, 0, 2)
	if r.Host != "" {
		hosts = append(hosts, r.Host)
	}
	if r.CustomDomain != "" {
		hosts = append(hosts, r.CustomDomain)
	}
	return map[string]any{
		"match":  []any{map[string]any{"host": hosts}},
		"handle": []any{buildRouteHandler(r)},
	}
}

// buildRouteHandler centralizes the per-route handler. With upstreams it is a
// reverse_proxy over each "ip:port" dial target. With ZERO upstreams the route
// has no live replica yet, so it serves a 503 today.
func buildRouteHandler(r usecase.IngressRoute) map[string]any {
	if len(r.Upstreams) == 0 {
		// rt#11: zero-upstream -> activator (scale-to-zero wake). Until then a
		// route with no live replica returns 503 rather than a dead upstream.
		return map[string]any{
			"handler":     "static_response",
			"status_code": 503,
			"body":        "no upstream available",
		}
	}
	ups := make([]any, 0, len(r.Upstreams))
	for _, u := range r.Upstreams {
		ups = append(ups, map[string]any{"dial": u})
	}
	return map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": ups,
	}
}
