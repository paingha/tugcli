package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/session"
)

const upstreamToken = "cluster-credential"

// fakeCluster is an API server that accepts only the cluster's credential.
type fakeCluster struct {
	srv     *httptest.Server
	deletes atomic.Int32
	sawAuth atomic.Value // last Authorization header
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	f := &fakeCluster{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+upstreamToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","apiVersion":"v1","items":[{"metadata":{"name":"default"}}]}`)
		case r.Method == http.MethodDelete:
			f.deletes.Add(1)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			// An upgraded connection that echoes what it is sent.
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprint(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
			buf.Flush()
			line, _ := buf.ReadString('\n')
			fmt.Fprint(buf, "echo: "+line)
			buf.Flush()
		case r.URL.Path == "/stream":
			fmt.Fprint(w, "first\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type fixture struct {
	runtime string
	sess    *session.Session
	srv     *Server
	cluster *fakeCluster
	proxy   *httptest.Server
	token   string
}

// setup connects a read-only session to a fake cluster and registers it.
func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{runtime: t.TempDir(), cluster: newFakeCluster(t)}
	mgr := session.NewManager(f.runtime)
	s, err := mgr.Create(os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	cl := &catalog.Cluster{ID: "kubeconfig/x/prod", Name: "prod", Server: f.cluster.srv.URL}
	if err := s.Connect(cl, "default", true, session.AuthCommand{Command: "tug", ProxyURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	f.sess = s
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.cluster.srv.Certificate().Raw})
	f.srv = New(Config{
		RuntimeDir: f.runtime,
		Sessions:   mgr,
		Upstream: func(context.Context, string) (*rest.Config, error) {
			return &rest.Config{Host: f.cluster.srv.URL, BearerToken: upstreamToken, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, nil
		},
	})
	if f.token, err = f.srv.Register(s.State.ID); err != nil {
		t.Fatal(err)
	}
	f.proxy = httptest.NewServer(f.srv.ProxyHandler())
	t.Cleanup(f.proxy.Close)
	return f
}

func (f *fixture) do(t *testing.T, method, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.proxy.URL+"/s/"+f.sess.State.ID+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestProxyForwardsWithTheClusterCredential(t *testing.T) {
	f := setup(t)
	code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token)
	if code != http.StatusOK || !strings.Contains(body, "NamespaceList") {
		t.Fatalf("GET through the proxy = %d %s", code, body)
	}
	if got := f.cluster.sawAuth.Load(); got != "Bearer "+upstreamToken {
		t.Fatalf("upstream saw Authorization %q; the session token must not be forwarded", got)
	}
}

func TestProxyNeedsTheSessionToken(t *testing.T) {
	f := setup(t)
	other, err := session.NewManager(f.runtime).Create(os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _ := f.srv.Register(other.State.ID)
	for name, tok := range map[string]string{"none": "", "wrong": "nope", "another session's": otherToken} {
		code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", tok)
		if code != http.StatusUnauthorized || !strings.Contains(body, `"kind":"Status"`) {
			t.Errorf("%s token: %d %s", name, code, body)
		}
	}
}

func TestProxyGuardsWritesForEveryClient(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/api/v1/namespaces/payments", "/api/v1/namespaces/default/pods/p/exec"} {
		method := http.MethodDelete
		if strings.HasSuffix(path, "/exec") {
			method = http.MethodPost
		}
		code, body := f.do(t, method, path, f.token)
		var st struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(body), &st)
		if code != http.StatusForbidden || st.Reason != "Forbidden" || !strings.Contains(st.Message, "read-only") || !strings.Contains(st.Message, "tug unlock") {
			t.Fatalf("%s %s on a read-only session = %d %s", method, path, code, body)
		}
	}
	if f.cluster.deletes.Load() != 0 {
		t.Fatal("a refused write reached the cluster")
	}

	// tug unlock takes effect on the next request.
	if err := f.sess.Unlock(time.Minute); err != nil {
		t.Fatal(err)
	}
	if code, body := f.do(t, http.MethodDelete, "/api/v1/namespaces/payments", f.token); code != http.StatusOK {
		t.Fatalf("DELETE after unlock = %d %s", code, body)
	}
	if f.cluster.deletes.Load() != 1 {
		t.Fatal("the unlocked write did not reach the cluster")
	}
}

func TestProxyCarriesUpgradedConnections(t *testing.T) {
	f := setup(t)
	if err := f.sess.Unlock(time.Minute); err != nil { // exec is a write
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "POST /s/%s/api/v1/namespaces/default/pods/p/exec HTTP/1.1\r\nHost: tugd\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\nContent-Length: 0\r\n\r\n", f.sess.State.ID, f.token)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %s", resp.Status)
	}
	fmt.Fprint(conn, "ping\n")
	line, err := r.ReadString('\n')
	if err != nil || line != "echo: ping\n" {
		t.Fatalf("over the upgraded connection: %q %v", line, err)
	}
}

func TestProxyStreamsWithoutBuffering(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest(http.MethodGet, f.proxy.URL+"/s/"+f.sess.State.ID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 1)
	go func(r io.Reader) {
		line, _ := bufio.NewReader(r).ReadString('\n')
		got <- line
	}(resp.Body)
	select {
	case line := <-got:
		if line != "first\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first line was held back until the stream ended")
	}
}

func TestProxyRefusesEndedSessions(t *testing.T) {
	f := setup(t)
	if err := session.NewManager(f.runtime).Remove(f.sess.State.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusGone {
		t.Fatalf("ended session = %d, want 410", code)
	}
}

func TestRunServesTheControlAPI(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.srv.Run(ctx) }()

	c := NewClient(f.runtime)
	var st Status
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if st, err = c.Status(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tugd did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.PID != os.Getpid() || !strings.HasPrefix(st.ProxyURL, "https://127.0.0.1:") {
		t.Fatalf("status = %+v", st)
	}
	second := New(Config{RuntimeDir: f.runtime, Sessions: session.NewManager(f.runtime)})
	if err := second.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("a second tugd: %v, want ErrAlreadyRunning", err)
	}
	reg, err := c.Register(context.Background(), f.sess.State.ID)
	if err != nil || reg.Token != f.token || reg.ProxyURL != st.ProxyURL || !strings.Contains(string(reg.ProxyCA), "BEGIN CERTIFICATE") {
		t.Fatalf("register = %+v, %v; want the existing token", reg, err)
	}
	if _, err := c.Register(context.Background(), "nosuchsession"); err == nil {
		t.Fatal("registered a session that does not exist")
	}
	if _, err := os.Stat(stateFile(f.runtime)); err != nil {
		t.Fatalf("state file: %v", err)
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tugd did not stop")
	}
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("status after shutdown: %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(stateFile(f.runtime)); !os.IsNotExist(err) {
		t.Fatal("state file left behind")
	}
}

func TestRunStopsWhenIdle(t *testing.T) {
	runtime := t.TempDir()
	srv := New(Config{RuntimeDir: runtime, Sessions: session.NewManager(runtime), IdleTimeout: 60 * time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- srv.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle tugd kept running")
	}
}

func TestSocketPathStaysShort(t *testing.T) {
	short := SocketPath("/run/user/1000/tug")
	if short != "/run/user/1000/tug/tugd.sock" {
		t.Fatalf("short runtime dir: %s", short)
	}
	long := SocketPath("/" + strings.Repeat("very-long-directory/", 8))
	if len(long) >= 104 || !strings.HasPrefix(long, os.TempDir()) {
		t.Fatalf("long runtime dir gave %s", long)
	}
	if long != SocketPath("/"+strings.Repeat("very-long-directory/", 8)) {
		t.Fatal("the fallback path must be stable")
	}
}

func TestSplitSession(t *testing.T) {
	tests := []struct {
		in, id, rest string
		ok           bool
	}{
		{"/s/abc/api/v1/pods", "abc", "/api/v1/pods", true},
		{"/s/abc", "abc", "/", true},
		{"/s//api", "", "", false},
		{"/api/v1", "", "", false},
	}
	for _, tt := range tests {
		id, rest, ok := splitSession(tt.in)
		if id != tt.id || rest != tt.rest || ok != tt.ok {
			t.Errorf("splitSession(%q) = %q, %q, %v", tt.in, id, rest, ok)
		}
	}
}
