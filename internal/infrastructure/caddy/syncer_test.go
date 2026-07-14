package caddy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sentiae/runtime-service/internal/usecase"
)

func TestBuildConfig(t *testing.T) {
	tests := []struct {
		name          string
		routes        []usecase.IngressRoute
		wantRoutes    int
		wantHosts     []string // every host that must appear in a matcher
		wantContains  []string // JSON fragments that must appear
		wantNotHave   []string // JSON fragments that must NOT appear
		wantTLSPolicy bool
	}{
		{
			name: "one route one upstream",
			routes: []usecase.IngressRoute{
				{Host: "app-prod.fleet.sentiae.local", Upstreams: []string{"10.201.0.2:8080"}},
			},
			wantRoutes:    1,
			wantHosts:     []string{"app-prod.fleet.sentiae.local"},
			wantContains:  []string{`"handler":"reverse_proxy"`, `"dial":"10.201.0.2:8080"`, `"module":"internal"`},
			wantTLSPolicy: true,
		},
		{
			name: "one route two upstreams",
			routes: []usecase.IngressRoute{
				{Host: "app-prod.fleet.sentiae.local", Upstreams: []string{"10.201.0.2:8080", "10.201.0.6:8080"}},
			},
			wantRoutes:    1,
			wantHosts:     []string{"app-prod.fleet.sentiae.local"},
			wantContains:  []string{`"dial":"10.201.0.2:8080"`, `"dial":"10.201.0.6:8080"`},
			wantTLSPolicy: true,
		},
		{
			name: "custom domain",
			routes: []usecase.IngressRoute{
				{Host: "app-prod.fleet.sentiae.local", CustomDomain: "www.example.com", Upstreams: []string{"10.201.0.2:8080"}},
			},
			wantRoutes: 1,
			wantHosts:  []string{"app-prod.fleet.sentiae.local", "www.example.com"},
			// Only the platform subdomain gets the internal issuer; the custom
			// domain is left to default automation.
			wantContains:  []string{`"module":"internal"`, `"app-prod.fleet.sentiae.local"`, `"www.example.com"`},
			wantTLSPolicy: true,
		},
		{
			name: "zero upstream routes to activator",
			routes: []usecase.IngressRoute{
				{Host: "app-prod.fleet.sentiae.local"},
			},
			wantRoutes: 1,
			wantHosts:  []string{"app-prod.fleet.sentiae.local"},
			// rt#11 — a scaled-to-zero route rewrites to /_activate and reverse-proxies
			// to the loopback activator with the requested host tagged.
			wantContains:  []string{`"handler":"rewrite"`, `/_activate{http.request.uri}`, `"handler":"reverse_proxy"`, `"dial":"127.0.0.1:8090"`, `"X-Fleet-Host"`, `{http.request.host}`},
			wantNotHave:   []string{`"handler":"static_response"`},
			wantTLSPolicy: true,
		},
		{
			name:       "empty routes yields no tls policy",
			routes:     nil,
			wantRoutes: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildConfig(tt.routes, "127.0.0.1:8090", "")
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			js := string(raw)

			// route count on the fleet server
			apps := cfg["apps"].(map[string]any)
			httpApp := apps["http"].(map[string]any)
			servers := httpApp["servers"].(map[string]any)
			fleet := servers["fleet"].(map[string]any)
			gotRoutes := fleet["routes"].([]any)
			if len(gotRoutes) != tt.wantRoutes {
				t.Fatalf("routes = %d, want %d", len(gotRoutes), tt.wantRoutes)
			}

			// server listens on :443 + :80
			listen := fleet["listen"].([]any)
			if len(listen) != 2 || listen[0] != ":443" || listen[1] != ":80" {
				t.Fatalf("listen = %v, want [:443 :80]", listen)
			}

			for _, h := range tt.wantHosts {
				if !strings.Contains(js, `"`+h+`"`) {
					t.Fatalf("host %q missing from config: %s", h, js)
				}
			}
			for _, frag := range tt.wantContains {
				if !strings.Contains(js, frag) {
					t.Fatalf("fragment %q missing from config: %s", frag, js)
				}
			}
			for _, frag := range tt.wantNotHave {
				if strings.Contains(js, frag) {
					t.Fatalf("fragment %q should be absent: %s", frag, js)
				}
			}

			_, hasTLS := apps["tls"]
			if hasTLS != tt.wantTLSPolicy {
				t.Fatalf("tls policy present = %v, want %v", hasTLS, tt.wantTLSPolicy)
			}
		})
	}
}

// TestBuildConfigAccessLog covers the #fleet-scale-to-zero-activity-feed (D-122)
// logging wiring: an empty path emits no logging block; a set path emits a
// top-level JSON access log + routes the fleet server to it.
func TestBuildConfigAccessLog(t *testing.T) {
	routes := []usecase.IngressRoute{
		{Host: "app-prod.fleet.sentiae.local", Upstreams: []string{"10.201.0.2:8080"}},
	}

	t.Run("no path yields no logging block", func(t *testing.T) {
		cfg := buildConfig(routes, "127.0.0.1:8090", "")
		if _, ok := cfg["logging"]; ok {
			t.Fatalf("logging block present with empty access-log path")
		}
		fleet := cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["fleet"].(map[string]any)
		if _, ok := fleet["logs"]; ok {
			t.Fatalf("server logs present with empty access-log path")
		}
	})

	t.Run("path yields json access log routed from the fleet server", func(t *testing.T) {
		const path = "/var/log/sentiae/caddy-access.log"
		cfg := buildConfig(routes, "127.0.0.1:8090", path)

		fleet := cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["fleet"].(map[string]any)
		logs, ok := fleet["logs"].(map[string]any)
		if !ok || logs["default_logger_name"] != fleetAccessLogName {
			t.Fatalf("fleet server not routed to %q log: %v", fleetAccessLogName, fleet["logs"])
		}

		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		js := string(raw)
		for _, frag := range []string{
			`"logging"`,
			`"fleet_access"`,
			`"output":"file"`,
			`"filename":"` + path + `"`,
			`"format":"json"`,
			`"http.log.access.fleet_access"`,
		} {
			if !strings.Contains(js, frag) {
				t.Fatalf("fragment %q missing from config: %s", frag, js)
			}
		}
	})
}
