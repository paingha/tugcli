package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/paingha/tugcli/pkg/provider"
)

const (
	subProd      = "00000000-0000-0000-0000-00000000000a"
	subDev       = "00000000-0000-0000-0000-00000000000b"
	subOff       = "00000000-0000-0000-0000-00000000000c"
	subGone      = "00000000-0000-0000-0000-00000000000d"
	subThrottled = "00000000-0000-0000-0000-00000000000e"
	subElsewhere = "00000000-0000-0000-0000-00000000000f"
	homeTenant   = "11111111-1111-1111-1111-111111111111"
	entraTenant  = "22222222-2222-2222-2222-222222222222"
)

var (
	testNow     = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tokenExpiry = time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
)

// fakeCredential hands out one token per scope and records every request.
type fakeCredential struct {
	tokens map[string]string
	err    error

	mu       sync.Mutex
	requests []policy.TokenRequestOptions
}

func (f *fakeCredential) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, o)
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	return azcore.AccessToken{Token: f.tokens[o.Scopes[0]], ExpiresOn: tokenExpiry}, nil
}

// fakeARM is Azure Resource Manager with two pages of subscriptions, two
// pages of clusters in subProd and one error per other subscription.
type fakeARM struct {
	*httptest.Server

	mu   sync.Mutex
	hits []string // method and path of every authorised request
}

func newFakeARM(t *testing.T) *fakeARM {
	t.Helper()
	f := &fakeARM{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /subscriptions", f.subscriptions)
	mux.HandleFunc("GET /subscriptions/{sub}/providers/Microsoft.ContainerService/managedClusters", f.clusters)
	mux.HandleFunc("GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{name}", f.cluster)
	mux.HandleFunc("POST /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.ContainerService/managedClusters/{name}/listClusterUserCredential", f.userCredential)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer arm-token" {
			armFail(w, http.StatusUnauthorized, "InvalidAuthenticationToken", "The access token is invalid.")
			return
		}
		// The versions whose response shapes Microsoft's REST reference
		// documents and these fakes copy.
		want := "2025-10-01"
		if r.URL.Path == "/subscriptions" {
			want = "2022-12-01"
		}
		if got := r.URL.Query().Get("api-version"); got != want {
			armFail(w, http.StatusBadRequest, "InvalidApiVersionParameter", "api-version "+got+" is not "+want)
			return
		}
		f.mu.Lock()
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeARM) requested(methodPath string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.hits, methodPath)
}

func armFail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, code, msg)
}

func (f *fakeARM) subscriptions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("$skiptoken") == "2" {
		fmt.Fprintf(w, `{"value":[{"id":"/subscriptions/%[1]s","subscriptionId":%[1]q,"displayName":"Development","state":"Warned","tenantId":%[2]q}]}`, subDev, homeTenant)
		return
	}
	fmt.Fprintf(w, `{"nextLink":%q,"value":[
		{"id":"/subscriptions/%[2]s","subscriptionId":%[2]q,"displayName":"Production","state":"Enabled","tenantId":%[4]q},
		{"id":"/subscriptions/%[3]s","subscriptionId":%[3]q,"displayName":"Old","state":"Disabled","tenantId":%[4]q}]}`,
		f.URL+"/subscriptions?api-version="+subscriptionsAPIVersion+"&%24skiptoken=2", subProd, subOff, homeTenant)
}

func clusterJSON(rg, name, tags, props string) string {
	return fmt.Sprintf(`{"id":"/subscriptions/%s/resourcegroups/%s/providers/Microsoft.ContainerService/managedClusters/%s","name":%q,"location":"westeurope","tags":{%s},"properties":{%s}}`,
		subProd, rg, name, name, tags, props)
}

var (
	paymentsJSON = clusterJSON("Payments-RG", "payments-prod", `"Environment":"prod","Team":"pay"`,
		`"kubernetesVersion":"1.34","currentKubernetesVersion":"1.34.1","fqdn":"payments-prod-dns-1a2b3c.hcp.westeurope.azmk8s.io",
		 "provisioningState":"Succeeded","aadProfile":{"managed":true,"enableAzureRBAC":true,"tenantID":"`+entraTenant+`"}`)
	internalJSON = clusterJSON("platform", "internal", "",
		`"kubernetesVersion":"1.33","fqdn":"internal-dns-4d5e6f.hcp.westeurope.azmk8s.io","privateFQDN":"internal-a1b2.privatelink.westeurope.azmk8s.io",
		 "apiServerAccessProfile":{"enablePrivateCluster":true,"enablePrivateClusterPublicFQDN":true}`)
	creatingJSON = clusterJSON("platform", "creating", "", `"kubernetesVersion":"1.34","provisioningState":"Creating"`)
	lockedJSON   = clusterJSON("locked", "secret", "", `"kubernetesVersion":"1.34","fqdn":"secret-dns.hcp.westeurope.azmk8s.io"`)
	noLoginJSON  = clusterJSON("platform", "nologin", "", `"kubernetesVersion":"1.34","fqdn":"nologin-dns.hcp.westeurope.azmk8s.io","disableLocalAccounts":true`)
	legacyJSON   = clusterJSON("legacy", "old", "",
		`"kubernetesVersion":"1.30","fqdn":"old-dns.hcp.westeurope.azmk8s.io","aadProfile":{"managed":false,"serverAppID":"33333333-3333-3333-3333-333333333333"}`)
)

func (f *fakeARM) clusters(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("sub") {
	case subProd:
		if r.URL.Query().Get("$skiptoken") == "2" {
			fmt.Fprintf(w, `{"value":[%s,%s]}`, lockedJSON, noLoginJSON)
			return
		}
		next := f.URL + r.URL.Path + "?api-version=" + aksAPIVersion + "&%24skiptoken=2"
		fmt.Fprintf(w, `{"nextLink":%q,"value":[%s,%s,%s]}`, next, paymentsJSON, internalJSON, creatingJSON)
	case subGone:
		armFail(w, http.StatusNotFound, "SubscriptionNotFound", "The subscription '"+subGone+"' could not be found.")
	case subThrottled:
		w.Header().Set("Retry-After", "7")
		armFail(w, http.StatusTooManyRequests, "TooManyRequests", "The request is being throttled.")
	case subElsewhere:
		fmt.Fprintf(w, `{"nextLink":"https://attacker.example.com/next","value":[]}`)
	default:
		fmt.Fprint(w, `{"value":[]}`)
	}
}

func (f *fakeARM) cluster(w http.ResponseWriter, r *http.Request) {
	clusters := map[string]string{
		"payments-rg/payments-prod": paymentsJSON,
		"platform/internal":         internalJSON,
		"platform/nologin":          noLoginJSON,
		"legacy/old":                legacyJSON,
	}
	body, ok := clusters[r.PathValue("rg")+"/"+r.PathValue("name")]
	if !ok {
		armFail(w, http.StatusNotFound, "ResourceNotFound", "The Resource 'Microsoft.ContainerService/managedClusters/"+r.PathValue("name")+"' was not found.")
		return
	}
	fmt.Fprint(w, body)
}

const execUser = `    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kubelogin
      args: [get-token, --login, devicecode, --server-id, 6dae42f8-4368-4678-94ff-3960e28e3630]`

var localUser = `    client-certificate-data: ` + b64("-----BEGIN CERTIFICATE-----") + `
    client-key-data: ` + b64("-----BEGIN RSA PRIVATE KEY-----") + `
    token: local-token`

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// userKubeconfigJSON is a listClusterUserCredential answer, shaped as the
// ones AKS returns.
func userKubeconfigJSON(rg, name, server, ca, user string) string {
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    certificate-authority-data: %[3]s
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: clusterUser_%[4]s_%[1]s
current-context: %[1]s
users:
- name: clusterUser_%[4]s_%[1]s
  user:
%[5]s
`, name, server, b64(ca), rg, user)
	out, _ := json.Marshal(map[string]any{"kubeconfigs": []map[string]any{{"name": "clusterUser", "value": []byte(kc)}}})
	return string(out)
}

func (f *fakeARM) userCredential(w http.ResponseWriter, r *http.Request) {
	switch rg, name := strings.ToLower(r.PathValue("rg")), r.PathValue("name"); rg + "/" + name {
	case "payments-rg/payments-prod":
		fmt.Fprint(w, userKubeconfigJSON(rg, name, "https://payments-prod-dns-1a2b3c.hcp.westeurope.azmk8s.io:443", "payments-ca", execUser))
	case "platform/internal":
		fmt.Fprint(w, userKubeconfigJSON(rg, name, "https://internal-a1b2.privatelink.westeurope.azmk8s.io:443", "internal-ca", localUser))
	case "locked/secret":
		armFail(w, http.StatusForbidden, "AuthorizationFailed", "The client does not have authorization to perform action 'Microsoft.ContainerService/managedClusters/listClusterUserCredential/action'.")
	default:
		armFail(w, http.StatusBadRequest, "BadRequest", "Getting static credential isn't allowed because this cluster is set to disable local accounts.")
	}
}

// newTestProvider returns a provider that talks to arm, with no AZURE_*
// variables and an empty home directory unless opts say otherwise.
func newTestProvider(t *testing.T, arm *fakeARM, cred *fakeCredential, opts ...Option) *Provider {
	t.Helper()
	if cred.tokens == nil {
		cred.tokens = map[string]string{arm.URL + "/.default": "arm-token", aksScope: "aks-token"}
	}
	return New(append([]Option{
		WithARMURL(arm.URL + "/"),
		WithHTTPClient(arm.Client()),
		WithTokenCredential(cred),
		WithNow(func() time.Time { return testNow }),
		WithGetenv(env()),
		WithHome(t.TempDir()),
	}, opts...)...)
}

// env returns a getenv that knows only the given name and value pairs.
func env(kv ...string) func(string) string {
	return func(k string) string {
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == k {
				return kv[i+1]
			}
		}
		return ""
	}
}

// writeProfile writes an Azure CLI profile into dir, as `az login` does.
func writeProfile(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "azureProfile.json"), []byte(`{"subscriptions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// azureCLIHome returns a home directory with an Azure CLI profile in .azure.
func azureCLIHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeProfile(t, filepath.Join(home, ".azure"))
	return home
}

func prodAccount() provider.Account {
	return provider.Account{ID: subProd, Config: map[string]any{ConfigSubscription: subProd, ConfigTenant: homeTenant}}
}

func TestAccounts(t *testing.T) {
	arm := newFakeARM(t)
	cred := &fakeCredential{}
	accts, err := newTestProvider(t, arm, cred, WithHome(azureCLIHome(t))).Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range accts {
		got = append(got, a.ID+"|"+a.Description+"|"+a.ConfigString(ConfigSubscription)+"|"+a.ConfigString(ConfigTenant))
	}
	// The description is the display name, which the import picker can show.
	want := []string{
		subProd + "|Production|" + subProd + "|" + homeTenant,
		subDev + "|Development|" + subDev + "|" + homeTenant,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Accounts() = %q, want %q", got, want)
	}
	if len(cred.requests) != 1 || cred.requests[0].Scopes[0] != arm.URL+"/.default" || cred.requests[0].TenantID != "" {
		t.Fatalf("token requests = %+v, want one for the ARM scope in the default tenant", cred.requests)
	}
}

func TestAccountsSignInSetUp(t *testing.T) {
	arm := newFakeARM(t)
	configDir := t.TempDir()
	writeProfile(t, configDir)
	failed := errors.New("DefaultAzureCredential: failed to acquire a token")
	tests := []struct {
		name     string
		getenv   func(string) string
		home     string
		tokenErr error
		accounts int
		code     provider.Code
	}{
		{name: "nothing set up", getenv: env(), home: t.TempDir()},
		{name: "Azure CLI profile", getenv: env(), home: azureCLIHome(t), accounts: 2},
		{name: "profile in AZURE_CONFIG_DIR", getenv: env("AZURE_CONFIG_DIR", configDir), home: t.TempDir(), accounts: 2},
		{name: "AZURE_CONFIG_DIR without a profile", getenv: env("AZURE_CONFIG_DIR", t.TempDir()), home: azureCLIHome(t)},
		{name: "service principal variables", getenv: env("AZURE_TENANT_ID", homeTenant, "AZURE_CLIENT_ID", "app"), home: t.TempDir(), accounts: 2},
		{name: "workload identity variables", getenv: env("AZURE_FEDERATED_TOKEN_FILE", "/var/run/secrets/azure/tokens/token"), home: t.TempDir(), accounts: 2},
		{name: "Azure CLI signed out", getenv: env(), home: azureCLIHome(t), tokenErr: failed, code: provider.Unauthenticated},
		{name: "service principal rejected", getenv: env("AZURE_CLIENT_ID", "app"), home: t.TempDir(), tokenErr: failed, code: provider.Unauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred := &fakeCredential{err: tt.tokenErr}
			accts, err := newTestProvider(t, arm, cred, WithGetenv(tt.getenv), WithHome(tt.home)).Accounts(context.Background())
			if tt.code != "" {
				if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Hint, "az login") {
					t.Fatalf("Accounts() error = %+v, want %s with an az login hint", pe, tt.code)
				}
				return
			}
			if err != nil || len(accts) != tt.accounts {
				t.Fatalf("Accounts() = %d accounts, %v; want %d, no error", len(accts), err, tt.accounts)
			}
			// Asking for a token is what runs the managed identity probe.
			if tt.accounts == 0 && len(cred.requests) > 0 {
				t.Fatalf("Accounts() asked for %d tokens with no sign-in set up", len(cred.requests))
			}
		})
	}
}

func TestList(t *testing.T) {
	arm := newFakeARM(t)
	cred := &fakeCredential{}
	clusters, err := newTestProvider(t, arm, cred).List(context.Background(), provider.ListRequest{Account: prodAccount()})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	// creating has no address yet, secret's credential is forbidden and
	// nologin has no way to sign in.
	if want := []string{"payments-rg/payments-prod", "platform/internal"}; !slices.Equal(ids, want) {
		t.Fatalf("List() IDs = %v, want %v", ids, want)
	}
	if arm.requested("POST /subscriptions/" + subProd + "/resourceGroups/platform/providers/Microsoft.ContainerService/managedClusters/nologin/listClusterUserCredential") {
		t.Error("List() asked for the user credential of a cluster nobody can sign in to")
	}

	prod := clusters[0]
	if prod.Name != "payments-prod" || prod.Region != "westeurope" || prod.Version != "1.34.1" ||
		prod.Endpoints.Public != "https://payments-prod-dns-1a2b3c.hcp.westeurope.azmk8s.io:443" || prod.Endpoints.Private != "" ||
		string(prod.CAData) != "payments-ca" || prod.Credential != nil {
		t.Errorf("payments-prod = %+v", prod)
	}
	wantLabels := map[string]string{"env": "prod", "team": "pay", "azure-subscription": subProd, "azure-resource-group": "Payments-RG"}
	if fmt.Sprint(prod.Labels) != fmt.Sprint(wantLabels) {
		t.Errorf("payments-prod labels = %v, want %v", prod.Labels, wantLabels)
	}

	internal := clusters[1]
	if internal.Version != "1.33" || internal.Endpoints.Public != "" ||
		internal.Endpoints.Private != "https://internal-a1b2.privatelink.westeurope.azmk8s.io:443" || string(internal.CAData) != "internal-ca" {
		t.Errorf("private cluster = %+v", internal)
	}

	if len(cred.requests) != 1 || cred.requests[0].TenantID != homeTenant {
		t.Errorf("token requests = %+v, want one from the account's tenant %s", cred.requests, homeTenant)
	}
}

func TestListErrors(t *testing.T) {
	arm := newFakeARM(t)
	tests := []struct {
		name    string
		cred    *fakeCredential
		account provider.Account
		code    provider.Code
		hint    string
	}{
		{"no credentials", &fakeCredential{err: errors.New("DefaultAzureCredential: failed to acquire a token")},
			provider.Account{ID: subProd}, provider.Unauthenticated, "sign in with `az login`"},
		{"no credentials for the tenant", &fakeCredential{err: errors.New("AzureCLICredential: run az login")},
			prodAccount(), provider.Unauthenticated, "az login --tenant " + homeTenant},
		{"token ARM rejects", &fakeCredential{tokens: map[string]string{arm.URL + "/.default": "expired"}},
			prodAccount(), provider.Unauthenticated, "az login"},
		{"unknown subscription", &fakeCredential{}, provider.Account{ID: subGone}, provider.NotFound, "az account list"},
		{"not a subscription ID", &fakeCredential{}, provider.Account{ID: "work"}, provider.InvalidConfig, "--account <subscription-id>"},
		{"next page on another host", &fakeCredential{}, provider.Account{ID: subElsewhere}, provider.Internal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestProvider(t, arm, tt.cred).List(context.Background(), provider.ListRequest{Account: tt.account})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Fatalf("List() error = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}

	_, err := newTestProvider(t, arm, &fakeCredential{}).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: subThrottled}})
	if pe := provider.AsError(err); pe.Code != provider.RateLimited || pe.RetryAfterSeconds != 7 {
		t.Fatalf("List() when throttled = %+v, want RateLimited after 7 seconds", pe)
	}
}

func TestCredential(t *testing.T) {
	arm := newFakeARM(t)
	hourLater := testNow.Add(localCredentialTTL)
	tests := []struct {
		name    string
		cluster string
		typ     provider.CredentialType
		token   string
		expires time.Time
		code    provider.Code
		hint    string
	}{
		{name: "Entra ID", cluster: "payments-rg/payments-prod", typ: provider.CredentialToken, token: "aks-token", expires: tokenExpiry},
		{name: "local account", cluster: "platform/internal", typ: provider.CredentialToken, token: "local-token", expires: hourLater},
		{name: "legacy Azure AD", cluster: "legacy/old", code: provider.Unimplemented, hint: "--enable-aad"},
		{name: "no way to sign in", cluster: "platform/nologin", code: provider.PermissionDenied, hint: "--enable-aad"},
		{name: "deleted cluster", cluster: "platform/gone", code: provider.NotFound},
		{name: "not resource-group/name", cluster: "payments-prod", code: provider.InvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred := &fakeCredential{}
			c, err := newTestProvider(t, arm, cred).Credential(context.Background(), provider.CredentialRequest{Account: prodAccount(), ClusterID: tt.cluster})
			if tt.code != "" {
				if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
					t.Fatalf("Credential(%s) error = %+v, want %s with hint %q", tt.cluster, pe, tt.code, tt.hint)
				}
				return
			}
			if err != nil {
				t.Fatalf("Credential(%s): %v", tt.cluster, err)
			}
			if c.Type != tt.typ || c.Token != tt.token || c.ExpiresAt == nil || !c.ExpiresAt.Equal(tt.expires) {
				t.Fatalf("Credential(%s) = %+v, want a %s %q expiring at %s", tt.cluster, c, tt.typ, tt.token, tt.expires)
			}
		})
	}

	cred := &fakeCredential{}
	if _, err := newTestProvider(t, arm, cred).Credential(context.Background(), provider.CredentialRequest{Account: prodAccount(), ClusterID: "payments-rg/payments-prod"}); err != nil {
		t.Fatal(err)
	}
	if len(cred.requests) != 2 || cred.requests[1].Scopes[0] != aksScope || cred.requests[1].TenantID != entraTenant {
		t.Fatalf("token requests = %+v, want the AKS scope from the cluster's tenant %s", cred.requests, entraTenant)
	}
}
