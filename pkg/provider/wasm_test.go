package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestHostError(t *testing.T) {
	tests := []struct {
		err      *HostError
		want     string
		timeout  bool
		deadline bool
	}{
		{&HostError{Errno: HostDenied, Message: "GET https://x/ refused"}, "tug_http: GET https://x/ refused", false, false},
		{&HostError{Errno: HostTimeout, Message: "GET https://x/: context deadline exceeded"}, "tug_http: GET https://x/: context deadline exceeded", true, true},
		{&HostError{Errno: HostShortBuffer}, "tug_http: short buffer", false, false},
		{&HostError{Errno: 42}, "tug_http: errno 42", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
			// As http.Client wraps it.
			wrapped := &url.Error{Op: "Get", URL: "https://x/", Err: tt.err}
			if got := wrapped.Timeout(); got != tt.timeout {
				t.Errorf("url.Error.Timeout() = %v, want %v", got, tt.timeout)
			}
			if got := errors.Is(fmt.Errorf("listing: %w", wrapped), context.DeadlineExceeded); got != tt.deadline {
				t.Errorf("errors.Is(DeadlineExceeded) = %v, want %v", got, tt.deadline)
			}
			var he *HostError
			if !errors.As(wrapped, &he) || he.Errno != tt.err.Errno {
				t.Errorf("errors.As found %v, want errno %d", he, tt.err.Errno)
			}
		})
	}
}

func TestHTTPClientUsesThePlatformTransport(t *testing.T) {
	if c := HTTPClient(); c.Transport != platformTransport() {
		t.Fatalf("HTTPClient().Transport = %T, want the platform's", c.Transport)
	}
}
