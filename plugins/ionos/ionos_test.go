package ionos

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
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

const (
	goodToken    = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.good.token"
	deniedToken  = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.denied.token"
	limitedToken = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.limited.token"
	contract     = "31721234"

	prodID      = "1e072e52-2ed3-492f-b6b6-c6b116907527"
	mlID        = "b0a6ed23-0636-4bd4-94aa-be01c355af1d"
	deployingID = "30f8a4f6-6515-4c34-b49d-dea807453b90"
	emptyID     = "5a2c1f3e-7b8d-4c9e-a0f1-2b3c4d5e6f70"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIDBjCCAe6gAwIBAgIBATANBgkqhkiG9w0BAQsFADAVMRMwEQYDVQQDEwptaW5p\n-----END CERTIFICATE-----\n"

// jwt returns an unsigned JSON Web Token with the given exp claim. The
// plugin reads the claim only; the API server checks the signature.
func jwt(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"iss": "ionos", "exp": exp.Unix()})
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + "." + enc([]byte("signature"))
}

// fakeIONOS serves the Managed Kubernetes part of the Cloud API v6, with
// bodies shaped like the schemas in its OpenAPI document.
type fakeIONOS struct {
	t   *testing.T
	srv *httptest.Server
	// prodToken is the token in prod's kubeconfig; tokenExpires its exp.
	prodToken    string
	tokenExpires time.Time
	// wantContract, when set, is the X-Contract-Number every request needs.
	wantContract string
}

func newFake(t *testing.T) *fakeIONOS {
	t.Helper()
	f := &fakeIONOS{t: t, tokenExpires: time.Now().Add(time.Hour).Truncate(time.Second).UTC()}
	f.prodToken = jwt(f.tokenExpires)
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func clusterJSON(id, name, state string, public bool, location string) string {
	return fmt.Sprintf(`{"id":%q,"type":"k8s","href":"https://api.ionos.com/cloudapi/v6/k8s/%[1]s",
	  "metadata":{"etag":"45480eb3fbfc31f1d916c1eaa4abdcc3","createdDate":"2026-05-04T14:34:09.809Z","createdBy":"user@example.com",
	    "createdByUserId":"63cef532-26fe-4a64-a4e0-de7c8a506c90","lastModifiedDate":"2026-11-04T14:34:09.809Z",
	    "lastModifiedBy":"user@example.com","lastModifiedByUserId":"63cef532-26fe-4a64-a4e0-de7c8a506c90","state":%q},
	  "properties":{"name":%q,"k8sVersion":"1.34.3","maintenanceWindow":{"dayOfTheWeek":"Monday","time":"13:00:00"},
	    "availableUpgradeVersions":["1.35.1"],"viableNodePoolVersions":["1.33.6","1.34.3"],"public":%t,"location":%q,
	    "apiSubnetAllowList":[],"s3Buckets":[]},
	  "entities":{"nodepools":{"id":"%[1]s/nodepools","type":"collection","href":"https://api.ionos.com/cloudapi/v6/k8s/%[1]s/nodepools"}}}`,
		id, state, name, public, location)
}

func nodePoolsJSON(serverTypes ...string) string {
	items := make([]string, len(serverTypes))
	for i, st := range serverTypes {
		items[i] = fmt.Sprintf(`{"id":"np-%d","type":"nodepool","href":"","metadata":{"state":"ACTIVE"},
		  "properties":{"name":"pool-%d","datacenterId":"1e072e52-2ed3-492f-b6b6-c6b116907521","nodeCount":2,"serverType":%q,
		    "coresCount":4,"ramSize":8192,"availabilityZone":"AUTO","storageType":"SSD","storageSize":100,"k8sVersion":"1.34.3"}}`, i, i, st)
	}
	return `{"id":"nodepools","type":"collection","href":"","items":[` + strings.Join(items, ",") + `]}`
}

// kubeconfig returns a kubeconfig for the cluster, in the JSON form the
// API returns for Accept: application/json.
func kubeconfig(id, token string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []any{map[string]any{"name": "cluster-" + id[:8], "cluster": map[string]any{
			"server":                     "https://" + id[:8] + ".k8s.de-fra.ionos.com:443",
			"certificate-authority-data": base64.StdEncoding.EncodeToString([]byte(caPEM)),
		}}},
		"contexts":        []any{map[string]any{"name": "cluster-admin@cluster", "context": map[string]any{"cluster": "cluster-" + id[:8], "user": "cluster-admin"}}},
		"current-context": "cluster-admin@cluster",
		"users":           []any{map[string]any{"name": "cluster-admin", "user": map[string]any{"token": token}}},
	}
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"httpStatus":%d,"messages":[{"errorCode":%q,"message":%q}]}`, status, code, message)
}

func (f *fakeIONOS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Limit", "600")
	w.Header().Set("X-RateLimit-Burst", "50")
	switch r.Header.Get("Authorization") {
	case "Bearer " + goodToken:
	case "Bearer " + deniedToken:
		fail(w, http.StatusForbidden, "345", "Access denied: the user does not have the privilege to access the Kubernetes cluster")
		return
	case "Bearer " + limitedToken:
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "40")
		fail(w, http.StatusTooManyRequests, "451", "Too many requests")
		return
	default:
		fail(w, http.StatusUnauthorized, "315", "Unauthorized")
		return
	}
	if got := r.Header.Get("X-Contract-Number"); got != f.wantContract {
		fail(w, http.StatusForbidden, "309", "contract "+got+" is not the user's")
		return
	}

	depth := r.URL.Query().Get("depth")
	switch path := r.URL.Path; {
	case path == "/k8s" && depth == "1":
		fmt.Fprintf(w, `{"id":"k8s","type":"collection","href":"https://api.ionos.com/cloudapi/v6/k8s","items":[%s,%s,%s,%s]}`,
			clusterJSON(prodID, "prod", "ACTIVE", true, ""),
			clusterJSON(mlID, "ml", "UPDATING", false, "de/fra"),
			clusterJSON(deployingID, "new", "DEPLOYING", true, ""),
			clusterJSON(emptyID, "empty", "ACTIVE", true, ""))
	case path == "/k8s":
		// At depth 0 a collection holds only references.
		fmt.Fprintf(w, `{"id":"k8s","type":"collection","href":"","items":[{"id":%q,"type":"k8s","href":""}]}`, prodID)
	case path == clusterPath(prodID)+"/nodepools" && depth == "1":
		fmt.Fprint(w, nodePoolsJSON("DedicatedCore", "VCPU"))
	case path == clusterPath(mlID)+"/nodepools" && depth == "1":
		fmt.Fprint(w, nodePoolsJSON("DedicatedCore", "GPU"))
	case strings.HasSuffix(path, "/nodepools") && depth == "1":
		fmt.Fprint(w, nodePoolsJSON())
	case strings.HasSuffix(path, "/kubeconfig") && r.Header.Get("Accept") != "application/json":
		w.Header().Set("Content-Type", "application/yaml")
		fmt.Fprint(w, "apiVersion: v1\n")
	case path == clusterPath(prodID)+"/kubeconfig":
		_ = json.NewEncoder(w).Encode(kubeconfig(prodID, f.prodToken))
	case path == clusterPath(mlID)+"/kubeconfig":
		// The YAML form inside a JSON string, which the schema also allows.
		b, err := yaml.Marshal(kubeconfig(mlID, "not-a-jwt"))
		if err != nil {
			f.t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(string(b))
	case path == clusterPath(emptyID)+"/kubeconfig":
		fail(w, http.StatusForbidden, "345", "Access denied: create a node pool first")
	default:
		fail(w, http.StatusNotFound, "309", "Resource does not exist")
	}
}

func (f *fakeIONOS) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

func secrets(token string) map[string]string { return map[string]string{SecretToken: token} }

func TestList(t *testing.T) {
	f := newFake(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := New()
	p.now = func() time.Time { return now }
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodToken)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	if want := []string{prodID, mlID}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v (without the cluster being deployed or the one without node pools)", ids, want)
	}

	prod := clusters[0]
	if prod.Name != "prod" || prod.Version != "1.34.3" || prod.Region != "" || prod.Endpoints.Public != "https://1e072e52.k8s.de-fra.ionos.com:443" {
		t.Errorf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the kubeconfig's certificate-authority-data", prod.CAData)
	}
	if c := prod.Credential; c == nil || c.Type != provider.CredentialToken || c.Token != f.prodToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.tokenExpires) {
		t.Errorf("prod credential = %+v, want the kubeconfig token until its exp claim, %s", c, f.tokenExpires)
	}
	if len(prod.Labels) != 0 {
		t.Errorf("prod labels = %v, want none", prod.Labels)
	}

	ml := clusters[1]
	if ml.Region != "de/fra" || ml.Labels["gpu"] != "true" || ml.Labels["private-nodes"] != "true" || ml.Endpoints.Public != "https://b0a6ed23.k8s.de-fra.ionos.com:443" {
		t.Errorf("ml = %+v, want region de/fra and the gpu and private-nodes labels", ml)
	}
	if c := ml.Credential; c == nil || c.Token != "not-a-jwt" || c.ExpiresAt == nil || !c.ExpiresAt.Equal(now.Add(fallbackLifetime)) {
		t.Errorf("ml credential = %+v, want the token for %s", c, fallbackLifetime)
	}
}

func TestContract(t *testing.T) {
	f := newFake(t)
	f.wantContract = contract
	if _, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodToken)}); provider.CodeOf(err) != provider.PermissionDenied {
		t.Errorf("List without the contract = %v, want PermissionDenied", err)
	}
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigContract: contract}), Secrets: secrets(goodToken)})
	if err != nil || len(clusters) != 2 {
		t.Errorf("List with contract=%s = %d clusters, %v; want 2", contract, len(clusters), err)
	}
	_, err = New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigContract: "abc"}), Secrets: secrets(goodToken)})
	if provider.CodeOf(err) != provider.InvalidConfig {
		t.Errorf("List with contract=abc = %v, want InvalidConfig", err)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: prodID, Secrets: secrets(goodToken)})
	if err != nil || c.Token != f.prodToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(f.tokenExpires) {
		t.Fatalf("Credential(prod) = %+v, %v; want the kubeconfig token until %s", c, err, f.tokenExpires)
	}
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "gone", Secrets: secrets(goodToken)})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || pe.Hint == "" {
		t.Errorf("Credential(gone) = %+v, want NotFound with a hint", pe)
	}
}

func testCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cluster-admin"}, NotBefore: notAfter.Add(-time.Hour), NotAfter: notAfter}
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

func TestParseKubeconfig(t *testing.T) {
	notAfter := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	cert, key := testCert(t, notAfter)
	withCert := kubeconfig(prodID, "")
	withCert["users"] = []any{map[string]any{"name": "cluster-admin", "user": map[string]any{
		"client-certificate-data": base64.StdEncoding.EncodeToString(cert),
		"client-key-data":         base64.StdEncoding.EncodeToString(key),
	}}}
	noUser := kubeconfig(prodID, "")
	noUser["users"] = []any{map[string]any{"name": "cluster-admin", "user": map[string]any{}}}
	noServer := kubeconfig(prodID, "t")
	noServer["clusters"] = []any{map[string]any{"name": "cluster-" + prodID[:8], "cluster": map[string]any{}}}
	encode := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name     string
		raw      []byte
		wantType provider.CredentialType
		wantErr  string
	}{
		{"token", encode(kubeconfig(prodID, jwt(notAfter))), provider.CredentialToken, ""},
		{"client certificate", encode(withCert), provider.CredentialClientCert, ""},
		{"no credential", encode(noUser), "", "neither a token nor a client certificate"},
		{"no server", encode(noServer), "", "no API server address"},
		{"no users", []byte(`{"clusters":[]}`), "", "no cluster or user"},
		{"not a kubeconfig", []byte(`"clusters: ["`), "", "parsing kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc, err := New().parseKubeconfig(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseKubeconfig = %+v, %v, want an error containing %q", kc, err, tt.wantErr)
				}
				return
			}
			if err != nil || kc.cred.Type != tt.wantType || kc.cred.ExpiresAt == nil || !kc.cred.ExpiresAt.Equal(notAfter) {
				t.Fatalf("parseKubeconfig = %+v, %v, want a %s credential until %s", kc, err, tt.wantType, notAfter)
			}
		})
	}
}

func TestTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := New()
	p.now = func() time.Time { return now }
	exp := now.Add(30 * time.Minute)
	for token, want := range map[string]time.Time{
		jwt(exp):            exp,
		"opaque":            now.Add(fallbackLifetime),
		"a.not-base64!.c":   now.Add(fallbackLifetime),
		"a." + "e30" + ".c": now.Add(fallbackLifetime), // {} has no exp
	} {
		if got := p.tokenExpiry(token); !got.Equal(want) {
			t.Errorf("tokenExpiry(%q) = %s, want %s", token, got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		token   string
		code    provider.Code
		message string
		hint    string
	}{
		{"bad token", "nope", provider.Unauthenticated, "Unauthorized", "Token Manager"},
		{"no privilege", deniedToken, provider.PermissionDenied, "Access denied", "Create Kubernetes Clusters"},
		{"rate limited", limitedToken, provider.RateLimited, "Too many requests", "wait a minute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(tt.token)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: prodID, Secrets: secrets(tt.token)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(limitedToken)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 40 {
		t.Errorf("List when rate limited: RetryAfterSeconds = %d, want 40 from Retry-After", pe.RetryAfterSeconds)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account(nil)}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without a token = %v, want Unauthenticated", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com"}}
	if _, err := New().List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets(goodToken)}); provider.CodeOf(err) != provider.InvalidConfig {
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
	bin := filepath.Join(t.TempDir(), "tug-provider-ionos")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-ionos").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(nil),
		Secrets:         secrets(goodToken),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the ionos plugin does not conform")
	}
}
