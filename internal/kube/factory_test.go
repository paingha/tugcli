package kube

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/pkg/provider"
)

const testToken = "s3cr3t-bearer-token"

// fakeAPI serves just enough of the Kubernetes API for the factory's clients
// and records every request. Callers must present testToken.
type fakeAPI struct {
	*httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD /path"
	auth     []string // Authorization headers, in request order
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch route := r.Method + " " + r.URL.Path; route {
	case "GET /api":
		fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`)
	case "GET /apis":
		fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
	case "GET /api/v1":
		fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"namespaces","namespaced":false,"kind":"Namespace","verbs":["get","list","delete"]}]}`)
	case "GET /api/v1/namespaces":
		fmt.Fprint(w, `{"kind":"NamespaceList","apiVersion":"v1","items":[{"metadata":{"name":"default"}},{"metadata":{"name":"payments"}}]}`)
	case "DELETE /api/v1/namespaces/payments":
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`)
	}
}

// count returns how many requests matched "METHOD /path".
func (f *fakeAPI) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == route {
			n++
		}
	}
	return n
}

func (f *fakeAPI) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

// clusterFor returns a catalog cluster that trusts srv's certificate.
func clusterFor(srv *httptest.Server, id string) *catalog.Cluster {
	return &catalog.Cluster{
		ID:     id,
		Name:   "payments-prod",
		Server: srv.URL,
		CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}),
	}
}

// newFactory returns a factory for cl whose broker holds cred.
func newFactory(t *testing.T, cl *catalog.Cluster, cred provider.Credential) *Factory {
	t.Helper()
	broker := &credentials.Broker{Store: credentials.NewMemoryStore()}
	if err := broker.Put(cl.ID, cred); err != nil {
		t.Fatal(err)
	}
	return &Factory{Cluster: cl, Broker: broker, CacheDir: t.TempDir(), UserAgent: "tug/test"}
}

func TestRESTConfigCopiesCluster(t *testing.T) {
	cl := &catalog.Cluster{
		ID:                    "fake/work/c-1",
		Name:                  "c-1",
		Server:                "https://k8s.example:6443",
		CAData:                []byte("CA"),
		TLSServerName:         "k8s.internal",
		InsecureSkipTLSVerify: true,
	}
	f := newFactory(t, cl, provider.Token(testToken, provider.NoExpiry))
	cfg, err := f.RESTConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != cl.Server || cfg.UserAgent != "tug/test" {
		t.Errorf("Host, UserAgent = %q, %q; want %q, %q", cfg.Host, cfg.UserAgent, cl.Server, "tug/test")
	}
	if string(cfg.CAData) != "CA" || cfg.ServerName != "k8s.internal" || !cfg.Insecure {
		t.Errorf("TLSClientConfig = %+v, want the cluster's CA, server name and insecure flag", cfg.TLSClientConfig)
	}
	if cfg.Proxy != nil {
		t.Error("Proxy set for a cluster without a proxy URL")
	}
	if cfg.QPS != 50 || cfg.Burst != 100 {
		t.Errorf("QPS, Burst = %v, %d; want 50, 100", cfg.QPS, cfg.Burst)
	}
}

func TestTokenReachesServer(t *testing.T) {
	srv := newFakeAPI(t)
	f := newFactory(t, clusterFor(srv.Server, "fake/work/c-1"), provider.Token(testToken, time.Now().Add(time.Hour)))

	cs, err := f.Clientset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	list, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[1].Name != "payments" {
		t.Fatalf("namespaces = %+v, want default and payments", list.Items)
	}
	if got := srv.authHeaders(); len(got) != 1 || got[0] != "Bearer "+testToken {
		t.Fatalf("Authorization headers = %q, want [%q]", got, "Bearer "+testToken)
	}
}

// TestConcurrentRequestsShareTheToken makes requests at once, as discovery
// and tree do; under -race it fails if the token source is not safe for
// that.
func TestConcurrentRequestsShareTheToken(t *testing.T) {
	srv := newFakeAPI(t)
	f := newFactory(t, clusterFor(srv.Server, "fake/work/c-1"), provider.Token(testToken, time.Now().Add(time.Hour)))
	cs, err := f.Clientset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent List() = %v", err)
		}
	}
	for _, h := range srv.authHeaders() {
		if h != "Bearer "+testToken {
			t.Errorf("Authorization header = %q, want %q", h, "Bearer "+testToken)
		}
	}
}

func TestClientCertCredential(t *testing.T) {
	cl := &catalog.Cluster{ID: "fake/work/c-1", Name: "c-1", Server: "https://k8s.example"}
	f := newFactory(t, cl, provider.ClientCert([]byte("CERT PEM"), []byte("KEY PEM"), provider.NoExpiry))
	cfg, err := f.RESTConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.CertData) != "CERT PEM" || string(cfg.KeyData) != "KEY PEM" {
		t.Fatalf("CertData, KeyData = %q, %q; want the stored certificate and key", cfg.CertData, cfg.KeyData)
	}
	if cfg.ExecProvider != nil || cfg.WrapTransport != nil {
		t.Fatal("client certificate config also has an exec provider or a token transport")
	}
}

func TestDelegateCredential(t *testing.T) {
	cl := &catalog.Cluster{ID: "fake/work/c-1", Name: "c-1", Server: "https://k8s.example"}
	f := newFactory(t, cl, provider.Delegate("aws", "eks", "get-token", "--cluster-name", "prod"))
	cfg, err := f.RESTConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecProvider == nil {
		t.Fatal("ExecProvider not set for a delegate credential")
	}
	if cfg.ExecProvider.Command != "aws" || !reflect.DeepEqual(cfg.ExecProvider.Args, []string{"eks", "get-token", "--cluster-name", "prod"}) {
		t.Fatalf("ExecProvider = %+v, want aws eks get-token --cluster-name prod", cfg.ExecProvider)
	}
	if len(cfg.CertData) != 0 || cfg.WrapTransport != nil {
		t.Fatal("delegate config also has a client certificate or a token transport")
	}
}

func TestExecProvider(t *testing.T) {
	tests := []struct {
		name string
		in   provider.ExecConfig
		want *clientcmdapi.ExecConfig
	}{
		{
			name: "defaults",
			in:   provider.ExecConfig{Command: "gke-gcloud-auth-plugin"},
			want: &clientcmdapi.ExecConfig{
				Command:         "gke-gcloud-auth-plugin",
				APIVersion:      "client.authentication.k8s.io/v1",
				InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
			},
		},
		{
			name: "explicit values",
			in: provider.ExecConfig{
				Command:         "aws",
				Args:            []string{"eks", "get-token"},
				Env:             map[string]string{"AWS_PROFILE": "work"},
				APIVersion:      "client.authentication.k8s.io/v1beta1",
				InteractiveMode: "Never",
				InstallHint:     "install the AWS CLI",
			},
			want: &clientcmdapi.ExecConfig{
				Command:         "aws",
				Args:            []string{"eks", "get-token"},
				Env:             []clientcmdapi.ExecEnvVar{{Name: "AWS_PROFILE", Value: "work"}},
				APIVersion:      "client.authentication.k8s.io/v1beta1",
				InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
				InstallHint:     "install the AWS CLI",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := execProvider(&credentials.Resolved{Delegate: &tt.in})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("execProvider() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProxyURL(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cl := &catalog.Cluster{ID: "fake/work/c-1", Name: "c-1", Server: "https://k8s.example", ProxyURL: "socks5://127.0.0.1:1080"}
		f := newFactory(t, cl, provider.Token(testToken, provider.NoExpiry))
		cfg, err := f.RESTConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Proxy == nil {
			t.Fatal("Proxy not set")
		}
		req, _ := http.NewRequest(http.MethodGet, "https://k8s.example/api", nil)
		u, err := cfg.Proxy(req)
		if err != nil || u == nil || u.String() != cl.ProxyURL {
			t.Fatalf("Proxy(req) = %v, %v; want %s", u, err, cl.ProxyURL)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		cl := &catalog.Cluster{ID: "fake/work/c-1", Name: "c-1", Server: "https://k8s.example", ProxyURL: "://no-scheme"}
		f := newFactory(t, cl, provider.Token(testToken, provider.NoExpiry))
		ctx := context.Background()
		calls := []struct {
			name string
			fn   func() error
		}{
			{"RESTConfig", func() error { _, err := f.RESTConfig(ctx); return err }},
			{"Clientset", func() error { _, err := f.Clientset(ctx); return err }},
			{"Discovery", func() error { _, err := f.Discovery(ctx); return err }},
		}
		for _, c := range calls {
			if err := c.fn(); err == nil || !strings.Contains(err.Error(), "invalid proxy URL") {
				t.Errorf("%s error = %v, want an invalid proxy URL error", c.name, err)
			}
		}
	})
}

func TestMissingCredential(t *testing.T) {
	cl := &catalog.Cluster{ID: "fake/work/c-1", Name: "payments-prod", Server: "https://k8s.example"}
	f := &Factory{Cluster: cl, Broker: &credentials.Broker{Store: credentials.NewMemoryStore()}}
	_, err := f.RESTConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "getting credentials for payments-prod") {
		t.Fatalf("RESTConfig error = %v, want one about getting credentials for payments-prod", err)
	}
}

func TestGuard(t *testing.T) {
	tests := []struct {
		name       string
		policy     *guard.Policy
		wantDenied bool
	}{
		{"no guard", nil, false},
		{"read-write", &guard.Policy{Cluster: "payments-prod"}, false},
		{"read-only", &guard.Policy{Cluster: "payments-prod", ReadOnly: true}, true},
		{"read-only unlocked", &guard.Policy{Cluster: "payments-prod", ReadOnly: true, UnlockedUntil: time.Now().Add(time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeAPI(t)
			f := newFactory(t, clusterFor(srv.Server, "fake/work/c-1"), provider.Token(testToken, provider.NoExpiry))
			f.Guard = tt.policy
			ctx := context.Background()
			cs, err := f.Clientset(ctx)
			if err != nil {
				t.Fatal(err)
			}

			// Reads always work.
			if _, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
				t.Fatalf("listing namespaces: %v", err)
			}

			err = cs.CoreV1().Namespaces().Delete(ctx, "payments", metav1.DeleteOptions{})
			deletes := srv.count("DELETE /api/v1/namespaces/payments")
			if !tt.wantDenied {
				if err != nil || deletes != 1 {
					t.Fatalf("delete = %v with %d requests at the server, want success with 1", err, deletes)
				}
				return
			}
			var denied *guard.DeniedError
			if !errors.As(err, &denied) {
				t.Fatalf("delete error = %v (%T), want *guard.DeniedError", err, err)
			}
			if denied.Cluster != "payments-prod" || denied.Action != "DELETE namespaces/payments" {
				t.Fatalf("DeniedError = %+v, want cluster payments-prod and action DELETE namespaces/payments", denied)
			}
			if deletes != 0 {
				t.Fatalf("refused delete reached the server %d times", deletes)
			}
		})
	}
}

func TestDiscoveryCachesPerCluster(t *testing.T) {
	srv := newFakeAPI(t)
	cacheDir := t.TempDir()
	ctx := context.Background()
	factoryFor := func(id string) *Factory {
		f := newFactory(t, clusterFor(srv.Server, id), provider.Token(testToken, provider.NoExpiry))
		f.CacheDir = cacheDir
		return f
	}
	serverGroups := func(f *Factory) {
		t.Helper()
		d, err := f.Discovery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		groups, err := d.ServerGroups()
		if err != nil {
			t.Fatal(err)
		}
		if len(groups.Groups) != 1 || groups.Groups[0].PreferredVersion.Version != "v1" {
			t.Fatalf("ServerGroups() = %+v, want the core v1 group", groups.Groups)
		}
	}

	work := factoryFor("fake/work/c-1")
	serverGroups(work)
	if n := srv.count("GET /api"); n != 1 {
		t.Fatalf("first discovery made %d requests for /api, want 1", n)
	}
	cached := filepath.Join(cacheDir, "discovery", "fake_work_c-1", "servergroups.json")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("discovery not cached under the cluster's directory: %v", err)
	}

	// A new client for the same cluster answers from the disk cache.
	serverGroups(factoryFor("fake/work/c-1"))
	if n := srv.count("GET /api"); n != 1 {
		t.Fatalf("cached discovery made %d requests for /api in total, want 1", n)
	}

	// Another cluster on the same server has its own cache.
	serverGroups(factoryFor("fake/home/c-1"))
	if n := srv.count("GET /api"); n != 2 {
		t.Fatalf("second cluster made %d requests for /api in total, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "discovery", "fake_home_c-1", "servergroups.json")); err != nil {
		t.Fatalf("second cluster's discovery not cached separately: %v", err)
	}
}

func TestSafeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"kubeconfig/default/kind-dev", "kubeconfig_default_kind-dev"},
		{"aws/work/arn:aws:eks:eu-west-1:123:cluster/prod", "aws_work_arn_aws_eks_eu-west-1_123_cluster_prod"},
		{"a b\\c..d", "a_b_c..d"},
		{"plain-name_1.2", "plain-name_1.2"},
	}
	for _, tt := range tests {
		if got := safeName(tt.in); got != tt.want {
			t.Errorf("safeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBrokerTokenSource(t *testing.T) {
	exp := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	store := credentials.NewMemoryStore()
	broker := &credentials.Broker{Store: store, Now: func() time.Time { return exp.Add(-time.Hour) }}
	ts := newBrokerTokenSource(broker, "c", &credentials.Resolved{Token: "first"})

	// The first token is the one RESTConfig already resolved; the empty store
	// proves the broker is not asked again.
	tok, err := ts.Token()
	if err != nil || tok.AccessToken != "first" || tok.TokenType != "Bearer" || !tok.Expiry.IsZero() {
		t.Fatalf("first Token() = %+v, %v; want bearer token %q without expiry", tok, err, "first")
	}

	if _, err := ts.Token(); err == nil {
		t.Fatal("Token() with nothing stored succeeded, want the broker's error")
	}

	if err := broker.Put("c", provider.Token("refreshed", exp)); err != nil {
		t.Fatal(err)
	}
	tok, err = ts.Token()
	if err != nil || tok.AccessToken != "refreshed" || !tok.Expiry.Equal(exp) {
		t.Fatalf("Token() after refresh = %+v, %v; want %q expiring at %v", tok, err, "refreshed", exp)
	}

	if err := broker.Put("c", provider.ClientCert([]byte("C"), []byte("K"), provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(); err == nil || !strings.Contains(err.Error(), "not a token") {
		t.Fatalf("Token() for a certificate = %v, want a not-a-token error", err)
	}
}
