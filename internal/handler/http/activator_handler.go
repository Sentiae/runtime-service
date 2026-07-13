package http

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/platform-kit/logger"
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
func (h *ActivatorHandler) RegisterRoutes(r chi.Router) {
	r.Handle("/_activate", http.HandlerFunc(h.Activate))
	r.Handle("/_activate/*", http.HandlerFunc(h.Activate))
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
