package provider

import (
	"net/http"
	"strconv"
	"strings"
)

// StatusError converts an HTTP error status from a provider's API into an
// *Error with the code tug acts on: 401 asks the user to sign in again, 429
// backs off for Retry-After seconds, and 5xx is reported as unavailable.
func StatusError(status int, message string, header http.Header) *Error {
	if message == "" {
		message = http.StatusText(status)
	}
	e := &Error{Message: strings.TrimSpace(message)}
	switch {
	case status == http.StatusUnauthorized:
		e.Code = Unauthenticated
	case status == http.StatusForbidden:
		e.Code = PermissionDenied
	case status == http.StatusNotFound:
		e.Code = NotFound
	case status == http.StatusTooManyRequests:
		e.Code = RateLimited
		if s, err := strconv.Atoi(header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAfterSeconds = s
		}
	case status == http.StatusRequestTimeout || status >= 500:
		e.Code = Unavailable
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		e.Code = InvalidConfig
	default:
		e.Code = Internal
	}
	return e
}
