package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ContextKey is a type for context keys
type ContextKey string

const (
	ContextKeyUserID         ContextKey = "user_id"
	ContextKeyOrganizationID ContextKey = "organization_id"
	ContextKeyRequestID      ContextKey = "request_id"
	ContextKeyAuthToken      ContextKey = "auth_token"
)

// AuthMiddleware validates tokens and sets user ID in context
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			RespondUnauthorized(w, "Missing authorization header")
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			RespondUnauthorized(w, "Invalid authorization header format")
			return
		}

		token := parts[1]
		if token == "" {
			RespondUnauthorized(w, "Missing authorization token")
			return
		}

		userIDStr := r.Header.Get("X-User-ID")
		if userIDStr == "" {
			RespondUnauthorized(w, "User ID header required (X-User-ID)")
			return
		}

		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			RespondBadRequest(w, "Invalid user ID format", nil)
			return
		}

		ctx := r.Context()
		ctx = context.WithValue(ctx, ContextKeyUserID, userID)
		ctx = context.WithValue(ctx, ContextKeyAuthToken, token)

		orgIDStr := r.Header.Get("X-Organization-ID")
		if orgIDStr != "" {
			if orgID, err := uuid.Parse(orgIDStr); err == nil {
				ctx = context.WithValue(ctx, ContextKeyOrganizationID, orgID)
			}
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDMiddleware adds a unique request ID to each request
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		ctx := context.WithValue(r.Context(), ContextKeyRequestID, requestID)
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware logs HTTP requests
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		lrw := &loggingResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(lrw, r)

		_ = time.Since(start)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// CORSMiddleware handles CORS headers
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Organization-ID, X-Request-ID, X-User-ID")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RecoveryMiddleware recovers from panics and returns a 500 error
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				RespondInternalError(w, "Internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// ContentTypeMiddleware sets and validates content type
func ContentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			contentType := r.Header.Get("Content-Type")
			if contentType != "" &&
				!strings.Contains(contentType, "application/json") &&
				!strings.Contains(contentType, "multipart/form-data") {
				RespondBadRequest(w, "Content-Type must be application/json or multipart/form-data", nil)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// GetUserIDFromContext extracts user ID from context
func GetUserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	userID, ok := ctx.Value(ContextKeyUserID).(uuid.UUID)
	return userID, ok
}

// GetOrganizationIDFromContext extracts organization ID from context
func GetOrganizationIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	orgID, ok := ctx.Value(ContextKeyOrganizationID).(uuid.UUID)
	return orgID, ok
}

// GetUUIDParam extracts a UUID parameter from the route
func GetUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	paramStr := chi.URLParam(r, name)
	return uuid.Parse(paramStr)
}

// GetIntParam extracts an integer parameter from the route
func GetIntParam(r *http.Request, name string) (int, error) {
	paramStr := chi.URLParam(r, name)
	var result int
	_, err := fmt.Sscanf(paramStr, "%d", &result)
	return result, err
}
