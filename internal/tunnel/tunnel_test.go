package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/tunnel/tunneltest"
)

// echoServer answers every connection with what it reads.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func roundTrip(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("read %q, %v; want the echo", buf, err)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		spec, want string
	}{
		{"ssh://admin@bastion.example.com", ""},
		{"socks5://127.0.0.1:1080", ""},
		{"http://proxy.corp:3128", ""},
		{"ftp://example.com", "scheme must be one of"},
		{"ssh://", "name the host"},
		{"::", "missing protocol scheme"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.spec)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("Parse(%q) = %v, want %q", tt.spec, err, tt.want)
		}
	}
	if got := Redacted("http://user:secret@proxy:3128"); strings.Contains(got, "secret") {
		t.Errorf("Redacted kept the password: %s", got)
	}
}

func TestLocalProxyNeedsItsCredentials(t *testing.T) {
	target := echoServer(t)
	var d net.Dialer
	lp, err := startLocalProxy(d.DialContext, nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer lp.close()
	ctx := context.Background()

	c, err := dialProxy(ctx, lp.url(), target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)

	wrong := lp.url()
	wrong.User = url.UserPassword(lp.user, "guess")
	if c, err := dialProxy(ctx, wrong, target); err == nil {
		c.Close()
		t.Fatal("the local proxy accepted a wrong password")
	}
	anon := lp.url()
	anon.User = nil
	if c, err := dialProxy(ctx, anon, target); err == nil {
		c.Close()
		t.Fatal("the local proxy accepted a client without credentials")
	}
}

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	p := NewPool(WithHome(t.TempDir()), WithGetenv(func(string) string { return "" }), WithLogger(discard()))
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestSSHTunnel(t *testing.T) {
	spec, srv := tunneltest.SSH(t, true)
	p := newTestPool(t)
	ctx := context.Background()

	target := echoServer(t)
	c, err := p.Dial(ctx, spec, target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)

	// A Kubernetes client gets a proxy URL; plain HTTP through it reaches
	// a server only the jump host can see.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "through the jump host") }))
	defer api.Close()
	proxyURL, err := p.ProxyURL(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := p.ProxyURL(ctx, spec); again.String() != proxyURL.String() {
		t.Fatal("the pool opened a second local proxy for the same tunnel")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "through the jump host" {
		t.Fatalf("body %q", body)
	}
	if dials := srv.Dials(); len(dials) != 2 || dials[0] != target {
		t.Fatalf("the jump host opened %v; want the echo server, then the API server", dials)
	}
}

func TestSSHTunnelRefusesUnknownHosts(t *testing.T) {
	spec, _ := tunneltest.SSH(t, false)
	_, err := newTestPool(t).Dial(context.Background(), spec, "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "SOCKS") && !strings.Contains(err.Error(), "refused") {
		// The local proxy reports a failed tunnel as a refused SOCKS
		// connection; the reason is logged.
		t.Fatalf("Dial through an unknown jump host = %v, want a refusal", err)
	}
	u, _ := url.Parse(spec)
	dial, closer, err := newSSH(u, newTestPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "not in known_hosts") {
		t.Fatalf("dial = %v, want the unknown host key explained", err)
	}
}

func TestSSHTunnelNeedsAKey(t *testing.T) {
	u, _ := url.Parse("ssh://tester@127.0.0.1:22")
	if _, _, err := newSSH(u, newTestPool(t)); err == nil || !strings.Contains(err.Error(), "no SSH key") {
		t.Fatalf("newSSH without keys = %v, want an error naming ssh-agent", err)
	}
}

// Ensure the SOCKS reply encodes the port as RFC 1928 says.
func TestReplyLayout(t *testing.T) {
	var b strings.Builder
	if err := reply(&b, replySucceeded); err != nil {
		t.Fatal(err)
	}
	got := []byte(b.String())
	if len(got) != 10 || got[0] != socksVersion || got[1] != replySucceeded || binary.BigEndian.Uint16(got[8:]) != 0 {
		t.Fatalf("reply = %v", got)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
