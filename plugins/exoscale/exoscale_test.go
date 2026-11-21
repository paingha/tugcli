package exoscale

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
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
	apiKey     = "EXO29147e9f89102b7ac1e88514" // the key in the signature documentation
	apiSecret  = "tug-test-secret"
	deniedKey  = "EXO00000000000000000000denied" // a key whose role does not allow sks
	limitedKey = "EXO00000000000000000000limited"

	prodID     = "a02baf5a-a3e4-49a0-857b-8a08d276c1c0"
	stagingID  = "2f4c6c3b-0b5e-4e63-8a3d-6b2a1c0d9e8f"
	creatingID = "7e1d9c8b-6a5f-4e3d-2c1b-0a9f8e7d6c5b"
	fraID      = "c3b2a190-8f7e-4d6c-b5a4-938271605f4e"

	gpuTypeID      = "4fb1f4b8-35b5-4c1a-a3c5-4a5c4a8a1d8e"
	standardTypeID = "b6cd1ff5-3a2f-4e9d-a4d1-8988c1191fe8"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIBfjCCASOgAwIBAgIRAPv0Q5Q9mUxWXZ5CXjFXTRYwCgYIKoZIzj0EAwIwGzEZ\n-----END CERTIFICATE-----\n"

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestSign(t *testing.T) {
	// The messages of the examples in the signature documentation. The
	// expected signatures were computed with Python's hmac module for the
	// secret tug-test-secret.
	expires := time.Unix(1599140767, 0)
	s := &signer{key: apiKey, secret: []byte(apiSecret), now: func() time.Time { return expires.Add(-requestLifetime) }}
	tests := []struct {
		method, url, body string
		want              string
	}{
		{http.MethodGet, "https://api-ch-gva-2.exoscale.com/v2/resource/a02baf5a-a3e4-49a0-857b-8a08d276c1c0?p2=v2&p1=v1", "",
			"EXO2-HMAC-SHA256 credential=" + apiKey + ",signed-query-args=p1;p2,expires=1599140767,signature=g4b6Dyd3oxxBMHSaNhNZvGdvcZ+8X58Jz3h7ERCoBwg="},
		{http.MethodPost, "https://api-ch-gva-2.exoscale.com/v2/security-group", `{"name": "my-security-group"}`,
			"EXO2-HMAC-SHA256 credential=" + apiKey + ",expires=1599140767,signature=rMF+Did3YLRD1xLZO2COmGC4xoYi15n6nnErPoIfKn8="},
	}
	for _, tt := range tests {
		var body io.Reader
		if tt.body != "" {
			body = strings.NewReader(tt.body)
		}
		req, err := http.NewRequest(tt.method, tt.url, body)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.sign(req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != tt.want {
			t.Errorf("sign(%s %s) Authorization = %q, want %q", tt.method, tt.url, got, tt.want)
		}
		if req.Body != nil {
			if b, _ := io.ReadAll(req.Body); string(b) != tt.body {
				t.Errorf("sign(%s %s) left the body %q, want %q", tt.method, tt.url, b, tt.body)
			}
		}
	}
}

// verify checks a request's signature the way the Python reference
// implementation makes it, and returns the API key.
func verify(r *http.Request, body []byte, secrets map[string]string) (string, bool) {
	scheme, rest, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "EXO2-HMAC-SHA256" {
		return "", false
	}
	pragmas := map[string]string{}
	for p := range strings.SplitSeq(rest, ",") {
		k, v, _ := strings.Cut(p, "=")
		pragmas[k] = v
	}
	secret, ok := secrets[pragmas["credential"]]
	expires, err := strconv.ParseInt(pragmas["expires"], 10, 64)
	if !ok || err != nil || time.Unix(expires, 0).Before(time.Now()) {
		return "", false
	}
	q := r.URL.Query()
	var names, values []string
	for name := range q {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, n := range names {
		values = append(values, q.Get(n))
	}
	if strings.Join(names, ";") != pragmas["signed-query-args"] {
		return "", false
	}
	msg := strings.Join([]string{r.Method + " " + r.URL.EscapedPath(), string(body), strings.Join(values, ""), "", pragmas["expires"]}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return pragmas["credential"], hmac.Equal([]byte(pragmas["signature"]), []byte(b64(mac.Sum(nil))))
}

// fakeExoscale serves the parts of the API v2 the plugin uses, one zone per
// path prefix, with bodies shaped like the schemas in its OpenAPI document.
type fakeExoscale struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	requests   []string
	kubeconfig []map[string]any // bodies of kubeconfig requests
}

func newFake(t *testing.T) *fakeExoscale {
	t.Helper()
	f := &fakeExoscale{t: t}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func clusterJSON(zone, id, name, state, level string, labels map[string]string, typeIDs ...string) string {
	endpoint := "https://" + id + ".sks-" + zone + ".exo.io:443"
	if state == "creating" {
		endpoint = ""
	}
	pools := make([]string, len(typeIDs))
	for i, tid := range typeIDs {
		pools[i] = fmt.Sprintf(`{"id":"pool-%d","name":"pool-%d","state":"running","size":2,"disk-size":50,"version":"1.36.0",
		  "instance-type":{"id":%q},"template":{"id":"e3c5e4b1-0000-4000-8000-000000000001"},"instance-pool":{"id":"e3c5e4b1-0000-4000-8000-000000000002"},
		  "instance-prefix":"pool","created-at":"2026-10-01T09:00:00Z"}`, i, i, tid)
	}
	labelJSON, _ := json.Marshal(labels)
	return fmt.Sprintf(`{"id":%q,"name":%q,"description":"","labels":%s,"cni":"calico","auto-upgrade":false,"state":%q,
	  "level":%q,"version":"1.36.0","endpoint":%q,"created-at":"2026-10-01T09:00:00Z","nodepools":[%s],
	  "addons":["exoscale-cloud-controller","metrics-server"],"enable-kube-proxy":true,"enable-operators-ca":true,
	  "default-security-group-id":"e3c5e4b1-0000-4000-8000-000000000003"}`,
		id, name, labelJSON, state, level, endpoint, strings.Join(pools, ","))
}

func fail(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"message":%q}`, message)
}

func (f *fakeExoscale) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	key, ok := verify(r, body, map[string]string{apiKey: apiSecret, deniedKey: apiSecret, limitedKey: apiSecret})
	switch {
	case !ok:
		fail(w, http.StatusForbidden, "Invalid request signature")
		return
	case key == deniedKey:
		fail(w, http.StatusForbidden, "Forbidden: the IAM role of this key does not allow sks")
		return
	case key == limitedKey:
		w.Header().Set("Retry-After", "25")
		fail(w, http.StatusTooManyRequests, "Too many requests, please slow down")
		return
	}

	zone, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/v2")
	switch {
	case r.Method == http.MethodGet && path == "/zone":
		fmt.Fprint(w, `{"zones":[{"name":"ch-gva-2","api-endpoint":"https://api-ch-gva-2.exoscale.com/v2","sos-endpoint":"https://sos-ch-gva-2.exo.io"},
		  {"name":"de-fra-1","api-endpoint":"https://api-de-fra-1.exoscale.com/v2","sos-endpoint":"https://sos-de-fra-1.exo.io"},
		  {"name":"at-vie-1","api-endpoint":"https://api-at-vie-1.exoscale.com/v2","sos-endpoint":"https://sos-at-vie-1.exo.io"}]}`)
	case r.Method == http.MethodGet && path == "/sks-cluster" && zone == "ch-gva-2":
		fmt.Fprintf(w, `{"sks-clusters":[%s,%s,%s]}`,
			clusterJSON(zone, prodID, "prod", "running", "pro", map[string]string{"env": "prod", "Team": "payments"}, standardTypeID, gpuTypeID),
			clusterJSON(zone, stagingID, "staging", "updating", "starter", nil, standardTypeID),
			clusterJSON(zone, creatingID, "new", "creating", "starter", nil))
	case r.Method == http.MethodGet && path == "/sks-cluster" && zone == "de-fra-1":
		fmt.Fprintf(w, `{"sks-clusters":[%s]}`, clusterJSON(zone, fraID, "fra", "running", "starter", nil))
	case r.Method == http.MethodGet && path == "/sks-cluster":
		fmt.Fprint(w, `{"sks-clusters":[]}`)
	case r.Method == http.MethodGet && path == "/instance-type":
		fmt.Fprintf(w, `{"instance-types":[
		  {"id":%q,"size":"small","family":"gpu3","cpus":12,"gpus":1,"authorized":true,"memory":60129542144,"zones":["ch-gva-2"]},
		  {"id":%q,"size":"medium","family":"standard","cpus":2,"gpus":0,"authorized":true,"memory":4294967296,"zones":["ch-gva-2"]}]}`,
			gpuTypeID, standardTypeID)
	case r.Method == http.MethodGet && path == "/sks-cluster/"+fraID+"/authority/control-plane/cert":
		fail(w, http.StatusInternalServerError, "Internal error")
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/authority/control-plane/cert"):
		fmt.Fprintf(w, `{"cacert":%q}`, b64([]byte(caPEM)))
	case r.Method == http.MethodPost && path == "/sks-cluster-kubeconfig/"+prodID && zone == "ch-gva-2":
		f.generateKubeconfig(w, body)
	default:
		fail(w, http.StatusNotFound, "Resource not found")
	}
}

// generateKubeconfig answers like the API: a kubeconfig, in base64, whose
// client certificate has the requested user, groups and lifetime.
func (f *fakeExoscale) generateKubeconfig(w http.ResponseWriter, body []byte) {
	var req struct {
		User   string   `json:"user"`
		Groups []string `json:"groups"`
		TTL    int64    `json:"ttl"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.User == "" || len(req.Groups) == 0 {
		fail(w, http.StatusBadRequest, "user and groups are required")
		return
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	f.mu.Lock()
	f.kubeconfig = append(f.kubeconfig, raw)
	f.mu.Unlock()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: req.User, Organization: req.Groups},
		NotBefore:    now,
		NotAfter:     now.Add(time.Duration(req.TTL) * time.Second),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		f.t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		f.t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster:
    certificate-authority-data: %s
    server: https://%s.sks-ch-gva-2.exo.io:443
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
contexts:
- name: prod
  context:
    cluster: prod
    user: %[3]s
current-context: prod
`, b64([]byte(caPEM)), prodID, req.User, b64(cert), b64(keyPEM))
	fmt.Fprintf(w, `{"kubeconfig":%q}`, b64([]byte(kc)))
}

func (f *fakeExoscale) sent(prefix string) []string {
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

func (f *fakeExoscale) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL + "/{zone}/v2"}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

func secrets(key string) map[string]string {
	return map[string]string{SecretAPIKey: key, SecretAPISecret: apiSecret}
}

func TestList(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(apiKey)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	want := []string{"ch-gva-2/" + prodID, "ch-gva-2/" + stagingID, "de-fra-1/" + fraID}
	if !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v (every zone, without the cluster being created)", ids, want)
	}

	prod := clusters[0]
	if prod.Name != "prod" || prod.Region != "ch-gva-2" || prod.Version != "1.36.0" || prod.Endpoints.Public != "https://"+prodID+".sks-ch-gva-2.exo.io:443" {
		t.Errorf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM || prod.Credential != nil {
		t.Errorf("prod CA = %q, credential %+v; want the control plane CA and no credential", prod.CAData, prod.Credential)
	}
	wantLabels := map[string]string{"env": "prod", "team": "payments", "ha": "true", "gpu": "true"}
	if fmt.Sprint(prod.Labels) != fmt.Sprint(wantLabels) {
		t.Errorf("prod labels = %v, want %v", prod.Labels, wantLabels)
	}
	if staging := clusters[1]; len(staging.Labels) != 0 {
		t.Errorf("staging labels = %v, want none", staging.Labels)
	}
	if fra := clusters[2]; fra.CAData != nil || fra.Region != "de-fra-1" {
		t.Errorf("fra = %+v, want it listed without a CA while the CA cannot be read", fra)
	}
	if got := f.sent("POST"); len(got) != 0 {
		t.Errorf("List minted credentials: %v", got)
	}
	if got := f.sent("GET /ch-gva-2/v2/instance-type"); len(got) != 1 {
		t.Errorf("instance type requests = %v, want one for the zone with node pools", got)
	}
}

func TestListZones(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigZones: "DE-FRA-1, de-fra-1"}), Secrets: secrets(apiKey)})
	if err != nil || len(clusters) != 1 || clusters[0].ID != "de-fra-1/"+fraID {
		t.Fatalf("List with zones=de-fra-1 = %+v, %v; want the de-fra-1 cluster only", clusters, err)
	}
	if got := f.sent("GET /ch-gva-2/"); len(got) != 0 {
		t.Errorf("List with zones set asked ch-gva-2: %v", got)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	before := time.Now()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "ch-gva-2/" + prodID, Secrets: secrets(apiKey)})
	if err != nil || c.Type != provider.CredentialClientCert || c.Validate() != nil || c.ExpiresAt == nil {
		t.Fatalf("Credential(prod) = %+v, %v; want a client certificate with an expiry", c, err)
	}
	if left := c.ExpiresAt.Sub(before); left < 59*time.Minute || left > time.Hour+time.Minute {
		t.Errorf("credential expires in %s, want about an hour", left)
	}
	block, _ := pem.Decode(c.CertData)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.Subject.CommonName != "tug" || !slices.Equal(cert.Subject.Organization, []string{"system:masters"}) {
		t.Errorf("certificate subject = %+v, %v; want tug in system:masters", cert.Subject, err)
	}

	_, err = New().Credential(ctx, provider.CredentialRequest{
		Account: f.account(map[string]any{ConfigUser: "alice", ConfigGroups: "dev, ops, dev"}), ClusterID: "ch-gva-2/" + prodID, Secrets: secrets(apiKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.kubeconfig[len(f.kubeconfig)-1]
	f.mu.Unlock()
	if fmt.Sprint(last) != fmt.Sprint(map[string]any{"user": "alice", "groups": []any{"dev", "ops"}, "ttl": float64(3600)}) {
		t.Errorf("kubeconfig request = %v, want user alice, groups dev and ops, and ttl 3600", last)
	}

	for _, id := range []string{"de-fra-1/" + prodID, "ch-gva-2/00000000-0000-0000-0000-000000000000", prodID} {
		_, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: id, Secrets: secrets(apiKey)})
		if pe := provider.AsError(err); pe.Code != provider.NotFound || pe.Hint == "" {
			t.Errorf("Credential(%s) = %+v, want NotFound with a hint", id, pe)
		}
	}
}

func TestCertCredential(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"no users", "kind: Config\n", "no users"},
		{"token user", "users:\n- name: a\n  user: {token: x}\n", "no client certificate"},
		{"certificate not PEM", "users:\n- name: a\n  user: {client-certificate-data: " + b64([]byte("x")) + ", client-key-data: " + b64([]byte("y")) + "}\n", "not PEM"},
		{"no such user", "users:\n- name: a\n  user: {}\ncontexts:\n- name: c\n  context: {user: b}\ncurrent-context: c\n", `no user "b"`},
		{"not yaml", "users: [", "parsing kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := certCredential([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("certCredential = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestZoneURL(t *testing.T) {
	tests := []struct {
		base, want string
		wantErr    bool
	}{
		{DefaultAPIURL, "https://api-de-fra-1.exoscale.com/v2", false},
		{"https://api.example.com/{zone}/v2/", "https://api.example.com/de-fra-1/v2", false},
		{"http://127.0.0.1:8080/v2", "http://127.0.0.1:8080/v2", false},
		{"http://api-{zone}.example.com/v2", "", true},
	}
	for _, tt := range tests {
		got, err := zoneURL(tt.base, "de-fra-1")
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("zoneURL(%q) = %q, %v, want %q", tt.base, got, err, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		secrets map[string]string
		code    provider.Code
		message string
		hint    string
	}{
		{"wrong secret", map[string]string{SecretAPIKey: apiKey, SecretAPISecret: "nope"}, provider.Unauthenticated, "Invalid request signature", "IAM"},
		{"unknown key", secrets("EXOnope"), provider.Unauthenticated, "Invalid request signature", "IAM"},
		{"role without sks", secrets(deniedKey), provider.Unauthenticated, "does not allow sks", "sks service"},
		{"rate limited", secrets(limitedKey), provider.RateLimited, "Too many requests", "wait a minute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: tt.secrets})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			if strings.Contains(pe.Message+pe.Hint, apiSecret) {
				t.Errorf("error %+v contains the API secret", pe)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "ch-gva-2/" + prodID, Secrets: tt.secrets})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(limitedKey)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 25 {
		t.Errorf("List when rate limited: RetryAfterSeconds = %d, want 25 from Retry-After", pe.RetryAfterSeconds)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: map[string]string{SecretAPIKey: apiKey}}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without the secret = %v, want Unauthenticated", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com/{zone}"}}
	if _, err := New().List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets(apiKey)}); provider.CodeOf(err) != provider.InvalidConfig {
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
	bin := filepath.Join(t.TempDir(), "tug-provider-exoscale")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-exoscale").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(map[string]any{ConfigZones: "ch-gva-2"}),
		Secrets:         secrets(apiKey),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the exoscale plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: f.account(map[string]any{ConfigZones: "ch-gva-2"}).Config, Secrets: secrets(apiKey)})
}
