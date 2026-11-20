package google

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/paingha/tugcli/pkg/provider"
)

const listBody = `{"clusters":[
 {"name":"payments-prod","location":"europe-west1","endpoint":"34.1.2.3","currentMasterVersion":"1.34.1-gke.100",
  "masterAuth":{"clusterCaCertificate":"%s"},
  "resourceLabels":{"environment":"prod","team":"pay"},
  "nodePools":[{"config":{}},{"config":{"accelerators":[{"acceleratorType":"nvidia-l4"}]}}]},
 {"name":"internal","location":"us-central1-a","endpoint":"34.9.9.9","currentMasterVersion":"1.33.5",
  "privateClusterConfig":{"enablePrivateEndpoint":true,"privateEndpoint":"10.0.0.2"}}
],"missingZones":[]}`

func fakeContainerAPI(t *testing.T, token string) *httptest.Server {
	t.Helper()
	ca := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer "+token:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`)
		case r.URL.Path == "/v1/projects/acme-prod/locations/-/clusters":
			fmt.Fprintf(w, listBody, ca)
		case r.URL.Path == "/v1/projects/partial/locations/-/clusters":
			fmt.Fprint(w, `{"clusters":[],"missingZones":["asia-east1-a"]}`)
		case r.URL.Path == "/v1/projects/disabled/locations/-/clusters":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":403,"message":"Kubernetes Engine API has not been used in project disabled before or it is disabled.","status":"PERMISSION_DENIED"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(srv *httptest.Server, token string, tokenErr error) *Provider {
	return New(
		WithAPIURL(srv.URL+"/v1"),
		WithTokenSource(func(context.Context) (oauth2.TokenSource, error) {
			if tokenErr != nil {
				return nil, tokenErr
			}
			return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token, Expiry: time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)}), nil
		}),
		WithLookPath(func(string) (string, error) { return "", errors.New("not found") }),
	)
}

func TestList(t *testing.T) {
	srv := fakeContainerAPI(t, "ya29.good")
	p := newTestProvider(srv, "ya29.good", nil)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "acme-prod"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("got %d clusters", len(clusters))
	}
	prod, internal := clusters[0], clusters[1]
	if prod.ID != "europe-west1/payments-prod" || prod.Endpoints.Public != "https://34.1.2.3" || string(prod.CAData) != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("prod: %+v", prod)
	}
	if prod.Labels["env"] != "prod" || prod.Labels["gpu"] != "true" || prod.Labels["gcp-project"] != "acme-prod" {
		t.Fatalf("labels: %v", prod.Labels)
	}
	if internal.Endpoints.Public != "" || internal.Endpoints.Private != "https://10.0.0.2" || internal.Labels["gpu"] != "" {
		t.Fatalf("private cluster: %+v", internal)
	}
}

func TestListErrors(t *testing.T) {
	srv := fakeContainerAPI(t, "ya29.good")
	tests := []struct {
		name    string
		p       *Provider
		project string
		code    provider.Code
		hint    string
	}{
		{"bad token", newTestProvider(srv, "ya29.stale", nil), "acme-prod", provider.Unauthenticated, "application-default login"},
		{"no ADC", newTestProvider(srv, "", errors.New("could not find default credentials")), "acme-prod", provider.Unauthenticated, "application-default login"},
		{"API disabled", newTestProvider(srv, "ya29.good", nil), "disabled", provider.PermissionDenied, "gcloud services enable container.googleapis.com"},
		{"no project", newTestProvider(srv, "ya29.good", nil), "", provider.InvalidConfig, "--account"},
		// A partial list would mark the missing locations' clusters stale.
		{"missing zones", newTestProvider(srv, "ya29.good", nil), "partial", provider.Unavailable, "sync again"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: tt.project}})
			pe := provider.AsError(err)
			if pe.Code != tt.code || !strings.Contains(pe.Hint, tt.hint) {
				t.Fatalf("got %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}
}

func TestCredential(t *testing.T) {
	srv := fakeContainerAPI(t, "ya29.good")
	c, err := newTestProvider(srv, "ya29.good", nil).Credential(context.Background(), provider.CredentialRequest{ClusterID: "europe-west1/payments-prod"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "ya29.good" || c.ExpiresAt == nil || c.ExpiresAt.Hour() != 13 {
		t.Fatalf("credential: %+v", c)
	}

	noADC := newTestProvider(srv, "", errors.New("no ADC"))
	if _, err := noADC.Credential(context.Background(), provider.CredentialRequest{}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Fatalf("without ADC or plugin: %v", err)
	}
	withPlugin := New(
		WithTokenSource(func(context.Context) (oauth2.TokenSource, error) { return nil, errors.New("no ADC") }),
		WithLookPath(func(string) (string, error) { return "/usr/bin/gke-gcloud-auth-plugin", nil }),
	)
	c, err = withPlugin.Credential(context.Background(), provider.CredentialRequest{})
	if err != nil || c.Type != provider.CredentialDelegate || c.Exec.Command != authPlugin {
		t.Fatalf("delegate fallback: %+v %v", c, err)
	}
}

func TestAccountsFromGcloudConfigurations(t *testing.T) {
	dir := t.TempDir()
	confs := filepath.Join(dir, "configurations")
	os.MkdirAll(confs, 0o700)
	os.WriteFile(filepath.Join(confs, "config_default"), []byte("[core]\nproject = acme-prod\naccount = me@example.com\n"), 0o600)
	os.WriteFile(filepath.Join(confs, "config_dev"), []byte("[core]\nproject = acme-dev\n"), 0o600)
	os.WriteFile(filepath.Join(confs, "config_dup"), []byte("[core]\nproject = acme-dev\n"), 0o600)
	os.WriteFile(filepath.Join(confs, "config_empty"), []byte("[core]\n"), 0o600)

	p := New(WithGetenv(func(k string) string {
		if k == "CLOUDSDK_CONFIG" {
			return dir
		}
		return ""
	}))
	accts, err := p.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 2 || accts[0].ID != "acme-dev" || accts[1].ID != "acme-prod" || accts[1].ConfigString(ConfigProject) != "acme-prod" {
		t.Fatalf("accounts: %+v", accts)
	}
}
