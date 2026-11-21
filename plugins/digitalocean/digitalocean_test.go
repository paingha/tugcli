package digitalocean

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const (
	goodToken     = "dop_v1_good"
	readOnlyToken = "dop_v1_read" // kubernetes:read without kubernetes:access_cluster
	burstToken    = "dop_v1_burst"

	prodID    = "bd5f5959-5e1e-4205-a714-a914373942af"
	stagingID = "8d91899c-0739-4a1a-acc5-deadbeefbabe"
	legacyID  = "a2c53a4b-7bc9-4f49-9a3a-1d4d4f1b1c2e"
	newID     = "c0ffee00-0000-4000-8000-000000000001"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIDJzCCAg+gAwIBAgICBnUw\n-----END CERTIFICATE-----\n"

// fakeDO serves the DOKS endpoints the plugin uses, with bodies shaped like
// the examples in DigitalOcean's API reference.
type fakeDO struct {
	srv *httptest.Server

	mu     sync.Mutex
	expiry []string // expiry_seconds of each credentials request
}

func newFakeDO(t *testing.T) *fakeDO {
	t.Helper()
	f := &fakeDO{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDO) clusterJSON(id, name, region, endpoint, state string, tags, sizes []string, ha bool) string {
	pools := make([]string, len(sizes))
	for i, s := range sizes {
		pools[i] = fmt.Sprintf(`{"id":"pool-%d","name":"pool-%d","size":%q,"count":1,"tags":["k8s","k8s:%s","k8s:worker"],"nodes":[]}`, i, i, s, id)
	}
	tags = append(tags, "k8s", "k8s:"+id)
	return fmt.Sprintf(`{"id":%q,"name":%q,"region":%q,"version":"1.34.1-do.0","cluster_subnet":"10.244.0.0/16","service_subnet":"10.245.0.0/16",
	  "vpc_uuid":"c33931f2-a26a-4e61-b85c-4e95a2ec431b","ipv4":"","endpoint":%q,"tags":[%s],"node_pools":[%s],
	  "auto_upgrade":false,"status":{"state":%q,"message":%q},"created_at":"2026-11-15T16:00:11Z","updated_at":"2026-11-15T16:00:11Z","ha":%t}`,
		id, name, region, endpoint, quoteAll(tags), strings.Join(pools, ","), state, state, ha)
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ",")
}

func endpointFor(id string) string { return "https://" + id + ".k8s.ondigitalocean.com" }

func fail(w http.ResponseWriter, status int, id, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"id":%q,"message":%q,"request_id":"4d9d8375-3c56-4925-a3e7-eb137fed17e9"}`, id, message)
}

func (f *fakeDO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ratelimit-limit", "5000")
	w.Header().Set("ratelimit-remaining", "4816")
	w.Header().Set("ratelimit-reset", "1444931833")
	auth := r.Header.Get("Authorization")
	switch auth {
	case "Bearer " + goodToken, "Bearer " + readOnlyToken:
	case "Bearer " + burstToken:
		w.Header().Set("ratelimit-remaining", "0")
		w.Header().Set("retry-after", "29")
		fail(w, http.StatusTooManyRequests, "too_many_requests", "API Rate limit exceeded.")
		return
	default:
		fail(w, http.StatusUnauthorized, "unauthorized", "Unable to authenticate you.")
		return
	}

	path, page := r.URL.Path, r.URL.Query().Get("page")
	switch {
	case path == "/v2/kubernetes/clusters" && page == "1":
		next := f.srv.URL + "/v2/kubernetes/clusters?page=2&per_page=200"
		fmt.Fprintf(w, `{"kubernetes_clusters":[%s],"links":{"pages":{"last":%q,"next":%q}},"meta":{"total":4}}`,
			f.clusterJSON(prodID, "prod-cluster-01", "nyc1", endpointFor(prodID), "running",
				[]string{"production", "web-team", "env:prod", "Team:payments"}, []string{"s-1vcpu-2gb", "gpu-h100x1-80gb"}, true),
			next, next)
	case path == "/v2/kubernetes/clusters" && page == "2":
		first := f.srv.URL + "/v2/kubernetes/clusters?page=1&per_page=200"
		fmt.Fprintf(w, `{"kubernetes_clusters":[%s,%s,%s],"links":{"pages":{"first":%q,"prev":%q}},"meta":{"total":4}}`,
			f.clusterJSON(newID, "just-created", "ams3", "", "provisioning", nil, []string{"s-2vcpu-4gb"}, false),
			f.clusterJSON(stagingID, "staging", "fra1", endpointFor(stagingID), "provisioning", nil, []string{"s-2vcpu-4gb"}, false),
			f.clusterJSON(legacyID, "legacy", "lon1", endpointFor(legacyID), "running", []string{"env:dev"}, []string{"s-2vcpu-4gb"}, false),
			first, first)
	case strings.HasSuffix(path, "/credentials"):
		f.credentials(w, r, auth, strings.TrimSuffix(strings.TrimPrefix(path, "/v2/kubernetes/clusters/"), "/credentials"))
	default:
		fail(w, http.StatusNotFound, "not_found", "The resource you were accessing could not be found.")
	}
}

func (f *fakeDO) credentials(w http.ResponseWriter, r *http.Request, auth, id string) {
	if auth == "Bearer "+readOnlyToken {
		fail(w, http.StatusForbidden, "forbidden", "You do not have access for the attempted action.")
		return
	}
	seconds := r.URL.Query().Get("expiry_seconds")
	f.mu.Lock()
	f.expiry = append(f.expiry, seconds)
	f.mu.Unlock()
	lifetime := 7 * 24 * time.Hour
	if n, err := strconv.Atoi(seconds); err == nil && n > 0 {
		lifetime = time.Duration(n) * time.Second
	}
	expires := time.Now().Add(lifetime).UTC().Format(time.RFC3339Nano)
	ca := base64.StdEncoding.EncodeToString([]byte(caPEM))
	switch id {
	case prodID:
		fmt.Fprintf(w, `{"server":%q,"certificate_authority_data":%q,"client_certificate_data":null,"client_key_data":null,"token":"doks-token-prod","expires_at":%q}`,
			endpointFor(id), ca, expires)
	case legacyID:
		cert := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nclient\n-----END CERTIFICATE-----\n"))
		key := base64.StdEncoding.EncodeToString([]byte("-----BEGIN EC PRIVATE KEY-----\nkey\n-----END EC PRIVATE KEY-----\n"))
		fmt.Fprintf(w, `{"server":%q,"certificate_authority_data":%q,"client_certificate_data":%q,"client_key_data":%q,"expires_at":%q}`,
			endpointFor(id), ca, cert, key, expires)
	default:
		fail(w, http.StatusNotFound, "not_found", "The resource you were accessing could not be found.")
	}
}

func (f *fakeDO) account() provider.Account {
	return provider.Account{ID: "work", Config: map[string]any{"apiURL": f.srv.URL}}
}

func secrets(token string) map[string]string { return map[string]string{"api_token": token} }

func TestList(t *testing.T) {
	f := newFakeDO(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(), Secrets: secrets(goodToken)})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 3 {
		t.Fatalf("List = %d clusters, want 3 (both pages, without the one with no endpoint): %+v", len(clusters), clusters)
	}

	prod := clusters[0]
	if prod.ID != prodID || prod.Name != "prod-cluster-01" || prod.Region != "nyc1" || prod.Version != "1.34.1-do.0" ||
		prod.Endpoints.Public != endpointFor(prodID) {
		t.Fatalf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the decoded certificate_authority_data", prod.CAData)
	}
	if c := prod.Credential; c == nil || c.Type != provider.CredentialToken || c.Token != "doks-token-prod" || c.ExpiresAt == nil {
		t.Fatalf("prod credential = %+v, want the token with its expiry", c)
	}
	if left := time.Until(*prod.Credential.ExpiresAt); left < 50*time.Minute || left > time.Hour {
		t.Errorf("prod token expires in %s, want about an hour", left)
	}
	want := map[string]string{"env": "prod", "team": "payments", "ha": "true", "gpu": "true"}
	if len(prod.Labels) != len(want) {
		t.Errorf("prod labels = %v, want %v (no k8s tags)", prod.Labels, want)
	}
	for k, v := range want {
		if prod.Labels[k] != v {
			t.Errorf("prod label %s = %q, want %q", k, prod.Labels[k], v)
		}
	}

	staging := clusters[1]
	if staging.ID != stagingID || staging.Endpoints.Public != endpointFor(stagingID) || staging.CAData != nil || staging.Credential != nil {
		t.Fatalf("a cluster without credentials yet is listed by endpoint alone: %+v", staging)
	}

	legacy := clusters[2]
	if c := legacy.Credential; c == nil || c.Type != provider.CredentialClientCert || len(c.CertData) == 0 || len(c.KeyData) == 0 {
		t.Fatalf("legacy credential = %+v, want a client certificate", c)
	}
	if legacy.Labels["env"] != "dev" || legacy.Labels["gpu"] != "" || legacy.Labels["ha"] != "" {
		t.Errorf("legacy labels = %v", legacy.Labels)
	}

	for _, s := range f.expiry {
		if s != "3600" {
			t.Errorf("credentials asked with expiry_seconds=%q, want 3600", s)
		}
	}
}

func TestCredential(t *testing.T) {
	f := newFakeDO(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: prodID, Secrets: secrets(goodToken)})
	if err != nil || c.Token != "doks-token-prod" || c.ExpiresAt == nil {
		t.Fatalf("Credential(%s) = %+v, %v, want the token with an expiry", prodID, c, err)
	}
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: "gone", Secrets: secrets(goodToken)})
	if provider.CodeOf(err) != provider.NotFound {
		t.Fatalf("Credential(gone) = %v, want NotFound", err)
	}
}

func TestCredentialShapes(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := New()
	p.now = func() time.Time { return now }
	expires := now.Add(30 * time.Minute)
	tests := []struct {
		name     string
		resp     credentialsResponse
		wantType provider.CredentialType
		wantExp  *time.Time
		wantErr  bool
	}{
		{"token", credentialsResponse{Token: "t", ExpiresAt: expires}, provider.CredentialToken, &expires, false},
		{"token without expiry", credentialsResponse{Token: "t"}, provider.CredentialToken, new(now.Add(tokenLifetime)), false},
		{"client certificate", credentialsResponse{ClientCertificateData: []byte("c"), ClientKeyData: []byte("k")}, provider.CredentialClientCert, nil, false},
		{"nothing", credentialsResponse{CertificateAuthorityData: []byte("ca")}, "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := p.credential("x", &tt.resp)
			if tt.wantErr {
				if provider.CodeOf(err) != provider.Internal {
					t.Fatalf("credential = %+v, %v, want Internal", c, err)
				}
				return
			}
			if err != nil || c.Type != tt.wantType {
				t.Fatalf("credential = %+v, %v, want type %s", c, err, tt.wantType)
			}
			if (c.ExpiresAt == nil) != (tt.wantExp == nil) || (c.ExpiresAt != nil && !c.ExpiresAt.Equal(*tt.wantExp)) {
				t.Fatalf("credential expires %v, want %v", c.ExpiresAt, tt.wantExp)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	f := newFakeDO(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		token   string
		code    provider.Code
		message string
		hint    string
	}{
		{"bad token", "nope", provider.Unauthenticated, "Unable to authenticate you.", "kubernetes:access_cluster"},
		{"no access_cluster scope", readOnlyToken, provider.PermissionDenied, "You do not have access", "kubernetes:access_cluster"},
		{"burst limit", burstToken, provider.RateLimited, "API Rate limit exceeded.", "250 requests a minute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(), Secrets: secrets(tt.token)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: prodID, Secrets: secrets(tt.token)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(), Secrets: secrets(burstToken)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 29 {
		t.Errorf("List over the burst limit: RetryAfterSeconds = %d, want 29 from retry-after", pe.RetryAfterSeconds)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account()}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without a token = %v, want Unauthenticated", err)
	}
	insecure := provider.Account{Config: map[string]any{"apiURL": "http://api.example.com"}}
	if _, err := New().List(ctx, provider.ListRequest{Account: insecure, Secrets: secrets(goodToken)}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Errorf("List over plain http to a remote host = %v, want InvalidConfig", err)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFakeDO(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-digitalocean")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-digitalocean").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(),
		Secrets:         secrets(goodToken),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the digitalocean plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: f.account().Config, Secrets: secrets(goodToken)})
}
