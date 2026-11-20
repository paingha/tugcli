// Package digitalocean is tug's first-party provider plugin for DigitalOcean
// Kubernetes (DOKS). It lists clusters with the DigitalOcean API. The API
// hands out a cluster's CA only with a credential, so listing fetches each
// cluster's credentials too and returns the short-lived token inline; tug
// moves it to the keychain and asks for a new one when it expires.
package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "digitalocean"
	Version = "0.1.0"
)

// DefaultAPIURL is the DigitalOcean API.
const DefaultAPIURL = "https://api.digitalocean.com"

// pageSize is the largest page the API serves.
const pageSize = 200

// tokenLifetime is how long the cluster tokens the plugin asks for last.
// DigitalOcean's default is seven days; an hour limits what a leaked token
// can do, and tug fetches a new one a minute before it runs out.
const tokenLifetime = time.Hour

const (
	tokenHint = "create a personal access token with the kubernetes:read and kubernetes:access_cluster scopes under API > Tokens in the DigitalOcean control panel"
	scopeHint = "the token needs the kubernetes:read scope to list clusters and kubernetes:access_cluster to fetch their credentials; " +
		"create one with both under API > Tokens in the DigitalOcean control panel"
	rateHint = "the DigitalOcean API allows 250 requests a minute and 5,000 an hour; wait a minute and try again"
)

// Provider implements the plugin.
type Provider struct {
	http *http.Client
	// now returns the current time; tests replace it.
	now func() time.Time
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{now: time.Now} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "DigitalOcean Kubernetes (DOKS)",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   "api_token",
			Prompt: "DigitalOcean personal access token (kubernetes:read and kubernetes:access_cluster)",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	token := secrets["api_token"]
	if token == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no api_token").WithHint(tokenHint)
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     token,
		HTTP:      p.http,
		UserAgent: "tug-provider-digitalocean/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"id": ..., "message": ...} bodies.
func errorMessage(body []byte) string {
	var e struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.ID
}

// withHint adds the next step to the API errors a user can fix.
func withHint(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the token is invalid, expired or revoked; " + tokenHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint(scopeHint)
	case provider.RateLimited:
		return provider.AsError(err).WithHint(rateHint)
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	doCluster struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Region    string   `json:"region"`
		Version   string   `json:"version"`
		Endpoint  string   `json:"endpoint"`
		Tags      []string `json:"tags"`
		HA        bool     `json:"ha"`
		NodePools []struct {
			Size string `json:"size"`
		} `json:"node_pools"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	listResponse struct {
		KubernetesClusters []doCluster `json:"kubernetes_clusters"`
		Links              struct {
			Pages struct {
				Next string `json:"next"`
			} `json:"pages"`
		} `json:"links"`
	}
	// credentialsResponse holds a token, or a client certificate for
	// clusters older than token authentication. The *_data fields are
	// base64 in JSON, which encoding/json decodes into []byte.
	credentialsResponse struct {
		CertificateAuthorityData []byte    `json:"certificate_authority_data"`
		ClientCertificateData    []byte    `json:"client_certificate_data"`
		ClientKeyData            []byte    `json:"client_key_data"`
		Token                    string    `json:"token"`
		ExpiresAt                time.Time `json:"expires_at"`
	}
)

// listClusters follows the page links until the last page.
func listClusters(ctx context.Context, c *apiclient.Client) ([]doCluster, error) {
	var out []doCluster
	for page := 1; ; page++ {
		var resp listResponse
		path := fmt.Sprintf("/v2/kubernetes/clusters?page=%d&per_page=%d", page, pageSize)
		if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.KubernetesClusters...)
		if resp.Links.Pages.Next == "" || len(resp.KubernetesClusters) == 0 {
			return out, nil
		}
	}
}

// List returns every DOKS cluster the token can see, with its CA and a
// token inline. Clusters without an API server endpoint yet are skipped
// until the next sync.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	clusters, err := listClusters(ctx, c)
	if err != nil {
		return nil, withHint(err)
	}
	out := make([]provider.Cluster, 0, len(clusters))
	for _, cl := range clusters {
		if cl.Endpoint == "" {
			fmt.Fprintf(os.Stderr, "digitalocean: skipping %s: no API server endpoint yet (status %s)\n", cl.Name, cl.Status.State)
			continue
		}
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Name,
			Region:    cl.Region,
			Version:   cl.Version,
			Endpoints: provider.Endpoints{Public: cl.Endpoint},
			Labels:    clusterLabels(cl),
		}
		creds, err := credentials(ctx, c, cl.ID)
		switch {
		case err == nil:
			cred, err := p.credential(cl.ID, creds)
			if err != nil {
				return nil, err
			}
			pc.CAData = creds.CertificateAuthorityData
			pc.Credential = &cred
		case notReady(err):
			// A cluster that is still provisioning may have an endpoint
			// before it has credentials. List it now and pick up the CA and
			// token at the next sync; leaving it out would mark it stale.
			fmt.Fprintf(os.Stderr, "digitalocean: %s has no credentials yet (status %s); run tug sync when it is running\n", cl.Name, cl.Status.State)
		default:
			return nil, withHint(err)
		}
		out = append(out, pc)
	}
	return out, nil
}

// clusterLabels turns "key:value" tags into labels, leaving out the k8s and
// k8s:<cluster id> tags DigitalOcean adds to every cluster, and marks GPU
// node pools and highly available control planes.
func clusterLabels(cl doCluster) map[string]string {
	labels := map[string]string{}
	for _, t := range cl.Tags {
		k, v, ok := strings.Cut(t, ":")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if ok && k != "" && k != "k8s" && v != "" {
			labels[k] = v
		}
	}
	if cl.HA {
		labels["ha"] = "true"
	}
	for _, np := range cl.NodePools {
		if strings.Contains(np.Size, "gpu") {
			labels["gpu"] = "true"
		}
	}
	return labels
}

func notReady(err error) bool {
	switch provider.CodeOf(err) {
	case provider.Unavailable, provider.NotFound:
		return true
	default:
		return false
	}
}

// credentials asks for a cluster's credentials with a token that lasts
// tokenLifetime.
func credentials(ctx context.Context, c *apiclient.Client, id string) (*credentialsResponse, error) {
	path := fmt.Sprintf("/v2/kubernetes/clusters/%s/credentials?expiry_seconds=%d", url.PathEscape(id), int(tokenLifetime.Seconds()))
	var resp credentialsResponse
	if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// credential converts the API's credentials to tug's: the token, or the
// client certificate of a cluster without token authentication.
func (p *Provider) credential(id string, r *credentialsResponse) (provider.Credential, error) {
	switch {
	case r.Token != "":
		expires := r.ExpiresAt
		if expires.IsZero() {
			// The plugin asked for tokenLifetime, so assume that rather than
			// a token that never expires.
			expires = p.now().Add(tokenLifetime)
		}
		return provider.Token(r.Token, expires), nil
	case len(r.ClientCertificateData) > 0 && len(r.ClientKeyData) > 0:
		return provider.ClientCert(r.ClientCertificateData, r.ClientKeyData, r.ExpiresAt), nil
	default:
		return provider.Credential{}, provider.Errorf(provider.Internal, "the credentials for cluster %s have neither a token nor a client certificate", id)
	}
}

// Credential returns a token for the cluster that lasts tokenLifetime.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	creds, err := credentials(ctx, c, req.ClusterID)
	if err != nil {
		return provider.Credential{}, withHint(err)
	}
	return p.credential(req.ClusterID, creds)
}
