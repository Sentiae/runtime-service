package http

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker"
)

// FleetHandler exposes visibility + control over the warm-VM clones/templates
// the runtime manages on THIS host: list the live fleet, kill a buffered ready
// clone, and force a template rebuild. It is the per-host data source the
// control plane (deployment-service Fleet RPC) aggregates across hosts.
//
// pool may be nil (warm pool disabled): the routes are still registered, and
// GET /fleet reports {enabled:false} rather than 500ing. The mutating routes
// 503 when the pool is absent.
type FleetHandler struct {
	pool *firecracker.WarmPool
	// serviceToken is the shared service-to-service token guarding the /fleet
	// control surface. Empty ⇒ in-cluster traffic is trusted (dev parity).
	serviceToken string
}

// NewFleetHandler builds the handler. Pass the live *WarmPool, or nil when the
// warm pool is disabled (the handler then reports the fleet as disabled). The
// serviceToken guards the /fleet routes: when empty, in-cluster traffic is
// trusted (dev parity); when set, callers must present a matching x-api-key.
func NewFleetHandler(pool *firecracker.WarmPool, serviceToken string) *FleetHandler {
	return &FleetHandler{pool: pool, serviceToken: serviceToken}
}

// RegisterRoutes mounts the fleet routes on the router, gated by the shared
// service token (the /fleet surface lives OUTSIDE the /api/v1 JWT group because
// its sole caller is a service, not a user).
func (h *FleetHandler) RegisterRoutes(r chi.Router) {
	r.Route("/fleet", func(r chi.Router) {
		r.Use(h.requireServiceToken)
		r.Get("/", h.GetFleet)
		r.Delete("/clones/{id}", h.KillClone)
		r.Post("/templates/{language}/refresh", h.RefreshTemplate)
	})
}

// requireServiceToken guards the /fleet control surface with the shared
// service-to-service token. The sole caller is deployment-service's Fleet RPC,
// which presents the token as the x-api-key header. An empty configured token
// trusts in-cluster traffic (dev parity, matching the internal_auth idiom used
// across services); a non-empty token requires a constant-time match.
func (h *FleetHandler) requireServiceToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.serviceToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		presented := r.Header.Get("x-api-key")
		if subtle.ConstantTimeCompare([]byte(presented), []byte(h.serviceToken)) != 1 {
			RespondUnauthorized(w, "invalid service token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetFleet handles GET /fleet — the whole warm fleet on this host. When the
// warm pool is disabled it reports {enabled:false} (never 500).
func (h *FleetHandler) GetFleet(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		RespondSuccess(w, map[string]any{"enabled": false})
		return
	}
	snap := h.pool.Fleet()
	RespondSuccess(w, map[string]any{
		"enabled":      true,
		"ready_target": snap.ReadyTarget,
		"total_ready":  snap.TotalReady,
		"total_active": snap.TotalActive,
		"languages":    snap.Languages,
	})
}

// KillClone handles DELETE /fleet/clones/{id} — destroy ONE buffered ready
// clone. 200 {killed:true} when it was found + killed; 404 when no ready clone
// has that id (in-flight clones are never killed); 503 when the pool is off.
func (h *FleetHandler) KillClone(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		RespondError(w, http.StatusServiceUnavailable, "WARM_POOL_DISABLED", "warm pool is not enabled", nil)
		return
	}
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		RespondBadRequest(w, "Invalid clone id", nil)
		return
	}
	killed, err := h.pool.KillClone(id)
	if err != nil {
		// The clone WAS removed from the buffer; only its host teardown failed.
		RespondInternalError(w, "Failed to destroy clone: "+err.Error())
		return
	}
	if !killed {
		RespondNotFound(w, "No ready clone with that id (it may be in-flight or already gone)")
		return
	}
	RespondSuccess(w, map[string]any{"killed": true, "id": id})
}

// RefreshTemplate handles POST /fleet/templates/{language}/refresh — drop the
// cached template for a language so the next ensureTemplate rebuilds/re-pulls
// it. 202 Accepted; live clones are left untouched. 503 when the pool is off.
func (h *FleetHandler) RefreshTemplate(w http.ResponseWriter, r *http.Request) {
	// Validate the untrusted path param BEFORE any state-dependent branch: an
	// out-of-allowlist language flows into object-store keys + filesystem paths
	// (traversal), so reject it regardless of whether the pool is enabled here.
	language := chi.URLParam(r, "language")
	if language == "" {
		RespondBadRequest(w, "Missing language", nil)
		return
	}
	if !domain.Language(language).IsValid() {
		RespondBadRequest(w, "Unsupported language", nil)
		return
	}
	if h.pool == nil {
		RespondError(w, http.StatusServiceUnavailable, "WARM_POOL_DISABLED", "warm pool is not enabled", nil)
		return
	}
	if err := h.pool.RefreshTemplate(domain.Language(language)); err != nil {
		RespondInternalError(w, "Failed to refresh template: "+err.Error())
		return
	}
	RespondJSON(w, http.StatusAccepted, Response{
		Success: true,
		Data:    map[string]any{"refreshed": language},
	})
}
