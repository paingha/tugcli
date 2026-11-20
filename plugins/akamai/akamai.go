// Package akamai is tug's first-party provider plugin for Akamai (Linode)
// Kubernetes Engine. LKE hands out a kubeconfig with a long-lived token, so
// the plugin returns that token inline when it lists clusters and tug moves
// it to the keychain. It can also regenerate the token and check the control
// plane access list.
package akamai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	Name    = "akamai"
	Version = "0.1.0"
)

// DefaultAPIURL is the Linode API v4.
const DefaultAPIURL = "https://api.linode.com/v4"

const pageSize = 500

// rotateWaits are the pauses between polls for a regenerated kubeconfig,
// about 15 seconds in all. The plugin host allows rotate a minute.
var rotateWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// Provider implements the plugin.
type Provider struct {
	http *http.Client
	// wait pauses for d or until ctx ends; tests replace it.
	wait func(ctx context.Context, d time.Duration) error
}

var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.Rotator       = (*Provider)(nil)
	_ provider.AccessChecker = (*Provider)(nil)
)

// New returns the plugin.
func New() *Provider { return &Provider{wait: sleep} }

// sleep waits for d, returning early with the context's error when it ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "Akamai (Linode) Kubernetes Engine",
		Capabilities: provider.CapList | provider.CapCredential | provider.CapRotate | provider.CapCheckAccess,
		Secrets: []provider.SecretSpec{{
			Name:   "api_token",
			Prompt: "Akamai Cloud personal access token (Kubernetes read/write)",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	token := secrets["api_token"]
	if token == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no api_token").
			WithHint("create a personal access token with Kubernetes read/write in Akamai Cloud Manager")
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     token,
		HTTP:      p.http,
		UserAgent: "tug-provider-akamai/" + Version,
		Message:   linodeMessage,
	}, nil
}

// linodeMessage reads {"errors":[{"reason":"..."}]}.
func linodeMessage(body []byte) string {
	var e struct {
		Errors []struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(e.Errors))
	for _, x := range e.Errors {
		if x.Field != "" {
			reasons = append(reasons, x.Field+": "+x.Reason)
		} else {
			reasons = append(reasons, x.Reason)
		}
	}
	return strings.Join(reasons, "; ")
}

// API types, trimmed to the fields the plugin uses.
type (
	page[T any] struct {
		Data  []T `json:"data"`
		Page  int `json:"page"`
		Pages int `json:"pages"`
	}
	lkeCluster struct {
		ID           int      `json:"id"`
		Label        string   `json:"label"`
		Region       string   `json:"region"`
		K8sVersion   string   `json:"k8s_version"`
		Tags         []string `json:"tags"`
		ControlPlane struct {
			HighAvailability bool `json:"high_availability"`
		} `json:"control_plane"`
	}
	nodePool struct {
		Type string `json:"type"`
	}
	apiEndpoint struct {
		Endpoint string `json:"endpoint"`
	}
	kubeconfigResponse struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	aclResponse struct {
		ACL struct {
			Enabled   bool `json:"enabled"`
			Addresses struct {
				IPv4 []string `json:"ipv4"`
				IPv6 []string `json:"ipv6"`
			} `json:"addresses"`
		} `json:"acl"`
	}
)

func list[T any](ctx context.Context, c *apiclient.Client, path string) ([]T, error) {
	var out []T
	for n := 1; ; n++ {
		var pg page[T]
		if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d&page_size=%d", path, n, pageSize), nil, &pg); err != nil {
			return nil, err
		}
		out = append(out, pg.Data...)
		if pg.Pages <= n {
			return out, nil
		}
	}
}

// List returns every LKE cluster the token can see, with its token inline.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	clusters, err := list[lkeCluster](ctx, c, "/lke/clusters")
	if err != nil {
		return nil, err
	}
	out := make([]provider.Cluster, 0, len(clusters))
	for _, cl := range clusters {
		pc := provider.Cluster{
			ID:      strconv.Itoa(cl.ID),
			Name:    cl.Label,
			Region:  cl.Region,
			Version: cl.K8sVersion,
			Labels:  tagLabels(cl.Tags),
		}
		if cl.ControlPlane.HighAvailability {
			pc.Labels["ha"] = "true"
		}
		pools, err := list[nodePool](ctx, c, fmt.Sprintf("/lke/clusters/%d/pools", cl.ID))
		if err != nil {
			return nil, err
		}
		for _, np := range pools {
			if strings.Contains(np.Type, "gpu") {
				pc.Labels["gpu"] = "true"
			}
		}

		kc, err := p.kubeconfig(ctx, c, cl.ID)
		switch {
		case err == nil:
			pc.Endpoints.Public = kc.server
			pc.CAData = kc.ca
			cred := provider.Token(kc.token, provider.NoExpiry)
			pc.Credential = &cred
		case notReady(err):
			// New clusters take a few minutes to get a kubeconfig; list them
			// by endpoint now and pick up the CA and token at the next sync.
			// Any error here fails the listing: leaving the cluster out
			// would mark it stale.
			endpoints, eerr := list[apiEndpoint](ctx, c, fmt.Sprintf("/lke/clusters/%d/api-endpoints", cl.ID))
			if eerr != nil {
				return nil, eerr
			}
			if len(endpoints) == 0 {
				fmt.Fprintf(os.Stderr, "akamai: skipping %s: it has no API endpoint yet\n", cl.Label)
				continue
			}
			pc.Endpoints.Public = endpoints[0].Endpoint
			fmt.Fprintf(os.Stderr, "akamai: %s has no kubeconfig yet; run tug sync in a few minutes\n", cl.Label)
		default:
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

// tagLabels turns "key:value" or "key=value" tags into labels.
func tagLabels(tags []string) map[string]string {
	labels := map[string]string{}
	for _, t := range tags {
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			k, v, ok = strings.Cut(t, "=")
		}
		if ok && k != "" && v != "" {
			labels[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
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

type kubeconfig struct {
	server string
	ca     []byte
	token  string
}

// kubeconfig fetches and decodes a cluster's kubeconfig.
func (p *Provider) kubeconfig(ctx context.Context, c *apiclient.Client, id int) (*kubeconfig, error) {
	var resp kubeconfigResponse
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/lke/clusters/%d/kubeconfig", id), nil, &resp); err != nil {
		if provider.CodeOf(err) == provider.PermissionDenied {
			return nil, provider.AsError(err).WithHint("reading an LKE kubeconfig needs a token with Kubernetes read/write")
		}
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(resp.Kubeconfig)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "cluster %d: kubeconfig is not base64: %v", id, err)
	}
	return parseKubeconfig(raw)
}

func parseKubeconfig(raw []byte) (*kubeconfig, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Token string `json:"token"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, provider.Errorf(provider.Internal, "parsing kubeconfig: %v", err)
	}
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 || kc.Users[0].User.Token == "" {
		return nil, provider.Errorf(provider.Internal, "kubeconfig has no cluster or token")
	}
	ca, err := base64.StdEncoding.DecodeString(kc.Clusters[0].Cluster.CAData)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "kubeconfig CA is not base64: %v", err)
	}
	return &kubeconfig{server: kc.Clusters[0].Cluster.Server, ca: ca, token: kc.Users[0].User.Token}, nil
}

func clusterID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, provider.Errorf(provider.InvalidConfig, "cluster id %q is not a number", s)
	}
	return id, nil
}

// Credential returns the cluster's kubeconfig token.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	id, err := clusterID(req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	kc, err := p.kubeconfig(ctx, c, id)
	if err != nil {
		if notReady(err) {
			return provider.Credential{}, provider.AsError(err).WithHint("LKE kubeconfigs can take 2 to 5 minutes after a cluster is created")
		}
		return provider.Credential{}, err
	}
	return provider.Token(kc.token, provider.NoExpiry), nil
}

// Rotate regenerates the cluster's kubeconfig, revoking the old token, and
// returns the new one.
func (p *Provider) Rotate(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	id, err := clusterID(req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	if err := c.Do(ctx, http.MethodDelete, fmt.Sprintf("/lke/clusters/%d/kubeconfig", id), nil, nil); err != nil {
		return provider.Credential{}, err
	}
	// The old token is revoked now; poll until the new kubeconfig is ready.
	for attempt := 0; ; attempt++ {
		kc, err := p.kubeconfig(ctx, c, id)
		if err == nil {
			return provider.Token(kc.token, provider.NoExpiry), nil
		}
		if !notReady(err) {
			return provider.Credential{}, err
		}
		if attempt == len(rotateWaits) {
			return provider.Credential{}, regenerating(err)
		}
		if werr := p.wait(ctx, rotateWaits[attempt]); werr != nil {
			return provider.Credential{}, regenerating(werr)
		}
	}
}

// regenerating explains a rotate that revoked the old token but could not
// fetch the new one in time.
func regenerating(err error) error {
	return provider.Errorf(provider.Unavailable, "the kubeconfig was regenerated but is not ready yet: %v", err).
		WithHint("the old token no longer works; run tug sync in a minute to fetch the new one")
}

// CheckAccess compares the caller's egress IP with the control plane ACL.
func (p *Provider) CheckAccess(ctx context.Context, req provider.AccessRequest) ([]provider.Finding, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	id, err := clusterID(req.Cluster.ID)
	if err != nil {
		return nil, err
	}
	var acl aclResponse
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/lke/clusters/%d/control_plane_acl", id), nil, &acl); err != nil {
		if provider.CodeOf(err) == provider.InvalidConfig {
			return []provider.Finding{{Severity: provider.SeverityInfo, Message: "this cluster does not support a control plane ACL"}}, nil
		}
		return nil, err
	}
	if !acl.ACL.Enabled {
		return []provider.Finding{{Severity: provider.SeverityInfo, Message: "the control plane ACL is off; any address can reach the API server"}}, nil
	}
	if req.EgressIP == "" {
		return []provider.Finding{{Severity: provider.SeverityWarning, Message: "the control plane ACL is on, and your egress IP is unknown"}}, nil
	}
	allowed := append(append([]string{}, acl.ACL.Addresses.IPv4...), acl.ACL.Addresses.IPv6...)
	if ipAllowed(req.EgressIP, allowed) {
		return []provider.Finding{{Severity: provider.SeverityInfo, Message: fmt.Sprintf("the control plane ACL allows %s", req.EgressIP)}}, nil
	}
	return []provider.Finding{{
		Severity: provider.SeverityError,
		Message:  fmt.Sprintf("the control plane ACL does not allow your address %s", req.EgressIP),
		Fix:      fmt.Sprintf("add %s to the cluster's control plane ACL in Akamai Cloud Manager", hostCIDR(req.EgressIP)),
	}}, nil
}

func ipAllowed(ip string, entries []string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, e := range entries {
		if _, n, err := net.ParseCIDR(e); err == nil && n.Contains(addr) {
			return true
		}
		if other := net.ParseIP(e); other != nil && other.Equal(addr) {
			return true
		}
	}
	return false
}

func hostCIDR(ip string) string {
	if strings.Contains(ip, ":") {
		return ip + "/128"
	}
	return ip + "/32"
}
