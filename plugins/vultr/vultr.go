// Package vultr is tug's first-party provider plugin for Vultr Kubernetes
// Engine (VKE). A VKE kubeconfig authenticates with a client certificate, so
// listing fetches each cluster's kubeconfig for its CA and returns the
// certificate inline, with the certificate's own expiry; tug moves it to the
// keychain.
package vultr

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
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
	Name    = "vultr"
	Version = "0.1.1"
)

// DefaultAPIURL is the Vultr API.
const DefaultAPIURL = "https://api.vultr.com"

// pageSize is the largest page the API serves.
const pageSize = 500

// apiServerPort is the port of a VKE control plane, which the API leaves
// out of a cluster's endpoint.
const apiServerPort = "6443"

const keyHint = "check the API key, and that its access control allows this machine's IP address, under Account > API in the Vultr customer portal"

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
		Description:  "Vultr Kubernetes Engine (VKE)",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   "api_key",
			Prompt: "Vultr API key",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	key := secrets["api_key"]
	if key == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no api_key").
			WithHint("enable the API and copy its key under Account > API in the Vultr customer portal")
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     key,
		HTTP:      p.http,
		UserAgent: "tug-provider-vultr/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"error": ..., "status": ...} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Error
}

// withHint adds the next step to the API errors a user can fix.
func withHint(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint(keyHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint("the API key's user is not allowed to manage Kubernetes; give it access under Account > Users in the Vultr customer portal")
	case provider.RateLimited:
		return provider.AsError(err).WithHint("the Vultr API limits how fast a key may call it; wait a few seconds and try again")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	vkeCluster struct {
		ID              string `json:"id"`
		Label           string `json:"label"`
		Endpoint        string `json:"endpoint"`
		Version         string `json:"version"`
		Region          string `json:"region"`
		Status          string `json:"status"`
		HAControlPlanes bool   `json:"ha_controlplanes"`
		NodePools       []struct {
			Plan string `json:"plan"`
		} `json:"node_pools"`
	}
	listResponse struct {
		VKEClusters []vkeCluster `json:"vke_clusters"`
		Meta        struct {
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		} `json:"meta"`
	}
	configResponse struct {
		KubeConfig string `json:"kube_config"`
	}
)

// listClusters follows the cursors until the last page.
func listClusters(ctx context.Context, c *apiclient.Client) ([]vkeCluster, error) {
	var out []vkeCluster
	q := url.Values{"per_page": {strconv.Itoa(pageSize)}}
	for {
		var resp listResponse
		if err := c.Do(ctx, http.MethodGet, "/v2/kubernetes/clusters?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.VKEClusters...)
		next := resp.Meta.Links.Next
		if next == "" || len(resp.VKEClusters) == 0 {
			return out, nil
		}
		q.Set("cursor", next)
	}
}

// List returns every VKE cluster the key can see, with its CA and client
// certificate inline. Clusters without an endpoint yet are skipped until the
// next sync.
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
			fmt.Fprintf(os.Stderr, "vultr: skipping %s: no control plane endpoint yet (status %s)\n", cl.Label, cl.Status)
			continue
		}
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Label,
			Region:    cl.Region,
			Version:   cl.Version,
			Endpoints: provider.Endpoints{Public: endpointURL(cl.Endpoint)},
			Labels:    clusterLabels(cl),
		}
		kc, err := kubeconfig(ctx, c, cl.ID)
		switch {
		case err == nil:
			if kc.server != "" {
				pc.Endpoints.Public = kc.server
			}
			pc.CAData = kc.ca
			pc.Credential = &kc.cred
		case notReady(err) || strings.EqualFold(cl.Status, "pending"):
			// A cluster that is still being created has an endpoint before
			// it has a kubeconfig. List it now and pick up the CA and
			// certificate at the next sync; leaving it out would mark it
			// stale.
			fmt.Fprintf(os.Stderr, "vultr: %s has no kubeconfig yet (status %s); run tug sync when it is active\n", cl.Label, cl.Status)
		default:
			return nil, withHint(err)
		}
		out = append(out, pc)
	}
	return out, nil
}

// endpointURL turns the API's bare control plane host name into a URL.
func endpointURL(host string) string {
	if u, err := url.Parse(host); err == nil && u.Scheme != "" && u.Host != "" {
		return host
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, apiServerPort)
}

// clusterLabels marks GPU node pools and highly available control planes.
// Vultr Cloud GPU plans start with vcg-. VKE clusters have no tags.
func clusterLabels(cl vkeCluster) map[string]string {
	labels := map[string]string{}
	if cl.HAControlPlanes {
		labels["ha"] = "true"
	}
	for _, np := range cl.NodePools {
		if strings.HasPrefix(np.Plan, "vcg-") || strings.Contains(np.Plan, "gpu") {
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

type clusterConfig struct {
	server string
	ca     []byte
	cred   provider.Credential
}

// kubeconfig fetches and decodes a cluster's kubeconfig.
func kubeconfig(ctx context.Context, c *apiclient.Client, id string) (*clusterConfig, error) {
	var resp configResponse
	if err := c.Do(ctx, http.MethodGet, "/v2/kubernetes/clusters/"+url.PathEscape(id)+"/config", nil, &resp); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(resp.KubeConfig)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "cluster %s: kubeconfig is not base64: %v", id, err)
	}
	kc, err := parseKubeconfig(raw)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "cluster %s: %v", id, err)
	}
	return kc, nil
}

// parseKubeconfig reads the server, CA and client certificate from a VKE
// kubeconfig. The certificate's expiry becomes the credential's, so tug
// fetches a new one before it runs out.
func parseKubeconfig(raw []byte) (*clusterConfig, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData []byte `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				CertData []byte `json:"client-certificate-data"`
				KeyData  []byte `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return nil, fmt.Errorf("kubeconfig has no cluster or user")
	}
	user := kc.Users[0].User
	if len(user.CertData) == 0 || len(user.KeyData) == 0 {
		return nil, fmt.Errorf("kubeconfig has no client certificate")
	}
	expires, err := certExpiry(user.CertData)
	if err != nil {
		return nil, err
	}
	return &clusterConfig{
		server: kc.Clusters[0].Cluster.Server,
		ca:     kc.Clusters[0].Cluster.CAData,
		cred:   provider.ClientCert(user.CertData, user.KeyData, expires),
	}, nil
}

// certExpiry returns when the first certificate in a PEM bundle expires.
func certExpiry(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, fmt.Errorf("kubeconfig client certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing kubeconfig client certificate: %w", err)
	}
	return cert.NotAfter, nil
}

// Credential returns the client certificate from the cluster's kubeconfig.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	kc, err := kubeconfig(ctx, c, req.ClusterID)
	if err != nil {
		return provider.Credential{}, withHint(err)
	}
	return kc.cred, nil
}
