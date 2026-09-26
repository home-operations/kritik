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

	CodeForbidden          ErrorCode = "forbidden"
	CodeInvalidSpec        ErrorCode = "invalid_spec"
	CodeOperatorOnly       ErrorCode = "operator_only"
	CodeRevisionConflict   ErrorCode = "revision_conflict"
	CodeConfigBlocked      ErrorCode = "config_blocked"
	CodeSlugTaken          ErrorCode = "slug_taken"
	CodeFileManaged        ErrorCode = "file_managed"
	CodeManagementDisabled ErrorCode = "management_disabled"
	CodeInviteExists       ErrorCode = "invite_exists"
	CodeNotInviteMember    ErrorCode = "not_invite_member"
	CodeLastAdmin          ErrorCode = "last_admin"
	CodeNoHead             ErrorCode = "no_head"
	CodeNotCancelable      ErrorCode = "not_cancelable"
	CodeActionsDisabled    ErrorCode = "actions_disabled"
	CodeAlreadyQueued      ErrorCode = "already_queued"
	CodeReenterSecret      ErrorCode = "reenter_secret"
	CodeAlreadyMember      ErrorCode = "already_member"
)

// apiError is an error a handler returns to be written as ErrorBody.
type apiError struct {
	status  int
	code    ErrorCode
	message string
	details json.RawMessage
}

func (e *apiError) Error() string { return string(e.code) + ": " + e.message }

func errNotFound(what string) error {
	return &apiError{status: http.StatusNotFound, code: CodeNotFound, message: what + " not found"}
}

func errBadRequest(code ErrorCode, message string) error {
	return &apiError{status: http.StatusBadRequest, code: code, message: message}
}

// errStatus is an error with any status, and details when non-nil.
func errStatus(status int, code ErrorCode, message string, details any) error {
	e := &apiError{status: status, code: code, message: message}
	if details != nil {
		e.details, _ = json.Marshal(details) // details are always plain structs
	}
	return e
}

// pathDetails names where in a request body an error is.
type pathDetails struct {
	Path string `json:"path"`
}

// ambiguousRepoDetails are an ambiguous repository's details: the
// installations that hold a repository of the name asked for, one of which
// ?installation= must name.
type ambiguousRepoDetails struct {
	Installations []string `json:"installations"`
}

// slugTakenDetails are a slug_taken error's details. Adoptable is set only
// when the slug belonged to a tenant that is gone, so creating it again
// with adopt would succeed; never for a slug a live tenant holds.
type slugTakenDetails struct {
	Path      string `json:"path"`
	Adoptable bool   `json:"adoptable,omitempty"`
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
		writeJSON(w, e.status, ErrorBody{Code: e.code, Message: e.message, Details: e.details})
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
