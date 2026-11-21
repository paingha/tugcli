package civo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const (
	goodKey    = "civo-good-key"
	deniedKey  = "civo-team-key-without-kubernetes"
	limitedKey = "civo-limited-key"

	prodID     = "69a23478-a89e-41d2-97b1-6f4c341cee70"
	mlID       = "d1cd0b71-5da1-492e-9d0d-a46ccdaae2fa"
	buildingID = "2843e46e-7f87-4c8b-864e-1e85505ee70b"
	waitingID  = "4a07ac84-70f8-11e5-8fe9-5cf9389be614"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIBdzCCAR2gAwIBAgIBADAKBggqhkjOPQQDAjAjMSEwHwYDVQQDDBhrM3Mtc2Vy\n-----END CERTIFICATE-----\n"

// testCert returns a self-signed client certificate and key in PEM that
// expire at notAfter, like the admin certificate in a k3s kubeconfig.
func testCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "system:admin", Organization: []string{"system:masters"}},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// fakeCivo serves the parts of the Civo API v2 the plugin uses, with bodies
// shaped like the examples in Civo's API documentation and its Go client's
// test fixtures.
type fakeCivo struct {
	srv      *httptest.Server
	notAfter time.Time
	cert     []byte
	key      []byte
	// sizesDown makes the sizes listing fail.
	sizesDown bool

	mu       sync.Mutex
	requests []string
}

func newFake(t *testing.T) *fakeCivo {
	t.Helper()
	f := &fakeCivo{notAfter: time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second).UTC()}
	f.cert, f.key = testCert(t, f.notAfter)
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// kubeconfig is a k3s kubeconfig as Civo returns it in the cluster object.
func (f *fakeCivo) kubeconfig(name, ip string) string {
	return fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://%s:6443
  name: %s
contexts:
- context:
    cluster: %[3]s
    user: %[3]s
  name: %[3]s
current-context: %[3]s
kind: Config
preferences: {}
users:
- name: %[3]s
  user:
    client-certificate-data: %s
    client-key-data: %s
`, b64([]byte(caPEM)), ip, name, b64(f.cert), b64(f.key))
}

func (f *fakeCivo) clusterJSON(id, name, status, ip, kubeconfig, clusterType string, tags []string, sizes ...string) string {
	pools := make([]string, len(sizes))
	for i, s := range sizes {
		pools[i] = fmt.Sprintf(`{"id":"pool-%d","count":1,"size":%q,"instance_names":["k3s-%s-node-%d"]}`, i, s, name, i)
	}
	tagJSON, _ := json.Marshal(tags)
	endpoint := "https://" + ip + ":6443"
	var kc any
	if kubeconfig != "" {
		kc = kubeconfig
	}
	kcJSON, _ := json.Marshal(kc)
	return fmt.Sprintf(`{"id":%q,"name":%q,"version":"1.30.5-k3s1","status":%q,"ready":%t,"cluster_type":%q,
	  "num_target_nodes":1,"target_nodes_size":"g4s.kube.medium","built_at":"2026-09-23T13:04:23Z","kubeconfig":%s,
	  "kubernetes_version":"1.30.5-k3s1","api_endpoint":%q,"master_ip":%q,"dns_entry":%q,
	  "tags":%s,"created_at":"2026-09-23T13:02:59Z","firewall_id":"42118911-44c2-4cab-ad77-bcae062815b3",
	  "pools":[%s],"required_pools":[],"installed_applications":[],"cni_plugin":"flannel","ccm_installed":"true"}`,
		id, name, status, status == "ACTIVE", clusterType, kcJSON, endpoint, ip, id+".k8s.civo.com", tagJSON, strings.Join(pools, ","))
}

func fail(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (f *fakeCivo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	scheme, key, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "bearer") {
		key = ""
	}
	switch key {
	case goodKey:
	case deniedKey:
		fail(w, http.StatusForbidden, `{"code":"authentication_access_denied","reason":"You don't have permission to access this resource"}`)
		return
	case limitedKey:
		w.Header().Set("Retry-After", "20")
		fail(w, http.StatusTooManyRequests, `{"code":"too_many_requests","reason":"Rate limit exceeded"}`)
		return
	default:
		fail(w, http.StatusUnauthorized, `{"code":"authentication_invalid_key","reason":"The API key provided is invalid"}`)
		return
	}

	q := r.URL.Query()
	region := strings.ToLower(q.Get("region"))
	switch path := r.URL.Path; {
	case path == "/v2/regions":
		fmt.Fprint(w, `[
		  {"code":"LON1","name":"London 1","type":"civostack","default":false,"out_of_capacity":false,"country":"uk","country_name":"United Kingdom","features":{"iaas":true,"kubernetes":true}},
		  {"code":"NYC1","name":"New York 1","type":"civostack","default":true,"out_of_capacity":false,"country":"us","country_name":"United States","features":{"iaas":true,"kubernetes":true}},
		  {"code":"OBJ1","name":"Object store","type":"civostack","default":false,"out_of_capacity":false,"country":"uk","country_name":"United Kingdom","features":{"iaas":false,"kubernetes":false}}
		]`)
	case path == "/v2/kubernetes/clusters" && q.Get("per_page") != "100":
		fail(w, http.StatusBadRequest, `{"code":"parameter_invalid","reason":"want per_page=100"}`)
	case path == "/v2/kubernetes/clusters" && region == "lon1" && q.Get("page") == "1":
		fmt.Fprintf(w, `{"page":1,"per_page":100,"pages":2,"items":[%s]}`,
			f.clusterJSON(prodID, "prod", "ACTIVE", "74.220.21.10", f.kubeconfig("prod", "74.220.21.10"), "k3s",
				[]string{"env:prod", "Team=payments", "web"}, "g4s.kube.medium"))
	case path == "/v2/kubernetes/clusters" && region == "lon1" && q.Get("page") == "2":
		fmt.Fprintf(w, `{"page":2,"per_page":100,"pages":2,"items":[%s]}`,
			f.clusterJSON(mlID, "ml", "ACTIVE", "74.220.21.11", f.kubeconfig("ml", "74.220.21.11"), "talos", nil, "g4s.kube.small", "an.g1.l40s.kube.x1"))
	case path == "/v2/kubernetes/clusters" && region == "nyc1":
		fmt.Fprintf(w, `{"page":1,"per_page":100,"pages":1,"items":[%s,%s]}`,
			f.clusterJSON(buildingID, "just-created", "BUILDING", "", "", "k3s", nil, "g4s.kube.small"),
			f.clusterJSON(waitingID, "waiting", "BUILDING", "212.2.240.7", "", "k3s", nil))
	case path == "/v2/kubernetes/clusters":
		fmt.Fprint(w, `{"page":1,"per_page":100,"pages":1,"items":[]}`)
	case path == "/v2/sizes" && f.sizesDown:
		fail(w, http.StatusInternalServerError, `{"code":"unknown_error","reason":"Something went wrong"}`)
	case path == "/v2/sizes":
		fmt.Fprint(w, `[
		  {"type":"kubernetes","name":"g4s.kube.small","nice_name":"Small - Standard","cpu_cores":1,"gpu_count":0,"gpu_type":"","ram_mb":2048,"disk_gb":40,"transfer_tb":1,"description":"Small - Standard","selectable":true},
		  {"type":"kubernetes","name":"g4s.kube.medium","nice_name":"Medium - Standard","cpu_cores":2,"gpu_count":0,"gpu_type":"","ram_mb":4096,"disk_gb":50,"transfer_tb":2,"description":"Medium - Standard","selectable":true},
		  {"type":"kubernetes","name":"an.g1.l40s.kube.x1","nice_name":"Small - NVIDIA L40S","cpu_cores":12,"gpu_count":1,"gpu_type":"L40S","ram_mb":131072,"disk_gb":200,"transfer_tb":5,"description":"Small - NVIDIA L40S","selectable":true}
		]`)
	case path == "/v2/kubernetes/clusters/"+prodID && region == "lon1":
		fmt.Fprint(w, f.clusterJSON(prodID, "prod", "ACTIVE", "74.220.21.10", f.kubeconfig("prod", "74.220.21.10"), "k3s", nil))
	case path == "/v2/kubernetes/clusters/"+waitingID && region == "nyc1":
		fmt.Fprint(w, f.clusterJSON(waitingID, "waiting", "BUILDING", "212.2.240.7", "", "k3s", nil))
	default:
		fail(w, http.StatusNotFound, `{"code":"database_kubernetes_cluster_not_found","reason":"The requested Kubernetes cluster couldn't be found"}`)
	}
}

func (f *fakeCivo) sent(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCivo) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

func secrets(key string) map[string]string { return map[string]string{SecretAPIKey: key} }

func TestList(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodKey)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	want := []string{"lon1/" + prodID, "lon1/" + mlID, "nyc1/" + waitingID}
	if !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v (both pages of lon1, then nyc1 without the cluster that has no address)", ids, want)
	}

	prod := clusters[0]
	if prod.Name != "prod" || prod.Region != "lon1" || prod.Version != "1.30.5-k3s1" || prod.Endpoints.Public != "https://74.220.21.10:6443" {
		t.Errorf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the kubeconfig's certificate-authority-data", prod.CAData)
	}
	c := prod.Credential
	if c == nil || c.Type != provider.CredentialClientCert || string(c.CertData) != string(f.cert) || string(c.KeyData) != string(f.key) {
		t.Fatalf("prod credential = %+v, want the kubeconfig's client certificate", c)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Errorf("prod credential expires %v, want the certificate's NotAfter %v", c.ExpiresAt, f.notAfter)
	}
	wantLabels := map[string]string{"env": "prod", "team": "payments", "cluster-type": "k3s"}
	if fmt.Sprint(prod.Labels) != fmt.Sprint(wantLabels) {
		t.Errorf("prod labels = %v, want %v", prod.Labels, wantLabels)
	}

	if ml := clusters[1]; ml.Labels["gpu"] != "true" || ml.Labels["cluster-type"] != "talos" {
		t.Errorf("ml labels = %v, want gpu=true from the L40S pool's gpu_count and cluster-type=talos", ml.Labels)
	}

	waiting := clusters[2]
	if waiting.Endpoints.Public != "https://212.2.240.7:6443" || waiting.CAData != nil || waiting.Credential != nil {
		t.Errorf("a cluster without a kubeconfig yet is listed by endpoint alone: %+v", waiting)
	}
	if got := f.sent("/v2/sizes"); len(got) != 2 {
		t.Errorf("sizes requests = %v, want one for each region with node pools", got)
	}
}

func TestListRegions(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigRegions: " NYC1, nyc1 "}), Secrets: secrets(goodKey)})
	if err != nil || len(clusters) != 1 || clusters[0].ID != "nyc1/"+waitingID {
		t.Fatalf("List with regions=NYC1 = %+v, %v; want the nyc1 cluster only", clusters, err)
	}
	if got := f.sent("/v2/regions"); len(got) != 0 {
		t.Errorf("List with regions set asked for the regions: %v", got)
	}
	if got := f.sent("/v2/kubernetes/clusters"); len(got) != 1 || !strings.Contains(got[0], "region=nyc1") {
		t.Errorf("cluster requests = %v, want one for nyc1", got)
	}
}

func TestListWithoutSizes(t *testing.T) {
	f := newFake(t)
	f.sizesDown = true
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigRegions: "lon1"}), Secrets: secrets(goodKey)})
	if err != nil || len(clusters) != 2 {
		t.Fatalf("List while sizes fail = %+v, %v; want both lon1 clusters", clusters, err)
	}
	if clusters[1].Labels["gpu"] != "" {
		t.Errorf("ml labels = %v, want no gpu label without sizes", clusters[1].Labels)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "lon1/" + prodID, Secrets: secrets(goodKey)})
	if err != nil || c.Type != provider.CredentialClientCert || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Fatalf("Credential(prod) = %+v, %v, want the client certificate expiring %v", c, err, f.notAfter)
	}
	tests := []struct {
		name string
		id   string
		code provider.Code
	}{
		{"wrong region", "nyc1/" + prodID, provider.NotFound},
		{"gone", "lon1/00000000-0000-0000-0000-000000000000", provider.NotFound},
		{"no region", prodID, provider.NotFound},
		{"no kubeconfig yet", "nyc1/" + waitingID, provider.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: tt.id, Secrets: secrets(goodKey)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential(%s) = %+v, want %s with a hint", tt.id, pe, tt.code)
			}
		})
	}
}

func TestParseKubeconfig(t *testing.T) {
	f := newFake(t)
	raw := f.kubeconfig("prod", "74.220.21.10")
	tokenOnly := strings.Replace(raw, "    client-certificate-data:", "    token: abc\n    other:", 1)
	badCert := strings.Replace(raw, b64(f.cert), b64([]byte("not a certificate")), 1)
	tests := []struct {
		name     string
		raw      string
		wantType provider.CredentialType
		wantErr  string
	}{
		{"k3s kubeconfig", raw, provider.CredentialClientCert, ""},
		{"base64", b64([]byte(raw)), provider.CredentialClientCert, ""},
		{"token", tokenOnly, provider.CredentialToken, ""},
		{"certificate not PEM", badCert, "", "not PEM"},
		{"no users", "apiVersion: v1\nkind: Config\nclusters: []\n", "", "no cluster or user"},
		{"not yaml", "clusters: [", "", "parsing kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc, err := parseKubeconfig(tt.raw)
			var cred provider.Credential
			if err == nil {
				cred, err = kc.credential()
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseKubeconfig = %+v, %v, want an error containing %q", kc, err, tt.wantErr)
				}
				return
			}
			if err != nil || kc.server != "https://74.220.21.10:6443" || string(kc.ca) != caPEM || cred.Type != tt.wantType {
				t.Fatalf("parseKubeconfig = %+v, %+v, %v, want the server, CA and a %s credential", kc, cred, err, tt.wantType)
			}
		})
	}
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://74.220.21.10:6443": "https://74.220.21.10:6443",
		"74.220.21.10:6443":         "https://74.220.21.10:6443",
		"https://:6443":             "",
		"":                          "",
	} {
		if got := endpointURL(in); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTagList(t *testing.T) {
	for in, want := range map[string][]string{
		`["env:prod","web"]`: {"env:prod", "web"},
		`"env:prod web"`:     {"env:prod", "web"},
		`null`:               nil,
	} {
		var got tagList
		if err := json.Unmarshal([]byte(in), &got); err != nil || !slices.Equal(got, want) {
			t.Errorf("tagList(%s) = %q, %v, want %q", in, got, err, want)
		}
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		key     string
		code    provider.Code
		message string
		hint    string
	}{
		{"bad key", "nope", provider.Unauthenticated, "The API key provided is invalid", "Settings > Profile > Security"},
		{"no Kubernetes permission", deniedKey, provider.Unauthenticated, "permission", "Kubernetes permission"},
		{"rate limited", limitedKey, provider.RateLimited, "Rate limit exceeded", "wait a minute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(tt.key)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "lon1/" + prodID, Secrets: secrets(tt.key)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(limitedKey)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 20 {
		t.Errorf("List when rate limited: RetryAfterSeconds = %d, want 20 from Retry-After", pe.RetryAfterSeconds)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account(nil)}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without a key = %v, want Unauthenticated", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com"}}
	if _, err := New().List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets(goodKey)}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Errorf("List over plain http to a remote host = %v, want InvalidConfig", err)
	}
}

func TestManifestMatchesInfo(t *testing.T) {
	b, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Metadata struct{ Name, Version string }
		Spec     struct {
			Capabilities provider.Capability
			Secrets      []provider.SecretSpec
		}
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	info := New().Info()
	if m.Metadata.Name != info.Name || m.Metadata.Version != info.Version ||
		m.Spec.Capabilities != provider.Capabilities(New()) || !slices.Equal(m.Spec.Secrets, info.Secrets) {
		t.Errorf("plugin.yaml = %+v, want Info() = %+v", m, info)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFake(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-civo")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-civo").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(nil),
		Secrets:         secrets(goodKey),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the civo plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: f.account(nil).Config, Secrets: secrets(goodKey)})
}
