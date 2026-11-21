package scaleway

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

const (
	goodKey    = "6f0f6a4c-6c1d-4f6e-9c1c-111111111111"
	deniedKey  = "6f0f6a4c-6c1d-4f6e-9c1c-222222222222" // an application without a Kubernetes permission set
	limitedKey = "6f0f6a4c-6c1d-4f6e-9c1c-333333333333"

	accessKey       = "SCWXXXXXXXXXXXXXXXXX"
	unreadableKey   = "SCWYYYYYYYYYYYYYYYYY"
	projectID       = "a4d2d2f0-0e2a-4f8b-8c2e-0123456789ab"
	otherProjectID  = "b5e3e3f1-1f3b-4f9c-9d3f-0123456789ab"
	prodID          = "0b5e2a2e-7c5e-4a5c-9d4b-111111111111"
	mlID            = "0b5e2a2e-7c5e-4a5c-9d4b-222222222222"
	stagingID       = "0b5e2a2e-7c5e-4a5c-9d4b-333333333333"
	creatingID      = "0b5e2a2e-7c5e-4a5c-9d4b-444444444444"
	poolRequiredID  = "0b5e2a2e-7c5e-4a5c-9d4b-555555555555"
	keyExpiryString = "2027-03-01T12:00:00Z"
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIC5zCCAc+gAwIBAgIBADANBgkqhkiG9w0BAQsFADAVMRMwEQYDVQQDEwprdWJl\n-----END CERTIFICATE-----\n"

func clusterURL(region, id string) string {
	return "https://" + id + ".api.k8s." + region + ".scw.cloud:6443"
}

// fakeScaleway serves the parts of the Kubernetes and IAM APIs the plugin
// uses, with bodies shaped like the types of Scaleway's Go SDK, which is
// generated from the API's definitions.
type fakeScaleway struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
}

func newFake(t *testing.T) *fakeScaleway {
	t.Helper()
	f := &fakeScaleway{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func clusterJSON(id, name, region, status, project string, tags ...string) string {
	if tags == nil {
		tags = []string{}
	}
	url := clusterURL(region, id)
	if status == "creating" {
		url = ""
	}
	tagJSON := `["` + strings.Join(tags, `","`) + `"]`
	if len(tags) == 0 {
		tagJSON = "[]"
	}
	return fmt.Sprintf(`{"id":%q,"type":"kapsule","name":%q,"status":%q,"version":"1.34.2","region":%q,
	  "organization_id":"c4d2d2f0-0e2a-4f8b-8c2e-0123456789ab","project_id":%q,"tags":%s,"cni":"cilium",
	  "description":"","cluster_url":%q,"dns_wildcard":"*.%[1]s.nodes.k8s.%[4]s.scw.cloud",
	  "created_at":"2026-06-01T10:00:00Z","updated_at":"2026-11-01T10:00:00Z","upgrade_available":false,
	  "feature_gates":[],"admission_plugins":[],"apiserver_cert_sans":[],"private_network_id":"d8e2d2f0-0e2a-4f8b-8c2e-0123456789ab",
	  "commitment_ends_at":"2026-06-01T10:00:00Z","iam_nodes_group_id":"e9e2d2f0-0e2a-4f8b-8c2e-0123456789ab"}`,
		id, name, status, region, project, tagJSON, url)
}

func poolsJSON(clusterID string, nodeTypes ...string) string {
	pools := make([]string, len(nodeTypes))
	for i, nt := range nodeTypes {
		pools[i] = fmt.Sprintf(`{"id":"pool-%d","cluster_id":%q,"name":"pool-%d","status":"ready","version":"1.34.2",
		  "node_type":%q,"autoscaling":false,"size":1,"min_size":1,"max_size":1,"autohealing":true,"tags":[],
		  "zone":"fr-par-1","root_volume_type":"sbs_15k","public_ip_disabled":false,"region":"fr-par"}`, i, clusterID, i, nt)
	}
	return fmt.Sprintf(`{"total_count":%d,"pools":[%s]}`, len(pools), strings.Join(pools, ","))
}

// kubeconfigJSON is a scw.File holding a redacted kubeconfig, as the
// kubeconfig endpoint returns it.
func kubeconfigJSON(region, id, name string) string {
	kc := fmt.Sprintf(`apiVersion: v1
clusters:
- name: %[1]s
  cluster:
    certificate-authority-data: %[2]s
    server: %[3]s
contexts:
- name: admin@%[1]s
  context:
    cluster: %[1]s
    user: %[1]s-admin
current-context: admin@%[1]s
kind: Config
preferences: {}
users:
- name: %[1]s-admin
  user:
    token: REDACTED
`, name, base64.StdEncoding.EncodeToString([]byte(caPEM)), clusterURL(region, id))
	return fmt.Sprintf(`{"name":"kubeconfig.yaml","content_type":"application/octet-stream","content":%q}`,
		base64.StdEncoding.EncodeToString([]byte(kc)))
}

func fail(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (f *fakeScaleway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "" {
		fail(w, http.StatusBadRequest, `{"message":"the plugin sent an Authorization header","type":"invalid_arguments"}`)
		return
	}
	switch r.Header.Get("X-Auth-Token") {
	case goodKey:
	case deniedKey:
		fail(w, http.StatusForbidden, `{"type":"permissions_denied","message":"insufficient permissions","details":[{"resource":"k8s_cluster","action":"read"}]}`)
		return
	case limitedKey:
		w.Header().Set("Retry-After", "15")
		fail(w, http.StatusTooManyRequests, `{"message":"rate limit exceeded","type":"too_many_requests"}`)
		return
	default:
		fail(w, http.StatusUnauthorized, `{"message":"authentication is denied","method":"api_key","reason":"not_found","type":"denied_authentication"}`)
		return
	}

	q := r.URL.Query()
	page, project := q.Get("page"), q.Get("project_id")
	switch path := r.URL.Path; {
	case strings.HasSuffix(path, "/clusters") && q.Get("page_size") != "100":
		fail(w, http.StatusBadRequest, `{"message":"want page_size=100","type":"invalid_arguments"}`)
	case path == "/k8s/v1/regions/fr-par/clusters" && project == otherProjectID:
		fmt.Fprint(w, `{"total_count":0,"clusters":[]}`)
	case path == "/k8s/v1/regions/fr-par/clusters" && page == "1":
		// A short first page checks that the plugin pages by total_count.
		fmt.Fprintf(w, `{"total_count":3,"clusters":[%s,%s]}`,
			clusterJSON(prodID, "prod", "fr-par", "ready", projectID, "env=prod", "team:payments", "web"),
			clusterJSON(mlID, "ml", "fr-par", "updating", projectID))
	case path == "/k8s/v1/regions/fr-par/clusters" && page == "2":
		fmt.Fprintf(w, `{"total_count":3,"clusters":[%s]}`, clusterJSON(stagingID, "staging", "fr-par", "ready", projectID, "env=staging"))
	case path == "/k8s/v1/regions/nl-ams/clusters" && page == "1":
		fmt.Fprintf(w, `{"total_count":2,"clusters":[%s,%s]}`,
			clusterJSON(creatingID, "creating", "nl-ams", "creating", projectID),
			clusterJSON(poolRequiredID, "empty", "nl-ams", "pool_required", projectID))
	case strings.HasSuffix(path, "/clusters"):
		fmt.Fprint(w, `{"total_count":0,"clusters":[]}`)
	case path == "/k8s/v1/regions/fr-par/clusters/"+prodID+"/pools":
		fmt.Fprint(w, poolsJSON(prodID, "PRO2-S", "POP2-HC-2C-4G"))
	case path == "/k8s/v1/regions/fr-par/clusters/"+mlID+"/pools":
		fmt.Fprint(w, poolsJSON(mlID, "PRO2-S", "H100-1-80G"))
	case strings.HasSuffix(path, "/pools"):
		fmt.Fprint(w, poolsJSON(""))
	case strings.HasSuffix(path, "/kubeconfig") && q.Get("redacted") != "true":
		fail(w, http.StatusBadRequest, `{"message":"the plugin asked for the legacy token","type":"invalid_arguments"}`)
	case path == "/k8s/v1/regions/fr-par/clusters/"+stagingID+"/kubeconfig":
		fail(w, http.StatusServiceUnavailable, `{"message":"service unavailable","type":"unknown"}`)
	case strings.HasSuffix(path, "/kubeconfig"):
		parts := strings.Split(path, "/")
		fmt.Fprint(w, kubeconfigJSON(parts[4], parts[6], "k8s-"+parts[6][:8]))
	case path == "/k8s/v1/regions/fr-par/clusters/"+prodID:
		fmt.Fprint(w, clusterJSON(prodID, "prod", "fr-par", "ready", projectID))
	case path == "/iam/v1alpha1/api-keys/"+accessKey:
		fmt.Fprintf(w, `{"access_key":%q,"secret_key":null,"application_id":"f1e2d2f0-0e2a-4f8b-8c2e-0123456789ab",
		  "description":"tug","created_at":"2026-03-01T12:00:00Z","updated_at":"2026-03-01T12:00:00Z","expires_at":%q,
		  "default_project_id":%q,"editable":true,"deletable":true,"managed":false,"creation_ip":"198.51.100.7"}`,
			accessKey, keyExpiryString, projectID)
	case path == "/iam/v1alpha1/api-keys/"+unreadableKey:
		fail(w, http.StatusForbidden, `{"type":"permissions_denied","message":"insufficient permissions","details":[{"resource":"api_key","action":"read"}]}`)
	default:
		fail(w, http.StatusNotFound, `{"message":"resource is not found","resource":"k8s_cluster","resource_id":"`+path+`","type":"not_found"}`)
	}
}

func (f *fakeScaleway) sent(prefix string) []string {
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

func (f *fakeScaleway) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

func secrets(key string) map[string]string { return map[string]string{SecretSecretKey: key} }

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
	want := []string{"fr-par/" + prodID, "fr-par/" + mlID, "fr-par/" + stagingID, "nl-ams/" + poolRequiredID}
	if !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v (both pages of fr-par, then nl-ams without the cluster being created)", ids, want)
	}

	prod := clusters[0]
	if prod.Name != "prod" || prod.Region != "fr-par" || prod.Version != "1.34.2" || prod.Endpoints.Public != clusterURL("fr-par", prodID) {
		t.Errorf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the kubeconfig's certificate-authority-data", prod.CAData)
	}
	if c := prod.Credential; c == nil || c.Type != provider.CredentialToken || c.Token != goodKey || c.ExpiresAt != nil {
		t.Errorf("prod credential = %+v, want the secret key without an expiry", c)
	}
	wantLabels := map[string]string{"env": "prod", "team": "payments"}
	if fmt.Sprint(prod.Labels) != fmt.Sprint(wantLabels) {
		t.Errorf("prod labels = %v, want %v (POP2-HC-2C-4G is not a GPU)", prod.Labels, wantLabels)
	}
	if ml := clusters[1]; ml.Labels["gpu"] != "true" {
		t.Errorf("ml labels = %v, want gpu=true for its H100-1-80G pool", ml.Labels)
	}
	if staging := clusters[2]; staging.CAData != nil || staging.Endpoints.Public != clusterURL("fr-par", stagingID) {
		t.Errorf("staging = %+v, want it listed without a CA while its kubeconfig is unavailable", staging)
	}
	for _, r := range f.sent("/k8s/v1/regions/fr-par/clusters/") {
		if strings.Contains(r, "/kubeconfig") && !strings.Contains(r, "redacted=true") {
			t.Errorf("kubeconfig request %s does not leave out the legacy token", r)
		}
	}
	if got := f.sent("/iam/"); len(got) != 0 {
		t.Errorf("List without accessKey asked IAM: %v", got)
	}
}

func TestListSettings(t *testing.T) {
	f := newFake(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{
		Account: f.account(map[string]any{ConfigRegions: "NL-AMS, nl-ams", ConfigProject: projectID}), Secrets: secrets(goodKey),
	})
	if err != nil || len(clusters) != 1 || clusters[0].ID != "nl-ams/"+poolRequiredID {
		t.Fatalf("List with regions=nl-ams = %+v, %v; want the nl-ams cluster only", clusters, err)
	}
	got := f.sent("/k8s/v1/regions/")
	if len(got) == 0 || !strings.HasPrefix(got[0], "/k8s/v1/regions/nl-ams/clusters?") || !strings.Contains(got[0], "project_id="+projectID) {
		t.Errorf("requests = %v, want nl-ams clusters filtered by project_id", got)
	}
	clusters, err = New().List(context.Background(), provider.ListRequest{
		Account: f.account(map[string]any{ConfigRegions: "fr-par", ConfigProject: otherProjectID}), Secrets: secrets(goodKey),
	})
	if err != nil || len(clusters) != 0 {
		t.Errorf("List in an empty Project = %+v, %v; want no clusters", clusters, err)
	}
}

func TestKeyExpiry(t *testing.T) {
	f := newFake(t)
	expires, err := time.Parse(time.RFC3339, keyExpiryString)
	if err != nil {
		t.Fatal(err)
	}
	clusters, err := New().List(context.Background(), provider.ListRequest{
		Account: f.account(map[string]any{ConfigRegions: "fr-par", ConfigAccessKey: accessKey}), Secrets: secrets(goodKey),
	})
	if err != nil || len(clusters) != 3 {
		t.Fatalf("List = %+v, %v", clusters, err)
	}
	for _, c := range clusters {
		if c.Credential == nil || c.Credential.ExpiresAt == nil || !c.Credential.ExpiresAt.Equal(expires) {
			t.Errorf("%s credential = %+v, want the secret key until %s", c.Name, c.Credential, expires)
		}
	}
	if got := f.sent("/iam/"); len(got) != 1 {
		t.Errorf("IAM requests = %v, want one per listing", got)
	}

	c, err := New().Credential(context.Background(), provider.CredentialRequest{
		Account: f.account(map[string]any{ConfigAccessKey: unreadableKey}), ClusterID: "fr-par/" + prodID, Secrets: secrets(goodKey),
	})
	if err != nil || c.Token != goodKey || c.ExpiresAt != nil {
		t.Errorf("Credential when the key cannot be read = %+v, %v; want the secret key without an expiry", c, err)
	}
}

func TestCredential(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	c, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "fr-par/" + prodID, Secrets: secrets(goodKey)})
	if err != nil || c.Type != provider.CredentialToken || c.Token != goodKey {
		t.Fatalf("Credential(prod) = %+v, %v; want the secret key", c, err)
	}
	for _, id := range []string{"nl-ams/" + prodID, "fr-par/00000000-0000-0000-0000-000000000000", prodID} {
		_, err := New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: id, Secrets: secrets(goodKey)})
		if pe := provider.AsError(err); pe.Code != provider.NotFound || pe.Hint == "" {
			t.Errorf("Credential(%s) = %+v, want NotFound with a hint", id, pe)
		}
	}
}

func TestGPUType(t *testing.T) {
	for typ, want := range map[string]bool{
		"GPU-3070-S":       true,
		"RENDER-S":         true,
		"H100-1-80G":       true,
		"H100-SXM-8-80G":   true,
		"L4-1-24G":         true,
		"L40S-2-48G":       true,
		"B300-SXM-8-288G":  true,
		"h100_1_80g":       true,
		"render_s":         true,
		"DEV1-M":           false,
		"PRO2-S":           false,
		"POP2-HC-2C-4G":    false,
		"POP2-HM-48C-384G": false,
		"PLAY2-NANO":       false,
		"GP1-XS":           false,
		"external":         false,
	} {
		if got := gpuType(typ); got != want {
			t.Errorf("gpuType(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestParseKubeconfig(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantServer string
		wantErr    string
	}{
		{"current context", "clusters:\n- name: a\n  cluster: {server: https://a}\n- name: b\n  cluster: {server: https://b}\ncontexts:\n- name: x\n  context: {cluster: b}\ncurrent-context: x\n", "https://b", ""},
		{"first cluster", "clusters:\n- name: a\n  cluster: {server: https://a}\n", "https://a", ""},
		{"no clusters", "kind: Config\n", "", "no clusters"},
		{"not yaml", "clusters: [", "", "parsing kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, err := parseKubeconfig([]byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseKubeconfig = %q, %v, want an error containing %q", server, err, tt.wantErr)
				}
				return
			}
			if err != nil || server != tt.wantServer {
				t.Fatalf("parseKubeconfig = %q, %v, want %q", server, err, tt.wantServer)
			}
		})
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
		{"bad key", "nope", provider.Unauthenticated, "authentication is denied", "IAM > API keys"},
		{"no Kubernetes permission set", deniedKey, provider.PermissionDenied, "insufficient permissions", "KubernetesReadOnly"},
		{"rate limited", limitedKey, provider.RateLimited, "rate limit exceeded", "wait a minute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(tt.key)})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Message, tt.message) || !strings.Contains(pe.Hint, tt.hint) {
				t.Errorf("List = %+v, want %s with %q and a hint naming %q", pe, tt.code, tt.message, tt.hint)
			}
			_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "fr-par/" + prodID, Secrets: secrets(tt.key)})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint == "" {
				t.Errorf("Credential = %+v, want %s with a hint", pe, tt.code)
			}
		})
	}

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(limitedKey)})
	if pe := provider.AsError(err); pe.RetryAfterSeconds != 15 {
		t.Errorf("List when rate limited: RetryAfterSeconds = %d, want 15 from Retry-After", pe.RetryAfterSeconds)
	}
	if _, err := New().List(ctx, provider.ListRequest{Account: f.account(nil)}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Errorf("List without a secret key = %v, want Unauthenticated", err)
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
	bin := filepath.Join(t.TempDir(), "tug-provider-scaleway")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-scaleway").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(map[string]any{ConfigAccessKey: accessKey}),
		Secrets:         secrets(goodKey),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the scaleway plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: f.account(map[string]any{ConfigAccessKey: accessKey}).Config, Secrets: secrets(goodKey)})
}
