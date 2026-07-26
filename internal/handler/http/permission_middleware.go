package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// PermissionChecker is the minimum surface a runtime permission gate
// needs. Same shape as platform-kit/middleware.PermissionChecker.
//
// Returning (false, nil) => 403; returning an error => 503.
type PermissionChecker interface {
	CheckPermission(ctx context.Context, subjectID, permission, resourceType, resourceID string) (bool, error)
}

// DevAllowAllPermissionChecker is the explicit fail-open implementation
// used ONLY in dev / single-tenant deploys. Operators must opt-in via
// APP_PERMISSION_ALLOW_ALL=true (see MustPermissionChecker) and the
// service logs a loud warning on every check so the footgun is visible.
type DevAllowAllPermissionChecker struct{}

// CheckPermission always returns true, loudly. See package doc.
func (DevAllowAllPermissionChecker) CheckPermission(_ context.Context, subjectID, permission, resourceType, resourceID string) (bool, error) {
	log.Printf("WARNING: runtime permission check fail-open (dev) — subject=%s perm=%s resource=%s/%s. Wire a real PermissionChecker in production.",
		subjectID, permission, resourceType, resourceID)
	return true, nil
}

// MustPermissionChecker returns a real checker when one is provided, or
// the dev fail-open checker when APP_PERMISSION_ALLOW_ALL=true. In every
// other case it fail-CLOSED. The error is non-nil when the caller is
// running in a production environment without a real checker — the
// container should refuse to start.
func MustPermissionChecker(real PermissionChecker) (PermissionChecker, bool, error) {
	if real != nil {
		return real, true, nil
	}
	env := strings.ToLower(os.Getenv("APP_APP_ENVIRONMENT"))
	allowAll := strings.EqualFold(os.Getenv("APP_PERMISSION_ALLOW_ALL"), "true")
	if env == "production" && !allowAll {
		return denyAllPermissionChecker{}, false, &permissionMisconfiguredError{}
	}
	if allowAll {
		log.Printf("WARNING: runtime-service wired with DevAllowAllPermissionChecker — APP_PERMISSION_ALLOW_ALL=true. Do NOT use this in production.")
		return DevAllowAllPermissionChecker{}, false, nil
	}
	// Not a misconfiguration, and the line should not read like one. runtime-service
	// deliberately has NO permission-service client: every other surface it serves
	// is gated by mesh SVID + attested org carriage (the gRPC auth interceptor plus
	// the per-handler org checks), not by a permission check. The ONLY
	// permission-gated routes are the hermetic-build mutations, so deny-all costs
	// exactly those two routes and nothing else — which is the correct fail-closed
	// answer while nothing calls them.
	log.Printf("runtime-service: no PermissionChecker wired (by design — this service is gated by mesh SVID + org carriage). The permission-gated hermetic-build mutation routes (POST /hermetic-builds/complete, PUT /hermetic-builds/{id}/artifact) will therefore REFUSE every request; set APP_PERMISSION_ALLOW_ALL=true to allow them in dev.")
	return denyAllPermissionChecker{}, false, nil
}

// denyAllPermissionChecker fails every request closed.
type denyAllPermissionChecker struct{}

func (denyAllPermissionChecker) CheckPermission(_ context.Context, _, _, _, _ string) (bool, error) {
	return false, nil
}

// permissionMisconfiguredError signals production startup should abort.
type permissionMisconfiguredError struct{}

func (*permissionMisconfiguredError) Error() string {
	return "runtime-service requires a PermissionChecker in production — set APP_PERMISSION_ALLOW_ALL=true for dev or wire a real client via SetPermissionChecker"
}

// IsPermissionMisconfigured is true when the error is a production
// misconfiguration.
func IsPermissionMisconfigured(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*permissionMisconfiguredError)
	return ok
}

// AllowAllPermissionChecker is retained as a DEPRECATED alias so existing
// test fixtures still compile. New code MUST use MustPermissionChecker.
//
// Deprecated: use MustPermissionChecker.
type AllowAllPermissionChecker = DevAllowAllPermissionChecker

// RequireRuntimePermission gates access to a runtime resource by calling
// the configured PermissionChecker. paramName names the chi URL param
// that carries the resource id (e.g. "executionId").
func RequireRuntimePermission(checker PermissionChecker, resourceType, permission, paramName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if checker == nil {
				writePermissionError(w, http.StatusInternalServerError, "permission checker not configured")
				return
			}
			userID, ok := GetUserIDFromContext(r.Context())
			if !ok {
				writePermissionError(w, http.StatusUnauthorized, "authenticated user required")
				return
			}
			resourceID := chi.URLParam(r, paramName)
			if resourceID == "" {
				writePermissionError(w, http.StatusBadRequest, "missing resource id")
				return
			}
			allowed, err := checker.CheckPermission(r.Context(), userID.String(), permission, resourceType, resourceID)
			if err != nil {
				writePermissionError(w, http.StatusServiceUnavailable, "permission check failed: "+err.Error())
				return
			}
			if !allowed {
				writePermissionError(w, http.StatusForbidden, "permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maxGatedBodyBytes bounds the body a body-field gate is willing to buffer. The
// gated routes are small JSON envelopes; a larger body is refused rather than
// read into memory, and no artifact upload goes through this gate (those carry
// their id in the URL).
const maxGatedBodyBytes = 1 << 20

// RequireRuntimePermissionFromBody is the body-carried counterpart of
// RequireRuntimePermission, for a mutation whose target resource is named in the
// JSON body rather than the path (e.g. POST /hermetic-builds/complete carries
// build_id). It buffers the bounded body, reads the field, then RESTORES the body
// so the handler decodes it normally.
//
// It exists because the alternative is a handler that checks its own permission
// after decoding, which puts the same control in two shapes; keeping every gate
// in middleware is what makes "is this route gated" answerable by reading the
// route table.
func RequireRuntimePermissionFromBody(checker PermissionChecker, resourceType, permission, field string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if checker == nil {
				writePermissionError(w, http.StatusInternalServerError, "permission checker not configured")
				return
			}
			userID, ok := GetUserIDFromContext(r.Context())
			if !ok {
				writePermissionError(w, http.StatusUnauthorized, "authenticated user required")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxGatedBodyBytes+1))
			if err != nil {
				writePermissionError(w, http.StatusBadRequest, "unreadable request body")
				return
			}
			if len(body) > maxGatedBodyBytes {
				writePermissionError(w, http.StatusRequestEntityTooLarge, "request body too large to authorize")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				writePermissionError(w, http.StatusBadRequest, "invalid request body")
				return
			}
			var resourceID string
			if raw, present := fields[field]; present {
				// Only a JSON string is accepted: a resource id that is not a string is
				// not a resource id, and coercing one would authorize a value the
				// handler will later reject or reinterpret.
				_ = json.Unmarshal(raw, &resourceID)
			}
			if resourceID == "" {
				writePermissionError(w, http.StatusBadRequest, "missing resource id: "+field+" is required")
				return
			}
			allowed, err := checker.CheckPermission(r.Context(), userID.String(), permission, resourceType, resourceID)
			if err != nil {
				writePermissionError(w, http.StatusServiceUnavailable, "permission check failed: "+err.Error())
				return
			}
			if !allowed {
				writePermissionError(w, http.StatusForbidden, "permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writePermissionError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}
