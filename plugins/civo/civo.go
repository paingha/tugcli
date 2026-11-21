// Package civo is tug's first-party provider plugin for Civo Kubernetes. It
// lists the clusters in every region that offers Kubernetes, or in the
// regions the account names, with the Civo API v2. Each cluster object
// carries its kubeconfig, whose client certificate is the credential, so
// listing returns the certificate inline with its own expiry; tug moves it
// to the keychain and asks again before it runs out.
package civo

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "civo"
	Version = "0.1.0"
)

// DefaultAPIURL is the Civo API.
const DefaultAPIURL = "https://api.civo.com"

// SecretAPIKey is the Civo API key.
const SecretAPIKey = "api_key"

// ConfigRegions is the account setting that limits listing to some
// regions, comma separated, such as lon1,fra1. Without it the plugin lists
// every region that offers Kubernetes.
const ConfigRegions = "regions"

// pageSize is how many clusters to ask for at a time, as Civo's own Go
// client does.
const pageSize = 100

const keyHint = "copy the API key from Settings > Profile > Security in the Civo dashboard; " +
	"a team member's key also needs the Kubernetes permission in the team's role"

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
		Description:  "Civo Kubernetes",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   SecretAPIKey,
			Prompt: "Civo API key",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	key := secrets[SecretAPIKey]
	if key == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s", SecretAPIKey).WithHint(keyHint)
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     key,
		HTTP:      p.http,
		UserAgent: "tug-provider-civo/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"code": ..., "reason": ..., "details": ...} and
// {"result": "requires_authentication"} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Code    string `json:"code"`
		Reason  string `json:"reason"`
		Details string `json:"details"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	switch {
	case e.Reason != "" && e.Details != "":
		return e.Reason + ", " + e.Details
	case e.Reason != "":
		return e.Reason
	case e.Code != "":
		return e.Code
	default:
		return e.Result
	}
}

// apiError adds hints to the errors a person can fix. Civo answers 403 as
// well as 401 for a key it does not accept, and a key whose team role
// leaves out Kubernetes needs the same fix, so both are Unauthenticated.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated, provider.PermissionDenied:
		pe := *provider.AsError(err)
		pe.Code = provider.Unauthenticated
		return pe.WithHint("the API key is invalid, regenerated or without access to Kubernetes; " + keyHint)
	case provider.RateLimited:
		return provider.AsError(err).WithHint("the Civo API limits how fast a key may call it; wait a minute, then run tug sync")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	region struct {
		Code     string `json:"code"`
		Features struct {
			Kubernetes bool `json:"kubernetes"`
		} `json:"features"`
	}
	pool struct {
		Size string `json:"size"`
	}
	civoCluster struct {
		ID                string  `json:"id"`
		Name              string  `json:"name"`
		Status            string  `json:"status"`
		Version           string  `json:"version"`
		KubernetesVersion string  `json:"kubernetes_version"`
		ClusterType       string  `json:"cluster_type"`
		APIEndpoint       string  `json:"api_endpoint"`
		KubeConfig        string  `json:"kubeconfig"`
		Tags              tagList `json:"tags"`
		Pools             []pool  `json:"pools"`
		RequiredPools     []pool  `json:"required_pools"`
	}
	clusterPage struct {
		Page  int           `json:"page"`
		Pages int           `json:"pages"`
		Items []civoCluster `json:"items"`
	}
	size struct {
		Name     string `json:"name"`
		GPUCount int    `json:"gpu_count"`
	}
)

// tagList reads tags as the JSON array the API returns, or as the
// space-separated string that creating a cluster takes.
type tagList []string

// UnmarshalJSON implements json.Unmarshaler.
func (t *tagList) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = strings.Fields(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	*t = list
	return nil
}

// regions returns the account's regions, or every region with Kubernetes.
func regions(ctx context.Context, c *apiclient.Client, acct provider.Account) ([]string, error) {
	var out []string
	for r := range strings.SplitSeq(acct.ConfigString(ConfigRegions), ",") {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		var all []region
		if err := c.Do(ctx, http.MethodGet, "/v2/regions", nil, &all); err != nil {
			return nil, apiError(err)
		}
		for _, r := range all {
			if r.Features.Kubernetes && r.Code != "" {
				out = append(out, strings.ToLower(r.Code))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// listClusters follows the pages of one region's clusters.
func listClusters(ctx context.Context, c *apiclient.Client, region string) ([]civoCluster, error) {
	var out []civoCluster
	q := url.Values{"region": {region}, "per_page": {strconv.Itoa(pageSize)}}
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		var resp clusterPage
		if err := c.Do(ctx, http.MethodGet, "/v2/kubernetes/clusters?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if page >= resp.Pages || len(resp.Items) == 0 {
			return out, nil
		}
	}
}

// List returns the clusters in each of the account's regions, with the CA
// and client certificate from their kubeconfigs. Clusters without an API
// server address yet are skipped until the next sync.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	regs, err := regions(ctx, c, req.Account)
	if err != nil {
		return nil, err
	}
	out := []provider.Cluster{}
	for _, r := range regs {
		clusters, err := listClusters(ctx, c, r)
		if err != nil {
			return nil, apiError(err)
		}
		gpus := gpuSizes(ctx, c, r, clusters)
		for _, cl := range clusters {
			pc, ok, err := toCluster(r, cl, gpus)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, pc)
			}
		}
	}
	return out, nil
}

// toCluster converts a Civo cluster, and reports false for one that has no
// API server address yet.
func toCluster(region string, cl civoCluster, gpus map[string]bool) (provider.Cluster, bool, error) {
	endpoint := endpointURL(cl.APIEndpoint)
	var kc *kubeconfig
	if strings.TrimSpace(cl.KubeConfig) != "" {
		var err error
		if kc, err = parseKubeconfig(cl.KubeConfig); err != nil {
			return provider.Cluster{}, false, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cl.Name, err)
		}
		if kc.server != "" {
			endpoint = kc.server
		}
	}
	if endpoint == "" {
		fmt.Fprintf(os.Stderr, "civo: skipping %s: no API server address yet (status %s)\n", cl.Name, cl.Status)
		return provider.Cluster{}, false, nil
	}
	version := cl.KubernetesVersion
	if version == "" {
		version = cl.Version
	}
	pc := provider.Cluster{
		ID:        clusterID(region, cl.ID),
		Name:      cl.Name,
		Region:    region,
		Version:   version,
		Endpoints: provider.Endpoints{Public: endpoint},
		Labels:    clusterLabels(cl, gpus),
	}
	if kc == nil {
		// A cluster that is still being built has an address before it
		// has a kubeconfig. List it now and pick up the CA and certificate
		// at the next sync; leaving it out would mark it stale.
		fmt.Fprintf(os.Stderr, "civo: %s has no kubeconfig yet (status %s); run tug sync when it is ACTIVE\n", cl.Name, cl.Status)
		return pc, true, nil
	}
	pc.CAData = kc.ca
	cred, err := kc.credential()
	if err != nil {
		return provider.Cluster{}, false, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cl.Name, err)
	}
	pc.Credential = &cred
	return pc, true, nil
}

// clusterID joins the region and Civo's cluster ID: the API finds a cluster
// only in its own region, and neither ever changes.
func clusterID(region, id string) string { return region + "/" + id }

// splitID is the inverse of clusterID.
func splitID(id string) (region, clusterID string, err error) {
	region, clusterID, ok := strings.Cut(id, "/")
	if !ok || region == "" || clusterID == "" {
		return "", "", provider.Errorf(provider.NotFound, "%q is not a Civo cluster ID of the form <region>/<id>", id).
			WithHint("run tug sync to list the account's clusters again")
	}
	return region, clusterID, nil
}

// endpointURL returns the API endpoint as a URL, or "" when it has no host
// yet: the API answers "https://:6443" for a cluster still being built.
func endpointURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return s
}

// clusterLabels turns "key:value" and "key=value" tags into labels, marks
// clusters with GPU node pools, and records the cluster type (k3s or
// talos).
func clusterLabels(cl civoCluster, gpus map[string]bool) map[string]string {
	labels := map[string]string{}
	for _, t := range cl.Tags {
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			k, v, ok = strings.Cut(t, "=")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if ok && k != "" && v != "" {
			labels[k] = v
		}
	}
	if cl.ClusterType != "" {
		labels["cluster-type"] = strings.ToLower(cl.ClusterType)
	}
	for _, np := range slices.Concat(cl.Pools, cl.RequiredPools) {
		if gpus[np.Size] || strings.Contains(strings.ToLower(np.Size), "gpu") {
			labels["gpu"] = "true"
		}
	}
	return labels
}

// gpuSizes returns the region's node sizes that have GPUs, when any of the
// clusters has node pools. Listing goes ahead without them when the sizes
// cannot be read; size names with "gpu" in them still count.
func gpuSizes(ctx context.Context, c *apiclient.Client, region string, clusters []civoCluster) map[string]bool {
	if !slices.ContainsFunc(clusters, func(cl civoCluster) bool { return len(cl.Pools)+len(cl.RequiredPools) > 0 }) {
		return nil
	}
	var sizes []size
	if err := c.Do(ctx, http.MethodGet, "/v2/sizes?"+url.Values{"region": {region}}.Encode(), nil, &sizes); err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "civo: listing %s without GPU labels from node sizes: %v\n", region, err)
		}
		return nil
	}
	gpus := map[string]bool{}
	for _, s := range sizes {
		if s.GPUCount > 0 {
			gpus[s.Name] = true
		}
	}
	return gpus
}

// kubeconfig is what a cluster's kubeconfig says about reaching it.
type kubeconfig struct {
	server string
	ca     []byte
	cert   []byte
	key    []byte
	token  string
}

// parseKubeconfig reads the server, CA and user of the kubeconfig's current
// context, or of its first entries. The API returns the kubeconfig as YAML;
// base64 is accepted too.
func parseKubeconfig(raw string) (*kubeconfig, error) {
	if !strings.Contains(raw, "clusters") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw)); err == nil {
			raw = string(b)
		}
	}
	var kc struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
				User    string `json:"user"`
			} `json:"context"`
		} `json:"contexts"`
		Clusters []struct {
			Name    string `json:"name"`
			Cluster struct {
				Server string `json:"server"`
				CAData []byte `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			Name string `json:"name"`
			User struct {
				CertData []byte `json:"client-certificate-data"`
				KeyData  []byte `json:"client-key-data"`
				Token    string `json:"token"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal([]byte(raw), &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
		return nil, errors.New("it has no cluster or user")
	}
	clusterName, userName := kc.Clusters[0].Name, kc.Users[0].Name
	for _, kctx := range kc.Contexts {
		if kctx.Name == kc.CurrentContext {
			clusterName, userName = kctx.Context.Cluster, kctx.Context.User
		}
	}
	out := &kubeconfig{}
	for _, cl := range kc.Clusters {
		if cl.Name == clusterName {
			out.server, out.ca = cl.Cluster.Server, cl.Cluster.CAData
		}
	}
	for _, u := range kc.Users {
		if u.Name == userName {
			out.cert, out.key, out.token = u.User.CertData, u.User.KeyData, u.User.Token
		}
	}
	return out, nil
}

// credential returns the kubeconfig's client certificate, which expires
// when the certificate does, or its token.
func (kc *kubeconfig) credential() (provider.Credential, error) {
	switch {
	case len(kc.cert) > 0 && len(kc.key) > 0:
		expires, err := certExpiry(kc.cert)
		if err != nil {
			return provider.Credential{}, err
		}
		return provider.ClientCert(kc.cert, kc.key, expires), nil
	case kc.token != "":
		return provider.Token(kc.token, provider.NoExpiry), nil
	default:
		return provider.Credential{}, errors.New("it has neither a client certificate nor a token")
	}
}

// certExpiry returns when the first certificate in a PEM bundle expires.
func certExpiry(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, errors.New("its client certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing its client certificate: %w", err)
	}
	return cert.NotAfter, nil
}

// Credential returns the client certificate from the cluster's kubeconfig.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	reg, id, err := splitID(req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	var cl civoCluster
	path := "/v2/kubernetes/clusters/" + url.PathEscape(id) + "?" + url.Values{"region": {reg}}.Encode()
	if err := c.Do(ctx, http.MethodGet, path, nil, &cl); err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from Civo; run tug sync")
		}
		return provider.Credential{}, apiError(err)
	}
	if strings.TrimSpace(cl.KubeConfig) == "" {
		return provider.Credential{}, provider.Errorf(provider.Unavailable, "cluster %s has no kubeconfig yet (status %s)", cl.Name, cl.Status).
			WithHint("wait until the cluster is ACTIVE in the Civo dashboard, then try again")
	}
	kc, err := parseKubeconfig(cl.KubeConfig)
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cl.Name, err)
	}
	cred, err := kc.credential()
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cl.Name, err)
	}
	return cred, nil
}
