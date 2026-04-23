package http

import (
	"context"
	"encoding/json"
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
	log.Printf("WARNING: runtime-service has no PermissionChecker configured — defaulting to deny-all. Set APP_PERMISSION_ALLOW_ALL=true for dev or wire a real client.")
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
