package http

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
)

// activator is the scale-to-zero wake use case the handler drives. Satisfied by
// *usecase.FleetActivator; an interface keeps the handler testable + the DI
// wiring nil-safe.
type activator interface {
	Activate(ctx context.Context, host string) (endpoint string, err error)
}

// ActivatorHandler is the scale-to-zero wake endpoint (rt#11, D-082). Caddy
// routes a request whose app has zero live upstreams to /_activate with the
// original host in X-Fleet-Host. The handler wakes the app (blocking cold boot),
// then reverse-proxies the ORIGINAL request to the woken replica so the request
// is served rather than dropped. On failure it returns a retryable 503 so Caddy's
// caller retries — the next reconcile tick republishes the route with the live
// upstream and the activator is bypassed.
type ActivatorHandler struct {
	act activator
}

// NewActivatorHandler builds the handler over the activator use case.
func NewActivatorHandler(act activator) *ActivatorHandler {
	return &ActivatorHandler{act: act}
}

// RegisterRoutes mounts /_activate (and its subpaths) on the router. It lives
// OUTSIDE the /api/v1 JWT group: its sole caller is the co-located Caddy gateway
// on loopback, which prepends /_activate to the original request URI.
//
// That "on loopback" is an ENFORCED control, not an assumption: the HTTP listener
// binds every interface (the /fleet surface is polled remotely by delivery), so
// without loopbackOnly this unauthenticated endpoint is reachable from the LAN.
func (h *ActivatorHandler) RegisterRoutes(r chi.Router) {
	r.Handle("/_activate", loopbackOnly(http.HandlerFunc(h.Activate)))
	r.Handle("/_activate/*", loopbackOnly(http.HandlerFunc(h.Activate)))
}

// loopbackOnly refuses any request whose PEER address is not a loopback address.
//
// The peer address is the connection's real remote endpoint, which a caller
// cannot forge. No header is consulted — X-Forwarded-For, X-Real-IP and friends
// are attacker-supplied strings on a surface with no authenticated proxy in front
// of it, so trusting one here would hand the bypass straight back.
//
// The response carries no detail: a caller that is not the co-located gateway
// learns only that it may not be here.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackPeer(r.RemoteAddr) {
			logger.FromContext(r.Context()).Warn("fleet activate: refused a non-loopback caller",
				"remote_addr", r.RemoteAddr, "path", r.URL.Path, "fleet_host_header", r.Header.Get("X-Fleet-Host"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackPeer reports whether remoteAddr (net/http's RemoteAddr, normally
// "ip:port") is a loopback IP. Fails CLOSED on anything it cannot parse into an
// IP: an address this cannot read is not an address it can vouch for.
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Not "host:port" — accept a bare IP literal, refuse anything else (an
		// empty RemoteAddr, a unix-socket "@", a hostname).
		host = remoteAddr
	}
	// An IPv6 zone ("fe80::1%eth0") is never loopback, and ParseIP rejects it
	// anyway — no special handling needed.
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

// Activate wakes the app named by X-Fleet-Host and proxies the request to it.
// Caddy prepended /_activate to the original URI, so the prefix is stripped back
// off before proxying so the guest receives the caller's original path.
func (h *ActivatorHandler) Activate(w http.ResponseWriter, r *http.Request) {
	host := r.Header.Get("X-Fleet-Host")
	if host == "" {
		host = r.Host
	}

	endpoint, err := h.act.Activate(r.Context(), host)
	if err != nil {
		// A refused wake is PERMANENT — the app is not one this path may boot, and no
		// number of retries changes that. Answering it with the retryable 503 below
		// would turn a refusal into a hot retry loop (and read as a transient cold
		// boot in the caller's logs). No detail: the caller learns only that it may
		// not have this.
		if errors.Is(err, domain.ErrAnonymousWakeRefused) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		logger.FromContext(r.Context()).Warn("fleet activate: wake failed", "host", host, "err", err)
		writeRetryable503(w)
		return
	}

	target, err := url.Parse(endpoint)
	if err != nil {
		logger.FromContext(r.Context()).Warn("fleet activate: bad endpoint", "host", host, "endpoint", endpoint, "err", err)
		writeRetryable503(w)
		return
	}

	// Restore the caller's original path: Caddy rewrote "/foo" → "/_activate/foo".
	origPath := strings.TrimPrefix(r.URL.Path, "/_activate")
	if origPath == "" {
		origPath = "/"
	}
	r.URL.Path = origPath

	// Stream the original request through to the woken replica.
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(pw http.ResponseWriter, _ *http.Request, perr error) {
		logger.FromContext(r.Context()).Warn("fleet activate: proxy error", "host", host, "endpoint", endpoint, "err", perr)
		writeRetryable503(pw)
	}
	proxy.ServeHTTP(w, r)
}

// writeRetryable503 emits the stable retryable-503 the caller retries on
// (rt#11 §D). Retry-After keeps a busy client from hot-looping while the cold
// boot completes.
func writeRetryable503(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("waking application, please retry\n"))
}
