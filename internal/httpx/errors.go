package httpx

import (
	"errors"
	"fmt"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// HTTPError is an error that carries its own status and machine-readable code.
// It is the Go equivalent of the TypeScript HttpError class; feature packages
// define their own constructors on top of it (see bingo.Err and watcher.Err).
type HTTPError struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, e.Code)
}

// Errorf builds an HTTPError with a formatted message.
func Errorf(status int, code, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidationIssue describes one field-level validation failure. It stands in
// for a zod issue in the `details` slot of the error envelope.
type ValidationIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationError aggregates field-level failures. Classify renders it as a
// 400 VALIDATION_ERROR, matching how the TypeScript backend surfaced ZodError.
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "request validation failed"
	}
	return fmt.Sprintf("request validation failed: %s %s", e.Issues[0].Path, e.Issues[0].Message)
}

// Invalid builds a single-issue ValidationError.
func Invalid(path, message string) *ValidationError {
	return &ValidationError{Issues: []ValidationIssue{{Path: path, Message: message}}}
}

// Classify maps an error to the status and envelope the client receives. The
// ordering and the exact codes reproduce parseErrorDetails in the TypeScript
// implementation; see the table in the migration plan.
func Classify(err error) (int, ErrorResponse) {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status, NewError(httpErr.Message, httpErr.Code, httpErr.Details)
	}

	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return http.StatusBadRequest, NewError(
			"Request body validation failed", "VALIDATION_ERROR", validationErr.Issues)
	}

	var serverErr mongo.ServerError
	if errors.As(err, &serverErr) {
		switch {
		case serverErr.HasErrorCode(11000):
			return http.StatusConflict, NewError(
				"Duplicate key error", "DUPLICATE_KEY",
				"A document with this data already exists")
		case serverErr.HasErrorCode(121):
			return http.StatusBadRequest, NewError(
				"Document validation failed", "DOCUMENT_VALIDATION_ERROR", serverErr.Error())
		default:
			return http.StatusInternalServerError, NewError(
				"Database error", "DATABASE_ERROR", mongoErrorCode(err))
		}
	}

	// Any other driver-level failure (timeout, no reachable server, decode
	// error) is still a database error rather than a generic internal one.
	if errors.Is(err, mongo.ErrNoDocuments) || errors.Is(err, mongo.ErrClientDisconnected) {
		return http.StatusInternalServerError, NewError(
			"Database error", "DATABASE_ERROR", nil)
	}

	// Message deliberately scrubbed: never leak internal error text.
	return http.StatusInternalServerError, NewError(
		"Internal server error", "INTERNAL_ERROR", nil)
}

// mongoErrorCode digs the numeric server code out of whichever driver error
// wrapper is in play, mirroring the TypeScript handler leaking `err.code` into
// `details`. Returns nil when no code is available.
func mongoErrorCode(err error) any {
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Code
	}
	var writeErr mongo.WriteError
	if errors.As(err, &writeErr) {
		return writeErr.Code
	}
	var writeException mongo.WriteException
	if errors.As(err, &writeException) && len(writeException.WriteErrors) > 0 {
		return writeException.WriteErrors[0].Code
	}
	return nil
}
