package vultr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

const (
	goodKey    = "VULTR-GOOD-KEY"
	limitedKey = "VULTR-LIMITED-KEY"
	userKey    = "VULTR-USER-KEY" // a user without the Kubernetes permission

	activeID  = "014da059-21e3-47eb-acb5-91bf697c31aa"
	pendingID = "455dcd32-e621-48ee-a10e-0cb5f477ff3d"
	newID     = "7c3b2f9e-0000-4000-8000-000000000001"

	// cursor is the example cursor in Vultr's CLI documentation.
	cursor = "bmV4dF9fQU1T"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIBdzCCAR2gAwIBAgIBADAKBggqhkjOPQQDAjAjMSEwHwYDVQQDDBhrM3Mtc2Vy\n-----END CERTIFICATE-----\n"

// clientCert returns a self-signed client certificate and key in PEM that
// expire at notAfter.
func clientCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kubernetes-admin", Organization: []string{"system:masters"}},
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

// fakeVultr serves the VKE endpoints the plugin uses, with bodies shaped
// like those in Vultr's API documentation and its Go client's tests.
type fakeVultr struct {
	srv      *httptest.Server
	notAfter time.Time
	certPEM  []byte
	keyPEM   []byte
}

func newFakeVultr(t *testing.T) *fakeVultr {
	t.Helper()
	f := &fakeVultr{notAfter: time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second).UTC()}
	f.certPEM, f.keyPEM = clientCert(t, f.notAfter)
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func (f *fakeVultr) kubeconfig(id string) string {
	y := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://%s.vultr-k8s.com:6443
  name: vke-%[2]s
contexts:
- context:
    cluster: vke-%[2]s
    user: admin
  name: vke-%[2]s
current-context: vke-%[2]s
kind: Config
preferences: {}
users:
- name: admin
  user:
    client-certificate-data: %s
    client-key-data: %s
`, b64([]byte(caPEM)), id, b64(f.certPEM), b64(f.keyPEM))
	return b64([]byte(y))
}

func clusterJSON(id, label, endpoint, status string, ha bool, plans ...string) string {
	pools := make([]string, len(plans))
	for i, plan := range plans {
		pools[i] = fmt.Sprintf(`{"id":"pool-%d","date_created":"2026-07-13T14:20:16+00:00","label":"pool-%d","plan":%q,"status":%q,
		  "tag":"mytag","node_quantity":1,"min_nodes":1,"max_nodes":2,"auto_scaler":true,"nodes":[]}`, i, i, plan, status)
	}
	return fmt.Sprintf(`{"id":%q,"label":%q,"date_created":"2026-07-13T14:20:16+00:00","cluster_subnet":"10.244.0.0/16",
	  "service_subnet":"10.96.0.0/12","ip":"0.0.0.0","endpoint":%q,"version":"v1.34.1+1","region":"lax","status":%q,
	  "ha_controlplanes":%t,"firewall_group_id":"","node_pools":[%s]}`, id, label, endpoint, status, ha, strings.Join(pools, ","))
}

func fail(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q,"status":%d}`, message, status)
}

func (f *fakeVultr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Header.Get("Authorization") {
	case "Bearer " + goodKey, "Bearer " + userKey:
	case "Bearer " + limitedKey:
		fail(w, http.StatusTooManyRequests, "Rate limit reached - please try your request again later.")
		return
	default:
		fail(w, http.StatusUnauthorized, "Invalid API token.")
		return
	}
	if r.Header.Get("Authorization") == "Bearer "+userKey {
		fail(w, http.StatusForbidden, "Unauthorized access.")
		return
	}

	switch path, q := r.URL.Path, r.URL.Query(); {
	case path == "/v2/kubernetes/clusters" && q.Get("per_page") != "500":
		fail(w, http.StatusBadRequest, "want per_page=500")
	case path == "/v2/kubernetes/clusters" && q.Get("cursor") == "":
		fmt.Fprintf(w, `{"vke_clusters":[%s],"meta":{"total":3,"links":{"next":%q,"prev":""}}}`,
			clusterJSON(activeID, "prod", activeID+".vultr-k8s.com", "active", true, "vc2-4c-8gb", "vcg-a100-1c-6g-4vram"), cursor)
	case path == "/v2/kubernetes/clusters" && q.Get("cursor") == cursor:
		fmt.Fprintf(w, `{"vke_clusters":[%s,%s],"meta":{"total":3,"links":{"next":"","prev":"cHJldl9fQU1T"}}}`,
			clusterJSON(pendingID, "staging", pendingID+".vultr-k8s.com", "pending", false, "vc2-2c-4gb"),
			clusterJSON(newID, "just-created", "", "pending", false, "vc2-2c-4gb"))
	case path == "/v2/kubernetes/clusters/"+activeID+"/config":
		fmt.Fprintf(w, `{"kube_config":%q}`, f.kubeconfig(activeID))
	case path == "/v2/kubernetes/clusters/"+pendingID+"/config":
		// What the API answers for a cluster that is still being created is
		// not documented; any error for a pending cluster means not ready.
		fail(w, http.StatusBadRequest, "Cluster is not ready.")
	default:
		fail(w, http.StatusNotFound, "Invalid Kubernetes cluster ID.")
	}
}

func (f *fakeVultr) account() provider.Account {
	return provider.Account{ID: "work", Config: map[string]any{"apiURL": f.srv.URL}}
}

func secrets(key string) map[string]string { return map[string]string{"api_key": key} }

func TestList(t *testing.T) {
	f := newFakeVultr(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(), Secrets: secrets(goodKey)})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("List = %d clusters, want 2 (both pages, without the one with no endpoint): %+v", len(clusters), clusters)
	}

	prod := clusters[0]
	if prod.ID != activeID || prod.Name != "prod" || prod.Region != "lax" || prod.Version != "v1.34.1+1" ||
		prod.Endpoints.Public != "https://"+activeID+".vultr-k8s.com:6443" {
		t.Fatalf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the kubeconfig's certificate-authority-data", prod.CAData)
	}
	c := prod.Credential
	if c == nil || c.Type != provider.CredentialClientCert || string(c.CertData) != string(f.certPEM) || string(c.KeyData) != string(f.keyPEM) {
		t.Fatalf("prod credential = %+v, want the kubeconfig's client certificate", c)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Errorf("prod credential expires %v, want the certificate's NotAfter %v", c.ExpiresAt, f.notAfter)
	}
	if prod.Labels["ha"] != "true" || prod.Labels["gpu"] != "true" {
		t.Errorf("prod labels = %v, want ha and gpu", prod.Labels)
	}

	staging := clusters[1]
	if staging.ID != pendingID || staging.Endpoints.Public != "https://"+pendingID+".vultr-k8s.com:6443" ||
		staging.CAData != nil || staging.Credential != nil || len(staging.Labels) != 0 {
		t.Fatalf("a pending cluster is listed by endpoint alone: %+v", staging)
	}
}

func TestCredential(t *testing.T) {
	f := newFakeVultr(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: activeID, Secrets: secrets(goodKey)})
	if err != nil || c.Type != provider.CredentialClientCert || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Fatalf("Credential(%s) = %+v, %v, want the client certificate expiring %v", activeID, c, err, f.notAfter)
	}
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: "gone", Secrets: secrets(goodKey)})
	if provider.CodeOf(err) != provider.NotFound {
		t.Fatalf("Credential(gone) = %v, want NotFound", err)
	}
}

func TestParseKubeconfig(t *testing.T) {
	f := newFakeVultr(t)
	raw, err := base64.StdEncoding.DecodeString(f.kubeconfig(activeID))
	if err != nil {
		t.Fatal(err)
	}
	tokenOnly := strings.Replace(string(raw), "    client-certificate-data:", "    token: x\n    other:", 1)
	badCert := strings.Replace(string(raw), b64(f.certPEM), b64([]byte("not a certificate")), 1)
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"vke kubeconfig", string(raw), ""},
		{"no client certificate", tokenOnly, "no client certificate"},
		{"certificate not PEM", badCert, "not PEM"},
		{"no users", "apiVersion: v1\nkind: Config\nclusters: []\n", "no cluster or user"},
		{"not yaml", "{", "parsing kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc, err := parseKubeconfig([]byte(tt.raw))
			if tt.wantErr == "" {
				if err != nil || kc.server != "https://"+activeID+".vultr-k8s.com:6443" || string(kc.ca) != caPEM {
					t.Fatalf("parseKubeconfig = %+v, %v", kc, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseKubeconfig = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"c.vultr-k8s.com":              "https://c.vultr-k8s.com:6443",
		"c.vultr-k8s.com:443":          "https://c.vultr-k8s.com:443",
		"https://c.vultr-k8s.com:6443": "https://c.vultr-k8s.com:6443",
	} {
		if got := endpointURL(in); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	f := newFakeVultr(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		key     string
		code    provider.Code
		message string
		hint    string
	}{
		{"bad key", "nope", provider.Unauthenticated, "Invalid API token.", "access control"},
		{"no Kubernetes permission", userKey, provider.PermissionDenied, "Unauthorized access.", "Account > Users"},
		{"rate limited", limitedKey, provider.RateLimited, "Rate limit reached", "wait a few seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(), Secrets: secrets(tt.key)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: activeID, Secrets: secrets(tt.key)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	if _, err := New().List(ctx, provider.ListRequest{Account: f.account()}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without a key = %v, want Unauthenticated", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com"}}
	if _, err := New().List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets(goodKey)}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Errorf("List over plain http to a remote host = %v, want InvalidConfig", err)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFakeVultr(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-vultr")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-vultr").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(),
		Secrets:         secrets(goodKey),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the vultr plugin does not conform")
	}
}
