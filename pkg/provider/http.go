package provider

import (
	"net/http"
	"strconv"
	"strings"
)

// HTTPClient returns a client for calls to a provider's API. A native plugin
// gets http.DefaultTransport. A plugin compiled to WebAssembly (GOOS=wasip1)
// gets a transport that sends each request through tug's host functions,
// which allow only https to the hosts in the manifest's spec.network; the
// SDK also installs that transport as http.DefaultTransport there, so
// http.DefaultClient works too. Send each request with the operation's
// context so that a cancelled or timed-out operation stops its requests.
func HTTPClient() *http.Client { return &http.Client{Transport: platformTransport()} }

// StatusError converts an HTTP error status from a provider's API into an
// *Error with the matching code: 401 is Unauthenticated, 403
// PermissionDenied, 404 NotFound, 429 RateLimited (with the Retry-After
// seconds when the API sends them), 408 and 5xx Unavailable, 400 and 422
// InvalidConfig, and anything else Internal. Some APIs answer 403 for a
// token they do not accept; plugins for those map it to Unauthenticated
// themselves.
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
