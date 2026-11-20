package rancher

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

const (
	goodToken     = "token-abc12:4s8q2c7l9z5xk2v6n4m8b3h7j1w9p5r2t6y4u8i3o7e1a5s9d2f6g"
	readOnlyToken = "token-ro123:9z5xk2v6n4m8b3h7j1w9p5r2t6y4u8i3o7e1a5s9d2f6g4s8q2c7l"

	prodID    = "c-m-7x2kq9wd"
	stagingID = "c-m-5h4tbn2l"
	extID     = "c-m-ext00001" // its kubeconfig token cannot be looked up
	execID    = "c-m-exec0001" // kubeconfig-generate-token is off
)

var caPEM = "-----BEGIN CERTIFICATE-----\nMIIBvTCCAWOgAwIBAgIBADAKBggqhkjOPQQDAjBGMRwwGgYDVQQKExNkeW5hbWlj\n-----END CERTIFICATE-----\n"

// formatCert splits base64 the way Rancher's kubeconfig template does,
// with escaped line breaks inside a double-quoted YAML string.
func formatCert(data string) string {
	if len(data) <= 49 {
		return data
	}
	var b strings.Builder
	b.WriteString(data[:49])
	for _, part := range regexp.MustCompile(`\S{1,76}`).FindAllString(data[49:], -1) {
		b.WriteString("\\\n      " + part)
	}
	return b.String()
}

// fakeRancher serves the v3 API endpoints the plugin uses, with bodies
// shaped like Rancher's: norman collections, errors and actions, and the
// kubeconfig template of the generateKubeconfig action.
type fakeRancher struct {
	srv *httptest.Server

	mu          sync.Mutex
	generated   int  // kubeconfig tokens handed out
	cacertsDown bool // /v3/settings/cacerts answers 500
	expiresAt   time.Time
}

func newFakeRancher(t *testing.T) *fakeRancher {
	t.Helper()
	f := &fakeRancher{expiresAt: time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second).UTC()}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","status":%d,"code":%q,"message":%q}`, status, code, message)
}

func clusterJSON(id, name, gitVersion string, labels string) string {
	return fmt.Sprintf(`{"id":%q,"type":"cluster","name":%q,"state":"active","provider":"rke2","driver":"imported","internal":false,
	  "labels":%s,"version":{"gitVersion":%q,"major":"1","minor":"33"},"links":{"self":"https://rancher.example.com/v3/clusters/%[1]s"},
	  "actions":{"generateKubeconfig":"https://rancher.example.com/v3/clusters/%[1]s?action=generateKubeconfig"}}`, id, name, labels, gitVersion)
}

func (f *fakeRancher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+goodToken && auth != "Bearer "+readOnlyToken {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","status":"401","message":"must authenticate"}`)
		return
	}
	path, q := r.URL.Path, r.URL.Query()
	switch {
	case path == "/v3/clusters" && r.Method == http.MethodGet && q.Get("marker") == "":
		if q.Get("limit") != "1000" {
			fail(w, http.StatusBadRequest, "InvalidFormat", "want limit=1000")
			return
		}
		next := "https://rancher.example.com/v3/clusters?limit=1000&marker=" + stagingID
		fmt.Fprintf(w, `{"type":"collection","resourceType":"cluster","data":[%s,%s],"pagination":{"limit":2,"total":4,"next":%q,"partial":true}}`,
			clusterJSON("local", "local", "v1.33.5+rke2r1", `{"provider.cattle.io":"rke2"}`),
			clusterJSON(prodID, "payments-prod", "v1.33.5+rke2r1", `{"env":"prod","Team":"payments","provider.cattle.io":"rke2","cattle.io/creator":"norman"}`),
			next)
	case path == "/v3/clusters" && r.Method == http.MethodGet && q.Get("marker") == stagingID:
		fmt.Fprintf(w, `{"type":"collection","resourceType":"cluster","data":[%s],"pagination":{"limit":2,"total":3,"marker":%q}}`,
			fmt.Sprintf(`{"id":%q,"type":"cluster","name":"staging","state":"provisioning","labels":{}}`, stagingID), stagingID)
	case path == "/v3/settings/cacerts":
		if f.cacertsDown {
			fail(w, http.StatusInternalServerError, "ServerError", "settings unavailable")
			return
		}
		fmt.Fprintf(w, `{"id":"cacerts","type":"setting","name":"cacerts","value":%q,"default":"","customized":false,"source":""}`, caPEM)
	case strings.HasPrefix(path, "/v3/clusters/") && r.Method == http.MethodPost && q.Get("action") == "generateKubeconfig":
		f.generateKubeconfig(w, auth, strings.TrimPrefix(path, "/v3/clusters/"))
	case strings.HasPrefix(path, "/v3/tokens/kubeconfig-u-"):
		name := strings.TrimPrefix(path, "/v3/tokens/")
		fmt.Fprintf(w, `{"id":%q,"type":"token","name":%q,"authProvider":"local","current":false,"enabled":true,"expired":false,
		  "expiresAt":%q,"isDerived":true,"ttl":2592000000,"userId":"u-abc12","description":"Kubeconfig token"}`,
			name, name, f.expiresAt.Format(time.RFC3339))
	default:
		fail(w, http.StatusNotFound, "NotFound", "not found")
	}
}

func (f *fakeRancher) generateKubeconfig(w http.ResponseWriter, auth, id string) {
	if auth == "Bearer "+readOnlyToken {
		fail(w, http.StatusForbidden, "Forbidden", fmt.Sprintf(`clusters.management.cattle.io %q is forbidden: User "u-ro123" cannot create resource`, id))
		return
	}
	var name string
	switch id {
	case "local", prodID, stagingID, execID:
		name = id
	case extID:
		name = "ext"
	default:
		fail(w, http.StatusNotFound, "NotFound", fmt.Sprintf("clusters.management.cattle.io %q not found", id))
		return
	}
	f.generated++
	user := fmt.Sprintf("    token: \"kubeconfig-u-abc12x%d:secret%d\"\n", f.generated, f.generated)
	switch id {
	case extID:
		user = fmt.Sprintf("    token: \"ext-token-%d\"\n", f.generated)
	case execID:
		user = "    exec:\n      apiVersion: client.authentication.k8s.io/v1beta1\n      args:\n        - token\n        - --server=rancher.example.com\n        - --user=" + name + "\n      command: rancher\n"
	}
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: "%s"
  cluster:
    server: "https://rancher.example.com/k8s/clusters/%s"
    certificate-authority-data: "%s"

users:
- name: "%[1]s"
  user:
%[4]s

contexts:
- name: "%[1]s"
  context:
    user: "%[1]s"
    cluster: "%[1]s"

current-context: "%[1]s"
`, name, id, formatCert(base64.StdEncoding.EncodeToString([]byte(caPEM))), user)
	fmt.Fprintf(w, `{"type":"generateKubeconfigOutput","config":%q}`, cfg)
}

func (f *fakeRancher) account() provider.Account {
	return provider.Account{ID: "work", Config: map[string]any{"url": f.srv.URL}}
}

func secrets(token string) map[string]string { return map[string]string{"api_token": token} }

func TestList(t *testing.T) {
	f := newFakeRancher(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(), Secrets: secrets(goodToken)})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 3 {
		t.Fatalf("List = %d clusters, want 3 from both pages: %+v", len(clusters), clusters)
	}
	prod := clusters[1]
	if prod.ID != prodID || prod.Name != "payments-prod" || prod.Version != "v1.33.5+rke2r1" ||
		prod.Endpoints.Public != f.srv.URL+"/k8s/clusters/"+prodID {
		t.Fatalf("prod = %+v", prod)
	}
	if string(prod.CAData) != caPEM {
		t.Errorf("prod CA = %q, want the cacerts setting", prod.CAData)
	}
	if prod.Credential != nil {
		t.Errorf("listing must not generate kubeconfig tokens: %+v", prod.Credential)
	}
	if want := map[string]string{"env": "prod", "team": "payments"}; len(prod.Labels) != len(want) || prod.Labels["env"] != "prod" || prod.Labels["team"] != "payments" {
		t.Errorf("prod labels = %v, want %v", prod.Labels, want)
	}
	if local := clusters[0]; local.ID != "local" || len(local.Labels) != 0 {
		t.Errorf("local = %+v, want no labels", local)
	}
	if staging := clusters[2]; staging.ID != stagingID || staging.Version != "" || staging.Endpoints.Public != f.srv.URL+"/k8s/clusters/"+stagingID {
		t.Errorf("staging = %+v", staging)
	}
	f.mu.Lock()
	if f.generated != 0 {
		t.Errorf("List generated %d kubeconfigs, want none", f.generated)
	}
	f.cacertsDown = true
	f.mu.Unlock()
	clusters, err = New().List(context.Background(), provider.ListRequest{Account: f.account(), Secrets: secrets(goodToken)})
	if err != nil || len(clusters) != 3 || clusters[0].CAData != nil {
		t.Fatalf("without the cacerts setting, List = %+v, %v, want the clusters without a CA", clusters, err)
	}
}

func TestListThroughAPIURL(t *testing.T) {
	f := newFakeRancher(t)
	acct := provider.Account{ID: "work", Config: map[string]any{"url": "https://rancher.example.com", "apiURL": f.srv.URL}}
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: acct, Secrets: secrets(goodToken)})
	if err != nil || len(clusters) != 3 {
		t.Fatalf("List = %+v, %v", clusters, err)
	}
	if got, want := clusters[1].Endpoints.Public, "https://rancher.example.com/k8s/clusters/"+prodID; got != want {
		t.Errorf("endpoint = %q, want %q on the url setting, not apiURL", got, want)
	}
}

func TestCredential(t *testing.T) {
	f := newFakeRancher(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := New()
	p.now = func() time.Time { return now }

	tests := []struct {
		name      string
		cluster   string
		wantToken string
		wantExp   time.Time
		wantCode  provider.Code
	}{
		{"kubeconfig token", prodID, "kubeconfig-u-abc12x1:secret1", f.expiresAt, ""},
		{"token without a name", extID, "ext-token-2", now.Add(fallbackLifetime), ""},
		{"exec kubeconfig", execID, "", time.Time{}, provider.InvalidConfig},
		{"unknown cluster", "c-m-gone", "", time.Time{}, provider.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := p.Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: tt.cluster, Secrets: secrets(goodToken)})
			if tt.wantCode != "" {
				if pe := provider.AsError(err); pe.Code != tt.wantCode || pe.Hint == "" {
					t.Fatalf("Credential(%s) = %+v, %+v, want %s with a hint", tt.cluster, c, pe, tt.wantCode)
				}
				return
			}
			if err != nil || c.Type != provider.CredentialToken || c.Token != tt.wantToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(tt.wantExp) {
				t.Fatalf("Credential(%s) = %+v, %v, want token %q expiring %v", tt.cluster, c, err, tt.wantToken, tt.wantExp)
			}
		})
	}
}

func TestTokenExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		body string
		want time.Time
	}{
		{"expiresAt", `{"expiresAt":"2026-12-20T12:00:00Z","ttl":2592000000}`, time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC)},
		{"ttl only", `{"expiresAt":"","ttl":3600000}`, now.Add(time.Hour)},
		{"no expiry", `{"expiresAt":"","ttl":0}`, provider.NoExpiry},
		{"bad expiresAt", `{"expiresAt":"tomorrow"}`, now.Add(fallbackLifetime)},
		{"lookup fails", "", now.Add(fallbackLifetime)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v3/tokens/kubeconfig-u-abc12x9" || tt.body == "" {
					fail(w, http.StatusNotFound, "NotFound", "not found")
					return
				}
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			p := New()
			p.now = func() time.Time { return now }
			c, _, err := p.client(provider.Account{Config: map[string]any{"url": srv.URL}}, secrets(goodToken))
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.tokenExpiry(context.Background(), c, "kubeconfig-u-abc12x9:secret")
			if err != nil || !got.Equal(tt.want) {
				t.Fatalf("tokenExpiry = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	f := newFakeRancher(t)
	ctx := context.Background()

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(), Secrets: secrets("token-bad:nope")})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Message, "must authenticate") || !strings.Contains(pe.Hint, "Account & API Keys") {
		t.Errorf("List with a bad token = %+v, want Unauthenticated with a hint", pe)
	}
	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(), ClusterID: prodID, Secrets: secrets(readOnlyToken)})
	if pe := provider.AsError(err); pe.Code != provider.PermissionDenied || !strings.Contains(pe.Message, "forbidden") || !strings.Contains(pe.Hint, "Cluster Member") {
		t.Errorf("Credential without a role = %+v, want PermissionDenied with a hint", pe)
	}
	notRancher := provider.Account{Config: map[string]any{"url": f.srv.URL + "/not-rancher"}}
	_, err = New().List(ctx, provider.ListRequest{Account: notRancher, Secrets: secrets(goodToken)})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || !strings.Contains(pe.Hint, "Rancher server") {
		t.Errorf("List against something else = %+v, want NotFound with a hint", pe)
	}

	tests := []struct {
		name    string
		config  map[string]any
		secrets map[string]string
		want    provider.Code
	}{
		{"no token", map[string]any{"url": f.srv.URL}, nil, provider.Unauthenticated},
		{"access key alone", map[string]any{"url": f.srv.URL}, secrets("token-abc12"), provider.Unauthenticated},
		{"bad token and no url", nil, secrets("tug-conformance-invalid"), provider.Unauthenticated},
		{"no url", nil, secrets(goodToken), provider.InvalidConfig},
		{"plain http url", map[string]any{"url": "http://rancher.example.com"}, secrets(goodToken), provider.InvalidConfig},
		{"plain http apiURL", map[string]any{"url": "https://rancher.example.com", "apiURL": "http://rancher.example.com"}, secrets(goodToken), provider.InvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: provider.Account{Config: tt.config}, Secrets: tt.secrets})
			if provider.CodeOf(err) != tt.want {
				t.Errorf("List = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	f := newFakeRancher(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-rancher")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-rancher").CombinedOutput(); err != nil {
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
		t.Fatal("the rancher plugin does not conform")
	}
}
