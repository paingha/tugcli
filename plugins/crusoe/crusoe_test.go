package crusoe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// The API key from the example in Crusoe's API reference.
const (
	keyID     = "gYFONy-6QKS1acgUEQrR4Q"
	secretKey = "uZFGf918DmiBUwBWv8lnEg"
)

const (
	projID     = "9c8b7d6e-5f4a-3b2c-1e0f-2a3b4c5d6e7f"
	trainID    = "ab4a6b00-aa5f-408e-a9fb-ac6de5eb45ab"
	researchID = "1d2b3a4c-5e6f-7b9d-8c9a-0e1f2d3b4c5d"
	newID      = "09ae8411-0fbb-411c-898c-2b8f19622ae1"
)

var secrets = map[string]string{SecretAccessKeyID: keyID, SecretSecretKey: secretKey}

// The clusters follow KubernetesCluster in the v1 API's OpenAPI document:
// train uses the admin certificate, research is set up for OIDC, and the
// third is still being created.
var clusterJSON = map[string]string{
	trainID: `{"id":"` + trainID + `","name":"train-us-east","location":"us-east1-a","version":"1.35.8",
	  "configuration":"ha","subnet_id":"6f8e2a1b-7b1d-4c8e-a9f2-8e3d6c1f2a0c","project_id":"` + projID + `",
	  "node_pools":["np-h100","np-cpu"],"add_ons":["nvidia_gpu_operator","crusoe_csi"],"state":"STATE_RUNNING",
	  "dns_name":"train-us-east.crusoecloudcompute.com","created_at":"2026-10-03T19:58:34Z","updated_at":"2026-10-03T19:58:34Z",
	  "kubelogin_args":{},"private":false,"control_plane_generation":2}`,
	researchID: `{"id":"` + researchID + `","name":"research","location":"eu-iceland1-a","version":"1.34.5-cmk.3",
	  "configuration":"ha","subnet_id":"09ae8411-0fbb-411c-898c-000000000001","project_id":"` + projID + `",
	  "node_pools":["np-c1a"],"add_ons":[],"state":"STATE_RUNNING",
	  "dns_name":"research.crusoecloudcompute.com","created_at":"2026-05-01T10:00:00Z","updated_at":"2026-05-01T10:00:00Z",
	  "kubelogin_args":{"force-refresh":"","oidc-extra-scope":"email groups"},"private":true,"control_plane_generation":1,
	  "auth_config":{"oidc":{"issuer_url":"https://idp.example.com","client_id":"kubernetes","groups_claim":"groups","username_claim":"email","username_prefix":"oidc:"}}}`,
	newID: `{"id":"` + newID + `","name":"creating","location":"us-east1-a","version":"1.35","configuration":"ha",
	  "subnet_id":"6f8e2a1b-7b1d-4c8e-a9f2-8e3d6c1f2a0c","project_id":"` + projID + `","node_pools":[],"add_ons":[],
	  "state":"STATE_PROVISIONING","dns_name":"","created_at":"2026-11-20T09:00:00Z","updated_at":"2026-11-20T09:00:00Z","kubelogin_args":{}}`,
}

const nodePoolsJSON = `{"items":[
  {"id":"np-h100","name":"h100","count":4,"project_id":"` + projID + `","cluster_id":"` + trainID + `","type":"h100-80gb-sxm-ib.8x","state":"STATE_RUNNING"},
  {"id":"np-cpu","name":"system","count":2,"project_id":"` + projID + `","cluster_id":"` + trainID + `","type":"c1a.8x","state":"STATE_RUNNING"},
  {"id":"np-c1a","name":"cpu","count":3,"project_id":"` + projID + `","cluster_id":"` + researchID + `","type":"c1a.16x","state":"STATE_RUNNING"}
]}`

// testCert returns a self-signed admin client certificate and its key.
func testCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kubernetes-admin", Organization: []string{"system:masters"}},
		NotBefore:    notAfter.Add(-48 * time.Hour),
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

// fakeCrusoe is the part of the Crusoe Cloud API v1 the plugin calls. It
// checks every request's signature as the API reference describes.
type fakeCrusoe struct {
	t        *testing.T
	srv      *httptest.Server
	projects string
	// denyAdmin refuses admin certificates, as for a user who is not an
	// organisation admin; legacy answers in the form without a kubeconfig.
	denyAdmin, legacy, rateLimit bool
	requests                     atomic.Int32
	cert, key                    []byte
	notAfter                     time.Time
}

func newFake(t *testing.T) *fakeCrusoe {
	t.Helper()
	f := &fakeCrusoe{t: t, projects: `{"items":[{"id":"` + projID + `","name":"ml","organization_id":"ab4a6b00-0000-408e-a9fb-ac6de5eb45ab","relation":"owner"}]}`}
	f.notAfter = time.Now().Add(24 * time.Hour).Truncate(time.Second).UTC()
	f.cert, f.key = testCert(t, f.notAfter)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/organizations/projects", f.listProjects)
	mux.HandleFunc("GET /v1/projects/{project}/kubernetes/clusters", f.inProject(f.listClusters))
	mux.HandleFunc("GET /v1/projects/{project}/kubernetes/clusters/{cluster}", f.inProject(f.getCluster))
	mux.HandleFunc("GET /v1/projects/{project}/kubernetes/nodepools", f.inProject(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, nodePoolsJSON)
	}))
	mux.HandleFunc("POST /v1/projects/{project}/kubernetes/clusters/{cluster}/get-credentials", f.inProject(f.getCredentials))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !signedCorrectly(r):
			apiErr(w, http.StatusUnauthorized, "bad_credential")
		case f.rateLimit:
			w.Header().Set("Retry-After", "30")
			apiErr(w, http.StatusTooManyRequests, "too_many_requests")
		default:
			mux.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// signedCorrectly checks the signature the way the Python sample in the API
// reference makes it.
func signedCorrectly(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ":")
	ts := r.Header.Get("X-Crusoe-Timestamp")
	if _, err := time.Parse(time.RFC3339, ts); err != nil || len(parts) != 3 || parts[0] != "1.0" || parts[1] != keyID {
		return false
	}
	var params []string
	if r.URL.RawQuery != "" {
		params = strings.Split(r.URL.RawQuery, "&")
	}
	slices.Sort(params)
	key, _ := base64.URLEncoding.DecodeString(secretKey + "==")
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n", r.URL.Path, strings.Join(params, "&"), r.Method, ts)
	return hmac.Equal([]byte(parts[2]), []byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))))
}

func apiErr(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":"%d","message":%q}`, status, message)
}

func (f *fakeCrusoe) inProject(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("project") != projID {
			apiErr(w, http.StatusNotFound, "not_found")
			return
		}
		h(w, r)
	}
}

func (f *fakeCrusoe) listProjects(w http.ResponseWriter, r *http.Request) {
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(f.projects), &resp); err != nil {
		f.t.Fatal(err)
	}
	if name := r.URL.Query().Get("project_name"); name != "" {
		resp.Items = slices.DeleteFunc(resp.Items, func(p map[string]any) bool { return p["name"] != name })
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeCrusoe) listClusters(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, `{"items":[%s,%s,%s]}`, clusterJSON[trainID], clusterJSON[researchID], clusterJSON[newID])
}

func (f *fakeCrusoe) getCluster(w http.ResponseWriter, r *http.Request) {
	body, ok := clusterJSON[r.PathValue("cluster")]
	if !ok {
		apiErr(w, http.StatusNotFound, "not_found")
		return
	}
	fmt.Fprint(w, body)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func (f *fakeCrusoe) getCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cluster")
	var cl cmkCluster
	body, ok := clusterJSON[id]
	if !ok {
		apiErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err := json.Unmarshal([]byte(body), &cl); err != nil {
		f.t.Fatal(err)
	}
	server := "https://" + cl.Name + ".crusoecloudcompute.com"
	switch authType := r.URL.Query().Get("auth_type"); {
	case authType == "admin_cert" && f.denyAdmin:
		apiErr(w, http.StatusForbidden, "unauthorized")
	case authType == "admin_cert" && f.legacy:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"cluster_name": cl.Name, "cluster_address": server, "cluster_ca_certificate": string(f.cert),
			"user_name": "kubernetes-admin", "user_client_certificate": string(f.cert), "user_client_key": string(f.key),
		})
	case authType == "admin_cert":
		// An unrelated first context checks that the current one is used.
		kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: other
  cluster: {server: "https://other.example.com"}
- name: %[1]s
  cluster:
    server: %[2]s
    certificate-authority-data: %[3]s
users:
- name: other
  user: {token: other}
- name: %[1]s-admin
  user:
    client-certificate-data: %[3]s
    client-key-data: %[4]s
contexts:
- name: other
  context: {cluster: other, user: other}
- name: %[1]s
  context: {cluster: %[1]s, user: %[1]s-admin}
current-context: %[1]s
`, cl.Name, server, b64(f.cert), b64(f.key))
		_ = json.NewEncoder(w).Encode(map[string]string{"cluster_name": cl.Name, "kube_config": kc})
	case authType == "oidc" && cl.oidc() != nil:
		kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster: {server: %[2]s, certificate-authority-data: %[3]s}
users:
- name: oidc
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kubectl
      args: [oidc-login, get-token, --oidc-issuer-url=https://idp.example.com, --oidc-client-id=kubernetes, "--oidc-extra-scope=email groups"]
contexts:
- name: %[1]s
  context: {cluster: %[1]s, user: oidc}
current-context: %[1]s
`, cl.Name, server, b64(f.cert))
		_ = json.NewEncoder(w).Encode(map[string]string{"cluster_name": cl.Name, "kube_config": kc})
	default:
		apiErr(w, http.StatusBadRequest, "bad_request")
	}
}

func (f *fakeCrusoe) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL + "/v1", ConfigProject: projID}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

func byName(clusters []provider.Cluster) map[string]provider.Cluster {
	out := map[string]provider.Cluster{}
	for _, c := range clusters {
		out[c.Name] = c
	}
	return out
}

func TestSign(t *testing.T) {
	// The inputs of the example in the API reference. Its printed signature
	// cannot be reproduced, so the expected value was computed with the
	// reference's Python sample for the UTC form of its timestamp.
	at, err := time.Parse(time.RFC3339, "2022-03-01T01:23:45+09:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretKey, secretKey + "=="} {
		s, err := newSigner(keyID, secret, func() time.Time { return at })
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodGet, "https://api.cloud.crusoe.ai/v1/capacities?product_name=a100.8x&location=us-northcentral1-a", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.sign(req); err != nil {
			t.Fatal(err)
		}
		if got, want := req.Header.Get("X-Crusoe-Timestamp"), "2022-02-28T16:23:45Z"; got != want {
			t.Errorf("X-Crusoe-Timestamp = %q, want %q", got, want)
		}
		if got, want := req.Header.Get("Authorization"), "Bearer 1.0:"+keyID+":nOcrY2M_YBltNBzywob_IM6c9gaUNgHQLS3gjxu9ZpM"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
	}
}

func TestList(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("List = %d clusters, want 2 (the one being created is skipped): %+v", len(clusters), clusters)
	}
	got := byName(clusters)
	train := got["train-us-east"]
	if train.ID != trainID || train.Region != "us-east1-a" || train.Version != "1.35.8" ||
		train.Endpoints.Public != "https://train-us-east.crusoecloudcompute.com" || train.Endpoints.Private != "" ||
		string(train.CAData) != string(f.cert) || train.Labels["gpu"] != "true" {
		t.Errorf("train = %+v", train)
	}
	if c := train.Credential; c == nil || c.Type != provider.CredentialClientCert || string(c.CertData) != string(f.cert) ||
		string(c.KeyData) != string(f.key) || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Errorf("train credential = %+v, want the admin certificate until %s", c, f.notAfter)
	}
	research := got["research"]
	if research.Endpoints.Private != "https://research.crusoecloudcompute.com" || research.Endpoints.Public != "" || research.Labels["gpu"] != "" {
		t.Errorf("research = %+v", research)
	}
	if c := research.Credential; c == nil || c.Type != provider.CredentialOIDC || c.Issuer != "https://idp.example.com" ||
		c.ClientID != "kubernetes" || !slices.Equal(c.Scopes, []string{"email", "groups"}) || c.CertData != nil {
		t.Errorf("research credential = %+v, want OIDC with the kubelogin scopes", c)
	}
}

func TestListAuthModes(t *testing.T) {
	f := newFake(t)
	tests := []struct {
		auth string
		want map[string]provider.CredentialType
	}{
		{AuthAuto, map[string]provider.CredentialType{"train-us-east": provider.CredentialClientCert, "research": provider.CredentialOIDC}},
		{AuthAdminCert, map[string]provider.CredentialType{"train-us-east": provider.CredentialClientCert, "research": provider.CredentialClientCert}},
		{AuthOIDC, map[string]provider.CredentialType{"research": provider.CredentialOIDC}},
	}
	for _, tt := range tests {
		t.Run(tt.auth, func(t *testing.T) {
			clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigAuth: tt.auth}), Secrets: secrets})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]provider.CredentialType{}
			for _, c := range clusters {
				got[c.Name] = c.Credential.Type
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("List with auth=%s: credentials %v, want %v", tt.auth, got, tt.want)
			}
		})
	}
}

func TestListWithoutAdminRole(t *testing.T) {
	f := newFake(t)
	f.denyAdmin = true
	_, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets})
	if pe := provider.AsError(err); pe.Code != provider.PermissionDenied || !strings.Contains(pe.Hint, "auth=oidc") {
		t.Errorf("List = %v (hint %q), want PermissionDenied with a hint about auth=oidc", err, pe.Hint)
	}
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigAuth: AuthOIDC}), Secrets: secrets})
	if err != nil || len(clusters) != 1 || clusters[0].ID != researchID {
		t.Errorf("List with auth=oidc = %+v, %v; want research only", clusters, err)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	tests := []struct {
		name    string
		auth    string
		cluster string
		want    provider.CredentialType
		code    provider.Code
	}{
		{"admin certificate", "", trainID, provider.CredentialClientCert, ""},
		{"oidc", "", researchID, provider.CredentialOIDC, ""},
		{"admin certificate forced", AuthAdminCert, researchID, provider.CredentialClientCert, ""},
		{"oidc without configuration", AuthOIDC, trainID, "", provider.InvalidConfig},
		{"unknown cluster", "", "00000000-0000-0000-0000-000000000000", "", provider.NotFound},
		{"bad auth mode", "saml", trainID, "", provider.InvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New().Credential(context.Background(), provider.CredentialRequest{
				Account: f.account(map[string]any{ConfigAuth: tt.auth}), ClusterID: tt.cluster, Secrets: secrets,
			})
			if tt.code != "" {
				if provider.CodeOf(err) != tt.code {
					t.Fatalf("Credential = %+v, %v; want %s", c, err, tt.code)
				}
				return
			}
			if err != nil || c.Type != tt.want || c.Validate() != nil {
				t.Fatalf("Credential = %+v, %v; want %s", c, err, tt.want)
			}
		})
	}
}

func TestLegacyCredentials(t *testing.T) {
	f := newFake(t)
	f.legacy = true
	c, err := New().Credential(context.Background(), provider.CredentialRequest{Account: f.account(nil), ClusterID: trainID, Secrets: secrets})
	if err != nil || c.Type != provider.CredentialClientCert || string(c.CertData) != string(f.cert) || c.ExpiresAt == nil {
		t.Fatalf("Credential = %+v, %v; want the legacy client certificate", c, err)
	}
}

func TestBadKeys(t *testing.T) {
	f := newFake(t)
	tests := []struct {
		name     string
		secrets  map[string]string
		requests bool
	}{
		{"wrong secret key", map[string]string{SecretAccessKeyID: keyID, SecretSecretKey: "AAAAAAAAAAAAAAAAAAAAAA"}, true},
		{"wrong access key", map[string]string{SecretAccessKeyID: "nope", SecretSecretKey: secretKey}, true},
		{"secret key not base64", map[string]string{SecretAccessKeyID: keyID, SecretSecretKey: "not a key!"}, false},
		{"no secrets", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.requests.Load()
			_, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: tt.secrets})
			if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Hint, "Cloud API keys") {
				t.Errorf("List = %v (hint %q), want Unauthenticated with a hint", err, pe.Hint)
			}
			if sent := f.requests.Load() != before; sent != tt.requests {
				t.Errorf("List sent requests: %v, want %v", sent, tt.requests)
			}
			if tt.secrets != nil && strings.Contains(provider.AsError(err).Message, tt.secrets[SecretSecretKey]) {
				t.Errorf("error %q contains the secret key", err)
			}
		})
	}
}

func TestRateLimited(t *testing.T) {
	f := newFake(t)
	f.rateLimit = true
	_, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets})
	if pe := provider.AsError(err); pe.Code != provider.RateLimited || pe.RetryAfterSeconds != 30 || pe.Hint == "" {
		t.Errorf("List = %+v, want RateLimited after 30 seconds with a hint", pe)
	}
}

func TestProject(t *testing.T) {
	const two = `{"items":[{"id":"` + projID + `","name":"ml"},{"id":"11111111-2222-3333-4444-555555555555","name":"web"}]}`
	tests := []struct {
		name     string
		projects string
		project  string
		code     provider.Code
		hint     string
	}{
		{"by name", two, "ml", "", ""},
		{"the only project", "", "", "", ""},
		{"several projects", two, "", provider.InvalidConfig, "web (11111111-2222-3333-4444-555555555555)"},
		{"unknown name", two, "infra", provider.NotFound, "Projects page"},
		{"no projects", `{"items":[]}`, "", provider.NotFound, "add you to a project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if tt.projects != "" {
				f.projects = tt.projects
			}
			clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigProject: tt.project}), Secrets: secrets})
			if tt.code == "" {
				if err != nil || len(clusters) != 2 {
					t.Fatalf("List = %d clusters, %v; want 2", len(clusters), err)
				}
				return
			}
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %v (hint %q), want %s with a hint about %q", err, pe.Hint, tt.code, tt.hint)
			}
		})
	}
}

func TestGPUType(t *testing.T) {
	for typ, want := range map[string]bool{
		"h100-80gb-sxm-ib.8x":  true,
		"a100.8x":              true,
		"l40s-48gb.1x":         true,
		"mi300x-192gb-ib.8x":   true,
		"gb200-186gb.4x":       true,
		"c1a.8x":               false,
		"c2a.172x":             false,
		"s1a.120x":             false,
		"s2a.80x":              false,
		"":                     false,
		"b200-180gb-sxm-ib.8x": true,
	} {
		if got := gpuType(typ); got != want {
			t.Errorf("gpuType(%q) = %v, want %v", typ, got, want)
		}
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
	bin := filepath.Join(t.TempDir(), "tug-provider-crusoe")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-crusoe").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(nil),
		Secrets:         secrets,
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the crusoe plugin does not conform")
	}
}
