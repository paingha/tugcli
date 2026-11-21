package coreweave

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const (
	goodToken    = "CW-token"
	revokedToken = "CW-revoked"
)

const clusterJSON = `{"id":"c-7f3a","name":"train-us-east","zone":"US-EAST-04A","version":"v1.34","status":"RUNNING",
  "public":true,"vpcId":"vpc-1","apiServerEndpoint":"c-7f3a.k8s.us-east-04a.coreweave.com",
  "oidc":{"issuerUrl":"https://idp.example.com","clientId":"kubernetes"}}`

func fakeCKS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer "+revokedToken {
			// CoreWeave answers 403, not 401, for some tokens it rejects.
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"token revoked"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"invalid API access token"}`)
			return
		}
		switch r.URL.Path {
		case "/v1beta1/cks/clusters":
			fmt.Fprint(w, `{"items":[`+clusterJSON+`,
			  {"id":"c-new","name":"creating","zone":"US-EAST-04A","version":"v1.34","status":"CREATING","apiServerEndpoint":""},
			  {"id":"c-priv","name":"private-eu","zone":"EU-SOUTH-03A","version":"v1.33","public":false,"apiServerEndpoint":"https://c-priv.internal:6443"}
			]}`)
		case "/v1beta1/cks/clusters/c-7f3a":
			fmt.Fprint(w, `{"cluster":`+clusterJSON+`}`)
		case "/v1beta1/cks/clusters/c-priv":
			fmt.Fprint(w, `{"cluster":{"id":"c-priv","name":"private-eu"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"cluster not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func account(srv *httptest.Server, extra map[string]any) provider.Account {
	cfg := map[string]any{"apiURL": srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	return provider.Account{ID: "work", Config: cfg}
}

var secrets = map[string]string{"api_token": goodToken}

func TestList(t *testing.T) {
	srv := fakeCKS(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: account(srv, nil), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters (the one being created is skipped), got %+v", clusters)
	}
	train := clusters[0]
	if train.ID != "c-7f3a" || train.Region != "US-EAST-04A" || train.Version != "v1.34" ||
		train.Endpoints.Public != "https://c-7f3a.k8s.us-east-04a.coreweave.com" || train.CAData != nil {
		t.Fatalf("train: %+v", train)
	}
	if train.Labels["zone"] != "us-east-04a" || train.Labels["vpc-id"] != "vpc-1" || train.Labels["gpu"] != "" {
		t.Fatalf("labels: %v", train.Labels)
	}
	if train.Credential == nil || train.Credential.Token != goodToken {
		t.Fatalf("managed auth should return the token inline: %+v", train.Credential)
	}
	if priv := clusters[1]; priv.Endpoints.Private != "https://c-priv.internal:6443" || priv.Endpoints.Public != "" {
		t.Fatalf("private cluster: %+v", priv.Endpoints)
	}

	zoned, _ := New().List(context.Background(), provider.ListRequest{Account: account(srv, map[string]any{ConfigZone: "eu-south-03a"}), Secrets: secrets})
	if len(zoned) != 1 || zoned[0].ID != "c-priv" {
		t.Fatalf("zone filter: %+v", zoned)
	}
}

func TestListReturnsTheCredentialForTheAuthMode(t *testing.T) {
	srv := fakeCKS(t)
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: account(srv, map[string]any{ConfigAuth: "oidc"}), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	// With auth=oidc the inline credential must be the OIDC issuer, so that
	// re-importing a managed-auth account replaces its cached API token.
	train := clusters[0]
	if c := train.Credential; c == nil || c.Type != provider.CredentialOIDC || c.Issuer != "https://idp.example.com" || c.Token != "" {
		t.Fatalf("oidc account, cluster with OIDC: %+v", c)
	}
	if priv := clusters[1]; priv.Credential != nil {
		t.Fatalf("oidc account, cluster without OIDC: %+v", priv.Credential)
	}
}

func TestCredential(t *testing.T) {
	srv := fakeCKS(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		auth    string
		cluster string
		check   func(provider.Credential, error) bool
	}{
		{"managed", "", "c-7f3a", func(c provider.Credential, err error) bool { return err == nil && c.Token == goodToken }},
		{"oidc", "oidc", "c-7f3a", func(c provider.Credential, err error) bool {
			return err == nil && c.Type == provider.CredentialOIDC && c.Issuer == "https://idp.example.com" && c.ClientID == "kubernetes"
		}},
		{"oidc without config", "oidc", "c-priv", func(_ provider.Credential, err error) bool { return provider.CodeOf(err) == provider.InvalidConfig }},
		{"unknown cluster", "", "c-gone", func(_ provider.Credential, err error) bool { return provider.CodeOf(err) == provider.NotFound }},
		{"bad auth mode", "saml", "c-7f3a", func(_ provider.Credential, err error) bool { return provider.CodeOf(err) == provider.InvalidConfig }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New().Credential(ctx, provider.CredentialRequest{
				Account: account(srv, map[string]any{ConfigAuth: tt.auth}), ClusterID: tt.cluster, Secrets: secrets,
			})
			if !tt.check(c, err) {
				t.Fatalf("got %+v, %v", c, err)
			}
		})
	}
}

func TestBadToken(t *testing.T) {
	srv := fakeCKS(t)
	tests := []struct {
		name, token, message string
	}{
		{"401", "nope", "invalid API access token"},
		{"403", revokedToken, "token revoked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := map[string]string{"api_token": tt.token}
			_, err := New().List(context.Background(), provider.ListRequest{Account: account(srv, nil), Secrets: bad})
			if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Message, tt.message) || pe.Hint == "" {
				t.Errorf("List = %+v, want Unauthenticated with a hint", pe)
			}
			_, err = New().Credential(context.Background(), provider.CredentialRequest{Account: account(srv, nil), Secrets: bad, ClusterID: "c-7f3a"})
			if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || pe.Hint == "" {
				t.Errorf("Credential = %+v, want Unauthenticated with a hint", pe)
			}
		})
	}
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"c.k8s.coreweave.com":         "https://c.k8s.coreweave.com",
		"https://c.k8s.coreweave.com": "https://c.k8s.coreweave.com",
		"c.k8s.coreweave.com:6443":    "https://c.k8s.coreweave.com:6443",
	} {
		if got := endpointURL(in); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	srv := fakeCKS(t)
	bin := filepath.Join(t.TempDir(), "tug-provider-coreweave")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-coreweave").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         account(srv, nil),
		Secrets:         secrets,
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the coreweave plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin, Settings: account(srv, nil).Config, Secrets: secrets})
}
