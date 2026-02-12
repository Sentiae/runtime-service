package http

import (
	"encoding/json"
	"net/http"
)

// Response represents a standard API response
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   *ErrorInfo  `json:"error,omitempty"`
}

// ErrorInfo contains error details
type ErrorInfo struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// RespondJSON writes a JSON response
func RespondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		}
	}
}

// RespondSuccess writes a successful JSON response
func RespondSuccess(w http.ResponseWriter, data interface{}) {
	RespondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    data,
	})
}

// RespondCreated writes a 201 Created response
func RespondCreated(w http.ResponseWriter, data interface{}) {
	RespondJSON(w, http.StatusCreated, Response{
		Success: true,
		Data:    data,
	})
}

// RespondNoContent writes a 204 No Content response
func RespondNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// RespondError writes an error JSON response
func RespondError(w http.ResponseWriter, statusCode int, code, message string, details map[string]interface{}) {
	RespondJSON(w, statusCode, Response{
		Success: false,
		Error: &ErrorInfo{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}

// RespondBadRequest writes a 400 Bad Request response
func RespondBadRequest(w http.ResponseWriter, message string, details map[string]interface{}) {
	RespondError(w, http.StatusBadRequest, "BAD_REQUEST", message, details)
}

// RespondUnauthorized writes a 401 Unauthorized response
func RespondUnauthorized(w http.ResponseWriter, message string) {
	RespondError(w, http.StatusUnauthorized, "UNAUTHORIZED", message, nil)
}

// RespondForbidden writes a 403 Forbidden response
func RespondForbidden(w http.ResponseWriter, message string) {
	RespondError(w, http.StatusForbidden, "FORBIDDEN", message, nil)
}

// RespondNotFound writes a 404 Not Found response
func RespondNotFound(w http.ResponseWriter, message string) {
	RespondError(w, http.StatusNotFound, "NOT_FOUND", message, nil)
}

// RespondInternalError writes a 500 Internal Server Error response
func RespondInternalError(w http.ResponseWriter, message string) {
	RespondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", message, nil)
}
