// Package coreweave is tug's first-party provider plugin for CoreWeave
// Kubernetes Service (CKS). It lists clusters with the CKS API. With CKS
// managed auth the API access token is also the Kubernetes credential;
// organisations using their own OIDC provider set auth=oidc.
package coreweave

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "coreweave"
	Version = "0.1.0"
)

// DefaultAPIURL is the CoreWeave API.
const DefaultAPIURL = "https://api.coreweave.com"

// Account settings.
const (
	// ConfigAuth is "managed" (default) or "oidc".
	ConfigAuth = "auth"
	// ConfigZone limits listing to one availability zone.
	ConfigZone = "zone"
)

// Provider implements the plugin.
type Provider struct {
	http *http.Client
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "CoreWeave Kubernetes Service (CKS)",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   "api_token",
			Prompt: "CoreWeave API access token",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	token := secrets["api_token"]
	if token == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no api_token").
			WithHint("create an API access token in the CoreWeave Cloud Console")
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     token,
		HTTP:      p.http,
		UserAgent: "tug-provider-coreweave/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"message": ...} or {"error": ...} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Error
}

// CKS API types, trimmed to the fields the plugin uses.
type (
	cksCluster struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Zone              string `json:"zone"`
		Version           string `json:"version"`
		Status            string `json:"status"`
		Public            bool   `json:"public"`
		VPCID             string `json:"vpcId"`
		APIServerEndpoint string `json:"apiServerEndpoint"`
		OIDC              *struct {
			IssuerURL string `json:"issuerUrl"`
			ClientID  string `json:"clientId"`
		} `json:"oidc"`
	}
	listResponse struct {
		Items []cksCluster `json:"items"`
	}
	getResponse struct {
		Cluster cksCluster `json:"cluster"`
	}
)

// List returns the account's CKS clusters. Clusters without an API server
// endpoint yet (still being created) are skipped until the next sync.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	var resp listResponse
	if err := c.Do(ctx, http.MethodGet, "/v1beta1/cks/clusters", nil, &resp); err != nil {
		return nil, unauthorizedHint(err)
	}
	zone := req.Account.ConfigString(ConfigZone)
	out := make([]provider.Cluster, 0, len(resp.Items))
	for _, cl := range resp.Items {
		if zone != "" && !strings.EqualFold(cl.Zone, zone) {
			continue
		}
		if cl.APIServerEndpoint == "" {
			fmt.Fprintf(os.Stderr, "coreweave: skipping %s: no API server endpoint yet (status %s)\n", cl.Name, cl.Status)
			continue
		}
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Name,
			Region:    cl.Zone,
			Version:   cl.Version,
			Endpoints: provider.Endpoints{Public: endpointURL(cl.APIServerEndpoint)},
			// The managed endpoint has a publicly trusted certificate, so no
			// CA is needed. The API does not expose node pools, so there is
			// no gpu label.
			Labels: map[string]string{"zone": strings.ToLower(cl.Zone)},
		}
		// Returning the credential inline lets import and sync replace the
		// cached one, both when the token changes and when an account
		// switches between managed auth and OIDC.
		if cred, ok := inlineCredential(req.Account, c.Token, cl); ok {
			pc.Credential = &cred
		} else {
			fmt.Fprintf(os.Stderr, "coreweave: %s has no OIDC configuration; its credential is fetched on demand\n", cl.Name)
		}
		if !cl.Public {
			pc.Endpoints = provider.Endpoints{Private: pc.Endpoints.Public}
		}
		if cl.VPCID != "" {
			pc.Labels["vpc-id"] = cl.VPCID
		}
		out = append(out, pc)
	}
	return out, nil
}

func managed(acct provider.Account) bool {
	mode := acct.ConfigString(ConfigAuth)
	return mode == "" || mode == "managed"
}

// inlineCredential returns the credential a listed cluster uses: the API
// token for managed auth, or the cluster's OIDC issuer. It reports false for
// an OIDC account whose cluster has no OIDC configuration.
func inlineCredential(acct provider.Account, token string, cl cksCluster) (provider.Credential, bool) {
	if managed(acct) {
		return provider.Token(token, provider.NoExpiry), true
	}
	if o := cl.OIDC; o != nil && o.IssuerURL != "" && o.ClientID != "" {
		return provider.OIDC(o.IssuerURL, o.ClientID, "openid", "groups"), true
	}
	return provider.Credential{}, false
}

// endpointURL adds https:// to a bare host name.
func endpointURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return s
	}
	return "https://" + strings.TrimPrefix(s, "//")
}

// Credential returns the API access token for managed auth, or the
// cluster's OIDC issuer for auth=oidc.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	switch mode := req.Account.ConfigString(ConfigAuth); mode {
	case "", "managed":
		// Confirm the token works and the cluster exists before handing the
		// token to kubectl.
		var resp getResponse
		if err := c.Do(ctx, http.MethodGet, "/v1beta1/cks/clusters/"+url.PathEscape(req.ClusterID), nil, &resp); err != nil {
			return provider.Credential{}, unauthorizedHint(err)
		}
		return provider.Token(c.Token, provider.NoExpiry), nil
	case "oidc":
		var resp getResponse
		if err := c.Do(ctx, http.MethodGet, "/v1beta1/cks/clusters/"+url.PathEscape(req.ClusterID), nil, &resp); err != nil {
			return provider.Credential{}, unauthorizedHint(err)
		}
		o := resp.Cluster.OIDC
		if o == nil || o.IssuerURL == "" || o.ClientID == "" {
			return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster %s has no OIDC configuration", req.ClusterID).
				WithHint("use auth=managed, or configure OIDC on the cluster")
		}
		return provider.OIDC(o.IssuerURL, o.ClientID, "openid", "groups"), nil
	default:
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "%s must be managed or oidc, got %q", ConfigAuth, mode)
	}
}

// unauthorizedHint adds a hint to authentication failures. CoreWeave can
// answer 403 for an invalid or revoked token.
func unauthorizedHint(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated, provider.PermissionDenied:
		return provider.AsError(err).WithHint("check the API access token in the CoreWeave Cloud Console; tokens are shown only once when created")
	default:
		return err
	}
}
