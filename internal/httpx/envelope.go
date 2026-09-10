// Package httpx holds the HTTP response contract shared by every feature
// package: the success/error envelope, the error taxonomy, and the middleware
// that wraps the router.
//
// The envelope is byte-for-byte compatible with the TypeScript backend it
// replaces (src/types/apiResponse.ts). The React client in packages/api
// (apiClient.ts) rejects any response whose shape it does not recognise, so
// changes here are breaking changes.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// DefaultSuccessMessage matches createSuccessResponse's fallback in the
// TypeScript implementation. Clients do not depend on it, but the golden-file
// contract tests do.
const DefaultSuccessMessage = "Operation completed successfully"

// timestampLayout reproduces JavaScript's Date.prototype.toISOString(), which
// always emits exactly three fractional-second digits. time.RFC3339Nano trims
// trailing zeros, so it would render 12:34:56.780Z as 12:34:56.78Z and break a
// byte-for-byte comparison.
const timestampLayout = "2006-01-02T15:04:05.000Z07:00"

// SuccessResponse is the shape of every 2xx body.
type SuccessResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	Timestamp string `json:"timestamp"`
}

// ErrorBody is the nested error object. Code and Details are omitted when
// empty, mirroring how JSON.stringify drops undefined keys.
type ErrorBody struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

// ErrorResponse is the shape of every non-2xx body. Message intentionally
// duplicates ErrorBody.Message: createErrorResponse sets both from the same
// argument, and the client falls back from one to the other.
type ErrorResponse struct {
	Success   bool      `json:"success"`
	Message   string    `json:"message"`
	Error     ErrorBody `json:"error"`
	Timestamp string    `json:"timestamp"`
}

func now() string {
	return time.Now().UTC().Format(timestampLayout)
}

// NewSuccess builds a success envelope. An empty message becomes
// DefaultSuccessMessage.
func NewSuccess(data any, message string) SuccessResponse {
	if message == "" {
		message = DefaultSuccessMessage
	}
	return SuccessResponse{
		Success:   true,
		Message:   message,
		Data:      data,
		Timestamp: now(),
	}
}

// NewError builds an error envelope.
func NewError(message, code string, details any) ErrorResponse {
	return ErrorResponse{
		Success:   false,
		Message:   message,
		Error:     ErrorBody{Message: message, Code: code, Details: details},
		Timestamp: now(),
	}
}

// WriteJSON serialises v at the given status. It is the single place that
// touches the ResponseWriter, so a marshalling failure is logged rather than
// silently truncating the body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal response body", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"message":"Internal server error",` +
			`"error":{"message":"Internal server error","code":"INTERNAL_ERROR"},` +
			`"timestamp":"` + now() + `"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// OK writes a 200 success envelope.
func OK(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusOK, NewSuccess(data, ""))
}

// Created writes a 201 success envelope.
func Created(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusCreated, NewSuccess(data, ""))
}

// Fail writes an error envelope derived from err. See Classify for the mapping.
func Fail(w http.ResponseWriter, err error) {
	status, body := Classify(err)
	WriteJSON(w, status, body)
}
