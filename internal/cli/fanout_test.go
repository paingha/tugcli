package cli

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/tunnel/tunneltest"
)

// podsAPIServer serves a list with one pod, to callers presenting
// secretToken, and counts every request it gets.
func podsAPIServer(t *testing.T, pod string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/") && strings.HasSuffix(r.URL.Path, "/pods") {
			fmt.Fprintf(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":%q}}]}`, pod)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// writeFleetKubeconfig writes a kubeconfig with a context per cluster name,
// each on its own server, all with secretToken.
func writeFleetKubeconfig(t *testing.T, servers map[string]*httptest.Server) string {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: secretToken}
	for name, srv := range servers {
		ca := srv.Certificate()
		cfg.Clusters[name] = &clientcmdapi.Cluster{Server: srv.URL, CertificateAuthorityData: pemCert(ca.Raw)}
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: "u"}
		cfg.CurrentContext = name
	}
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertNoSessions fails when a temporary session was left behind.
func assertNoSessions(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "run", "sessions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary sessions left behind: %v", entries)
	}
}

func TestGetPodsAcrossClusters(t *testing.T) {
	home := isolate(t)
	useFakeKubectl(t)
	eu, euHits := podsAPIServer(t, "pod-eu")
	us, usHits := podsAPIServer(t, "pod-us")
	prod, prodHits := podsAPIServer(t, "pod-prod")
	gone, _ := podsAPIServer(t, "pod-ap")
	gone.Close() // shop-staging-ap is unreachable
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{
		"shop-staging-eu": eu, "shop-staging-us": us, "shop-staging-ap": gone, "shop-prod": prod,
	}))
	// shop-staging-us is reached only through a jump host.
	spec, jump := tunneltest.SSH(t, true)
	mustTug(t, "tunnel", "set", "shop-staging-us", spec)

	r := tug(t, "get", "pods", "--cluster-selector", "env=staging", "--parallel=2")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1 for the unreachable cluster\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"shop-staging-eu | NAME", "shop-staging-eu | pod-eu", "shop-staging-us | NAME", "shop-staging-us | pod-us"} {
		if !strings.Contains(r.stdout, want+"\n") {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "shop-prod") || strings.Contains(r.stdout, "shop-staging-ap") {
		t.Errorf("stdout has a cluster it should not:\n%s", r.stdout)
	}
	for _, want := range []string{"shop-staging-ap | error: ", "Failed on 1 of 3 clusters:\n  shop-staging-ap  kubectl exited with 1\n"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if euHits.Load() == 0 || usHits.Load() == 0 {
		t.Errorf("requests: eu %d, us %d; want both clusters asked", euHits.Load(), usHits.Load())
	}
	if prodHits.Load() != 0 {
		t.Errorf("shop-prod, which env=staging does not select, got %d requests", prodHits.Load())
	}
	if api := strings.TrimPrefix(us.URL, "https://"); !slices.Contains(jump.Dials(), api) {
		t.Errorf("the jump host opened %v; want %s, through the tunnel pool's proxy", jump.Dials(), api)
	}
	assertNoSessions(t, home)

	// A cluster must match every selector; when every one answers, tug
	// exits 0.
	mustTug(t, "label", "shop-staging-eu", "team=shop")
	mustTug(t, "label", "shop-staging-us", "team=shop")
	out := mustTug(t, "get", "pods", "--cluster-selector", "env=staging", "--cluster-selector", "team=shop")
	if !strings.Contains(out, "shop-staging-eu | pod-eu") || !strings.Contains(out, "shop-staging-us | pod-us") || strings.Contains(out, "shop-staging-ap") {
		t.Errorf("get pods on env=staging, team=shop:\n%s", out)
	}
	if r := tug(t, "get", "pods", "--cluster-selector", "env=nope"); r.code == 0 || !strings.Contains(r.stderr, "no cluster has labels env=nope") {
		t.Fatalf("selector matching nothing: %+v", r)
	}
}

func TestAcrossClustersRunsOnlyReads(t *testing.T) {
	home := isolate(t)
	useFakeKubectl(t)
	eu, hits := podsAPIServer(t, "pod-eu")
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{"shop-staging-eu": eu}))

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"delete", "pod", "pod-eu", "--cluster-selector", "env=staging"}, "refused: `kubectl delete pod pod-eu` may write, and --cluster-selector runs only commands that read"},
		{[]string{"--cluster-selector=env=staging", "exec", "pod-eu", "--", "rm", "-rf", "/"}, "refused: `kubectl exec pod-eu -- rm -rf /` may write"},
		{[]string{"-n", "shop", "port-forward", "pod-eu", "8080", "--cluster-selector", "env=staging"}, "refused: `kubectl port-forward` listens or attaches on this machine"},
		{[]string{"restart", "api", "--cluster-selector", "env=staging"}, "tug restart does not run across clusters; --cluster-selector works with kubectl commands that read and with tug check, tug gitops, tug logs, tug top, tug tree, tug watch and tug why"},
		{[]string{"gitops", "--cluster-selector", "env=staging"}, "tug gitops does not run across clusters, but its commands dependencies, drift, explain and status do"},
		{[]string{"ls", "--cluster-selector", "env=staging"}, "tug ls does not run across clusters"},
		{[]string{"--cluster-selector", "env=staging"}, "name a command to run on each cluster"},
		{[]string{"get", "pods", "--cluster-selector", "staging"}, "--cluster-selector wants label=value, got \"staging\""},
		{[]string{"get", "pods", "--cluster-selector", "env=staging", "--cluster-selector", "env=prod"}, "asks for env=staging and env=prod"},
		{[]string{"get", "pods", "--cluster-selector", "env=staging", "--parallel", "0"}, "--parallel wants a number from 1, got \"0\""},
		{[]string{"get", "pods", "--cluster-selector"}, "--cluster-selector needs a value"},
		{[]string{"why", "api", "--parallel", "2"}, "--parallel works only with --cluster-selector"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			r := tug(t, tt.args...)
			if r.code == 0 || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("tug %s = exit %d, stderr %q; want an error with %q", strings.Join(tt.args, " "), r.code, r.stderr, tt.want)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("the cluster got %d requests; nothing should have run", hits.Load())
	}
	assertNoSessions(t, home)
}

func TestWhyAcrossClusters(t *testing.T) {
	home := isolate(t)
	eu, euWrites := fakeAPIServer(t)
	us, usWrites := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{"shop-staging-eu": eu, "shop-staging-us": us}))

	// No session is needed: each run has its own.
	out := mustTug(t, "why", "checkout", "-n", "shop", "--cluster-selector", "env=staging")
	for _, name := range []string{"shop-staging-eu", "shop-staging-us"} {
		if !strings.Contains(out, name+" | deploy/checkout in shop is healthy") {
			t.Errorf("why output lacks %s:\n%s", name, out)
		}
	}

	// A target that is not there fails on every cluster, each saying why.
	r := tug(t, "tree", "nothing-like-this", "-n", "shop", "--cluster-selector", "env=staging")
	if r.code != 1 || !strings.Contains(r.stderr, "Failed on 2 of 2 clusters:") ||
		!strings.Contains(r.stderr, `  shop-staging-us  nothing in namespace shop matches "nothing-like-this"`) {
		t.Fatalf("tree of a missing target: %+v", r)
	}
	if euWrites.Load() != 0 || usWrites.Load() != 0 {
		t.Fatal("a read across clusters wrote")
	}
	assertNoSessions(t, home)
}

// syncWriter is a bytes.Buffer that a test can read while tug writes.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncWriter) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Ctrl-C ends a watch on every cluster, as it ends any stream: tug stops
// each kubectl and exits 0.
func TestAcrossClustersStopsOnCancel(t *testing.T) {
	home := isolate(t)
	useFakeKubectl(t)
	eu, _ := podsAPIServer(t, "pod-eu")
	us, _ := podsAPIServer(t, "pod-us")
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{"shop-staging-eu": eu, "shop-staging-us": us}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncWriter
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"get", "pods", "-w", "--cluster-selector", "env=staging"}, strings.NewReader(""), &stdout, &stderr)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(stdout.String(), "shop-staging-eu | pod-eu") || !strings.Contains(stdout.String(), "shop-staging-us | pod-us") {
		if time.Now().After(deadline) {
			t.Fatalf("both watches did not start\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d after Ctrl-C, want 0\nstderr: %s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tug did not stop the watches after its context was cancelled")
	}
	assertNoSessions(t, home)
}

func TestSplitFanoutFlags(t *testing.T) {
	tests := []struct {
		args     string
		rest     string
		selector string
		parallel int
	}{
		{"get pods -A", "get pods -A", "", 0},
		{"get pods --parallel 3", "get pods --parallel 3", "", 0},
		{"get pods --cluster-selector env=staging", "get pods", "env=staging", 8},
		{"--cluster-selector=env=staging --parallel=3 -n web get pods", "-n web get pods", "env=staging", 3},
		{"get pods --cluster-selector env=staging --cluster-selector team=a,b", "get pods", "env=staging, team=a,b", 8},
		{"exec api -- tug --cluster-selector env=x", "exec api -- tug --cluster-selector env=x", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			o, rest, err := splitFanoutFlags(strings.Fields(tt.args))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(rest, " "); got != tt.rest {
				t.Errorf("rest = %q, want %q", got, tt.rest)
			}
			switch {
			case tt.selector == "" && o != nil:
				t.Errorf("options = %+v, want none", o)
			case tt.selector != "" && (o == nil || selectorString(o.selector) != tt.selector || o.parallel != tt.parallel):
				t.Errorf("options = %+v, want selector %s, parallel %d", o, tt.selector, tt.parallel)
			}
		})
	}
}
