package clusterprofile

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/pkg/provider"
)

const hubToken = "hub-token"

// fakeAPI is a hub cluster serving ClusterProfiles to callers presenting
// hubToken.
type fakeAPI struct {
	versions []string
	profiles []map[string]any

	mu    sync.Mutex
	paths []string
}

func (f *fakeAPI) requested(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.paths, path)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+hubToken {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	// /apis/multicluster.x-k8s.io/<version>[/namespaces/<ns>]/clusterprofiles[/<name>]
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	served := len(parts) >= 3 && parts[0] == "apis" && parts[1] == Group && slices.Contains(f.versions, parts[2])
	switch {
	case r.Method != http.MethodGet:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	case r.URL.Path == "/apis/"+Group && len(f.versions) > 0:
		var vs []map[string]string
		for _, v := range f.versions {
			vs = append(vs, map[string]string{"groupVersion": Group + "/" + v, "version": v})
		}
		writeJSON(w, map[string]any{"kind": "APIGroup", "apiVersion": "v1", "name": Group, "versions": vs})
	case served && len(parts) == 4 && parts[3] == "clusterprofiles":
		writeJSON(w, f.list(parts[2], ""))
	case served && len(parts) == 6 && parts[3] == "namespaces" && parts[5] == "clusterprofiles":
		writeJSON(w, f.list(parts[2], parts[4]))
	case served && len(parts) == 7 && parts[3] == "namespaces" && parts[5] == "clusterprofiles":
		for _, p := range f.profiles {
			md := p["metadata"].(map[string]any)
			if md["namespace"] == parts[4] && md["name"] == parts[6] {
				writeJSON(w, p)
				return
			}
		}
		status(w, http.StatusNotFound, "NotFound")
	default:
		status(w, http.StatusNotFound, "NotFound")
	}
}

func (f *fakeAPI) list(version, ns string) map[string]any {
	items := []map[string]any{}
	for _, p := range f.profiles {
		if ns == "" || p["metadata"].(map[string]any)["namespace"] == ns {
			items = append(items, p)
		}
	}
	return map[string]any{"kind": "ClusterProfileList", "apiVersion": Group + "/" + version, "metadata": map[string]any{}, "items": items}
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func status(w http.ResponseWriter, code int, reason string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": code, "message": strings.ToLower(reason)})
}

// clusterProfile returns a ClusterProfile whose status has the given
// field (accessProviders or credentialProviders) set to providers.
func clusterProfile(ns, name, field string, providers ...map[string]any) map[string]any {
	p := map[string]any{
		"apiVersion": Group + "/v1alpha2", "kind": "ClusterProfile",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{"x-k8s.io/cluster-manager": "fleet-manager"}},
		"spec":     map[string]any{"displayName": name, "clusterManager": map[string]any{"name": "fleet-manager"}},
	}
	if field != "" {
		p["status"] = map[string]any{"version": map[string]any{"kubernetes": "v1.34.1"}, field: providers}
	}
	return p
}

func accessProvider(name, server string, extensions ...map[string]any) map[string]any {
	cluster := map[string]any{"server": server, "certificate-authority-data": []byte("CA-" + name)}
	if len(extensions) > 0 {
		cluster["extensions"] = extensions
	}
	return map[string]any{"name": name, "cluster": cluster}
}

func extension(name string, value any) map[string]any {
	return map[string]any{"name": name, "extension": value}
}

const providersJSON = `{"providers":[
  {"name":"google","execConfig":{"apiVersion":"client.authentication.k8s.io/v1beta1","command":"gke-gcloud-auth-plugin",
    "installHint":"gcloud components install gke-gcloud-auth-plugin","provideClusterInfo":false}},
  {"name":"azure","execConfig":{"apiVersion":"client.authentication.k8s.io/v1beta1","command":"kubelogin",
    "args":["get-token","--login","azurecli"],"env":[{"name":"AZURE_CLIENT_ID","value":"from-file"}],"interactiveMode":"Never"},
    "profileSourcedCLIArgsPolicy":"Append","profileSourcedEnvVarsPolicy":"AppendIfNotExists"},
  {"name":"secretreader","execConfig":{"apiVersion":"client.authentication.k8s.io/v1","command":"/bin/secretreader-plugin","provideClusterInfo":true}}
]}`

func writeProviders(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newFake(t *testing.T) (*fakeAPI, *Provider) {
	t.Helper()
	f := &fakeAPI{
		versions: []string{"v1alpha2", "v1alpha1"},
		profiles: []map[string]any{
			// google comes first in the providers file, so it wins.
			clusterProfile("fleet", "gke-prod", "accessProviders",
				accessProvider("kubeconfig", "https://direct.example.com"),
				accessProvider("google", "https://connectgateway.googleapis.com/v1/projects/1/locations/global/gkeMemberships/gke-prod")),
			clusterProfile("fleet", "aks-dev", "accessProviders",
				accessProvider("azure", "https://aks-dev.hcp.westeurope.azmk8s.io:443",
					extension(extensionArgs, []string{"--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630"}),
					extension(extensionEnvs, map[string]string{"AZURE_TENANT_ID": "tenant-1", "AZURE_CLIENT_ID": "from-profile"}),
					extension("client.authentication.k8s.io/exec", map[string]string{"clusterName": "aks-dev"}))),
			clusterProfile("fleet", "old", "credentialProviders", accessProvider("google", "https://old.example.com")),
			clusterProfile("fleet", "unmatched", "accessProviders", accessProvider("oidc", "https://oidc.example.com")),
			clusterProfile("fleet", "pending", ""),
			clusterProfile("other", "reader", "accessProviders", accessProvider("secretreader", "https://reader.example.com",
				extension("client.authentication.k8s.io/exec", map[string]any{"clusterName": "reader", "port": 6443}))),
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(WithClusterConfig(func(_ context.Context, name string) (*rest.Config, error) {
		if name != "hub" {
			return nil, provider.Errorf(provider.NotFound, "%s is not in the catalog", name)
		}
		return &rest.Config{Host: srv.URL, BearerToken: hubToken}, nil
	}))
	return f, p
}

func account(t *testing.T, providers string, settings ...string) provider.Account {
	t.Helper()
	cfg := map[string]any{ConfigCluster: "hub", ConfigProviderFile: writeProviders(t, providers)}
	for _, kv := range settings {
		k, v, _ := strings.Cut(kv, "=")
		cfg[k] = v
	}
	return provider.Account{ID: "hub", Config: cfg}
}

func TestList(t *testing.T) {
	f, p := newFake(t)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account(t, providersJSON)})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]provider.Cluster{}
	var ids []string
	for _, c := range clusters {
		byID[c.ID] = c
		ids = append(ids, c.ID)
	}
	if want := []string{"fleet/gke-prod", "fleet/aks-dev", "fleet/old", "other/reader"}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v", ids, want)
	}
	gke := byID["fleet/gke-prod"]
	if gke.Name != "gke-prod" || gke.Version != "v1.34.1" || !strings.HasPrefix(gke.Endpoints.Public, "https://connectgateway.googleapis.com/") ||
		string(gke.CAData) != "CA-google" || gke.Labels["x-k8s.io/cluster-manager"] != "fleet-manager" {
		t.Fatalf("gke-prod = %+v", gke)
	}
	if c := gke.Credential; c == nil || c.Type != provider.CredentialDelegate || c.Exec.Command != "gke-gcloud-auth-plugin" || c.ExpiresAt != nil {
		t.Fatalf("gke-prod credential = %+v, want the google plugin inline", c)
	}
	if old := byID["fleet/old"]; old.Endpoints.Public != "https://old.example.com" {
		t.Fatalf("old = %+v, want its deprecated credentialProviders read", old)
	}
	// The secretreader plugin asks for cluster information, so it gets the
	// profile's exec extension as its cluster config.
	if c := byID["other/reader"].Credential; c == nil || !c.Exec.ProvideClusterInfo || string(c.Exec.ClusterConfig) != `{"clusterName":"reader","port":6443}` {
		t.Fatalf("other/reader credential = %+v, want cluster information with the exec extension", c)
	}
	// azure's plugin does not ask, so the extension is not passed on.
	if c := byID["fleet/aks-dev"].Credential; c == nil || c.Exec.ProvideClusterInfo || c.Exec.ClusterConfig != nil {
		t.Fatalf("fleet/aks-dev credential = %+v, want no cluster information", c)
	}
	if !f.requested("/apis/multicluster.x-k8s.io/v1alpha2/clusterprofiles") {
		t.Fatal("List did not read v1alpha2 ClusterProfiles in every namespace")
	}
}

func TestListVersionNamespaceAndNoMatch(t *testing.T) {
	f, p := newFake(t)
	f.versions = []string{"v1alpha1"}
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account(t, providersJSON, "namespace=other")})
	if err != nil || len(clusters) != 1 || !f.requested("/apis/multicluster.x-k8s.io/v1alpha1/namespaces/other/clusterprofiles") {
		t.Fatalf("List(v1alpha1, namespace=other) = %+v, %v", clusters, err)
	}

	_, err = p.List(context.Background(), provider.ListRequest{Account: account(t, `{"providers":[{"name":"aws","execConfig":{"command":"aws"}}]}`)})
	pe := provider.AsError(err)
	if pe.Code != provider.InvalidConfig || !strings.Contains(pe.Message, "they offer azure, google, kubeconfig, oidc, secretreader") {
		t.Fatalf("List with no matching provider = %+v, want InvalidConfig naming what the hub offers", pe)
	}
}

func TestProvidersFileErrors(t *testing.T) {
	_, p := newFake(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	tests := []struct {
		name string
		acct provider.Account
		code provider.Code
		msg  string
	}{
		{"no setting", provider.Account{ID: "hub", Config: map[string]any{ConfigCluster: "hub"}}, provider.InvalidConfig, "no providerFile setting"},
		{"relative", provider.Account{ID: "hub", Config: map[string]any{ConfigCluster: "hub", ConfigProviderFile: "providers.json"}}, provider.InvalidConfig, "not an absolute path"},
		{"missing", provider.Account{ID: "hub", Config: map[string]any{ConfigCluster: "hub", ConfigProviderFile: missing}}, provider.NotFound, "does not exist"},
		{"malformed", account(t, `{"providers":`), provider.InvalidConfig, "parsing providers file"},
		{"no command", account(t, `{"providers":[{"name":"google","execConfig":{}}]}`), provider.InvalidConfig, "no execConfig.command"},
		{"bad policy", account(t, `{"providers":[{"name":"google","execConfig":{"command":"x"},"profileSourcedCLIArgsPolicy":"Prepend"}]}`), provider.InvalidConfig, `"Prepend" is not Append or Ignore`},
		{"bad env policy", account(t, `{"providers":[{"name":"google","execConfig":{"command":"x"},"profileSourcedEnvVarsPolicy":"Merge"}]}`), provider.InvalidConfig, `"Merge" is not`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.List(context.Background(), provider.ListRequest{Account: tt.acct})
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Message, tt.msg) {
				t.Fatalf("List() error = %v, want %s containing %q", err, tt.code, tt.msg)
			}
		})
	}
}

func TestCredential(t *testing.T) {
	_, p := newFake(t)
	baseArgs := []string{"get-token", "--login", "azurecli"}
	profileArgs := append(slices.Clone(baseArgs), "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630")
	tests := []struct {
		name      string
		argPolicy string
		envPolicy string
		wantArgs  []string
		wantEnv   map[string]string
	}{
		{"file wins", PolicyAppend, PolicyAppendIfNotExists, profileArgs,
			map[string]string{"AZURE_CLIENT_ID": "from-file", "AZURE_TENANT_ID": "tenant-1"}},
		{"profile wins", PolicyAppend, PolicyReplace, profileArgs,
			map[string]string{"AZURE_CLIENT_ID": "from-profile", "AZURE_TENANT_ID": "tenant-1"}},
		{"ignored by default", "", "", baseArgs, map[string]string{"AZURE_CLIENT_ID": "from-file"}},
		{"ignored", PolicyIgnore, PolicyIgnore, baseArgs, map[string]string{"AZURE_CLIENT_ID": "from-file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := strings.Replace(providersJSON, `"profileSourcedCLIArgsPolicy":"Append","profileSourcedEnvVarsPolicy":"AppendIfNotExists"`,
				`"profileSourcedCLIArgsPolicy":"`+tt.argPolicy+`","profileSourcedEnvVarsPolicy":"`+tt.envPolicy+`"`, 1)
			c, err := p.Credential(context.Background(), provider.CredentialRequest{Account: account(t, file), ClusterID: "fleet/aks-dev"})
			if err != nil {
				t.Fatal(err)
			}
			if c.Type != provider.CredentialDelegate || c.Exec.Command != "kubelogin" || !slices.Equal(c.Exec.Args, tt.wantArgs) ||
				!maps.Equal(c.Exec.Env, tt.wantEnv) || c.Exec.InteractiveMode != "Never" || c.Exec.APIVersion != "client.authentication.k8s.io/v1beta1" {
				t.Fatalf("Credential(aks-dev) = %+v, want args %v and env %v", c.Exec, tt.wantArgs, tt.wantEnv)
			}
		})
	}
}

func TestCredentialWithClusterInfo(t *testing.T) {
	f, p := newFake(t)
	f.profiles = append(f.profiles, clusterProfile("other", "plain", "accessProviders", accessProvider("secretreader", "https://plain.example.com")))
	tests := []struct {
		id         string
		wantConfig string // "" means none
	}{
		{"other/reader", `{"clusterName":"reader","port":6443}`},
		{"other/plain", ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			c, err := p.Credential(context.Background(), provider.CredentialRequest{Account: account(t, providersJSON), ClusterID: tt.id})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("Credential(%s) = %+v, which is invalid: %v", tt.id, c.Exec, err)
			}
			if c.Exec.Command != "/bin/secretreader-plugin" || !c.Exec.ProvideClusterInfo || string(c.Exec.ClusterConfig) != tt.wantConfig {
				t.Fatalf("Credential(%s) = %+v, want secretreader-plugin with cluster information and config %q", tt.id, c.Exec, tt.wantConfig)
			}
		})
	}
}

func TestCredentialRefusals(t *testing.T) {
	f, p := newFake(t)
	f.profiles = append(f.profiles, clusterProfile("fleet", "bad-args", "accessProviders",
		accessProvider("azure", "https://bad.example.com", extension(extensionArgs, map[string]string{"not": "a list"}))))
	tests := []struct {
		id   string
		code provider.Code
		msg  string
	}{
		{"fleet/unmatched", provider.NotFound, "offers no access provider"},
		{"fleet/gone", provider.NotFound, "reading ClusterProfile fleet/gone"},
		{"fleet/bad-args", provider.InvalidConfig, "not a list of strings"},
		{"gke-prod", provider.InvalidConfig, "not namespace/name"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			_, err := p.Credential(context.Background(), provider.CredentialRequest{Account: account(t, providersJSON), ClusterID: tt.id})
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Message, tt.msg) {
				t.Fatalf("Credential(%s) error = %v, want %s containing %q", tt.id, err, tt.code, tt.msg)
			}
		})
	}
}
