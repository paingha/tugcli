package ibm

import (
	"context"
	"encoding/base64"
	"fmt"
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

// API keys the fake IAM knows. They are made up.
const (
	goodKey      = "tug-test-key-good"
	noRoleKey    = "tug-test-key-without-roles"
	throttledKey = "tug-test-key-throttled"
)

const (
	prodID    = "c5ue8sbd0rjfo3e0b1qg"
	newID     = "c6br4kfd0b2k9e0aqtag"
	privateID = "cn1ofn2d0ab4c7tmh9eg"
	roksID    = "cr3rfu5d0l0rjm8p5q7g"
)

var testCA = "-----BEGIN CERTIFICATE-----\nMIIDTzCCAjegAwIBAgIUHt\n-----END CERTIFICATE-----\n"

// jwt returns an unsigned token with the claims IAM puts in an ID token.
func jwt(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"kid":"20261101","alg":"RS256"}`)) + "." +
		enc(fmt.Appendf(nil, `{"iam_id":"IBMid-6620001ABC","sub":"tug@example.com","aud":"kube","exp":%d}`, exp.Unix())) + "." +
		enc([]byte("signature"))
}

// fakeIBM serves IAM's token endpoint, the Kubernetes Service API calls the
// plugin makes under /global, and one OpenShift master with its OAuth
// server, with bodies shaped like the API reference's.
type fakeIBM struct {
	srv *httptest.Server

	// oauthRejects makes the OpenShift OAuth server refuse the API key.
	oauthRejects bool

	mu      sync.Mutex
	clients []string // the IAM client of each token request
	headers []http.Header
}

func newFake(t *testing.T) *fakeIBM {
	t.Helper()
	f := &fakeIBM{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /identity/token", f.token)
	mux.HandleFunc("GET /global/v2/classic/getClusters", f.api(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "[%s]", f.cluster(prodID))
	}))
	mux.HandleFunc("GET /global/v2/vpc/getClusters", f.api(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("provider") != "vpc-gen2" {
			apiErr(w, http.StatusBadRequest, "E0004", "want provider=vpc-gen2")
			return
		}
		// prod is listed again to check that it is listed once.
		fmt.Fprintf(w, "[%s,%s,%s,%s]", f.cluster(newID), f.cluster(privateID), f.cluster(roksID), f.cluster(prodID))
	}))
	mux.HandleFunc("GET /global/v2/getCluster", f.api(func(w http.ResponseWriter, r *http.Request) {
		body := f.cluster(r.URL.Query().Get("cluster"))
		if body == "" {
			apiErr(w, http.StatusNotFound, "G0004", "The specified cluster could not be found.")
			return
		}
		fmt.Fprint(w, body)
	}))
	mux.HandleFunc("GET /global/v2/getKubeconfig", f.api(f.kubeconfig))
	mux.HandleFunc("GET /masters/roks/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"issuer":"%[1]s/oauth","authorization_endpoint":"%[1]s/oauth/oauth/authorize","token_endpoint":"%[1]s/oauth/oauth/token",
		  "scopes_supported":["user:check-access","user:full"],"response_types_supported":["code","token"],"grant_types_supported":["authorization_code","implicit"]}`, f.srv.URL)
	})
	mux.HandleFunc("GET /oauth/oauth/authorize", f.authorize)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIBM) account(cfg map[string]any) provider.Account {
	c := map[string]any{"apiURL": f.srv.URL + "/global", ConfigIAMURL: f.srv.URL}
	for k, v := range cfg {
		c[k] = v
	}
	return provider.Account{ID: "work", Config: c}
}

func secrets(key string) map[string]string { return map[string]string{SecretAPIKey: key} }

// cluster returns a cluster as getClusters and getCluster show it.
func (f *fakeIBM) cluster(id string) string {
	type cl struct {
		name, provider, typ, state, version, master, public, private string
	}
	clusters := map[string]cl{
		prodID: {"payments-prod", "classic", "kubernetes", "normal", "1.31.4_1530",
			"https://c100.us-south.containers.cloud.ibm.com:30226", "https://c100.us-south.containers.cloud.ibm.com:30226",
			"https://c100.private.us-south.containers.cloud.ibm.com:30226"},
		newID: {"just-ordered", "vpc-gen2", "kubernetes", "deploying", "1.32.1_1520", "", "", ""},
		privateID: {"ledger", "vpc-gen2", "kubernetes", "warning", "1.30.8_1561",
			"https://c104.private.eu-de.containers.cloud.ibm.com:31544", "", "https://c104.private.eu-de.containers.cloud.ibm.com:31544"},
		roksID: {"shop", "vpc-gen2", "openshift", "normal", "4.16.21_1544_openshift", f.srv.URL + "/masters/roks", f.srv.URL + "/masters/roks", ""},
	}
	c, ok := clusters[id]
	if !ok {
		return ""
	}
	return fmt.Sprintf(`{"id":%q,"name":%q,"region":"us-south","location":"Dallas","resourceGroup":"a8a12accd63b437bbd6d58fb8b462ca7",
	  "resourceGroupName":"default","provider":%q,"type":%q,"state":%q,"status":"All Workers Normal","masterKubeVersion":%q,
	  "targetVersion":"","workerCount":3,"isPaid":true,"masterURL":%q,"ingress":{"hostname":"","secretName":""},
	  "lifecycle":{"masterStatus":"Ready","masterState":"deployed","masterHealth":"normal"},
	  "serviceEndpoints":{"privateServiceEndpointEnabled":%t,"privateServiceEndpointURL":%q,"publicServiceEndpointEnabled":%t,"publicServiceEndpointURL":%q},
	  "createdDate":"2026-03-04T15:20:11+0000"}`,
		id, c.name, c.provider, c.typ, c.state, c.version, c.master, c.private != "", c.private, c.public != "", c.public)
}

func apiErr(w http.ResponseWriter, status int, code, description string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"incidentID":"7b8c1e2d-1111-4e0f-a09d-0123456789ab","code":%q,"description":%q,"type":"General","recoveryCLI":""}`, code, description)
}

func (f *fakeIBM) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	client := r.Header.Get("Authorization")
	f.mu.Lock()
	f.clients = append(f.clients, client)
	f.mu.Unlock()
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil ||
		r.PostForm.Get("grant_type") != "urn:ibm:params:oauth:grant-type:apikey" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errorCode":"BXNIM0109E","errorMessage":"Property missing or empty.","context":{"requestId":"c2VydmljZQ"}}`)
		return
	}
	key := r.PostForm.Get("apikey")
	switch key {
	case goodKey, noRoleKey:
	case throttledKey:
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errorCode":"BXNIM0436E","errorMessage":"Too many requests.","context":{"requestId":"c2VydmljZQ"}}`)
		return
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errorCode":"BXNIM0415E","errorMessage":"Provided API key could not be found.","context":{"requestId":"c2VydmljZQ"}}`)
		return
	}
	exp := time.Now().Add(time.Hour)
	switch client {
	case "Basic Yng6Yng=": // bx:bx
		fmt.Fprintf(w, `{"access_token":"iam-access-%s","refresh_token":"iam-refresh","ims_user_id":11203,"token_type":"Bearer","expires_in":3600,"expiration":%d,"scope":"ibm openid"}`, key, exp.Unix())
	case "Basic a3ViZTprdWJl": // kube:kube
		fmt.Fprintf(w, `{"access_token":"iam-kube-access","refresh_token":"iam-kube-refresh","id_token":%q,"token_type":"Bearer","expires_in":3600,"expiration":%d,"scope":"ibm openid"}`,
			jwt(exp.Add(-5*time.Minute)), exp.Unix())
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errorCode":"BXNIM0101E","errorMessage":"Unknown client.","context":{"requestId":"c2VydmljZQ"}}`)
	}
}

// api checks the IAM token the Kubernetes Service API calls carry.
func (f *fakeIBM) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		switch r.Header.Get("Authorization") {
		case "Bearer iam-access-" + goodKey:
			h(w, r)
		case "Bearer iam-access-" + noRoleKey:
			apiErr(w, http.StatusForbidden, "E3ab2", "You do not have the required permissions to access this resource.")
		default:
			apiErr(w, http.StatusUnauthorized, "E0003", "The IAM token is invalid or expired.")
		}
	}
}

func (f *fakeIBM) kubeconfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("format") != "json" || r.Header.Get("X-Auth-Refresh-Token") != "iam-refresh" {
		apiErr(w, http.StatusBadRequest, "E0004", "want format=json and the refresh token")
		return
	}
	servers := map[string]string{
		prodID:    "https://c100.us-south.containers.cloud.ibm.com:30226",
		privateID: "https://c104.private.eu-de.containers.cloud.ibm.com:31544",
	}
	server, ok := servers[q.Get("cluster")]
	if !ok {
		apiErr(w, http.StatusNotFound, "G0004", "The specified cluster could not be found.")
		return
	}
	fmt.Fprintf(w, `{"apiVersion":"v1","kind":"Config","current-context":"%[1]s/%[2]s",
	  "clusters":[{"name":"%[1]s/%[2]s","cluster":{"server":%[3]q,"certificate-authority-data":%[4]q}}],
	  "contexts":[{"name":"%[1]s/%[2]s","context":{"cluster":"%[1]s/%[2]s","user":"tug@example.com/%[2]s","namespace":"default"}}],
	  "users":[{"name":"tug@example.com/%[2]s","user":{"auth-provider":{"name":"oidc","config":{"client-id":"kube","client-secret":"kube",
	    "id-token":"redacted","idp-issuer-url":"https://iam.cloud.ibm.com/identity","refresh-token":"redacted"}}}}]}`,
		"cluster", q.Get("cluster"), server, base64.StdEncoding.EncodeToString([]byte(testCA)))
}

func (f *fakeIBM) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	user, pass, ok := r.BasicAuth()
	if q.Get("client_id") != "openshift-challenging-client" || q.Get("response_type") != "token" || r.Header.Get("X-CSRF-Token") == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !ok || user != "apikey" || pass != goodKey || f.oauthRejects {
		w.Header().Set("WWW-Authenticate", `Basic realm="openshift"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Location", f.srv.URL+"/oauth/oauth/token/implicit#access_token=sha256~shop-token&expires_in=86400&scope=user%3Afull&token_type=Bearer")
	w.WriteHeader(http.StatusFound)
}

func TestList(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodKey)})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(clusters))
	for i, c := range clusters {
		ids[i] = c.ID
	}
	if !slices.Equal(ids, []string{prodID, privateID, roksID}) {
		t.Fatalf("List = %v, want prod, ledger and shop once each, without the cluster being deployed", ids)
	}
	prod, ledger, shop := clusters[0], clusters[1], clusters[2]

	if prod.Name != "payments-prod" || prod.Region != "us-south" || prod.Version != "1.31.4" ||
		prod.Endpoints.Public != "https://c100.us-south.containers.cloud.ibm.com:30226" ||
		prod.Endpoints.Private != "https://c100.private.us-south.containers.cloud.ibm.com:30226" || string(prod.CAData) != testCA {
		t.Errorf("prod = %+v", prod)
	}
	if want := map[string]string{"resource-group": "default", "infrastructure": "classic"}; !mapsEqual(prod.Labels, want) {
		t.Errorf("prod labels = %v, want %v", prod.Labels, want)
	}
	c := prod.Credential
	if c == nil || c.Type != provider.CredentialToken || jwtExpiry(c.Token).IsZero() || c.ExpiresAt == nil || !c.ExpiresAt.Equal(jwtExpiry(c.Token)) {
		t.Fatalf("prod credential = %+v, want the ID token until its exp", c)
	}
	if ledger.Credential == nil || ledger.Credential.Token != c.Token {
		t.Errorf("ledger credential = %+v, want the same ID token", ledger.Credential)
	}
	if ledger.Endpoints.Public != "" || ledger.Endpoints.Private != "https://c104.private.eu-de.containers.cloud.ibm.com:31544" || string(ledger.CAData) != testCA {
		t.Errorf("ledger = %+v, want only its private endpoint", ledger)
	}
	if shop.Version != "4.16.21" || shop.Endpoints.Public != f.srv.URL+"/masters/roks" || shop.CAData != nil || shop.Credential != nil ||
		shop.Labels["openshift"] != "true" || shop.Labels["infrastructure"] != "vpc-gen2" {
		t.Errorf("shop = %+v, want an OpenShift cluster without a CA or an inline token", shop)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.clients, []string{apiClient, kubeClient}) {
		t.Errorf("IAM token requests from %v, want one for the API (bx) and one for the clusters (kube)", f.clients)
	}
	for _, h := range f.headers {
		if h.Get("X-Auth-Refresh-Token") != "iam-refresh" || h.Get("X-Auth-Resource-Group") != "" {
			t.Errorf("API call headers %v, want the refresh token and no resource group", h)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestListResourceGroup(t *testing.T) {
	f := newFake(t)
	if _, err := New().List(context.Background(), provider.ListRequest{
		Account: f.account(map[string]any{ConfigResourceGroup: "a8a12accd63b437bbd6d58fb8b462ca7"}), Secrets: secrets(goodKey),
	}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.headers {
		if h.Get("X-Auth-Resource-Group") != "a8a12accd63b437bbd6d58fb8b462ca7" {
			t.Errorf("API call headers %v, want X-Auth-Resource-Group", h)
		}
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	now := time.Now()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: prodID, Secrets: secrets(goodKey)})
	if err != nil || c.Type != provider.CredentialToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(jwtExpiry(c.Token)) {
		t.Fatalf("Credential(prod) = %+v, %v, want the ID token", c, err)
	}

	c, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: roksID, Secrets: secrets(goodKey)})
	if err != nil || c.Token != "sha256~shop-token" || c.ExpiresAt == nil {
		t.Fatalf("Credential(shop) = %+v, %v, want the OpenShift OAuth token", c, err)
	}
	if left := c.ExpiresAt.Sub(now); left < 23*time.Hour || left > 25*time.Hour {
		t.Errorf("OpenShift token expires in %s, want the 24 hours of expires_in", left)
	}

	f.oauthRejects = true
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: roksID, Secrets: secrets(goodKey)})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || pe.Hint == "" {
		t.Errorf("Credential(shop) refused by OAuth = %+v, want Unauthenticated with a hint", pe)
	}

	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "gone", Secrets: secrets(goodKey)})
	if provider.CodeOf(err) != provider.NotFound {
		t.Errorf("Credential(gone) = %v, want NotFound", err)
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
		{"unknown key", "nope", provider.Unauthenticated, "BXNIM0415E: Provided API key could not be found.", "api-key-create"},
		{"no Kubernetes Service role", noRoleKey, provider.PermissionDenied, "required permissions", "Viewer platform role"},
		{"IAM throttling", throttledKey, provider.RateLimited, "Too many requests.", "wait a minute"},
		{"no key", "", provider.Unauthenticated, "no api_key", "api-key-create"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(tt.key)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint about %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: prodID, Secrets: secrets(tt.key)})
			if provider.CodeOf(err) != tt.code {
				t.Errorf("Credential = %v, want %s", err, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(throttledKey)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 15 {
		t.Errorf("throttled List: RetryAfterSeconds = %d, want 15", pe.RetryAfterSeconds)
	}
	for _, key := range []string{"apiURL", ConfigIAMURL} {
		acct := f.account(map[string]any{key: "http://iam.example.com"})
		if _, err := New().List(ctx, provider.ListRequest{Account: acct, Secrets: secrets(goodKey)}); provider.CodeOf(err) != provider.InvalidConfig {
			t.Errorf("List with %s over plain http = %v, want InvalidConfig", key, err)
		}
	}
}

func TestJWTExpiry(t *testing.T) {
	exp := time.Unix(1795000000, 0)
	for _, tt := range []struct {
		token string
		want  time.Time
	}{
		{jwt(exp), exp},
		{"not.a.jwt", time.Time{}},
		{"opaque", time.Time{}},
		{base64.RawURLEncoding.EncodeToString([]byte("{}")) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".sig", time.Time{}},
	} {
		if got := jwtExpiry(tt.token); !got.Equal(tt.want) {
			t.Errorf("jwtExpiry(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

func TestVersion(t *testing.T) {
	for in, want := range map[string]string{"1.31.4_1530": "1.31.4", "4.16.21_1544_openshift": "4.16.21", "1.32": "1.32", "": ""} {
		if got := version(in); got != want {
			t.Errorf("version(%q) = %q, want %q", in, got, want)
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
	bin := filepath.Join(t.TempDir(), "tug-provider-ibm")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-ibm").CombinedOutput(); err != nil {
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
		t.Fatal("the ibm plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: f.account(nil).Config, Secrets: secrets(goodKey)})
}
