package provider

import (
	"errors"
	"fmt"
	"time"
)

// Code classifies an error so that tug can act on it.
type Code string

const (
	// Unauthenticated means the secrets are missing, wrong or expired. tug
	// asks the user to sign in again.
	Unauthenticated Code = "Unauthenticated"
	// PermissionDenied means the identity is valid but not allowed.
	PermissionDenied Code = "PermissionDenied"
	// NotFound means the account or cluster does not exist.
	NotFound Code = "NotFound"
	// RateLimited means try again later; set RetryAfterSeconds when known.
	RateLimited Code = "RateLimited"
	// Unavailable means the provider API could not be reached.
	Unavailable Code = "Unavailable"
	// InvalidConfig means the account settings or the request are wrong.
	InvalidConfig Code = "InvalidConfig"
	// Unimplemented means the operation is not supported.
	Unimplemented Code = "Unimplemented"
	// Internal is anything else.
	Internal Code = "Internal"
)

// Error is a classified provider error. It is what plugins send on the wire.
type Error struct {
	Code              Code   `json:"code"`
	Message           string `json:"message"`
	Hint              string `json:"hint,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

// Error returns the code and message. The hint is for people, so callers
// print it separately.
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// RetryAfter returns the suggested wait for RateLimited errors.
func (e *Error) RetryAfter() time.Duration {
	return time.Duration(e.RetryAfterSeconds) * time.Second
}

// Errorf returns an *Error with the given code.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint returns a copy of e with a hint for the user.
func (e *Error) WithHint(hint string) *Error {
	c := *e
	c.Hint = hint
	return &c
}

// CodeOf returns the code of err, or Internal when err is not an *Error.
func CodeOf(err error) Code {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return Internal
}

// AsError converts any error to an *Error, keeping the code when it has one.
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: Internal, Message: err.Error()}
}
