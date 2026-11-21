package openshift

import (
	"context"
	"fmt"
	"maps"
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

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

// Offline tokens are JWTs that Red Hat's single sign-on signs; these only
// look like them.
const (
	goodOffline    = "eyJhbGciOiJIUzUxMiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICI0NzQzYTkzMC03YmJiIn0.eyJ0eXAiOiJPZmZsaW5lIiwiYXpwIjoiY2xvdWQtc2VydmljZXMifQ.c2lnbmF0dXJl"
	revokedOffline = "eyJhbGciOiJIUzUxMiJ9.eyJ0eXAiOiJPZmZsaW5lIn0.cmV2b2tlZA"
	accessValue    = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJhenAiOiJjbG91ZC1zZXJ2aWNlcyIsInNjb3BlIjoib3BlbmlkIn0.YWNjZXNz"
	tokenPath      = "/auth/realms/redhat-external/protocol/openid-connect/token"
)

// OCM cluster IDs are 32 lower-case letters and digits.
const (
	prodID         = "2f3k8l1m9n0p4q5r6s7t8u9v0w1x2y3z"
	hcpID          = "2g5h7j9k1m3n5p7q9r1s3t5v7w9x1y3z"
	osdID          = "2h6j8k0m2n4p6q8r0s2t4v6w8x0y2z4a"
	installingID   = "2j7k9m1n3p5q7r9s1t3v5w7x9y1z3a5b"
	uninstallingID = "2k8m0n2p4q6r8s0t2v4w6x8y0z2a4b6c"

	prodAPI         = "https://api.payments-prod.ab12.p1.openshiftapps.com:6443"
	hcpAPI          = "https://api.edge-hcp.cd34.p3.openshiftapps.com:443"
	osdAPI          = "https://api.data-osd.ef56.p2.openshiftapps.com:6443"
	uninstallingAPI = "https://api.old.gh78.p1.openshiftapps.com:6443"

	entraIssuer   = "https://login.microsoftonline.com/6f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b/v2.0"
	consoleClient = "a1b2c3d4-1111-4ea8-9c1e-5f6a7b8c9d0e"
	cliClientID   = "b2c3d4e5-2222-4ea8-9c1e-5f6a7b8c9d0e"
)

// clusterJSON follows the Cluster type of the clusters_mgmt v1 OpenAPI
// document: a ROSA classic cluster, a private ROSA HCP cluster with
// external authentication, an OSD cluster on GCP, one still being
// installed and one being uninstalled.
var clusterJSON = map[string]string{
	prodID: `{"kind":"Cluster","id":"` + prodID + `","href":"/api/clusters_mgmt/v1/clusters/` + prodID + `","name":"payments-prod",
	  "external_id":"7d1c8f4e-3a2b-4c5d-9e8f-1a2b3c4d5e6f","creation_timestamp":"2026-03-04T10:00:00Z","state":"ready","managed":true,
	  "cloud_provider":{"kind":"CloudProviderLink","id":"aws","href":"/api/clusters_mgmt/v1/cloud_providers/aws"},
	  "region":{"kind":"CloudRegionLink","id":"us-east-1","href":"/api/clusters_mgmt/v1/cloud_providers/aws/regions/us-east-1"},
	  "product":{"kind":"ProductLink","id":"rosa","href":"/api/clusters_mgmt/v1/products/rosa"},
	  "openshift_version":"4.17.9","version":{"kind":"Version","id":"openshift-v4.17.9","raw_id":"4.17.9","channel_group":"stable"},
	  "api":{"url":"` + prodAPI + `","listening":"external"},
	  "console":{"url":"https://console-openshift-console.apps.payments-prod.ab12.p1.openshiftapps.com"},
	  "hypershift":{"enabled":false},"external_auth_config":{"kind":"ExternalAuthConfig","enabled":false},
	  "status":{"state":"ready","dns_ready":true}}`,
	hcpID: `{"kind":"Cluster","id":"` + hcpID + `","href":"/api/clusters_mgmt/v1/clusters/` + hcpID + `","name":"edge-hcp",
	  "state":"ready","managed":true,"cloud_provider":{"kind":"CloudProviderLink","id":"aws"},"region":{"kind":"CloudRegionLink","id":"eu-west-1"},
	  "product":{"kind":"ProductLink","id":"rosa"},"openshift_version":"4.19.2","version":{"kind":"Version","raw_id":"4.19.2"},
	  "api":{"url":"` + hcpAPI + `","listening":"internal"},"hypershift":{"enabled":true},
	  "external_auth_config":{"kind":"ExternalAuthConfig","href":"/api/clusters_mgmt/v1/clusters/` + hcpID + `/external_auth_config","enabled":true}}`,
	osdID: `{"kind":"Cluster","id":"` + osdID + `","name":"data-osd","state":"ready","managed":true,
	  "cloud_provider":{"kind":"CloudProviderLink","id":"gcp"},"region":{"kind":"CloudRegionLink","id":"europe-west2"},
	  "product":{"kind":"ProductLink","id":"osd"},"version":{"kind":"Version","raw_id":"4.18.12"},
	  "api":{"url":"` + osdAPI + `","listening":"external"},"hypershift":{"enabled":false}}`,
	installingID: `{"kind":"Cluster","id":"` + installingID + `","name":"new-cluster","state":"installing","managed":true,
	  "product":{"kind":"ProductLink","id":"rosa"},"openshift_version":"4.19.2","api":{"listening":"external"},"hypershift":{"enabled":true}}`,
	uninstallingID: `{"kind":"Cluster","id":"` + uninstallingID + `","name":"old","state":"uninstalling","managed":true,
	  "product":{"kind":"ProductLink","id":"osd"},"openshift_version":"4.14.30","api":{"url":"` + uninstallingAPI + `","listening":"external"}}`,
}

var clusterOrder = []string{prodID, hcpID, osdID, installingID, uninstallingID}

// entraAuth is the ExternalAuth that rosa create external-auth-provider
// makes with a console client: the CLI's client is only among the
// audiences.
const entraAuth = `{"kind":"ExternalAuth","id":"entra","href":"/api/clusters_mgmt/v1/clusters/` + hcpID + `/external_auth_config/external_auths/entra",
  "issuer":{"url":"` + entraIssuer + `","audiences":["` + consoleClient + `","` + cliClientID + `"]},
  "claim":{"mappings":{"username":{"claim":"email","prefix_policy":""},"groups":{"claim":"groups"}}},
  "clients":[{"component":{"name":"console","namespace":"openshift-console"},"id":"` + consoleClient + `","type":"confidential"}],
  "status":{"state":{"value":"ready"}}}`

// fakeOCM serves Red Hat single sign-on's token endpoint and the parts of
// the OCM API the plugin calls, with OCM's error bodies.
type fakeOCM struct {
	srv *httptest.Server

	mu        sync.Mutex
	exchanges int
	sizes     []string
	rateLimit bool
	ssoDown   bool
	// auths holds the external_auths answer for each cluster.
	auths map[string]string
}

func newFakeOCM(t *testing.T) *fakeOCM {
	t.Helper()
	f := &fakeOCM{auths: map[string]string{hcpID: entraAuth}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func ocmError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"kind":"Error","id":"%d","href":"/api/clusters_mgmt/v1/errors/%[1]d","code":"CLUSTERS-MGMT-%[1]d","reason":%q,"operation_id":"9a8b7c6d"}`, status, reason)
}

func (f *fakeOCM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == tokenPath {
		f.token(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+accessValue {
		ocmError(w, http.StatusUnauthorized, "Invalid token")
		return
	}
	if f.rateLimit {
		w.Header().Set("Retry-After", "30")
		ocmError(w, http.StatusTooManyRequests, "Too many requests")
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == clustersPath:
		f.list(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/external_auth_config/external_auths"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, clustersPath+"/"), "/external_auth_config/external_auths")
		if _, ok := clusterJSON[id]; !ok {
			ocmError(w, http.StatusNotFound, fmt.Sprintf("Cluster '%s' not found", id))
			return
		}
		var items []string
		if a := f.auths[id]; a != "" {
			items = append(items, a)
		}
		fmt.Fprintf(w, `{"kind":"ExternalAuthList","page":1,"size":%d,"total":%[1]d,"items":[%s]}`, len(items), strings.Join(items, ","))
	case r.Method == http.MethodGet && strings.HasPrefix(path, clustersPath+"/"):
		body, ok := clusterJSON[strings.TrimPrefix(path, clustersPath+"/")]
		if !ok {
			ocmError(w, http.StatusNotFound, fmt.Sprintf("Cluster '%s' not found", strings.TrimPrefix(path, clustersPath+"/")))
			return
		}
		fmt.Fprint(w, body)
	default:
		ocmError(w, http.StatusNotFound, "Resource not found")
	}
}

// token answers the refresh token grant as Keycloak does.
func (f *fakeOCM) token(w http.ResponseWriter, r *http.Request) {
	if f.ssoDown {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<html><body>Service Unavailable</body></html>")
		return
	}
	if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "cloud-services" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"unsupported_grant_type","error_description":"Unsupported grant_type"}`)
		return
	}
	if r.PostForm.Get("refresh_token") != goodOffline {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Invalid refresh token"}`)
		return
	}
	f.exchanges++
	fmt.Fprintf(w, `{"access_token":%q,"expires_in":900,"refresh_expires_in":0,"refresh_token":%q,"token_type":"Bearer","id_token":"x.y.z","not-before-policy":0,"session_state":"3b6f","scope":"openid offline_access"}`, accessValue, goodOffline)
}

func (f *fakeOCM) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.sizes = append(f.sizes, q.Get("size"))
	var items []string
	for _, id := range clusterOrder {
		if q.Get("search") == "product.id = 'rosa'" && !strings.Contains(clusterJSON[id], `"id":"rosa"`) {
			continue
		}
		items = append(items, clusterJSON[id])
	}
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page < 1 || size < 1 {
		ocmError(w, http.StatusBadRequest, "page and size must be positive")
		return
	}
	start, end := min((page-1)*size, len(items)), min(page*size, len(items))
	fmt.Fprintf(w, `{"kind":"ClusterList","page":%d,"size":%d,"total":%d,"items":[%s]}`, page, end-start, len(items), strings.Join(items[start:end], ","))
}

func (f *fakeOCM) account(extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": f.srv.URL, ConfigTokenURL: f.srv.URL + tokenPath}
	maps.Copy(cfg, extra)
	return provider.Account{ID: "work", Config: cfg}
}

func secrets(token string) map[string]string { return map[string]string{SecretOfflineToken: token} }

func TestList(t *testing.T) {
	f := newFakeOCM(t)
	p := New()
	p.pageSize = 2
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodOffline)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	if want := []string{"payments-prod", "edge-hcp", "data-osd"}; !slices.Equal(names, want) {
		t.Fatalf("List = %v, want %v from every page, without the clusters being installed or uninstalled", names, want)
	}
	if f.exchanges != 1 || !slices.Equal(f.sizes, []string{"2", "2", "2"}) {
		t.Errorf("token exchanges %d and page sizes %v, want one exchange and three pages of 2", f.exchanges, f.sizes)
	}

	prod := clusters[0]
	if prod.ID != prodID || prod.Region != "us-east-1" || prod.Version != "4.17.9" || prod.Endpoints.Public != prodAPI || prod.Endpoints.Private != "" || prod.CAData != nil {
		t.Errorf("payments-prod = %+v", prod)
	}
	if want := map[string]string{"product": "rosa", "cloud": "aws"}; !maps.Equal(prod.Labels, want) {
		t.Errorf("payments-prod labels = %v, want %v", prod.Labels, want)
	}
	if prod.Credential != nil {
		t.Errorf("payments-prod credential = %+v, want none from a listing, since oc holds it", prod.Credential)
	}

	hcp := clusters[1]
	if hcp.Endpoints.Private != hcpAPI || hcp.Endpoints.Public != "" || hcp.Version != "4.19.2" {
		t.Errorf("edge-hcp = %+v, want a private endpoint", hcp)
	}
	if want := map[string]string{"product": "rosa", "cloud": "aws", "hcp": "true"}; !maps.Equal(hcp.Labels, want) {
		t.Errorf("edge-hcp labels = %v, want %v", hcp.Labels, want)
	}
	if c := hcp.Credential; c == nil || c.Type != provider.CredentialOIDC || c.Issuer != entraIssuer || c.ClientID != cliClientID ||
		!slices.Equal(c.Scopes, []string{"email"}) || c.ClientSecret != "" {
		t.Errorf("edge-hcp credential = %+v, want OIDC with the CLI's client from the audiences and the email scope", c)
	}

	if osd := clusters[2]; osd.Version != "4.18.12" || osd.Region != "europe-west2" || osd.Labels["cloud"] != "gcp" || osd.Labels["product"] != "osd" {
		t.Errorf("data-osd = %+v, want the version from version.raw_id", osd)
	}
}

func TestListSearch(t *testing.T) {
	f := newFakeOCM(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(map[string]any{ConfigSearch: "product.id = 'rosa'"}), Secrets: secrets(goodOffline)})
	if err != nil || len(clusters) != 2 || clusters[0].ID != prodID || clusters[1].ID != hcpID {
		t.Fatalf("List with a search = %+v, %v, want the two ready ROSA clusters", clusters, err)
	}
}

func TestAccessTokenReuse(t *testing.T) {
	f := newFakeOCM(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := New()
	p.now = func() time.Time { return now }
	req := provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodOffline)}
	for range 2 {
		if _, err := p.List(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if f.exchanges != 1 {
		t.Errorf("two listings exchanged the offline token %d times, want once", f.exchanges)
	}
	// The access token lasts 15 minutes; within a minute of that it is
	// exchanged again.
	now = now.Add(14*time.Minute + 30*time.Second)
	if _, err := p.List(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if f.exchanges != 2 {
		t.Errorf("exchanges = %d after the access token ran out, want 2", f.exchanges)
	}
}

func TestOIDCCredential(t *testing.T) {
	tests := []struct {
		name       string
		auth       string
		config     map[string]any
		wantClient string
		wantSecret string
		wantScopes []string
		wantCode   provider.Code
		wantHint   string
	}{
		{
			name: "client among the audiences", auth: entraAuth,
			wantClient: cliClientID, wantScopes: []string{"email"},
		},
		{
			name: "client for the CLI",
			auth: `{"id":"keycloak","issuer":{"url":"https://sso.example.com/realms/ocp","audiences":["console","oc-cli"]},
			  "claim":{"mappings":{"username":{"claim":"preferred_username"}}},
			  "clients":[{"id":"console","component":{"name":"console","namespace":"openshift-console"}},
			    {"id":"oc-cli","secret":"cli-secret","extra_scopes":["groups","openid"],"component":{"name":"cli","namespace":"openshift-console"}}]}`,
			wantClient: "oc-cli", wantSecret: "cli-secret", wantScopes: []string{"groups", "profile"},
		},
		{
			name: "oidcClientID setting", auth: entraAuth,
			config:     map[string]any{ConfigOIDCClientID: "c3d4e5f6-3333", ConfigOIDCScopes: "offline_access, groups email"},
			wantClient: "c3d4e5f6-3333", wantScopes: []string{"email", "offline_access", "groups"},
		},
		{
			name: "several clients to choose from",
			auth: `{"id":"okta","issuer":{"url":"https://example.okta.com","audiences":["console","cli-a","cli-b"]},
			  "clients":[{"id":"console","component":{"name":"console","namespace":"openshift-console"}}]}`,
			wantCode: provider.InvalidConfig, wantHint: "cli-a, cli-b",
		},
		{
			name: "no provider yet", auth: "",
			wantCode: provider.InvalidConfig, wantHint: "rosa create external-auth-provider --cluster=edge-hcp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOCM(t)
			f.auths[hcpID] = tt.auth
			c, err := New().Credential(context.Background(), provider.CredentialRequest{Account: f.account(tt.config), ClusterID: hcpID, Secrets: secrets(goodOffline)})
			if tt.wantCode != "" {
				if pe := provider.AsError(err); pe.Code != tt.wantCode || !strings.Contains(pe.Hint, tt.wantHint) {
					t.Fatalf("Credential = %+v, %+v, want %s with a hint naming %q", c, pe, tt.wantCode, tt.wantHint)
				}
				return
			}
			if err != nil || c.Type != provider.CredentialOIDC || c.ClientID != tt.wantClient || c.ClientSecret != tt.wantSecret || !slices.Equal(c.Scopes, tt.wantScopes) {
				t.Fatalf("Credential = %+v, %v, want client %q (secret %q) with scopes %v", c, err, tt.wantClient, tt.wantSecret, tt.wantScopes)
			}
		})
	}

	t.Run("listing", func(t *testing.T) {
		// A cluster whose client cannot be told is listed without a
		// credential; Credential then explains.
		f := newFakeOCM(t)
		f.auths[hcpID] = `{"id":"okta","issuer":{"url":"https://example.okta.com","audiences":["a","b"]}}`
		clusters, err := New().List(context.Background(), provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodOffline)})
		if err != nil || len(clusters) != 3 || clusters[1].Credential != nil {
			t.Errorf("List = %+v, %v, want edge-hcp without a credential", clusters, err)
		}
	})
}

// The kubeconfig oc config view --raw -o json prints after oc login --web
// to payments-prod as alice and as admin, and oc login --token to data-osd
// with a service account token. Server names are as users type them.
const kubeconfigJSON = `{
    "kind": "Config",
    "apiVersion": "v1",
    "preferences": {},
    "clusters": [
        {"name": "api-data-osd-ef56-p2-openshiftapps-com:6443", "cluster": {"server": "https://API.data-osd.ef56.p2.openshiftapps.com:6443/"}},
        {"name": "api-payments-prod-ab12-p1-openshiftapps-com:6443", "cluster": {"server": "https://api.payments-prod.ab12.p1.openshiftapps.com:6443"}},
        {"name": "kind-dev", "cluster": {"server": "https://127.0.0.1:6443", "certificate-authority-data": "LS0tLS1CRUdJTg=="}}
    ],
    "users": [
        {"name": "admin/api-payments-prod-ab12-p1-openshiftapps-com:6443", "user": {"token": "sha256~Z1x2c3v4b5n6m7a8s9d0f1g2h3j4k5l6q7w8e9r0t1y"}},
        {"name": "alice@example.com/api-payments-prod-ab12-p1-openshiftapps-com:6443", "user": {"token": "sha256~oV2rP7Yb3kQ9mX4tZ8wN1cL6hJ5gF0dS2aE7uI3yR9o"}},
        {"name": "ci/api-data-osd-ef56-p2-openshiftapps-com:6443", "user": {"token": "eyJhbGciOiJSUzI1NiJ9.c2VydmljZWFjY291bnQ.c2ln"}},
        {"name": "kind-dev", "user": {"client-certificate-data": "LS0t", "client-key-data": "LS0t"}}
    ],
    "contexts": [
        {"name": "ci/api-data-osd-ef56-p2-openshiftapps-com:6443/ci", "context": {"cluster": "api-data-osd-ef56-p2-openshiftapps-com:6443", "namespace": "ci", "user": "ci/api-data-osd-ef56-p2-openshiftapps-com:6443"}},
        {"name": "default/api-payments-prod-ab12-p1-openshiftapps-com:6443/admin", "context": {"cluster": "api-payments-prod-ab12-p1-openshiftapps-com:6443", "namespace": "default", "user": "admin/api-payments-prod-ab12-p1-openshiftapps-com:6443"}},
        {"name": "default/api-payments-prod-ab12-p1-openshiftapps-com:6443/alice@example.com", "context": {"cluster": "api-payments-prod-ab12-p1-openshiftapps-com:6443", "namespace": "default", "user": "alice@example.com/api-payments-prod-ab12-p1-openshiftapps-com:6443"}},
        {"name": "kind-dev", "context": {"cluster": "kind-dev", "user": "kind-dev"}}
    ],
    "current-context": "$FAKE_OC_CURRENT"
}`

const (
	aliceToken   = "sha256~oV2rP7Yb3kQ9mX4tZ8wN1cL6hJ5gF0dS2aE7uI3yR9o"
	aliceContext = "default/api-payments-prod-ab12-p1-openshiftapps-com:6443/alice@example.com"
	adminToken   = "sha256~Z1x2c3v4b5n6m7a8s9d0f1g2h3j4k5l6q7w8e9r0t1y"
	adminContext = "default/api-payments-prod-ab12-p1-openshiftapps-com:6443/admin"
)

// ocScript stands in for oc. It prints the kubeconfig above, and the
// UserOAuthAccessToken of the token the test names, as the OAuth API
// serves it; anything else fails, so the tests also check the arguments.
// It uses only shell builtins and no here-documents, which macOS's sh
// writes to /tmp, so that it also works in tug's sandbox: the JSON is in
// single quotes, with the variables spliced in.
var ocScript = `#!/bin/sh
[ -n "$FAKE_OC_LOG" ] && echo "$*" >> "$FAKE_OC_LOG"
case "$*" in
"config view --raw -o json")
	printf '%s\n' '` + strings.ReplaceAll(kubeconfigJSON, `$FAKE_OC_CURRENT`, `'"$FAKE_OC_CURRENT"'`) + `'
	;;
"get useroauthaccesstokens $FAKE_OC_TOKEN_NAME -o json --context=$FAKE_OC_CONTEXT")
	case "$FAKE_OC_STATE" in
	unauthorized)
		echo "error: You must be logged in to the server (Unauthorized)" >&2
		exit 1
		;;
	no-resource)
		echo 'error: the server doesn'"'"'t have a resource type "useroauthaccesstokens"' >&2
		exit 1
		;;
	esac
	printf '%s\n' '{
    "apiVersion": "oauth.openshift.io/v1",
    "authorizeToken": "sha256~bm90LXRoZS1hdXRob3JpemUtdG9rZW4",
    "clientName": "openshift-cli-client",
    "expiresIn": 86400,
    "inactivityTimeoutSeconds": '"${FAKE_OC_INACTIVITY:-0}"',
    "kind": "UserOAuthAccessToken",
    "metadata": {
        "creationTimestamp": "'"$FAKE_OC_CREATED"'",
        "name": "'"$FAKE_OC_TOKEN_NAME"'",
        "resourceVersion": "4471923",
        "uid": "5c0d3a2b-7e6f-4a1b-9c8d-0e1f2a3b4c5d"
    },
    "redirectURI": "http://127.0.0.1:41273/callback",
    "scopes": ["user:full"],
    "userName": "alice@example.com",
    "userUID": "9f8e7d6c-5b4a-4321-8765-0fedcba98765"
}'
	;;
*)
	echo "error: unexpected arguments: $*" >&2
	exit 1
	;;
esac
`

// fakeOC puts the fake oc first on PATH, for the token of the named
// context, and returns the file it logs its arguments to.
func fakeOC(t *testing.T, current, contextName, token string, created time.Time) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake oc is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oc"), []byte(ocScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(t.TempDir(), "oc.log")
	t.Setenv("FAKE_OC_LOG", log)
	t.Setenv("FAKE_OC_CURRENT", current)
	t.Setenv("FAKE_OC_CONTEXT", contextName)
	t.Setenv("FAKE_OC_TOKEN_NAME", tokenObjectName(token))
	t.Setenv("FAKE_OC_CREATED", created.UTC().Format(time.RFC3339))
	t.Setenv("FAKE_OC_STATE", "")
	t.Setenv("FAKE_OC_INACTIVITY", "")
	return log
}

func ocCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestOCToken(t *testing.T) {
	f := newFakeOCM(t)
	created := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	now := created.Add(4 * time.Hour)
	credential := func(t *testing.T, id string) (provider.Credential, error) {
		t.Helper()
		p := New()
		p.now = func() time.Time { return now }
		return p.Credential(context.Background(), provider.CredentialRequest{Account: f.account(nil), ClusterID: id, Secrets: secrets(goodOffline)})
	}

	t.Run("current context", func(t *testing.T) {
		log := fakeOC(t, aliceContext, aliceContext, aliceToken, created)
		c, err := credential(t, prodID)
		if want := created.Add(24 * time.Hour); err != nil || c.Type != provider.CredentialToken || c.Token != aliceToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(want) {
			t.Fatalf("Credential = %+v, %v, want alice's token until %s", c, err, want)
		}
		want := []string{"config view --raw -o json", "get useroauthaccesstokens sha256~xx1O_8EPEiaLc_l-4W2Pr-VY4bfcCqcvFehwVGknTEs -o json --context=" + aliceContext}
		if got := ocCalls(t, log); !slices.Equal(got, want) {
			t.Errorf("oc calls = %q, want %q", got, want)
		}
	})
	t.Run("first context", func(t *testing.T) {
		fakeOC(t, "kind-dev", adminContext, adminToken, created)
		if c, err := credential(t, prodID); err != nil || c.Token != adminToken {
			t.Fatalf("Credential = %+v, %v, want the first context's token", c, err)
		}
	})
	t.Run("inactivity timeout", func(t *testing.T) {
		fakeOC(t, aliceContext, aliceContext, aliceToken, created)
		t.Setenv("FAKE_OC_INACTIVITY", "18000")
		if c, err := credential(t, prodID); err != nil || c.ExpiresAt == nil || !c.ExpiresAt.Equal(created.Add(5*time.Hour)) {
			t.Fatalf("Credential = %+v, %v, want the token until it would time out", c, err)
		}
	})
	t.Run("cluster without the token API", func(t *testing.T) {
		fakeOC(t, aliceContext, aliceContext, aliceToken, created)
		t.Setenv("FAKE_OC_STATE", "no-resource")
		if c, err := credential(t, prodID); err != nil || c.Token != aliceToken || c.ExpiresAt == nil || !c.ExpiresAt.Equal(now.Add(fallbackLifetime)) {
			t.Fatalf("Credential = %+v, %v, want the token for %s", c, err, fallbackLifetime)
		}
	})
	t.Run("service account token", func(t *testing.T) {
		// The server in the kubeconfig differs in case and by a slash.
		log := fakeOC(t, aliceContext, aliceContext, aliceToken, created)
		c, err := credential(t, osdID)
		if err != nil || c.Token != "eyJhbGciOiJSUzI1NiJ9.c2VydmljZWFjY291bnQ.c2ln" || c.ExpiresAt == nil || !c.ExpiresAt.Equal(now.Add(fallbackLifetime)) {
			t.Fatalf("Credential = %+v, %v, want the service account token for %s", c, err, fallbackLifetime)
		}
		if got := ocCalls(t, log); len(got) != 1 {
			t.Errorf("oc calls = %q, want only config view", got)
		}
	})

	failures := []struct {
		name  string
		id    string
		state string
		now   time.Time
		code  provider.Code
		hint  string
	}{
		{"signed out", prodID, "unauthorized", now, provider.Unauthenticated, "the token oc keeps has expired or was revoked; sign in with `oc login --web " + prodAPI + "`"},
		{"expired", prodID, "", created.Add(25 * time.Hour), provider.Unauthenticated, "sign in with `oc login --web " + prodAPI + "`"},
		{"never signed in", uninstallingID, "", now, provider.Unauthenticated, "sign in with `oc login --web " + uninstallingAPI + "`; tug then uses the token oc keeps"},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			fakeOC(t, aliceContext, aliceContext, aliceToken, created)
			t.Setenv("FAKE_OC_STATE", tt.state)
			now := tt.now
			p := New()
			p.now = func() time.Time { return now }
			_, err := p.Credential(context.Background(), provider.CredentialRequest{Account: f.account(nil), ClusterID: tt.id, Secrets: secrets(goodOffline)})
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.HasPrefix(pe.Hint, tt.hint) {
				t.Errorf("Credential = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}

	t.Run("without oc", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := credential(t, prodID)
		if pe := provider.AsError(err); pe.Code != provider.Unavailable || pe.Hint != InstallHint {
			t.Errorf("Credential = %+v, want Unavailable with the install hint", pe)
		}
	})
}

func TestErrors(t *testing.T) {
	f := newFakeOCM(t)
	ctx := context.Background()

	_, err := New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(revokedOffline)})
	if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Message, "Invalid refresh token") || !strings.Contains(pe.Hint, "console.redhat.com/openshift/token") {
		t.Errorf("List with a revoked offline token = %+v, want Unauthenticated with a hint", pe)
	}

	_, err = New().Credential(ctx, provider.CredentialRequest{Account: f.account(nil), ClusterID: "2zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", Secrets: secrets(goodOffline)})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || !strings.Contains(pe.Message, "not found") || !strings.Contains(pe.Hint, "tug sync") {
		t.Errorf("Credential for a deleted cluster = %+v, want NotFound with a hint", pe)
	}

	f.mu.Lock()
	f.rateLimit = true
	f.mu.Unlock()
	_, err = New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodOffline)})
	if pe := provider.AsError(err); pe.Code != provider.RateLimited || pe.RetryAfterSeconds != 30 || pe.Message != "GET "+clustersPath+"?page=1&size=100: Too many requests" || pe.Hint == "" {
		t.Errorf("List when rate limited = %+v, want RateLimited after 30 seconds with a hint", pe)
	}
	f.mu.Lock()
	f.rateLimit, f.ssoDown = false, true
	f.mu.Unlock()
	_, err = New().List(ctx, provider.ListRequest{Account: f.account(nil), Secrets: secrets(goodOffline)})
	if provider.CodeOf(err) != provider.Unavailable {
		t.Errorf("List while single sign-on is down = %v, want Unavailable", err)
	}
	f.mu.Lock()
	f.ssoDown = false
	f.mu.Unlock()

	tests := []struct {
		name    string
		config  map[string]any
		secrets map[string]string
		want    provider.Code
	}{
		{"no offline token", nil, nil, provider.Unauthenticated},
		{"not an offline token", nil, secrets("tug-conformance-invalid"), provider.Unauthenticated},
		{"plain http tokenURL", map[string]any{ConfigTokenURL: "http://sso.example.com/token"}, secrets(goodOffline), provider.InvalidConfig},
		{"plain http apiURL", map[string]any{"apiURL": "http://api.example.com", ConfigTokenURL: f.srv.URL + tokenPath}, secrets(goodOffline), provider.InvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().List(ctx, provider.ListRequest{Account: provider.Account{ID: "work", Config: tt.config}, Secrets: tt.secrets})
			if provider.CodeOf(err) != tt.want {
				t.Errorf("List = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestNormaliseServer(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://api.x.openshiftapps.com:6443", "https://api.x.openshiftapps.com:6443"},
		{"https://API.X.openshiftapps.com:6443/", "https://api.x.openshiftapps.com:6443"},
		{"https://api.x.openshiftapps.com", "https://api.x.openshiftapps.com:443"},
		{"https://api.x.openshiftapps.com:443", "https://api.x.openshiftapps.com:443"},
		{"https://rancher.example.com/k8s/clusters/c-1/", "https://rancher.example.com:443/k8s/clusters/c-1"},
	}
	for _, tt := range tests {
		if got := normaliseServer(tt.in); got != tt.want {
			t.Errorf("normaliseServer(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTokenObjectName(t *testing.T) {
	// Computed separately, with Python's hashlib and base64, from the rule
	// in OpenShift's oauth-apiserver.
	if got, want := tokenObjectName(aliceToken), "sha256~xx1O_8EPEiaLc_l-4W2Pr-VY4bfcCqcvFehwVGknTEs"; got != want {
		t.Errorf("tokenObjectName(%q) = %q, want %q", aliceToken, got, want)
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-openshift")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-openshift").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	f := newFakeOCM(t)
	// The first cluster listed is payments-prod, whose token comes from oc.
	fakeOC(t, aliceContext, aliceContext, aliceToken, time.Now().Add(-time.Hour))
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         f.account(nil),
		Secrets:         secrets(goodOffline),
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the openshift plugin does not conform")
	}
	// The fake oc learns the token it answers for from the test's
	// environment, which tug passes on only when told to.
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin,
		Settings: f.account(nil).Config, Secrets: secrets(goodOffline),
		Env: []string{"FAKE_OC_CURRENT", "FAKE_OC_CONTEXT", "FAKE_OC_TOKEN_NAME", "FAKE_OC_CREATED", "FAKE_OC_STATE", "FAKE_OC_INACTIVITY"}})
}
