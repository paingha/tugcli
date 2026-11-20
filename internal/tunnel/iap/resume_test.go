package iap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/paingha/tugcli/pkg/provider"
)

func TestResumeAt(t *testing.T) {
	// The stream so far: 100 bytes written, the relay has acknowledged 40,
	// and the tunnel keeps bytes from offset base for resending.
	session := func(base uint64) *conn {
		return &conn{out: stream(int(base), int(100-base)), base: base, wrote: 100, confirmed: 40, received: 7, acked: 3}
	}
	tests := []struct {
		name    string
		base    uint64
		n       uint64
		wantErr error
	}{
		{name: "from the last ACK", base: 40, n: 40},
		{name: "from later bytes", base: 40, n: 75},
		{name: "everything arrived", base: 40, n: 100},
		{name: "before the last ACK", base: 40, n: 39, wantErr: errProtocol},
		{name: "beyond what was sent", base: 40, n: 101, wantErr: errProtocol},
		{name: "before what was kept", base: 60, n: 50, wantErr: errLost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := session(tt.base)
			err := c.resumeAt(tt.n)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resumeAt(%d) = %v, want %v", tt.n, err, tt.wantErr)
				}
				if retryable(err) {
					t.Errorf("retryable(%v) = true, want false", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resumeAt(%d): %v", tt.n, err)
			}
			if c.wrote != tt.n || c.confirmed != tt.n || c.base != tt.n || !bytes.Equal(c.out, stream(int(tt.n), int(100-tt.n))) {
				t.Errorf("after resumeAt(%d): wrote %d, confirmed %d, base %d, %d bytes kept; want %d, %d, %d, %d",
					tt.n, c.wrote, c.confirmed, c.base, len(c.out), tt.n, tt.n, tt.n, 100-tt.n)
			}
			if c.acked != c.received {
				t.Errorf("after resumeAt(%d): acked %d, want the %d received", tt.n, c.acked, c.received)
			}
		})
	}
}

func TestResumable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&websocket.CloseError{Code: closeReauthRequired}, true},
		{&websocket.CloseError{Code: websocket.CloseAbnormalClosure}, true},
		{&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{&websocket.CloseError{Code: websocket.CloseNormalClosure}, false},
		{&websocket.CloseError{Code: closeNotAuthorized}, false},
		{fmt.Errorf("%w: a text message", errProtocol), false},
		{websocket.ErrReadLimit, false},
	}
	for _, tt := range tests {
		if got := resumable(tt.err); got != tt.want {
			t.Errorf("resumable(%v) = %t, want %t", tt.err, got, tt.want)
		}
	}
}

func TestRetryable(t *testing.T) {
	tg := target{cfg: instanceConfig, port: 22}
	tests := []struct {
		err  error
		want bool
	}{
		{relayClosed(closeReauthRequired, "", tg), true},
		{provider.Errorf(provider.Unavailable, "503"), true},
		{provider.Errorf(provider.RateLimited, "429"), true},
		{io.ErrUnexpectedEOF, true},
		{relayClosed(closeNotAuthorized, "", tg), false},
		{relayClosed(closeBackendUnreachable, "", tg), false},
		{provider.Errorf(provider.NotFound, "404"), false},
		{provider.Errorf(provider.Unauthenticated, "401"), false},
		{fmt.Errorf("%w: CONNECT_SUCCESS_SID instead of RECONNECT_SUCCESS_ACK", errProtocol), false},
	}
	for _, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("retryable(%v) = %t, want %t", tt.err, got, tt.want)
		}
	}
}
