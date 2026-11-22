// Package rancher is tug's first-party provider plugin for clusters that
// Rancher manages. It lists the clusters a Rancher API token can see and
// reaches each one through Rancher's authenticating proxy at
// {url}/k8s/clusters/{id}. Rancher's generateKubeconfig action creates a new
// kubeconfig token every time it runs, so the plugin asks for one only when
// tug needs a credential, never while listing.
package rancher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "rancher"
	Version = "0.1.1"
)

// ConfigURL is the account setting that names the Rancher server, such as
// https://rancher.example.com. The apiURL setting, when set, takes its place
// for API calls only.
const ConfigURL = "url"

// pageSize is the largest page the v3 API serves.
const pageSize = 1000

// fallbackLifetime is how long the plugin trusts a kubeconfig token whose
// expiry it cannot read. Rancher's default for kubeconfig tokens is 30 days;
// asking again after 12 hours stays inside a shorter setting at the cost of
// a few more tokens.
const fallbackLifetime = 12 * time.Hour

const tokenHint = "create an API key with no scope under your avatar > Account & API Keys in Rancher, and give tug its bearer token"

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
		Description:  "Clusters managed by Rancher",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   "api_token",
			Prompt: "Rancher API bearer token (token-xxxxx:...)",
		}},
	}
}

// client returns an API client for the account and the Rancher server URL
// that cluster endpoints are built on.
func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, string, error) {
	token := secrets["api_token"]
	if token == "" {
		return nil, "", provider.Errorf(provider.Unauthenticated, "no api_token").WithHint(tokenHint)
	}
	// A bearer token is the API key's access key and secret key joined by a
	// colon. Checking that first catches either half pasted alone, and
	// rejects a bad token even when the account has no url.
	if name, secret, ok := strings.Cut(token, ":"); !ok || name == "" || secret == "" {
		return nil, "", provider.Errorf(provider.Unauthenticated, "the api_token is not a Rancher bearer token").
			WithHint("use the API key's bearer token, its access key and secret key joined by a colon (token-xxxxx:...), which Rancher shows only when it creates the key")
	}
	raw := acct.ConfigString(ConfigURL)
	if raw == "" {
		return nil, "", provider.Errorf(provider.InvalidConfig, "the account has no %s setting", ConfigURL).
			WithHint("import the account again with --set url=https://<your Rancher server>")
	}
	server, err := apiclient.CheckURL(ConfigURL, raw)
	if err != nil {
		return nil, "", err
	}
	base, err := apiclient.BaseURL(acct, server)
	if err != nil {
		return nil, "", err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     token,
		HTTP:      p.http,
		UserAgent: "tug-provider-rancher/" + Version,
		Message:   errorMessage,
	}, server, nil
}

// errorMessage reads {"type": "error", "code": ..., "message": ...} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// withHint adds the next step to the API errors a user can fix.
func withHint(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the API token is invalid, expired or disabled; " + tokenHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint("your Rancher user needs a role on the cluster, such as Cluster Member; an API key scoped to one cluster reaches only that one")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	rancherCluster struct {
		ID      string            `json:"id"`
		Name    string            `json:"name"`
		Labels  map[string]string `json:"labels"`
		Version *struct {
			GitVersion string `json:"gitVersion"`
		} `json:"version"`
	}
	collection struct {
		Data       []rancherCluster `json:"data"`
		Pagination struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	setting struct {
		Value string `json:"value"`
	}
	kubeconfigOutput struct {
		Config string `json:"config"`
	}
	token struct {
		ExpiresAt string `json:"expiresAt"`
		// TTL is in milliseconds; zero means the token does not expire.
		TTL int64 `json:"ttl"`
	}
)

// listClusters follows the pagination links until the last page. The
// links are absolute, so only their query, which carries the marker, is
// reused against the client's base URL.
func listClusters(ctx context.Context, c *apiclient.Client) ([]rancherCluster, error) {
	var out []rancherCluster
	query := url.Values{"limit": {strconv.Itoa(pageSize)}}.Encode()
	for {
		var resp collection
		if err := c.Do(ctx, http.MethodGet, "/v3/clusters?"+query, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.Pagination.Next == "" || len(resp.Data) == 0 {
			return out, nil
		}
		next, err := url.Parse(resp.Pagination.Next)
		if err != nil {
			return nil, provider.Errorf(provider.Internal, "the next page link %q is not a URL", resp.Pagination.Next)
		}
		query = next.RawQuery
	}
}

// List returns every cluster the token can see, reached through the
// Rancher server's proxy.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, server, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	clusters, err := listClusters(ctx, c)
	if err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return nil, provider.AsError(err).WithHint(fmt.Sprintf("check that %s is the address of a Rancher server", server))
		}
		return nil, withHint(err)
	}
	ca, err := caCerts(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Cluster, 0, len(clusters))
	for _, cl := range clusters {
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Name,
			Endpoints: provider.Endpoints{Public: server + "/k8s/clusters/" + url.PathEscape(cl.ID)},
			CAData:    ca,
			Labels:    clusterLabels(cl.Labels),
		}
		if pc.Name == "" {
			pc.Name = cl.ID
		}
		if cl.Version != nil {
			pc.Version = cl.Version.GitVersion
		}
		out = append(out, pc)
	}
	return out, nil
}

// caCerts returns the CA that signed the Rancher server's certificate, from
// its cacerts setting, which is also what Rancher writes into kubeconfigs.
// It is empty when the server's certificate is publicly trusted. When the
// setting cannot be read, the listing goes ahead without a CA, and tug keeps
// the one it already has.
func caCerts(ctx context.Context, c *apiclient.Client) ([]byte, error) {
	var s setting
	if err := c.Do(ctx, http.MethodGet, "/v3/settings/cacerts", nil, &s); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "rancher: listing without the server's CA: %v\n", err)
		return nil, nil
	}
	if strings.TrimSpace(s.Value) == "" {
		return nil, nil
	}
	return []byte(s.Value), nil
}

// clusterLabels keeps the cluster's labels with plain keys, such as env and
// team. Keys with a prefix or dots, such as provider.cattle.io, are
// Rancher's own.
func clusterLabels(in map[string]string) map[string]string {
	labels := map[string]string{}
	for k, v := range in {
		if k != "" && v != "" && !strings.ContainsAny(k, "./") {
			labels[strings.ToLower(k)] = v
		}
	}
	return labels
}

// Credential generates a kubeconfig for the cluster and returns its token,
// which expires when Rancher says it does.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, _, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	var out kubeconfigOutput
	path := "/v3/clusters/" + url.PathEscape(req.ClusterID) + "?action=generateKubeconfig"
	if err := c.Do(ctx, http.MethodPost, path, nil, &out); err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from Rancher, or your user can no longer see it; run tug sync")
		}
		return provider.Credential{}, withHint(err)
	}
	key, err := kubeconfigToken([]byte(out.Config))
	if err != nil {
		return provider.Credential{}, err
	}
	expires, err := p.tokenExpiry(ctx, c, key)
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.Token(key, expires), nil
}

// kubeconfigToken returns the token of the first user in a kubeconfig
// Rancher generated.
func kubeconfigToken(raw []byte) (string, error) {
	var kc struct {
		Users []struct {
			User struct {
				Token string          `json:"token"`
				Exec  json.RawMessage `json:"exec"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return "", provider.Errorf(provider.Internal, "parsing the generated kubeconfig: %v", err)
	}
	for _, u := range kc.Users {
		if u.User.Token != "" {
			return u.User.Token, nil
		}
	}
	for _, u := range kc.Users {
		if len(u.User.Exec) > 0 {
			return "", provider.Errorf(provider.InvalidConfig, "the generated kubeconfig runs the rancher CLI instead of holding a token").
				WithHint("ask a Rancher administrator to set kubeconfig-generate-token to true")
		}
	}
	return "", provider.Errorf(provider.Internal, "the generated kubeconfig has no token")
}

// tokenExpiry asks Rancher when a kubeconfig token expires. A token is
// "<name>:<secret>". When the token cannot be looked up, it assumes
// fallbackLifetime; the error is only for a cancelled request.
func (p *Provider) tokenExpiry(ctx context.Context, c *apiclient.Client, key string) (time.Time, error) {
	name, _, ok := strings.Cut(key, ":")
	if !ok || name == "" {
		fmt.Fprintf(os.Stderr, "rancher: the kubeconfig token has no name; asking for a new one in %s\n", fallbackLifetime)
		return p.now().Add(fallbackLifetime), nil
	}
	var t token
	if err := c.Do(ctx, http.MethodGet, "/v3/tokens/"+url.PathEscape(name), nil, &t); err != nil {
		if ctx.Err() != nil {
			return time.Time{}, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "rancher: reading when token %s expires: %v; asking for a new one in %s\n", name, err, fallbackLifetime)
		return p.now().Add(fallbackLifetime), nil
	}
	switch {
	case t.ExpiresAt != "":
		expires, err := time.Parse(time.RFC3339, t.ExpiresAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rancher: token %s expiresAt %q is not a time; asking for a new one in %s\n", name, t.ExpiresAt, fallbackLifetime)
			return p.now().Add(fallbackLifetime), nil
		}
		return expires, nil
	case t.TTL > 0:
		return p.now().Add(time.Duration(t.TTL) * time.Millisecond), nil
	default:
		return provider.NoExpiry, nil
	}
}
