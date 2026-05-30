package apierr

import (
	"errors"
	"net/http"
)

// Code is a stable, machine-readable error code returned in the API envelope.
type Code string

const (
	NotFound     Code = "not_found"
	Conflict     Code = "revision_conflict"
	Validation   Code = "validation_failed"
	Unauthorized Code = "unauthorized"
	Forbidden    Code = "policy_denied"
	BadRequest   Code = "bad_request"
	Internal     Code = "internal"
)

// Error is a typed service error carrying an API code and optional details.
type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

func New(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// HTTPStatus maps the code to an HTTP status.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case Validation:
		return http.StatusUnprocessableEntity
	case Unauthorized:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case BadRequest:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// As extracts an *Error from any error, if present.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
