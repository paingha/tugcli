package capi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/pkg/provider"
)

const mgmtToken = "mgmt-token"

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// fakeAPI is a management cluster serving Cluster objects and kubeconfig
// Secrets to callers presenting mgmtToken.
type fakeAPI struct {
	versions []string
	clusters []map[string]any
	secrets  map[string][]byte // namespace/name -> kubeconfig

	mu    sync.Mutex
	paths []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+mgmtToken {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/apis/"+Group && len(f.versions) > 0:
		g := map[string]any{"kind": "APIGroup", "apiVersion": "v1", "name": Group}
		var vs []map[string]string
		for _, v := range f.versions {
			vs = append(vs, map[string]string{"groupVersion": Group + "/" + v, "version": v})
		}
		g["versions"] = vs
		writeJSON(w, g)
	case len(parts) >= 4 && parts[0] == "apis" && parts[1] == Group && slices.Contains(f.versions, parts[2]) && parts[len(parts)-1] == "clusters":
		ns := ""
		if len(parts) == 6 && parts[3] == "namespaces" {
			ns = parts[4]
		}
		items := []map[string]any{}
		for _, c := range f.clusters {
			if ns == "" || c["metadata"].(map[string]any)["namespace"] == ns {
				items = append(items, c)
			}
		}
		writeJSON(w, map[string]any{"kind": "ClusterList", "apiVersion": Group + "/" + parts[2], "metadata": map[string]any{}, "items": items})
	case len(parts) == 6 && parts[0] == "api" && parts[2] == "namespaces" && parts[4] == "secrets":
		kc, ok := f.secrets[parts[3]+"/"+parts[5]]
		if !ok {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		writeJSON(w, map[string]any{
			"kind": "Secret", "apiVersion": "v1", "type": "cluster.x-k8s.io/secret",
			"metadata": map[string]any{"name": parts[5], "namespace": parts[3]},
			"data":     map[string][]byte{kubeconfigKey: kc},
		})
	default:
		status(w, http.StatusNotFound, "NotFound")
	}
}

// requested reports whether the fake served a path.
func (f *fakeAPI) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.paths, path)
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func status(w http.ResponseWriter, code int, reason string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": code, "message": strings.ToLower(reason)})
}

func capiCluster(ns, name, host string, port int, labels map[string]string) map[string]any {
	spec := map[string]any{"topology": map[string]any{"version": "v1.34.2"}}
	if host != "" {
		spec["controlPlaneEndpoint"] = map[string]any{"host": host, "port": port}
	}
	return map[string]any{
		"apiVersion": Group + "/v1beta1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"spec":     spec,
	}
}

func kubeconfigWith(user string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://10.0.0.1:6443
    certificate-authority-data: ` + b64("CA-PEM") + `
users:
- name: admin
  user:
` + user + `
contexts:
- name: admin@c
  context: {cluster: c, user: admin}
current-context: admin@c
`)
}

func b64(s string) string {
	out, _ := json.Marshal([]byte(s))
	return strings.Trim(string(out), `"`)
}

func testCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kubernetes-admin"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func newFake(t *testing.T) (*fakeAPI, *Provider) {
	t.Helper()
	certPEM, keyPEM := testCert(t, now.Add(30*time.Minute))
	f := &fakeAPI{
		versions: []string{"v1beta2", "v1beta1"},
		clusters: []map[string]any{
			capiCluster("team-a", "pay-prod", "pay.example.com", 6443, map[string]string{"env": "prod", "team": "pay"}),
			capiCluster("team-b", "web", "10.1.2.3", 0, nil),
			capiCluster("team-b", "provisioning", "", 0, nil),
			capiCluster("team-b", "no-secret", "nosecret.example.com", 6443, nil),
			capiCluster("team-b", "broken", "broken.example.com", 6443, nil),
		},
		secrets: map[string][]byte{
			"team-a/pay-prod-kubeconfig": kubeconfigWith("    token: workload-token"),
			"team-b/web-kubeconfig": kubeconfigWith(fmt.Sprintf("    client-certificate-data: %s\n    client-key-data: %s",
				b64(string(certPEM)), b64(string(keyPEM)))),
			"team-b/broken-kubeconfig": []byte("{not yaml"),
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(WithNow(func() time.Time { return now }), WithClusterConfig(func(_ context.Context, name string) (*rest.Config, error) {
		if name != "mgmt" {
			return nil, provider.Errorf(provider.NotFound, "%s is not in the catalog", name)
		}
		return &rest.Config{Host: srv.URL, BearerToken: mgmtToken}, nil
	}))
	return f, p
}

func account(settings ...string) provider.Account {
	cfg := map[string]any{ConfigCluster: "mgmt"}
	for _, kv := range settings {
		k, v, _ := strings.Cut(kv, "=")
		cfg[k] = v
	}
	return provider.Account{ID: "mgmt", Config: cfg}
}

func TestList(t *testing.T) {
	f, p := newFake(t)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account()})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	if want := []string{"team-a/pay-prod", "team-b/web", "team-b/broken"}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v", ids, want)
	}
	pay, web, broken := clusters[0], clusters[1], clusters[2]
	if pay.Name != "pay-prod" || pay.Endpoints.Public != "https://pay.example.com:6443" || string(pay.CAData) != "CA-PEM" ||
		pay.Version != "v1.34.2" || pay.Labels["env"] != "prod" || pay.Labels["team"] != "pay" {
		t.Fatalf("pay-prod = %+v", pay)
	}
	if pay.Credential == nil || pay.Credential.Token != "workload-token" || !pay.Credential.ExpiresAt.Equal(now.Add(tokenTTL)) {
		t.Fatalf("pay-prod credential = %+v, want the token for %s", pay.Credential, tokenTTL)
	}
	if web.Endpoints.Public != "https://10.1.2.3" || web.Credential == nil || web.Credential.Type != provider.CredentialClientCert {
		t.Fatalf("web = %+v", web)
	}
	if broken.Endpoints.Public != "https://broken.example.com:6443" || broken.CAData != nil || broken.Credential != nil {
		t.Fatalf("broken = %+v, want it listed without CA or credential", broken)
	}
	if !f.requested("/apis/cluster.x-k8s.io/v1beta1/clusters") {
		t.Fatal("List did not read v1beta1 Clusters in every namespace")
	}
}

func TestListVersionsAndNamespace(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		settings []string
		wantPath string
		wantIDs  int
		code     provider.Code
	}{
		{name: "only v1beta2", versions: []string{"v1beta2"}, wantPath: "/apis/cluster.x-k8s.io/v1beta2/clusters", wantIDs: 3},
		{name: "one namespace", versions: []string{"v1beta1"}, settings: []string{"namespace=team-a"}, wantPath: "/apis/cluster.x-k8s.io/v1beta1/namespaces/team-a/clusters", wantIDs: 1},
		{name: "not installed", code: provider.NotFound},
		{name: "unknown versions", versions: []string{"v1alpha4"}, code: provider.Unimplemented},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, p := newFake(t)
			f.versions = tt.versions
			clusters, err := p.List(context.Background(), provider.ListRequest{Account: account(tt.settings...)})
			if tt.code != "" {
				if provider.CodeOf(err) != tt.code {
					t.Fatalf("List() error = %v, want %s", err, tt.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(clusters) != tt.wantIDs || !f.requested(tt.wantPath) {
				t.Fatalf("List() = %d clusters, want %d read from %s", len(clusters), tt.wantIDs, tt.wantPath)
			}
		})
	}
}

func TestCredential(t *testing.T) {
	_, p := newFake(t)
	c, err := p.Credential(context.Background(), provider.CredentialRequest{Account: account(), ClusterID: "team-a/pay-prod"})
	if err != nil || c.Type != provider.CredentialToken || c.Token != "workload-token" {
		t.Fatalf("Credential(pay-prod) = %+v, %v", c, err)
	}
	// The certificate expires in 30 minutes, before the hour tug would
	// otherwise keep it.
	c, err = p.Credential(context.Background(), provider.CredentialRequest{Account: account(), ClusterID: "team-b/web"})
	if err != nil || c.Type != provider.CredentialClientCert || !c.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("Credential(web) = %+v, %v", c, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRefusals(t *testing.T) {
	f, p := newFake(t)
	f.secrets["team-c/exec-kubeconfig"] = kubeconfigWith("    exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/sh, args: [-c, id]}")
	f.secrets["team-c/file-kubeconfig"] = kubeconfigWith("    tokenFile: /var/run/secrets/token")
	f.secrets["team-c/basic-kubeconfig"] = kubeconfigWith("    username: admin\n    password: hunter2")
	f.secrets["team-c/empty-kubeconfig"] = kubeconfigWith("    {}")
	tests := []struct {
		id   string
		code provider.Code
		msg  string
	}{
		{"team-c/exec", provider.Unimplemented, "does not run commands"},
		{"team-c/file", provider.Unimplemented, "files on another machine"},
		{"team-c/basic", provider.Unimplemented, "basic authentication"},
		{"team-c/empty", provider.NotFound, "no credentials"},
		{"team-b/broken", provider.InvalidConfig, "does not hold a kubeconfig"},
		{"team-b/no-secret", provider.NotFound, "no Secret no-secret-kubeconfig"},
		{"pay-prod", provider.InvalidConfig, "not namespace/name"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			_, err := p.Credential(context.Background(), provider.CredentialRequest{Account: account(), ClusterID: tt.id})
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Message, tt.msg) {
				t.Fatalf("Credential(%s) error = %v, want %s containing %q", tt.id, err, tt.code, tt.msg)
			}
		})
	}
}

func TestManagementClusterErrors(t *testing.T) {
	_, p := newFake(t)
	tests := []struct {
		name string
		acct provider.Account
		code provider.Code
		hint string
	}{
		{"no cluster setting", provider.Account{ID: "x"}, provider.InvalidConfig, "--set cluster="},
		{"not in the catalog", provider.Account{ID: "x", Config: map[string]any{ConfigCluster: "elsewhere"}}, provider.NotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.List(context.Background(), provider.ListRequest{Account: tt.acct})
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Fatalf("List() error = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}

	srv := httptest.NewServer(&fakeAPI{versions: []string{"v1beta1"}})
	t.Cleanup(srv.Close)
	stale := New(WithClusterConfig(func(context.Context, string) (*rest.Config, error) {
		return &rest.Config{Host: srv.URL, BearerToken: "expired"}, nil
	}))
	_, err := stale.List(context.Background(), provider.ListRequest{Account: account()})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Hint, "tug doctor mgmt") {
		t.Fatalf("List() with a refused credential = %+v, want Unauthenticated with a doctor hint", pe)
	}
}
