package akamai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const goodToken = "linode-pat"

// fakeLinode serves the LKE endpoints the plugin uses.
type fakeLinode struct {
	mu            sync.Mutex
	generation    int  // kubeconfig token generation for cluster 101
	notReadyFor   int  // GETs of 101's kubeconfig that answer 503 after a DELETE
	endpointsDown bool // 102's api-endpoints answers 500
	noEndpoints   bool // 102's api-endpoints answers an empty list
}

func kubeconfigFor(server, token string) string {
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----"))
	y := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: lke101
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: lke101-admin
  user:
    token: %s
contexts:
- name: lke101-ctx
  context: {cluster: lke101, user: lke101-admin}
current-context: lke101-ctx
`, server, ca, token)
	return base64.StdEncoding.EncodeToString([]byte(y))
}

func (f *fakeLinode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+goodToken {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":[{"reason":"Invalid Token"}]}`)
		return
	}
	switch p := r.URL.Path; {
	case p == "/v4/lke/clusters" && r.URL.Query().Get("page") == "1":
		fmt.Fprint(w, `{"data":[{"id":101,"label":"payments-prod","region":"us-ord","k8s_version":"1.34","tags":["env:prod","team=pay","misc"],"control_plane":{"high_availability":true}}],"page":1,"pages":2,"results":2}`)
	case p == "/v4/lke/clusters" && r.URL.Query().Get("page") == "2":
		fmt.Fprint(w, `{"data":[{"id":102,"label":"just-created","region":"eu-central","k8s_version":"1.34","tags":[]}],"page":2,"pages":2,"results":2}`)
	case p == "/v4/lke/clusters/101/pools":
		fmt.Fprint(w, `{"data":[{"id":1,"type":"g6-standard-4"},{"id":2,"type":"g1-gpu-rtx6000-1"}],"page":1,"pages":1}`)
	case p == "/v4/lke/clusters/102/pools":
		fmt.Fprint(w, `{"data":[{"id":3,"type":"g6-standard-2"}],"page":1,"pages":1}`)
	case p == "/v4/lke/clusters/101/kubeconfig" && r.Method == http.MethodGet:
		if f.notReadyFor > 0 {
			f.notReadyFor--
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"errors":[{"reason":"Kubeconfig not yet available"}]}`)
			return
		}
		fmt.Fprintf(w, `{"kubeconfig":%q}`, kubeconfigFor("https://101.lke.example:443", fmt.Sprintf("lke-token-%d", f.generation)))
	case p == "/v4/lke/clusters/101/kubeconfig" && r.Method == http.MethodDelete:
		f.generation++
		f.notReadyFor = max(f.notReadyFor, 1)
		fmt.Fprint(w, `{}`)
	case p == "/v4/lke/clusters/102/kubeconfig":
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"errors":[{"reason":"Kubeconfig not yet available"}]}`)
	case p == "/v4/lke/clusters/102/api-endpoints" && f.endpointsDown:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":[{"reason":"Internal error"}]}`)
	case p == "/v4/lke/clusters/102/api-endpoints" && f.noEndpoints:
		fmt.Fprint(w, `{"data":[],"page":1,"pages":1}`)
	case p == "/v4/lke/clusters/102/api-endpoints":
		fmt.Fprint(w, `{"data":[{"endpoint":"https://102.lke.example:443"}],"page":1,"pages":1}`)
	case p == "/v4/lke/clusters/101/control_plane_acl":
		fmt.Fprint(w, `{"acl":{"enabled":true,"addresses":{"ipv4":["203.0.113.0/24"],"ipv6":["2001:db8::1"]},"revision-id":"x"}}`)
	case p == "/v4/lke/clusters/102/control_plane_acl":
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors":[{"reason":"Cluster does not support Control Plane ACL."}]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"reason":"Not found"}]}`)
	}
}

func setup(t *testing.T) (*Provider, provider.Account, *fakeLinode) {
	t.Helper()
	fake := &fakeLinode{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p := New()
	p.wait = func(context.Context, time.Duration) error { return nil }
	return p, provider.Account{ID: "work", Config: map[string]any{"apiURL": srv.URL + "/v4"}}, fake
}

var secrets = map[string]string{"api_token": goodToken}

func TestList(t *testing.T) {
	p, acct, _ := setup(t)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: acct, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("got %d clusters (pagination?)", len(clusters))
	}
	prod := clusters[0]
	if prod.ID != "101" || prod.Name != "payments-prod" || prod.Region != "us-ord" || prod.Endpoints.Public != "https://101.lke.example:443" {
		t.Fatalf("prod: %+v", prod)
	}
	if string(prod.CAData) != "-----BEGIN CERTIFICATE-----" || prod.Credential == nil || prod.Credential.Token != "lke-token-0" {
		t.Fatalf("prod kubeconfig not used: CA %q, credential %+v", prod.CAData, prod.Credential)
	}
	want := map[string]string{"env": "prod", "team": "pay", "ha": "true", "gpu": "true"}
	for k, v := range want {
		if prod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q (all: %v)", k, prod.Labels[k], v, prod.Labels)
		}
	}

	pending := clusters[1]
	if pending.Endpoints.Public != "https://102.lke.example:443" || pending.Credential != nil || pending.Labels["gpu"] != "" {
		t.Fatalf("cluster without a kubeconfig yet: %+v", pending)
	}
}

func TestListWhenEndpointsFail(t *testing.T) {
	p, acct, fake := setup(t)
	fake.endpointsDown = true
	_, err := p.List(context.Background(), provider.ListRequest{Account: acct, Secrets: secrets})
	if provider.CodeOf(err) != provider.Unavailable {
		t.Fatalf("an endpoint error should fail the listing, not drop the cluster: %v", err)
	}

	fake.endpointsDown, fake.noEndpoints = false, true
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: acct, Secrets: secrets})
	if err != nil || len(clusters) != 1 || clusters[0].ID != "101" {
		t.Fatalf("a cluster with no endpoint yet is skipped: %+v %v", clusters, err)
	}
}

func TestRotateStopsWhenCancelled(t *testing.T) {
	p, acct, fake := setup(t)
	p.wait = sleep // real, cancellable waits
	fake.mu.Lock()
	fake.notReadyFor = 100 // not ready again before the context ends
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Rotate(ctx, provider.CredentialRequest{Account: acct, ClusterID: "101", Secrets: secrets})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("rotate kept waiting %s after cancellation", time.Since(start))
	}
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || !strings.Contains(pe.Hint, "old token no longer works") {
		t.Fatalf("got %+v", pe)
	}
}

func TestCredentialAndRotate(t *testing.T) {
	p, acct, _ := setup(t)
	ctx := context.Background()
	c, err := p.Credential(ctx, provider.CredentialRequest{Account: acct, ClusterID: "101", Secrets: secrets})
	if err != nil || c.Token != "lke-token-0" || c.ExpiresAt != nil {
		t.Fatalf("credential: %+v %v", c, err)
	}
	rotated, err := p.Rotate(ctx, provider.CredentialRequest{Account: acct, ClusterID: "101", Secrets: secrets})
	if err != nil || rotated.Token != "lke-token-1" {
		t.Fatalf("rotate should wait out the regeneration: %+v %v", rotated, err)
	}

	_, err = p.Credential(ctx, provider.CredentialRequest{Account: acct, ClusterID: "102", Secrets: secrets})
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || !strings.Contains(pe.Hint, "2 to 5 minutes") {
		t.Fatalf("not-ready kubeconfig: %+v", pe)
	}
	if _, err := p.Credential(ctx, provider.CredentialRequest{Account: acct, ClusterID: "abc", Secrets: secrets}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Fatalf("bad id: %v", err)
	}
}

func TestErrors(t *testing.T) {
	p, acct, _ := setup(t)
	ctx := context.Background()
	_, err := p.List(ctx, provider.ListRequest{Account: acct, Secrets: map[string]string{"api_token": "nope"}})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Message, "Invalid Token") {
		t.Fatalf("bad token: %+v", pe)
	}
	if _, err := p.List(ctx, provider.ListRequest{Account: acct}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Fatalf("no token: %v", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com/v4"}}
	if _, err := p.List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Fatalf("plain http to a remote host: %v", err)
	}
}

func TestCheckAccess(t *testing.T) {
	p, acct, _ := setup(t)
	tests := []struct {
		cluster, ip string
		want        provider.Severity
	}{
		{"101", "203.0.113.7", provider.SeverityInfo},
		{"101", "2001:db8::1", provider.SeverityInfo},
		{"101", "198.51.100.9", provider.SeverityError},
		{"101", "", provider.SeverityWarning},
		{"102", "198.51.100.9", provider.SeverityInfo}, // ACL not supported
	}
	for _, tt := range tests {
		findings, err := p.CheckAccess(context.Background(), provider.AccessRequest{
			Account: acct, Cluster: provider.Cluster{ID: tt.cluster}, EgressIP: tt.ip, Secrets: secrets,
		})
		if err != nil || len(findings) != 1 || findings[0].Severity != tt.want {
			t.Errorf("cluster %s from %q: %+v %v, want %s", tt.cluster, tt.ip, findings, err, tt.want)
			continue
		}
		if tt.want == provider.SeverityError && !strings.Contains(findings[0].Fix, "198.51.100.9/32") {
			t.Errorf("fix should name the address: %q", findings[0].Fix)
		}
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	_, acct, _ := setup(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-akamai")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-akamai").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         acct,
		Secrets:         secrets,
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the akamai plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: acct.Config, Secrets: secrets})
}
