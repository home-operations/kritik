package webapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/home-operations/kritik/internal/store"
)

// ErrorCode is the machine-readable part of an API error.
type ErrorCode string

// Error codes the API returns.
const (
	CodeNotFound      ErrorCode = "not_found"
	CodeBadRequest    ErrorCode = "bad_request"
	CodeInvalidCursor ErrorCode = "invalid_cursor"
	CodeAmbiguous     ErrorCode = "ambiguous"
	CodeInternal      ErrorCode = "internal"
)

// apiError is an error a handler returns to be written as ErrorBody.
type apiError struct {
	status  int
	code    ErrorCode
	message string
}

func (e *apiError) Error() string { return string(e.code) + ": " + e.message }

func errNotFound(what string) error {
	return &apiError{status: http.StatusNotFound, code: CodeNotFound, message: what + " not found"}
}

func errBadRequest(code ErrorCode, message string) error {
	return &apiError{status: http.StatusBadRequest, code: code, message: message}
}

// writeJSON writes v as the response. Every API response is no-store:
// it is per-principal and must never be served from a shared cache.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a failed write has no one to report to
}

// writeError writes err as ErrorBody: an apiError as itself, a store miss
// as 404, a bad filter as 400, anything else as a logged 500 that says
// nothing about its cause.
func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	if e, ok := errors.AsType[*apiError](err); ok {
		writeJSON(w, e.status, ErrorBody{Code: e.code, Message: e.message})
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, ErrorBody{Code: CodeNotFound, Message: "not found"})
	case errors.Is(err, store.ErrFilter), errors.Is(err, store.ErrPageLimit):
		writeJSON(w, http.StatusBadRequest, ErrorBody{Code: CodeBadRequest, Message: "invalid filter"})
	case r.Context().Err() != nil:
		// The client went away; there is no one to answer.
	default:
		logger.ErrorContext(r.Context(), "webapi: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, ErrorBody{Code: CodeInternal, Message: "internal error"})
	}
}
