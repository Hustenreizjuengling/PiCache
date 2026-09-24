// Package apperr defines the user-facing error kinds shared by domain
// packages and the HTTP API. Domain packages return these for conditions the
// user can act on; the API maps them to HTTP status codes. Any other error is
// treated as an internal error (HTTP 500, details only in the log).
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an error for the API layer.
type Kind int

const (
	KindInternal     Kind = iota
	KindInvalid           // 400: validation failed
	KindNotFound          // 404
	KindConflict          // 409: duplicate, state conflict
	KindForbidden         // 403: not allowed in this state / by policy
	KindUnavailable       // 503: dependency offline (e.g. storage)
	KindUnauthorized      // 401
	KindTooMany           // 429: rate limited
)

// Error is a user-facing error. Message must be safe to show to the admin
// (no secrets, no internal paths beyond what the admin configured).
type Error struct {
	Kind    Kind
	Field   string // optional: the input field that is invalid
	Message string
	Err     error // optional wrapped cause (logged, never shown)
}

func (e *Error) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Message
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Invalid reports a validation error for one input field.
func Invalid(field, format string, args ...any) error {
	return &Error{Kind: KindInvalid, Field: field, Message: fmt.Sprintf(format, args...)}
}

// NotFound reports a missing entity, e.g. NotFound("list", 42).
func NotFound(what string, id any) error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf("%s %v not found", what, id)}
}

// Conflict reports a state conflict or duplicate.
func Conflict(format string, args ...any) error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

// Forbidden reports an action that is not allowed.
func Forbidden(format string, args ...any) error {
	return &Error{Kind: KindForbidden, Message: fmt.Sprintf(format, args...)}
}

// Unavailable reports that a dependency is currently unavailable.
func Unavailable(format string, args ...any) error {
	return &Error{Kind: KindUnavailable, Message: fmt.Sprintf(format, args...)}
}

// Unauthorized reports missing or invalid credentials.
func Unauthorized(format string, args ...any) error {
	return &Error{Kind: KindUnauthorized, Message: fmt.Sprintf(format, args...)}
}

// TooMany reports rate limiting.
func TooMany(format string, args ...any) error {
	return &Error{Kind: KindTooMany, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a cause to a user-facing error.
func Wrap(kind Kind, cause error, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: cause}
}

// KindOf returns the Kind of err (KindInternal if err is not an *Error).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// As returns the *Error in err's chain, if any.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
