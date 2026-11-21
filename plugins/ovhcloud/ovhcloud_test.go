package ovhcloud

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
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
)

// Keys the fake API knows. They are made up.
const (
	appKey      = "7kbG7Bk7S9Nt7ZSV"
	appSecret   = "EXEgWIz07P0HYwtQDs7cNIqCiQaWSuHF"
	consumerKey = "MtSwSrPpNjqfVSmJhLbPyr2i45lSwPU1"
	// limitedKey may not generate kubeconfigs.
	limitedKey = "G7vbDEkPHrU8Vt4nELmIpVqTnF4lCVR2"
	// burstKey is over the rate limit.
	burstKey = "S8jCwUTu8ZcMJpxBbQBoqrzD7Kdmy3Mi"
)

const (
	projectID = "5b4d6a2e1c3f4e5d9a8b7c6d5e4f3a2b"
	otherID   = "0f9e8d7c6b5a49382716a5b4c3d2e1f0"
	prodID    = "9b8f7d36-6b30-4a1c-8a93-1c4dbb6a2f10"
	newID     = "2a1d9c4e-0f5b-4e2a-9d7c-3b6e8f1a2c45"
	maintID   = "4c3e2a10-5d6f-4b7a-8e9c-0a1b2c3d4e5f"
)

// skew is how far the fake API's clock is behind the test's, to check
// that signatures carry the API's time.
const skew = 17 * time.Minute

var testCA = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVqgAwIBAgIUOVH\n-----END CERTIFICATE-----\n"

// clientCert returns a client certificate that expires at notAfter, and
// its key.
func clientCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "kubernetes-admin", Organization: []string{"system:masters"}},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
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

// fakeOVH serves the OVHcloud API calls the plugin makes, under /1.0, and
// checks every signed request's signature as the API guide describes.
type fakeOVH struct {
	t          *testing.T
	srv        *httptest.Server
	projects   []string
	cert, key  []byte
	notAfter   time.Time
	kubeconfig int // kubeconfigs generated
	mu         sync.Mutex
}

func newFake(t *testing.T) *fakeOVH {
	t.Helper()
	f := &fakeOVH{t: t, projects: []string{projectID}}
	f.notAfter = time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second).UTC()
	f.cert, f.key = clientCert(t, f.notAfter)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /1.0/cloud/project", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(f.projects)
	})
	mux.HandleFunc("GET /1.0/cloud/project/{project}/kube", f.inProject(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[%q,%q,%q]`, prodID, newID, maintID)
	}))
	mux.HandleFunc("GET /1.0/cloud/project/{project}/kube/{id}", f.inProject(f.getCluster))
	mux.HandleFunc("POST /1.0/cloud/project/{project}/kube/{id}/kubeconfig", f.inProject(f.generateKubeconfig))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Ovh-Queryid", "EU.ext-3.6620a1b2.1234.0f0e")
		if r.URL.Path == "/1.0/auth/time" {
			fmt.Fprint(w, time.Now().Add(-skew).Unix())
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch ck := r.Header.Get("X-Ovh-Consumer"); {
		case r.Header.Get("X-Ovh-Application") != appKey:
			apiErr(w, http.StatusForbidden, "Client::Forbidden", "INVALID_KEY", "This application key is invalid")
		case ck != consumerKey && ck != limitedKey && ck != burstKey:
			apiErr(w, http.StatusForbidden, "Client::Forbidden", "INVALID_CREDENTIAL", "This credential does not exist")
		case !f.signedCorrectly(r, body, ck):
			apiErr(w, http.StatusBadRequest, "Client::BadRequest::InvalidSignature", "INVALID_SIGNATURE", "Invalid signature")
		case ck == burstKey:
			w.Header().Set("Retry-After", "10")
			apiErr(w, http.StatusTooManyRequests, "Client::TooManyRequests", "", "Too many requests")
		case ck == limitedKey && strings.HasSuffix(r.URL.Path, "/kubeconfig"):
			apiErr(w, http.StatusForbidden, "Client::Forbidden", "NOT_GRANTED_CALL", "This call has not been granted")
		default:
			mux.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// signedCorrectly checks the signature and that its timestamp is the API's
// time rather than the local one.
func (f *fakeOVH) signedCorrectly(r *http.Request, body []byte, ck string) bool {
	ts, err := strconv.ParseInt(r.Header.Get("X-Ovh-Timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0).Add(skew)).Abs() > 5*time.Second {
		f.t.Logf("timestamp %q is not the API's time", r.Header.Get("X-Ovh-Timestamp"))
		return false
	}
	target := "http://" + r.Host + r.RequestURI
	sum := sha1.Sum([]byte(strings.Join([]string{appSecret, ck, r.Method, target, string(body), r.Header.Get("X-Ovh-Timestamp")}, "+")))
	return r.Header.Get("X-Ovh-Signature") == "$1$"+hex.EncodeToString(sum[:])
}

func apiErr(w http.ResponseWriter, status int, class, code, message string) {
	w.WriteHeader(status)
	if code == "" {
		fmt.Fprintf(w, `{"class":%q,"message":%q}`, class, message)
		return
	}
	fmt.Fprintf(w, `{"class":%q,"message":%q,"errorCode":%q,"httpCode":"%d %s"}`, class, message, code, status, http.StatusText(status))
}

func (f *fakeOVH) inProject(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("project") != projectID {
			apiErr(w, http.StatusNotFound, "Client::NotFound", "", "This service does not exist")
			return
		}
		h(w, r)
	}
}

// clusters follow cloud.kube.Cluster in the API's schema.
var clusters = map[string]string{
	prodID: `{"id":"` + prodID + `","region":"GRA11","name":"prod-gra","url":"x7k2qa.c1.gra.k8s.ovh.net","nodesUrl":"x7k2qa.nodes.c1.gra.k8s.ovh.net",
	  "version":"1.31","nextUpgradeVersions":["1.32"],"kubeProxyMode":"iptables","customization":{"apiServer":{"admissionPlugins":{"enabled":["NodeRestriction"],"disabled":[]}}},
	  "status":"READY","updatePolicy":"ALWAYS_UPDATE","isUpToDate":true,"controlPlaneIsUpToDate":true,"privateNetworkId":null,"nodesSubnetId":null,
	  "plan":"standard","createdAt":"2026-06-01T09:12:44Z","updatedAt":"2026-11-02T10:00:03Z","auditLogsSubscribed":false}`,
	newID: `{"id":"` + newID + `","region":"BHS5","name":"just-created","url":"","nodesUrl":"","version":"1.32","status":"INSTALLING",
	  "updatePolicy":"ALWAYS_UPDATE","isUpToDate":true,"controlPlaneIsUpToDate":true,"plan":"free","createdAt":"2026-11-20T09:00:00Z","updatedAt":"2026-11-20T09:00:00Z"}`,
	maintID: `{"id":"` + maintID + `","region":"SBG5","name":"batch","url":"b4n8vd.c2.sbg.k8s.ovh.net","nodesUrl":"b4n8vd.nodes.c2.sbg.k8s.ovh.net",
	  "version":"1.30","status":"MAINTENANCE","updatePolicy":"MINIMAL_DOWNTIME","isUpToDate":false,"controlPlaneIsUpToDate":false,"plan":"free",
	  "createdAt":"2025-02-01T09:00:00Z","updatedAt":"2026-11-20T08:00:00Z"}`,
}

func (f *fakeOVH) getCluster(w http.ResponseWriter, r *http.Request) {
	body, ok := clusters[r.PathValue("id")]
	if !ok {
		apiErr(w, http.StatusNotFound, "Client::NotFound", "", "This service does not exist")
		return
	}
	fmt.Fprint(w, body)
}

func (f *fakeOVH) generateKubeconfig(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("id") {
	case prodID:
	case maintID:
		apiErr(w, http.StatusBadRequest, "Client::BadRequest", "", "Cluster is in maintenance")
		return
	default:
		apiErr(w, http.StatusNotFound, "Client::NotFound", "", "This service does not exist")
		return
	}
	f.mu.Lock()
	f.kubeconfig++
	f.mu.Unlock()
	enc := base64.StdEncoding.EncodeToString
	content := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ` + enc([]byte(testCA)) + `
    server: https://x7k2qa.c1.gra.k8s.ovh.net
  name: prod-gra
contexts:
- context:
    cluster: prod-gra
    user: kubernetes-admin-prod-gra
  name: kubernetes-admin@prod-gra
current-context: kubernetes-admin@prod-gra
kind: Config
preferences: {}
users:
- name: kubernetes-admin-prod-gra
  user:
    client-certificate-data: ` + enc(f.cert) + `
    client-key-data: ` + enc(f.key) + `
`
	_ = json.NewEncoder(w).Encode(map[string]string{"content": content})
}

func (f *fakeOVH) account(cfg map[string]any) provider.Account {
	c := map[string]any{"apiURL": f.srv.URL + "/1.0"}
	for k, v := range cfg {
		c[k] = v
	}
	return provider.Account{ID: "work", Config: c}
}

func secrets(ck string) map[string]string {
	return map[string]string{SecretApplicationKey: appKey, SecretApplicationSecret: appSecret, SecretConsumerKey: ck}
}

func TestList(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(consumerKey)})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("List = %+v, want prod-gra and batch, without the cluster being installed", clusters)
	}
	prod, batch := clusters[0], clusters[1]
	if prod.ID != prodID || prod.Name != "prod-gra" || prod.Region != "GRA11" || prod.Version != "1.31" ||
		prod.Endpoints.Public != "https://x7k2qa.c1.gra.k8s.ovh.net" || string(prod.CAData) != testCA || prod.Labels["plan"] != "standard" {
		t.Errorf("prod = %+v", prod)
	}
	c := prod.Credential
	if c == nil || c.Type != provider.CredentialClientCert || string(c.CertData) != string(f.cert) || string(c.KeyData) != string(f.key) ||
		c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Fatalf("prod credential = %+v, want the kubeconfig's client certificate until it expires", c)
	}
	if batch.Endpoints.Public != "https://b4n8vd.c2.sbg.k8s.ovh.net" || batch.CAData != nil || batch.Credential != nil {
		t.Errorf("batch = %+v, want the cluster in maintenance listed by its URL alone", batch)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kubeconfig != 1 {
		t.Errorf("List generated %d kubeconfigs, want 1, for the cluster that is ready", f.kubeconfig)
	}
}

func TestProject(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	f.projects = []string{projectID, otherID}
	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(consumerKey)})
	if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || !strings.Contains(pe.Hint, projectID) || !strings.Contains(pe.Hint, otherID) {
		t.Errorf("List with two projects = %+v, want InvalidConfig naming both", pe)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account(map[string]any{ConfigProject: projectID}), Secrets: secrets(consumerKey)}); err != nil {
		t.Errorf("List with the project set = %v", err)
	}

	f.projects = []string{}
	_, err = New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(consumerKey)})
	if provider.CodeOf(err) != provider.NotFound {
		t.Errorf("List without projects = %v, want NotFound", err)
	}

	_, err = New().List(ctx, provider.ListRequest{Account: f.account(map[string]any{ConfigProject: otherID}), Secrets: secrets(consumerKey)})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || !strings.Contains(pe.Message, "This service does not exist") || !strings.Contains(pe.Hint, ConfigProject) {
		t.Errorf("List in an unknown project = %+v, want NotFound with a hint", pe)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: prodID, Secrets: secrets(consumerKey)})
	if err != nil || c.Type != provider.CredentialClientCert || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.notAfter) {
		t.Fatalf("Credential(prod) = %+v, %v, want the client certificate", c, err)
	}
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "gone", Secrets: secrets(consumerKey)})
	if provider.CodeOf(err) != provider.NotFound {
		t.Errorf("Credential(gone) = %v, want NotFound", err)
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	bad := func(key, value string) map[string]string {
		s := secrets(consumerKey)
		s[key] = value
		return s
	}
	tests := []struct {
		name    string
		secrets map[string]string
		code    provider.Code
		message string
		hint    string
	}{
		{"unknown application key", bad(SecretApplicationKey, "nope"), provider.Unauthenticated, "INVALID_KEY", "auth.eu.ovhcloud.com/api/createToken"},
		{"unknown consumer key", bad(SecretConsumerKey, "nope"), provider.Unauthenticated, "This credential does not exist", "validated"},
		{"wrong application secret", bad(SecretApplicationSecret, "nope"), provider.Unauthenticated, "INVALID_SIGNATURE", "createToken"},
		{"call not granted", secrets(limitedKey), provider.PermissionDenied, "NOT_GRANTED_CALL", "POST /cloud/project/*/kube/*/kubeconfig"},
		{"rate limited", secrets(burstKey), provider.RateLimited, "Too many requests", "wait a minute"},
		{"no keys", map[string]string{}, provider.Unauthenticated, "no application_key", "createToken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: tt.secrets})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint about %q", pe, tt.code, tt.message, tt.hint)
			}
		})
	}
	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(burstKey)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 10 {
		t.Errorf("List over the rate limit: RetryAfterSeconds = %d, want 10", pe.RetryAfterSeconds)
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct {
		cfg     map[string]any
		name    string
		url     string
		wantErr bool
	}{
		{nil, "eu", "https://api.eu.ovhcloud.com/1.0", false},
		{map[string]any{ConfigEndpoint: "ca"}, "ca", "https://api.ca.ovhcloud.com/1.0", false},
		{map[string]any{ConfigEndpoint: "ovh-us"}, "us", "https://api.us.ovhcloud.com/1.0", false},
		{map[string]any{ConfigEndpoint: "CA", "apiURL": "https://ca.api.example.com/1.0"}, "ca", "https://ca.api.example.com/1.0", false},
		{map[string]any{ConfigEndpoint: "asia"}, "", "", true},
		{map[string]any{"apiURL": "http://api.example.com/1.0"}, "", "", true},
	}
	for _, tt := range tests {
		name, url, err := endpoint(provider.Account{Config: tt.cfg})
		if tt.wantErr {
			if provider.CodeOf(err) != provider.InvalidConfig {
				t.Errorf("endpoint(%v) = %q, %q, %v, want InvalidConfig", tt.cfg, name, url, err)
			}
			continue
		}
		if err != nil || name != tt.name || url != tt.url {
			t.Errorf("endpoint(%v) = %q, %q, %v, want %q, %q", tt.cfg, name, url, err, tt.name, tt.url)
		}
	}
}

func TestSign(t *testing.T) {
	// The API's clock is an hour behind the local one.
	s := &signer{appKey: appKey, appSecret: appSecret, consumerKey: consumerKey, delta: time.Hour,
		now: func() time.Time { return time.Unix(1366560945, 0).Add(time.Hour) }}
	req, _ := http.NewRequest(http.MethodGet, "https://api.eu.ovhcloud.com/1.0/cloud/project?iamTags=x", nil)
	if err := s.sign(req); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(appSecret + "+" + consumerKey + "+GET+https://api.eu.ovhcloud.com/1.0/cloud/project?iamTags=x++1366560945"))
	want := map[string]string{
		"X-Ovh-Application": appKey,
		"X-Ovh-Consumer":    consumerKey,
		"X-Ovh-Timestamp":   "1366560945",
		"X-Ovh-Signature":   "$1$" + hex.EncodeToString(sum[:]),
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	post, _ := http.NewRequest(http.MethodPost, "https://api.eu.ovhcloud.com/1.0/x", strings.NewReader(`{"a":1}`))
	if err := s.sign(post); err != nil {
		t.Fatal(err)
	}
	sum = sha1.Sum([]byte(appSecret + "+" + consumerKey + `+POST+https://api.eu.ovhcloud.com/1.0/x+{"a":1}+1366560945`))
	if got := post.Header.Get("X-Ovh-Signature"); got != "$1$"+hex.EncodeToString(sum[:]) {
		t.Errorf("POST signature = %q, want one over the body", got)
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
			Network      []string
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
	for _, u := range Endpoints {
		if host := strings.Split(strings.TrimPrefix(u, "https://"), "/")[0]; !slices.Contains(m.Spec.Network, host) {
			t.Errorf("plugin.yaml network %v does not list %s", m.Spec.Network, host)
		}
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFake(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-ovhcloud")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-ovhcloud").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(nil),
		Secrets:         secrets(consumerKey),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the ovhcloud plugin does not conform")
	}
}
