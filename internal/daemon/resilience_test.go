package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/kube"
	sdk "github.com/paingha/tugcli/pkg/provider"
)

// upgrade opens an upgraded connection through the proxy, as exec, attach
// and port-forward do, and returns the status it was answered with. When
// it is upgraded, it checks that what is sent comes back, and returns the
// connection for more.
func (f *fixture) upgrade(t *testing.T, method, path, proto string) (int, *bufio.ReadWriter, func()) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "%s /s/%s%s HTTP/1.1\r\nHost: tugd\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: 0\r\n\r\n",
		method, f.sess.State.ID, path, f.token, proto)
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	resp, err := http.ReadResponse(rw.Reader, nil)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	resp.Body.Close() // a 101 has none; the connection carries on
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return resp.StatusCode, nil, func() {}
	}
	echo(t, rw, "ping")
	return resp.StatusCode, rw, func() { conn.Close() }
}

// echo sends a line over an upgraded connection and checks it comes back.
func echo(t *testing.T, rw *bufio.ReadWriter, line string) {
	t.Helper()
	fmt.Fprint(rw, line+"\n")
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := rw.ReadString('\n')
	if err != nil || got != "echo: "+line+"\n" {
		t.Fatalf("over the upgraded connection: %q, %v; want the line echoed", got, err)
	}
}

// stream starts a streaming GET through the proxy and returns its status
// and first line, which must arrive while the stream is still open.
func (f *fixture) stream(t *testing.T, path string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.proxy.URL+"/s/"+f.sess.State.ID+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return resp.StatusCode, line
}

// Watches, logs -f and port-forwards work on every session; exec and attach,
// directly or through a node's kubelet, only on unlocked or read-write ones.
func TestProxyStreamsOnReadOnlyAndUnlockedSessions(t *testing.T) {
	const pod = "/api/v1/namespaces/default/pods/api-0"
	streams := []struct {
		name, method, path, upgrade string
		write                       bool
	}{
		{"watch", http.MethodGet, "/api/v1/namespaces?watch=true&resourceVersion=1", "", false},
		{"logs -f", http.MethodGet, pod + "/log?follow=true&container=app", "", false},
		{"port-forward over SPDY", http.MethodPost, pod + "/portforward", "SPDY/3.1", false},
		{"port-forward over WebSocket", http.MethodGet, pod + "/portforward?ports=8080", "websocket", false},
		{"exec over SPDY", http.MethodPost, pod + "/exec?command=sh&stdin=true&tty=true", "SPDY/3.1", true},
		{"exec over WebSocket", http.MethodGet, pod + "/exec?command=sh&stdin=true", "websocket", true},
		{"attach", http.MethodPost, pod + "/attach?stdin=true", "SPDY/3.1", true},
		{"exec through the kubelet", http.MethodGet, "/api/v1/nodes/n1/proxy/exec/default/api-0/app?command=sh", "websocket", true},
	}
	for _, unlocked := range []bool{false, true} {
		mode := "read-only"
		if unlocked {
			mode = "unlocked"
		}
		t.Run(mode, func(t *testing.T) {
			rec := &recorder{}
			f := setup(t, func(c *Config) { c.Audit = rec })
			if unlocked {
				if err := f.sess.Unlock(time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			for _, st := range streams {
				t.Run(st.name, func(t *testing.T) {
					var code, want int
					switch {
					case st.upgrade == "":
						var line string
						code, line = f.stream(t, st.path)
						if code == http.StatusOK && line != "first\n" {
							t.Fatalf("first line = %q", line)
						}
						want = http.StatusOK
					case st.write && !unlocked:
						code, _, _ = f.upgrade(t, st.method, st.path, st.upgrade)
						want = http.StatusForbidden
					default:
						var closeConn func()
						code, _, closeConn = f.upgrade(t, st.method, st.path, st.upgrade)
						closeConn()
						want = http.StatusSwitchingProtocols
					}
					if code != want {
						t.Fatalf("%s %s on a %s session = %d, want %d", st.method, st.path, mode, code, want)
					}
				})
			}
			// Each exec and attach is in the audit log: as a write, with
			// its outcome, or as refused. Streams that only read are not.
			var writes, results, refused int
			for _, e := range rec.recorded() {
				switch e.Kind {
				case audit.KindWrite:
					writes++
					if strings.Contains(e.Path, "?") || e.Allowed != audit.AllowedUnlocked {
						t.Errorf("write entry %+v", e)
					}
				case audit.KindResult:
					results++
					if e.Status != http.StatusSwitchingProtocols {
						t.Errorf("result %+v, want 101", e)
					}
				case audit.KindRefused:
					refused++
				}
			}
			if unlocked && (writes != 4 || results != 4 || refused != 0) || !unlocked && (writes != 0 || refused != 4) {
				t.Fatalf("recorded %d writes, %d results and %d refusals: %+v", writes, results, refused, rec.recorded())
			}
		})
	}
}

// clock is a time that tests move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// An unlock allows writes up to the moment it ends and not after, whatever
// the client. Its end is recorded once. A stream opened while unlocked, such
// as an exec, is not cut off when the unlock ends.
func TestUnlockEndsOnTime(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	until := start.Add(15 * time.Minute)
	c := &clock{t: start}
	rec := &recorder{}
	f := setup(t, func(cfg *Config) {
		cfg.Now = c.now
		cfg.Audit = rec
	})
	f.sess.State.UnlockedUntil = until
	if err := f.sess.Save(); err != nil {
		t.Fatal(err)
	}
	const exec = "/api/v1/namespaces/default/pods/api-0/exec?command=sh"

	c.set(until.Add(-time.Second))
	if code, body := f.do(t, http.MethodDelete, "/api/v1/namespaces/scratch", f.token); code != http.StatusOK {
		t.Fatalf("DELETE a second before the unlock ends = %d %s", code, body)
	}
	code, open, closeOpen := f.upgrade(t, http.MethodPost, exec, "SPDY/3.1")
	defer closeOpen()
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("exec a second before the unlock ends = %d", code)
	}

	for _, at := range []time.Time{until, until.Add(time.Millisecond), until.Add(time.Hour)} {
		c.set(at)
		if code, body := f.do(t, http.MethodDelete, "/api/v1/namespaces/scratch", f.token); code != http.StatusForbidden ||
			!strings.Contains(body, "tug unlock") {
			t.Fatalf("DELETE %s after the unlock ends = %d %s", at.Sub(until), code, body)
		}
		if code, _, closeConn := f.upgrade(t, http.MethodPost, exec, "SPDY/3.1"); code != http.StatusForbidden {
			closeConn()
			t.Fatalf("exec %s after the unlock ends = %d", at.Sub(until), code)
		}
	}
	echo(t, open, "still here") // the exec opened before the end
	if f.cluster.deletes.Load() != 1 {
		t.Fatalf("the cluster saw %d deletes, want 1", f.cluster.deletes.Load())
	}

	var expired []audit.Entry
	for _, e := range rec.recorded() {
		if e.Kind == audit.KindUnlockExpired {
			expired = append(expired, e)
		}
	}
	if len(expired) != 1 || !expired[0].Time.Equal(until) || !expired[0].Until.Equal(until) ||
		expired[0].Session != f.sess.State.ID || expired[0].Cluster != "prod" {
		t.Fatalf("unlock-expired entries = %+v, want one at %s", expired, until)
	}
	if s, err := f.srv.cfg.Sessions.Open(f.sess.State.ID); err != nil || !s.State.UnlockedUntil.IsZero() {
		t.Fatalf("session after its unlock ended = %+v, %v", s, err)
	}

	// With no request after it, tugd's tick notices the end.
	f.sess.State.UnlockedUntil = until.Add(2 * time.Hour)
	if err := f.sess.Save(); err != nil {
		t.Fatal(err)
	}
	f.srv.endExpiredUnlocks(context.Background())
	c.set(until.Add(3 * time.Hour))
	f.srv.endExpiredUnlocks(context.Background())
	f.srv.endExpiredUnlocks(context.Background())
	n := 0
	for _, e := range rec.recorded() {
		if e.Kind == audit.KindUnlockExpired {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("recorded %d unlock-expired entries, want 2", n)
	}
}

// rotatingProvider hands out a new token on every fetch, each expiring at
// its time.
type rotatingProvider struct {
	mu      sync.Mutex
	tokens  []string
	expires []time.Time
	fetches int
}

func (p *rotatingProvider) fetch(context.Context, string) (sdk.Credential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetches >= len(p.tokens) {
		return sdk.Credential{}, sdk.Errorf(sdk.Unauthenticated, "the provider has no more credentials").WithHint("sign in again")
	}
	exp := p.expires[p.fetches]
	c := sdk.Credential{Type: sdk.CredentialToken, Token: p.tokens[p.fetches], ExpiresAt: &exp}
	p.fetches++
	return c, nil
}

func (p *rotatingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches
}

// Through tugd, an expiring token is replaced mid-session; a token the
// cluster refuses, as when it is revoked, is explained to the client and
// replaced from the provider for the next request.
func TestProxyRefreshesCredentialsMidSession(t *testing.T) {
	// The broker's clock, which says when a stored credential has expired.
	// It runs an hour behind, so client-go, which caches tokens by the real
	// clock, asks the broker every time.
	c := &clock{t: time.Now().Add(-time.Hour)}
	t0 := c.now()
	p := &rotatingProvider{
		tokens:  []string{"token-1", "token-2", "token-3"},
		expires: []time.Time{t0.Add(10 * time.Minute), t0.Add(2 * time.Hour), t0.Add(2 * time.Hour)},
	}
	broker := &credentials.Broker{Store: credentials.NewMemoryStore(), Fetch: p.fetch, Now: c.now}
	var cluster *fakeCluster
	var builds, refreshes atomic.Int32
	tugdClock := &clock{t: time.Now()}
	f := setup(t, func(cfg *Config) {
		cfg.Now = tugdClock.now
		cfg.Upstream = func(ctx context.Context, clusterID string, refresh bool) (*rest.Config, error) {
			builds.Add(1)
			if refresh {
				refreshes.Add(1)
			}
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.srv.Certificate().Raw})
			cl := &catalog.Cluster{ID: clusterID, Name: "prod", Server: cluster.srv.URL, CAData: ca}
			return (&kube.Factory{Cluster: cl, Broker: broker, Refresh: refresh}).RESTConfig(ctx)
		}
	})
	cluster = f.cluster
	cs := f.clientset(t)
	list := func() error {
		_, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
		return err
	}

	cluster.valid.Store("token-1")
	if err := list(); err != nil {
		t.Fatalf("list with the first token: %v", err)
	}
	// The token expires; the next request takes a new one.
	cluster.valid.Store("token-2")
	c.set(t0.Add(10 * time.Minute))
	if err := list(); err != nil {
		t.Fatalf("list once the first token expired: %v", err)
	}
	if n, b := p.count(), builds.Load(); n != 2 || b != 1 {
		t.Fatalf("the provider was asked %d times (want 2) and the upstream built %d (want 1)", n, b)
	}

	// The cluster stops taking token-2 before it expires.
	cluster.valid.Store("token-3")
	err := list()
	if !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "prod refused tug's credential") ||
		!strings.Contains(err.Error(), "tug doctor") {
		t.Fatalf("list with a revoked token: %v; want Unauthorized saying what to do", err)
	}
	if err := list(); err != nil {
		t.Fatalf("list after a revoked token: %v; want a new one from the provider", err)
	}
	if n, r := p.count(), refreshes.Load(); n != 3 || r != 1 {
		t.Fatalf("the provider was asked %d times (want 3) and the upstream built with refresh %d (want 1)", n, r)
	}

	// A credential the provider gave moments ago is not asked for again
	// at once, when the cluster refuses it too.
	cluster.valid.Store("token-4")
	for range 3 {
		if err := list(); !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "moments ago") {
			t.Fatalf("list with a new token refused: %v", err)
		}
	}
	if n := p.count(); n != 3 {
		t.Fatalf("the provider was asked %d times, want no more than 3", n)
	}
	// Later it is, and when the provider has nothing better, its error and
	// hint reach the client, as an authentication failure.
	tugdClock.set(tugdClock.now().Add(refreshBackoff))
	if err := list(); !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "asks its provider for a new one") {
		t.Fatalf("list with token-3 refused later: %v", err)
	}
	err = list()
	if !apierrors.IsUnauthorized(err) || !strings.Contains(err.Error(), "no more credentials") || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("list when the provider fails: %v; want its error and hint", err)
	}
}

// clientset is a client-go client for the fixture's session, as kubectl
// would use it.
func (f *fixture) clientset(t *testing.T) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: f.proxy.URL + "/s/" + f.sess.State.ID, BearerToken: f.token})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestProxyTokenErrors(t *testing.T) {
	f := setup(t)
	unknown, err := f.srv.cfg.Sessions.Create(1, "")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, session, token, want string
	}{
		{"none", f.sess.State.ID, "", "no session token"},
		{"wrong", f.sess.State.ID, "nope", "wrong session token"},
		{"from an earlier tugd", unknown.State.ID, "old", "most likely because it restarted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, f.proxy.URL+"/s/"+tt.session+"/api/v1/namespaces", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var st metav1.Status
			_ = json.NewDecoder(resp.Body).Decode(&st)
			if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(st.Message, tt.want) {
				t.Fatalf("GET = %d %q, want 401 saying %q", resp.StatusCode, st.Message, tt.want)
			}
		})
	}
}

// runTugd runs a tugd with the fixture's config until the test ends and
// returns it once it listens, with a client that trusts its certificate.
func (f *fixture) runTugd(t *testing.T) (*Server, *http.Client, func()) {
	t.Helper()
	srv := New(f.cfg)
	done := make(chan error, 1)
	go func() { done <- srv.Run(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.ProxyURL() == "" {
		if time.Now().After(deadline) {
			t.Fatal("tugd did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(srv.ProxyCA())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		srv.Shutdown()
		if err := <-done; err != nil {
			t.Errorf("tugd: %v", err)
		}
		client.CloseIdleConnections()
	}
	t.Cleanup(stop)
	return srv, client, stop
}

// While tugd is down, a client of a proxied session fails: it has only
// tugd's address, never the cluster's. A tugd started again on the same
// address refuses the old token, which it did not issue, and forwards
// nothing until the client fetches a new one.
func TestRestartedTugdFailsClosed(t *testing.T) {
	f := setup(t)
	first, client, stop := f.runTugd(t)
	tok, err := first.Register(f.sess.State.ID)
	if err != nil {
		t.Fatal(err)
	}
	url := first.ProxyURL() + "/s/" + f.sess.State.ID + "/api/v1/namespaces"
	get := func(token string) (int, string, error) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	if code, body, err := get(tok); err != nil || code != http.StatusOK {
		t.Fatalf("GET through tugd = %d %s %v", code, body, err)
	}
	seen := func() string { return f.cluster.sawAuth.Load().(string) }
	f.cluster.sawAuth.Store("")

	stop()
	if _, _, err := get(tok); err == nil {
		t.Fatal("GET while tugd is down succeeded")
	}
	if seen() != "" {
		t.Fatal("a request reached the cluster while tugd was down")
	}

	second, _, _ := f.runTugd(t)
	if second.ProxyURL() != first.ProxyURL() {
		t.Fatalf("restarted on %s, want %s", second.ProxyURL(), first.ProxyURL())
	}
	code, body, err := get(tok)
	if err != nil || code != http.StatusUnauthorized || !strings.Contains(body, "restarted") {
		t.Fatalf("GET with the old token = %d %s %v; want 401 saying tugd restarted", code, body, err)
	}
	if seen() != "" {
		t.Fatal("the restarted tugd forwarded a request with a token it did not issue")
	}
	newTok, err := second.Register(f.sess.State.ID)
	if err != nil || newTok == tok {
		t.Fatalf("register after the restart = %q, %v; want a new token", newTok, err)
	}
	if code, body, err := get(newTok); err != nil || code != http.StatusOK {
		t.Fatalf("GET with the new token = %d %s %v", code, body, err)
	}
}

// Connections the cluster drops, as a load balancer does or a network that
// changes underneath, are opened again for the next request; a stream on
// one ends, so the client starts it again; and a cluster that cannot be
// reached is named in the error.
func TestProxyRecoversFromDroppedConnections(t *testing.T) {
	rec := &recorder{}
	f := setup(t, func(c *Config) { c.Audit = rec })
	if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	f.cluster.srv.CloseClientConnections()
	if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusOK {
		t.Fatalf("GET after the cluster dropped the connection = %d %s", code, body)
	}

	// A watch whose upstream connection drops ends, rather than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.proxy.URL+"/s/"+f.sess.State.ID+"/api/v1/namespaces?watch=true", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, err := r.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("watch: %q %v", line, err)
	}
	f.cluster.srv.CloseClientConnections()
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		ended <- err
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the watch kept going after its upstream connection dropped")
	}
	if code, line := f.stream(t, "/api/v1/namespaces?watch=true"); code != http.StatusOK || line != "first\n" {
		t.Fatalf("watching again = %d %q", code, line)
	}

	// The cluster goes away altogether.
	if err := f.sess.Unlock(time.Hour); err != nil {
		t.Fatal(err)
	}
	f.cluster.srv.Close()
	f.srv.dropUpstreams()
	code, body := f.do(t, http.MethodDelete, "/api/v1/namespaces/scratch", f.token)
	if code != http.StatusBadGateway || !strings.Contains(body, "cannot reach prod") {
		t.Fatalf("DELETE with the cluster gone = %d %s", code, body)
	}
	got := rec.recorded()
	if len(got) != 2 || got[0].Kind != audit.KindWrite || got[1].Kind != audit.KindResult || got[1].Status != http.StatusBadGateway {
		t.Fatalf("recorded %+v, want the write and tugd's 502", got)
	}
}

// relay is a TCP relay to a cluster whose connections can be made to stop
// carrying anything while staying open, as a connection does when the
// machine sleeps or its network changes and nothing tells either end.
type relay struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	stuck  []*atomic.Bool
	conns  []net.Conn
	opened atomic.Int32
}

func newRelay(t *testing.T, target string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln, target: target}
	var conns sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		for _, c := range r.conns {
			c.Close()
		}
		r.mu.Unlock()
		conns.Wait()
	})
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			r.opened.Add(1)
			stuck := &atomic.Bool{}
			r.mu.Lock()
			r.stuck = append(r.stuck, stuck)
			r.conns = append(r.conns, in, out)
			r.mu.Unlock()
			conns.Add(2)
			pipe := func(dst, src net.Conn) {
				defer conns.Done()
				defer dst.Close()
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !stuck.Load() {
						if _, err := dst.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}
			go pipe(out, in)
			go pipe(in, out)
		}
	}()
	return r
}

// stall makes every connection open now stop carrying anything.
func (r *relay) stall() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.stuck {
		s.Store(true)
	}
}

// After the machine wakes, tugd stops reusing connections to clusters made
// before it slept, which may look open and never answer.
func TestProxyDropsIdleConnectionsOnWake(t *testing.T) {
	var r *relay
	f := setup(t, func(c *Config) {
		upstream := c.Upstream
		c.Upstream = func(ctx context.Context, id string, refresh bool) (*rest.Config, error) {
			cfg, err := upstream(ctx, id, refresh)
			if err != nil {
				return nil, err
			}
			cfg.Host = "https://" + r.ln.Addr().String()
			return cfg, nil
		}
	})
	r = newRelay(t, strings.TrimPrefix(f.cluster.srv.URL, "https://"))
	if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	r.stall()
	f.srv.dropUpstreams() // what Run does when it notices a wake

	done := make(chan int, 1)
	go func() {
		code, _ := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token)
		done <- code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("GET after waking = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GET after waking hung on a connection made before the machine slept")
	}
	if n := r.opened.Load(); n != 2 {
		t.Fatalf("opened %d connections to the cluster, want a new one after waking", n)
	}
}

func TestSleptFor(t *testing.T) {
	tests := []struct {
		wall, monotonic time.Duration
		want            bool
	}{
		{30 * time.Second, 30 * time.Second, false},
		{31 * time.Second, 30 * time.Second, false}, // a clock adjusted a little
		{2 * time.Hour, 30 * time.Second, true},     // asleep for most of two hours
		{20 * time.Second, 30 * time.Second, false}, // a clock set back
	}
	for _, tt := range tests {
		if got := sleptFor(tt.wall, tt.monotonic); got != tt.want {
			t.Errorf("sleptFor(%s, %s) = %v, want %v", tt.wall, tt.monotonic, got, tt.want)
		}
	}
	prev := time.Now()
	if slept(prev, time.Now()) {
		t.Error("slept between two readings in a row")
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("kubectl delete\x1b[2J\n", 100); got != "kubectl delete[2J" {
		t.Errorf("printable = %q", got)
	}
	if got := printable(strings.Repeat("a", 300), 200); len(got) != 200 {
		t.Errorf("printable kept %d bytes, want 200", len(got))
	}
}

// A provider's hint for a credential it cannot give reaches the client.
func TestProxyPassesOnProviderHints(t *testing.T) {
	var buildErr atomic.Pointer[error]
	fail := func(err error) { buildErr.Store(&err) }
	f := setup(t, func(c *Config) {
		c.Upstream = func(context.Context, string, bool) (*rest.Config, error) {
			return nil, *buildErr.Load()
		}
	})
	fail(fmt.Errorf("getting credentials for prod: %w",
		sdk.Errorf(sdk.Unauthenticated, "not signed in to https://idp").WithHint("run `tug auth login prod` to sign in")))
	code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token)
	if code != http.StatusUnauthorized || !strings.Contains(body, "not signed in") || !strings.Contains(body, "tug auth login prod") {
		t.Fatalf("GET when the provider needs a sign-in = %d %s", code, body)
	}
	fail(errors.New("catalog: no such cluster"))
	if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusBadGateway || !strings.Contains(body, "cannot forward to prod") {
		t.Fatalf("GET with a broken catalog = %d %s", code, body)
	}
}
