package iap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

var (
	instanceConfig = Config{Project: "p1", Zone: "europe-west2-a", Instance: "bastion"}
	groupConfig    = Config{Project: "p1", Region: "europe-west2", Network: "default", Group: "gke-masters"}
)

// dial opens a tunnel to port 22 of the test instance through r.
func dial(t *testing.T, r *fakeRelay, opts ...Option) *conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, instanceConfig, "bastion:22", r.options(opts...)...)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// stream returns n bytes of a pattern that does not repeat within a frame,
// starting at stream offset from.
func stream(from, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((from + i) % 251)
	}
	return b
}

// readN reads n bytes from c, failing the test after 10 seconds.
func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	b := make([]byte, n)
	if got, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("reading %d bytes: got %d, %v", n, got, err)
	}
	return b
}

// writeAsync writes p to c from another goroutine and reports the result
// on the returned channel.
func writeAsync(c net.Conn, p []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := c.Write(p)
		done <- err
	}()
	return done
}

// waitFor polls cond for up to 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	giveUp := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(giveUp) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readErr reads from c until it fails and returns the error.
func readErr(t *testing.T, c net.Conn) error {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := io.Copy(io.Discard, c)
	if err == nil {
		return io.EOF
	}
	return err
}

func TestDialInstance(t *testing.T) {
	r := newFakeRelay(t)
	// An unknown tag is skipped, as gcloud does.
	r.afterOpen = [][]byte{{0x00, 0x09, 1, 2, 3}}
	c := dial(t, r)

	req := r.request(0)
	if req.URL.Path != "/v4/connect" {
		t.Errorf("path = %s, want /v4/connect", req.URL.Path)
	}
	if got, want := req.URL.RawQuery, "instance=bastion&interface=nic0&newWebsocket=True&port=22&project=p1&zone=europe-west2-a"; got != want {
		t.Errorf("query = %s, want %s", got, want)
	}
	for k, want := range map[string]string{
		"Authorization":          "Bearer " + testToken,
		"Origin":                 "bot:iap-tunneler",
		"User-Agent":             "tug/" + version.String(),
		"Sec-Websocket-Protocol": "relay.tunnel.cloudproxy.app",
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if c.sid != "sid-1" {
		t.Errorf("session ID = %q, want sid-1", c.sid)
	}
	if got := c.RemoteAddr(); got.Network() != "iap" || got.String() != "bastion:22" {
		t.Errorf("RemoteAddr() = %s %s, want iap bastion:22", got.Network(), got)
	}
	if c.LocalAddr() == nil {
		t.Error("LocalAddr() = nil")
	}

	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 5); string(got) != "hello" {
		t.Errorf("read %q, want hello", got)
	}
}

func TestDialGroup(t *testing.T) {
	r := newFakeRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, groupConfig, "172.16.0.2:443", r.options(WithUserAgent("tug/test"))...)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	req := r.request(0)
	if got, want := req.URL.RawQuery, "group=gke-masters&host=172.16.0.2&network=default&newWebsocket=True&port=443&project=p1&region=europe-west2"; got != want {
		t.Errorf("query = %s, want %s", got, want)
	}
	if got := req.Header.Get("User-Agent"); got != "tug/test" {
		t.Errorf("User-Agent = %q, want tug/test", got)
	}
	if got := c.RemoteAddr().String(); got != "172.16.0.2:443" {
		t.Errorf("RemoteAddr() = %s, want 172.16.0.2:443", got)
	}
	if _, err := c.Write([]byte("GET /")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 5); string(got) != "GET /" {
		t.Errorf("read %q, want GET /", got)
	}
}

func TestDialOverTLS(t *testing.T) {
	r := newFakeTLSRelay(t)
	if !strings.HasPrefix(r.url(), "wss://") {
		t.Fatalf("relay URL = %s, want wss://", r.url())
	}
	c := dial(t, r)
	if _, err := c.Write([]byte("secure")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 6); string(got) != "secure" {
		t.Errorf("read %q, want secure", got)
	}
}

func TestEchoAcrossFramesWithAcks(t *testing.T) {
	r := newFakeRelay(t)
	c := dial(t, r)
	const n = 300 << 10
	sent := writeAsync(c, stream(0, n))
	if got := readN(t, c, n); !bytes.Equal(got, stream(0, n)) {
		t.Fatal("the echo differs from what was written")
	}
	if err := <-sent; err != nil {
		t.Fatalf("Write: %v", err)
	}

	s := r.session()
	if got := get(r, func() int { return s.maxFrame }); got != maxDataFrame {
		t.Errorf("largest DATA frame = %d bytes, want %d", got, maxDataFrame)
	}
	// The tunnel acknowledges once more than two frames' worth is
	// unacknowledged, so the last ACK is at most that far behind.
	waitFor(t, "the tunnel's ACKs", func() bool {
		return get(r, func() bool { return len(s.acks) > 0 && s.acks[len(s.acks)-1] >= n-ackWindow })
	})
	// Each ACK follows more than two frames' worth of new bytes, as in
	// gcloud, and none counts bytes the relay did not send.
	acks := get(r, func() []uint64 { return append([]uint64(nil), s.acks...) })
	prev := uint64(0)
	for _, a := range acks {
		if a > n || a-prev <= ackWindow {
			t.Fatalf("ACKs = %v, want counts up to %d more than %d apart", acks, n, ackWindow)
		}
		prev = a
	}
	// The relay's ACKs free what the tunnel kept for resending.
	waitFor(t, "the relay's ACKs to free the resend buffer", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.confirmed == n && len(c.out) == 0 && c.base == n
	})
}

// tokenFunc is an oauth2.TokenSource made from a function.
type tokenFunc func() (*oauth2.Token, error)

func (f tokenFunc) Token() (*oauth2.Token, error) { return f() }

func TestDialErrors(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(r *fakeRelay)
		tokens   oauth2.TokenSource
		code     provider.Code
		protocol bool
		msg      string
		hint     string
	}{
		{
			name:   "token refused",
			tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "wrong"}),
			code:   provider.Unauthenticated,
			msg:    "401 Unauthorized",
			hint:   "gcloud auth application-default login",
		},
		{
			name:  "forbidden",
			setup: func(r *fakeRelay) { r.refuse = http.StatusForbidden },
			code:  provider.PermissionDenied,
			msg:   "403 Forbidden: refused by the test",
			hint:  "roles/iap.tunnelResourceAccessor",
		},
		{
			name:  "not found",
			setup: func(r *fakeRelay) { r.refuse = http.StatusNotFound },
			code:  provider.NotFound,
			hint:  "check that the instance exists",
		},
		{
			name:  "unavailable",
			setup: func(r *fakeRelay) { r.refuse = http.StatusServiceUnavailable },
			code:  provider.Unavailable,
		},
		{
			name:  "backend unreachable",
			setup: func(r *fakeRelay) { r.closeFirst = 4003 },
			code:  provider.Unavailable,
			msg:   "[4003: closed by the test]",
			hint:  "35.235.240.0/20",
		},
		{
			name:  "reauthentication",
			setup: func(r *fakeRelay) { r.closeFirst = 4004 },
			code:  provider.Unauthenticated,
			hint:  "gcloud auth application-default login",
		},
		{
			name:  "not authorised",
			setup: func(r *fakeRelay) { r.closeFirst = 4033 },
			code:  provider.PermissionDenied,
			msg:   "[4033: closed by the test]",
			hint:  "roles/iap.tunnelResourceAccessor",
		},
		{
			name:  "instance not found",
			setup: func(r *fakeRelay) { r.closeFirst = 4047 },
			code:  provider.NotFound,
			hint:  "running",
		},
		{
			name:     "data before the session",
			setup:    func(r *fakeRelay) { r.firstFrame = relayFrame(tagData, []byte("x")) },
			protocol: true,
			msg:      "DATA instead of CONNECT_SUCCESS_SID",
		},
		{
			name:     "no subprotocol",
			setup:    func(r *fakeRelay) { r.wrongProto = true },
			protocol: true,
			msg:      "subprotocol",
		},
		{
			name: "no credentials",
			tokens: tokenFunc(func() (*oauth2.Token, error) {
				return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
			}),
			code: provider.Unauthenticated,
			hint: "gcloud auth application-default login",
		},
		{
			name: "token service down",
			tokens: tokenFunc(func() (*oauth2.Token, error) {
				return nil, errors.New("dial tcp: no route to host")
			}),
			code: provider.Unavailable,
			msg:  "getting a Google access token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRelay(t)
			if tt.setup != nil {
				tt.setup(r)
			}
			opts := r.options()
			if tt.tokens != nil {
				opts = append(opts, WithTokenSource(tt.tokens))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := Dial(ctx, instanceConfig, "bastion:22", opts...)
			if err == nil {
				c.Close()
				t.Fatal("Dial succeeded")
			}
			if !strings.Contains(err.Error(), "opening an IAP tunnel to port 22 of instance bastion") || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("Dial error = %q, want one about the tunnel containing %q", err, tt.msg)
			}
			if tt.protocol {
				if !errors.Is(err, errProtocol) {
					t.Errorf("Dial error = %v, want errProtocol", err)
				}
				return
			}
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("Dial error = %v, want a *provider.Error", err)
			}
			if pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("Dial error code, hint = %s, %q; want %s, one containing %q", pe.Code, pe.Hint, tt.code, tt.hint)
			}
		})
	}
}

func TestDialBadSettings(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		address string
		opts    []Option
		wantErr string
	}{
		{"no project", Config{Zone: "z", Instance: "vm"}, "vm:22", nil, "needs a project"},
		{"no port", instanceConfig, "bastion", nil, "parsing the IAP tunnel address"},
		{"plain relay", instanceConfig, "bastion:22", []Option{WithRelayURL("ws://relay.example/v4")}, "must use wss"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]Option{WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testToken}))}, tt.opts...)
			if _, err := Dial(context.Background(), tt.cfg, tt.address, opts...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Dial() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDialHonorsContext(t *testing.T) {
	r := newFakeRelay(t)
	r.pause() // the relay never sends CONNECT_SUCCESS_SID
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, instanceConfig, "bastion:22", r.options()...)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Dial error = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Dial took %s after its context ended", took)
	}
}

func TestClose(t *testing.T) {
	r := newFakeRelay(t)
	c := dial(t, r)
	reading := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 10))
		reading <- err
	}()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-reading:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("blocked Read = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read still blocked after Close")
	}
	if !isDone(c.done) {
		t.Error("the session goroutine is still running after Close")
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write after Close = %v, want net.ErrClosed", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read after Close = %v, want net.ErrClosed", err)
	}
	s := r.session()
	waitFor(t, "the relay to see a normal close", func() bool {
		return get(r, func() bool { return len(s.closed) == 1 && s.closed[0] == 1000 })
	})
}

func TestCloseSendsBufferedBytes(t *testing.T) {
	r := newFakeRelay(t)
	c := dial(t, r)
	const n = 200 << 10
	if _, err := c.Write(stream(0, n)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s := r.session()
	waitFor(t, "the relay to receive every byte", func() bool {
		return get(r, func() bool { return len(s.stream) == n })
	})
	if got := get(r, func() []byte { return s.stream }); !bytes.Equal(got, stream(0, n)) {
		t.Error("the relay received different bytes")
	}
}

func TestRelayCloses(t *testing.T) {
	tests := []struct {
		name string
		code int
		want func(error) bool
	}{
		{"normally", 1000, func(err error) bool { return errors.Is(err, io.EOF) }},
		{"with an error", 4010, func(err error) bool {
			return provider.CodeOf(err) == provider.Unavailable && strings.Contains(err.Error(), "[4010: drop]")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRelay(t)
			c := dial(t, r)
			s := r.session()
			r.set(func() { s.dropAt, s.dropCode = 3, tt.code })
			if _, err := c.Write([]byte("bye")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := readN(t, c, 3); string(got) != "bye" {
				t.Errorf("read %q, want bye", got)
			}
			if err := readErr(t, c); !tt.want(err) {
				t.Errorf("Read after the relay closed = %v", err)
			}
			if _, err := c.Write([]byte("more")); err == nil {
				t.Error("Write after the relay closed succeeded")
			}
		})
	}
}

func TestProtocolFaultEndsTunnel(t *testing.T) {
	r := newFakeRelay(t)
	// An ACK of bytes never sent.
	r.afterOpen = [][]byte{relayCount(tagAck, 1<<40)}
	c := dial(t, r)
	if err := readErr(t, c); !errors.Is(err, errProtocol) {
		t.Errorf("Read = %v, want errProtocol", err)
	}
}

func TestReadDeadline(t *testing.T) {
	r := newFakeRelay(t)
	c := dial(t, r)

	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("Read past its deadline = %v, want a timeout", err)
	}

	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Read = %v, want os.ErrDeadlineExceeded", err)
	}
	if took := time.Since(start); took < 40*time.Millisecond {
		t.Errorf("Read timed out after %s, before its deadline", took)
	}

	_ = c.SetDeadline(time.Time{})
	if _, err := c.Write([]byte("still open")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, c, 10); string(got) != "still open" {
		t.Errorf("read %q after a timeout, want still open", got)
	}
}

func TestWriteDeadline(t *testing.T) {
	r := newFakeRelay(t)
	c := dial(t, r)
	r.pause()
	_ = c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	written := 0
	var err error
	for err == nil {
		if written > 256<<20 {
			t.Fatal("Write never blocked on a relay that does not read")
		}
		var n int
		n, err = c.Write(stream(written, 64<<10))
		written += n
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write = %v, want os.ErrDeadlineExceeded", err)
	}

	// The timeout leaves the tunnel working, and every byte Write took
	// arrives.
	r.resume()
	_ = c.SetWriteDeadline(time.Time{})
	if got := readN(t, c, written); !bytes.Equal(got, stream(0, written)) {
		t.Fatal("the echo differs from what Write took")
	}
	sent := writeAsync(c, stream(written, 1000))
	if got := readN(t, c, 1000); !bytes.Equal(got, stream(written, 1000)) {
		t.Error("the echo after the timeout differs")
	}
	if err := <-sent; err != nil {
		t.Errorf("Write: %v", err)
	}
}
