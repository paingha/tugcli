package provider

import (
	"context"
	"fmt"
)

// HostModule is the import module of the host functions tug gives a plugin
// compiled to WebAssembly (GOOS=wasip1). Such a plugin has no sockets: it
// reaches the network only through these functions, and tug checks every
// request against the manifest's spec.network. [HTTPClient] uses them, and
// docs/plugins.md describes the ABI for plugins written in other languages.
const HostModule = "tug_http"

// Limits tug's host functions apply to each request.
const (
	// MaxHostRequestHeadBytes caps the JSON head of a request: method, URL,
	// headers and timeout.
	MaxHostRequestHeadBytes = 64 << 10
	// MaxHostRequestBodyBytes caps a request body.
	MaxHostRequestBodyBytes = 8 << 20
	// MaxHostResponseHeaderBytes caps the headers of a response.
	MaxHostResponseHeaderBytes = 1 << 20
	// MaxHostResponseBodyBytes caps a response body, after any gzip
	// encoding is removed.
	MaxHostResponseBodyBytes = 32 << 20
	// MaxHostOpenResponses caps the responses a module holds open at once.
	MaxHostOpenResponses = 32
	// MaxHostRedirects caps the redirects tug follows for one request. Each
	// one must also be https to a host in spec.network.
	MaxHostRedirects = 10
)

// HostErrno is the result of a host function: zero for success, otherwise
// why it failed. The message for the last failure is read with last_error.
type HostErrno uint32

// Host function results.
const (
	// HostOK means the call succeeded.
	HostOK HostErrno = iota
	// HostDenied means the URL is not https, or its host is not in the
	// manifest's spec.network.
	HostDenied
	// HostInvalid means the request is malformed or a pointer is outside
	// the module's memory.
	HostInvalid
	// HostTooLarge means a request or response is over a limit.
	HostTooLarge
	// HostNetwork means the request failed: DNS, connection, TLS or a
	// broken response.
	HostNetwork
	// HostTimeout means the request's deadline passed, or the operation
	// was cancelled.
	HostTimeout
	// HostBadHandle means no open response has the handle.
	HostBadHandle
	// HostTooMany means MaxHostOpenResponses responses are already open.
	HostTooMany
	// HostShortBuffer means the buffer is too small; the length needed has
	// been written to the length pointer.
	HostShortBuffer
)

// String names the result as the documentation does.
func (e HostErrno) String() string {
	switch e {
	case HostOK:
		return "ok"
	case HostDenied:
		return "denied"
	case HostInvalid:
		return "invalid"
	case HostTooLarge:
		return "too large"
	case HostNetwork:
		return "network"
	case HostTimeout:
		return "timeout"
	case HostBadHandle:
		return "bad handle"
	case HostTooMany:
		return "too many"
	case HostShortBuffer:
		return "short buffer"
	default:
		return fmt.Sprintf("errno %d", uint32(e))
	}
}

// HostError is a failed host function call, such as a request to a host
// the manifest does not declare. The transport HTTPClient uses in a
// WebAssembly plugin returns it, wrapped in a *url.Error.
type HostError struct {
	Errno   HostErrno
	Message string
}

// Error returns the message tug gave.
func (e *HostError) Error() string {
	if e.Message == "" {
		return HostModule + ": " + e.Errno.String()
	}
	return HostModule + ": " + e.Message
}

// Timeout reports whether the request ran out of time, as net.Error does.
func (e *HostError) Timeout() bool { return e.Errno == HostTimeout }

// Unwrap makes a timeout match context.DeadlineExceeded.
func (e *HostError) Unwrap() error {
	if e.Errno == HostTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

// HostRequest is the JSON head of a request a module passes to the request
// host function. The body travels separately, as raw bytes.
type HostRequest struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Header map[string][]string `json:"header,omitempty"`
	// TimeoutMillis is how long the request may take, headers and body;
	// zero leaves it to the operation's own timeout.
	TimeoutMillis int64 `json:"timeoutMillis,omitempty"`
}

// HostResponse is the JSON head of a response that response_head writes.
type HostResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	// ContentLength is the body's length, or -1 when it is not known.
	ContentLength int64 `json:"contentLength"`
	// URL is the URL that answered, after any redirects.
	URL string `json:"url"`
}
