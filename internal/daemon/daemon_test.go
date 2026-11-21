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
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/session"
)

const upstreamToken = "cluster-credential"

// fakeCluster is an API server that accepts only the cluster's credential.
type fakeCluster struct {
	srv     *httptest.Server
	deletes atomic.Int32
	sawAuth atomic.Value // last Authorization header
	// valid is the token it accepts, upstreamToken unless set.
	valid atomic.Value
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	f := &fakeCluster{}
	f.valid.Store(upstreamToken)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+f.valid.Load().(string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
			return
		}
		last := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces" && q.Get("watch") == "":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"kind":"NamespaceList","apiVersion":"v1","items":[{"metadata":{"name":"default"}}]}`)
		case r.Method == http.MethodDelete:
			f.deletes.Add(1)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		case r.Header.Get("Upgrade") != "" && (last == "exec" || last == "attach" || last == "portforward" ||
			strings.Contains(r.URL.Path, "/proxy/exec/")):
			// An upgraded connection, of the kind asked for, that echoes
			// every line it is sent.
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", r.Header.Get("Upgrade"))
			buf.Flush()
			for {
				line, err := buf.ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprint(buf, "echo: "+line)
				buf.Flush()
			}
		case r.URL.Path == "/stream", q.Get("watch") == "true", q.Get("follow") == "true":
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
	cfg     Config
	runtime string
	sess    *session.Session
	srv     *Server
	cluster *fakeCluster
	proxy   *httptest.Server
	token   string

	upstreamCalls atomic.Int32
	refreshes     atomic.Int32  // upstreams built with refresh
	upstreamDelay time.Duration // how long building an upstream takes
}

// setup connects a read-only session to a fake cluster and registers it.
// opts change tugd's config before it starts.
func setup(t *testing.T, opts ...func(*Config)) *fixture {
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
	cfg := Config{
		RuntimeDir: f.runtime,
		Sessions:   mgr,
		Upstream: func(_ context.Context, _ string, refresh bool) (*rest.Config, error) {
			f.upstreamCalls.Add(1)
			if refresh {
				f.refreshes.Add(1)
			}
			time.Sleep(f.upstreamDelay)
			return &rest.Config{Host: f.cluster.srv.URL, BearerToken: upstreamToken, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, nil
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	f.cfg = cfg
	f.srv = New(cfg)
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

// recorder keeps the audit entries it is given, or fails with err.
type recorder struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (r *recorder) Record(_ context.Context, e audit.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

func (r *recorder) recorded() []audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.entries)
}

func TestProxyRecordsTheWritesItForwards(t *testing.T) {
	rec := &recorder{}
	now := time.Now().UTC().Round(0)
	f := setup(t, func(c *Config) {
		c.Audit = rec
		c.Now = func() time.Time { return now }
	})
	base := audit.Entry{
		Time: now, Source: audit.SourceProxy, Cluster: "prod", ClusterID: "kubeconfig/x/prod", Session: f.sess.State.ID,
		Method: http.MethodDelete, Path: "/api/v1/namespaces/payments", Client: "Helm/3.16.2", Intent: "kubectl delete",
	}
	del := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, f.proxy.URL+"/s/"+f.sess.State.ID+"/api/v1/namespaces/payments?propagationPolicy=Foreground", nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("User-Agent", "Helm/3.16.2")
		req.Header.Set("Kubectl-Command", "kubectl delete")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A read is not recorded; a refused write is, as refused.
	f.do(t, http.MethodGet, "/api/v1/namespaces", f.token)
	if code := del(); code != http.StatusForbidden {
		t.Fatalf("DELETE on a read-only session = %d", code)
	}
	refused := base
	refused.Kind, refused.Status, refused.Reason = audit.KindRefused, http.StatusForbidden, audit.ReasonReadOnly
	if got := rec.recorded(); len(got) != 1 || !reflect.DeepEqual(got[0], refused) {
		t.Fatalf("recorded %+v, want only %+v", got, refused)
	}

	// An allowed write is recorded before it is sent, then its outcome.
	if err := f.sess.Unlock(time.Hour); err != nil {
		t.Fatal(err)
	}
	if code := del(); code != http.StatusOK {
		t.Fatalf("DELETE after unlock = %d", code)
	}
	got := rec.recorded()
	if len(got) != 3 {
		t.Fatalf("recorded %+v, want the refusal, the write and its result", got)
	}
	write, result := base, base
	write.Kind, write.Allowed, write.ID = audit.KindWrite, audit.AllowedUnlocked, got[1].ID
	result.Kind, result.Status, result.ID = audit.KindResult, http.StatusOK, got[1].ID
	if write.ID == "" || !reflect.DeepEqual(got[1], write) || !reflect.DeepEqual(got[2], result) {
		t.Fatalf("recorded %+v and %+v; want %+v and %+v", got[1], got[2], write, result)
	}

	// A write that cannot be recorded is not forwarded.
	rec.mu.Lock()
	rec.err = errors.New("no space left on device")
	rec.mu.Unlock()
	code, body := f.do(t, http.MethodDelete, "/api/v1/namespaces/payments", f.token)
	if code != http.StatusInternalServerError || !strings.Contains(body, "could not be recorded in the audit log") {
		t.Fatalf("DELETE with a failing audit log = %d %s", code, body)
	}
	if f.cluster.deletes.Load() != 1 {
		t.Fatalf("the cluster saw %d deletes, want only the recorded one", f.cluster.deletes.Load())
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
	// A deadline, so a second tugd that wrongly starts returns nil and fails
	// the test instead of serving forever.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSecond()
	if err := second.Run(secondCtx); !errors.Is(err, ErrAlreadyRunning) {
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

	// The next tugd listens where this one did, so session kubeconfigs keep
	// working after tug daemon stop or an idle exit.
	restarted := New(Config{RuntimeDir: f.runtime, Sessions: session.NewManager(f.runtime)})
	go func() { done <- restarted.Run(ctx) }()
	deadline = time.Now().Add(5 * time.Second)
	for restarted.ProxyURL() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the restarted tugd did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := restarted.ProxyURL(); got != st.ProxyURL {
		t.Fatalf("restarted on %s, want the previous %s", got, st.ProxyURL)
	}
	restarted.Shutdown()
	if err := <-done; err != nil {
		t.Fatal(err)
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
	dir := filepath.Join(string(filepath.Separator)+"run", "user", "1000", "tug")
	if got, want := SocketPath(dir), filepath.Join(dir, "tugd.sock"); got != want {
		t.Fatalf("short runtime dir: %s, want %s", got, want)
	}
	longDir := filepath.Join(string(filepath.Separator)+"home", strings.Repeat("very-long-directory", 6))
	long := SocketPath(longDir)
	if len(long) >= 104 || !strings.HasPrefix(long, os.TempDir()) {
		t.Fatalf("long runtime dir gave %s", long)
	}
	// In a directory of its own, which tugd keeps private, never loose in
	// the shared temp directory.
	if filepath.Dir(long) == filepath.Clean(os.TempDir()) {
		t.Fatalf("the fallback socket %s is not in a directory of its own", long)
	}
	if long != SocketPath(longDir) {
		t.Fatal("the fallback path must be stable")
	}
}

func TestRunRefusesDirectoriesOthersCanChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	// Short directories, so the socket goes in the runtime directory itself.
	short := func() string {
		dir, err := os.MkdirTemp("", "tugd-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	shared := short()
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(short(), "link")
	if err := os.Symlink(short(), link); err != nil {
		t.Fatal(err)
	}
	// A long runtime directory whose fallback socket directory, in the shared
	// temp directory, someone else made first.
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 80))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	taken := filepath.Dir(SocketPath(long))
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(taken) })
	if err := os.Chmod(taken, 0o777); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"writable by others": shared, "a link": link, "behind a fallback directory made by someone else": long} {
		if len(filepath.Join(dir, "tugd.sock")) >= 100 && dir != long {
			t.Fatalf("%s is too long for this test", dir)
		}
		srv := New(Config{RuntimeDir: dir, Sessions: session.NewManager(dir)})
		// A deadline, so a tugd that wrongly starts returns nil and fails the
		// test instead of serving forever.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := srv.Run(ctx)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "refusing to use") {
			t.Errorf("tugd in a directory that is %s: %v", name, err)
		}
		// Clients refuse to trust whatever answers there, too.
		if _, err := NewClient(dir).Status(context.Background()); err == nil || errors.Is(err, ErrNotRunning) {
			t.Errorf("client of a directory that is %s: %v", name, err)
		}
	}
}

func TestIdleForgetsSessionsNotUsingTheProxy(t *testing.T) {
	f := setup(t)
	tokens := func() int {
		f.srv.mu.Lock()
		defer f.srv.mu.Unlock()
		return len(f.srv.tokens)
	}
	f.srv.idle()
	if tokens() != 1 {
		t.Fatal("forgot a session that uses the proxy")
	}

	// Reconnected without --proxy: the old token no longer works, and tugd
	// forgets it, so it can stop when idle.
	cl := &catalog.Cluster{ID: "kubeconfig/x/prod", Name: "prod", Server: f.cluster.srv.URL}
	if err := f.sess.Connect(cl, "default", true, session.AuthCommand{Command: "tug"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusGone {
		t.Fatalf("a session not using the proxy = %d, want 410", code)
	}
	f.srv.idle()
	if tokens() != 0 {
		t.Fatal("kept the token of a session that no longer uses the proxy")
	}

	// Disconnected.
	tok, err := f.srv.Register(f.sess.State.ID)
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if err := f.sess.Disconnect(); err != nil {
		t.Fatal(err)
	}
	f.srv.idle()
	if tokens() != 0 {
		t.Fatal("kept the token of a disconnected session")
	}
}

func TestConcurrentRequestsBuildOneUpstream(t *testing.T) {
	f := setup(t)
	f.upstreamDelay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := f.do(t, http.MethodGet, "/api/v1/namespaces", f.token); code != http.StatusOK {
				t.Errorf("GET = %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if n := f.upstreamCalls.Load(); n != 1 {
		t.Fatalf("built the upstream %d times, want once", n)
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
