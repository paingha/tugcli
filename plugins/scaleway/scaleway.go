// Package scaleway is tug's first-party provider plugin for Scaleway
// Kubernetes Kapsule and Kosmos. It lists clusters in each region with the
// Scaleway Kubernetes API, authenticating with an IAM API key's secret key
// in X-Auth-Token. Kapsule's API servers accept that same secret key as a
// bearer token and map the key's IAM permission sets to Kubernetes groups,
// so the secret key is also the credential, as with
// `scw k8s kubeconfig install`; the legacy admin token in the kubeconfig is
// never fetched.
package scaleway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
	Name    = "scaleway"
	Version = "0.1.1"
)

// DefaultAPIURL is the Scaleway API.
const DefaultAPIURL = "https://api.scaleway.com"

// SecretSecretKey is the secret key of a Scaleway IAM API key.
const SecretSecretKey = "secret_key"

// Account settings.
const (
	// ConfigRegions limits listing to some regions, comma separated, such
	// as fr-par,nl-ams. Without it the plugin lists DefaultRegions.
	ConfigRegions = "regions"
	// ConfigProject limits listing to one Project, by ID.
	ConfigProject = "project"
	// ConfigAccessKey is the API key's access key (SCW...). It is not a
	// secret; with it, the plugin reads when the key expires.
	ConfigAccessKey = "accessKey"
)

// DefaultRegions are the regions that offer Kubernetes Kapsule.
var DefaultRegions = []string{"fr-par", "nl-ams", "pl-waw"}

// pageSize is how many clusters or pools to ask for at a time.
const pageSize = 100

// gpuNodeType matches the GPU Instance types, such as GPU-3070-S,
// RENDER-S, H100-1-80G, L40S-2-48G and B300-SXM-8-288G, once normalised to
// upper case with dashes.
var gpuNodeType = regexp.MustCompile(`^(GPU|RENDER)-|-\d+-\d+G$`)

const (
	keyHint    = "create an API key under IAM > API keys in the Scaleway console and give tug its secret key"
	deniedHint = "the API key's user or application needs the KubernetesReadOnly or KubernetesFullAccess permission set on the Project; add it with an IAM policy"
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
		Description:  "Scaleway Kubernetes Kapsule and Kosmos",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   SecretSecretKey,
			Prompt: "Scaleway API secret key",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	key := secrets[SecretSecretKey]
	if key == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s", SecretSecretKey).WithHint(keyHint)
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL: base,
		Token:   key,
		Sign: func(req *http.Request) error {
			req.Header.Set("X-Auth-Token", key)
			return nil
		},
		HTTP:      p.http,
		UserAgent: "tug-provider-scaleway/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"message": ..., "type": ...} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Type
}

// apiError adds hints to the errors a person can fix.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the secret key is wrong, expired or deleted; " + keyHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint(deniedHint)
	case provider.RateLimited:
		return provider.AsError(err).WithHint("the Scaleway API limits how fast a key may call it; wait a minute, then run tug sync")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	kapsuleCluster struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Status     string   `json:"status"`
		Version    string   `json:"version"`
		Region     string   `json:"region"`
		Tags       []string `json:"tags"`
		ClusterURL string   `json:"cluster_url"`
	}
	pool struct {
		NodeType string `json:"node_type"`
	}
	// file is a scw.File: the content is base64 in JSON, which
	// encoding/json decodes into []byte.
	file struct {
		Name    string `json:"name"`
		Content []byte `json:"content"`
	}
	apiKey struct {
		ExpiresAt *time.Time `json:"expires_at"`
	}
)

// listAll follows a paginated listing until it has total_count items. key
// is the name of the array in the response.
func listAll[T any](ctx context.Context, c *apiclient.Client, path string, q url.Values, key string) ([]T, error) {
	var out []T
	q.Set("page_size", strconv.Itoa(pageSize))
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		var resp map[string]json.RawMessage
		if err := c.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		var items []T
		if raw, ok := resp[key]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, provider.Errorf(provider.Internal, "decoding %s: %v", path, err)
			}
		}
		var total int
		if raw, ok := resp["total_count"]; ok {
			if err := json.Unmarshal(raw, &total); err != nil {
				return nil, provider.Errorf(provider.Internal, "decoding %s: %v", path, err)
			}
		}
		out = append(out, items...)
		if len(items) == 0 || len(out) >= total {
			return out, nil
		}
	}
}

func regions(acct provider.Account) []string {
	var out []string
	for r := range strings.SplitSeq(acct.ConfigString(ConfigRegions), ",") {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return DefaultRegions
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func regionPath(region string) string { return "/k8s/v1/regions/" + url.PathEscape(region) }

func clusterPath(region, id string) string {
	return regionPath(region) + "/clusters/" + url.PathEscape(id)
}

// List returns the clusters in each of the account's regions, with their CA
// and the secret key inline as their token. Clusters being created or
// deleted are skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if proj := req.Account.ConfigString(ConfigProject); proj != "" {
		q.Set("project_id", proj)
	}
	var cred *provider.Credential
	out := []provider.Cluster{}
	for _, r := range regions(req.Account) {
		clusters, err := listAll[kapsuleCluster](ctx, c, regionPath(r)+"/clusters", q, "clusters")
		if err != nil {
			return nil, apiError(err)
		}
		for _, cl := range clusters {
			if notReady(cl) {
				fmt.Fprintf(os.Stderr, "scaleway: skipping %s: it is %s\n", cl.Name, cl.Status)
				continue
			}
			pc, err := p.toCluster(ctx, c, r, cl)
			if err != nil {
				return nil, err
			}
			if cred == nil {
				tok, err := p.token(ctx, c, req.Account)
				if err != nil {
					return nil, err
				}
				cred = &tok
			}
			pc.Credential = cred
			out = append(out, pc)
		}
	}
	return out, nil
}

// notReady reports whether a cluster has no API server to reach.
func notReady(cl kapsuleCluster) bool {
	switch cl.Status {
	case "creating", "deleting", "deleted":
		return true
	default:
		return cl.ClusterURL == ""
	}
}

func (p *Provider) toCluster(ctx context.Context, c *apiclient.Client, region string, cl kapsuleCluster) (provider.Cluster, error) {
	if cl.Region != "" {
		region = cl.Region
	}
	pc := provider.Cluster{
		ID:        clusterID(region, cl.ID),
		Name:      cl.Name,
		Region:    region,
		Version:   cl.Version,
		Endpoints: provider.Endpoints{Public: cl.ClusterURL},
		Labels:    tagLabels(cl.Tags),
	}
	pools, err := listAll[pool](ctx, c, clusterPath(region, cl.ID)+"/pools", url.Values{}, "pools")
	if err != nil {
		return provider.Cluster{}, apiError(err)
	}
	if slices.ContainsFunc(pools, func(np pool) bool { return gpuType(np.NodeType) }) {
		pc.Labels["gpu"] = "true"
	}
	server, ca, err := kubeconfig(ctx, c, region, cl.ID)
	switch {
	case err == nil:
		if server != "" {
			pc.Endpoints.Public = server
		}
		pc.CAData = ca
	case ctx.Err() != nil:
		return provider.Cluster{}, ctx.Err()
	case provider.CodeOf(err) == provider.NotFound || provider.CodeOf(err) == provider.Unavailable:
		// tug keeps the CA it has while the endpoint stays the same; a
		// new cluster gets one at the next sync.
		fmt.Fprintf(os.Stderr, "scaleway: listing %s without its CA: %v\n", cl.Name, err)
	default:
		return provider.Cluster{}, apiError(err)
	}
	return pc, nil
}

// clusterID joins the region and the cluster's ID: the API finds a cluster
// only under its own region, and neither ever changes.
func clusterID(region, id string) string { return region + "/" + id }

// splitID is the inverse of clusterID.
func splitID(id string) (region, clusterID string, err error) {
	region, clusterID, ok := strings.Cut(id, "/")
	if !ok || region == "" || clusterID == "" {
		return "", "", provider.Errorf(provider.NotFound, "%q is not a Scaleway cluster ID of the form <region>/<id>", id).
			WithHint("run tug sync to list the account's clusters again")
	}
	return region, clusterID, nil
}

// tagLabels turns "key=value" and "key:value" tags into labels.
func tagLabels(tags []string) map[string]string {
	labels := map[string]string{}
	for _, t := range tags {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			k, v, ok = strings.Cut(t, ":")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if ok && k != "" && v != "" {
			labels[k] = v
		}
	}
	return labels
}

// gpuType reports whether a node type, as DEV1-M or h100_1_80g, is a GPU
// Instance.
func gpuType(t string) bool {
	return gpuNodeType.MatchString(strings.ReplaceAll(strings.ToUpper(t), "_", "-"))
}

// kubeconfig fetches the cluster's kubeconfig with the legacy token left
// out, and returns its server and CA.
func kubeconfig(ctx context.Context, c *apiclient.Client, region, id string) (server string, ca []byte, err error) {
	var f file
	if err := c.Do(ctx, http.MethodGet, clusterPath(region, id)+"/kubeconfig?redacted=true", nil, &f); err != nil {
		return "", nil, err
	}
	server, ca, err = parseKubeconfig(f.Content)
	if err != nil {
		return "", nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", id, err)
	}
	return server, ca, nil
}

// parseKubeconfig returns the server and CA of the kubeconfig's current
// context, or of its first cluster.
func parseKubeconfig(raw []byte) (server string, ca []byte, err error) {
	var kc struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
			} `json:"context"`
		} `json:"contexts"`
		Clusters []struct {
			Name    string `json:"name"`
			Cluster struct {
				Server string `json:"server"`
				CAData []byte `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return "", nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return "", nil, errors.New("it has no clusters")
	}
	name := kc.Clusters[0].Name
	for _, kctx := range kc.Contexts {
		if kctx.Name == kc.CurrentContext {
			name = kctx.Context.Cluster
		}
	}
	for _, cl := range kc.Clusters {
		if cl.Name == name {
			return cl.Cluster.Server, cl.Cluster.CAData, nil
		}
	}
	return "", nil, fmt.Errorf("it has no cluster %q", name)
}

// token returns the secret key as the cluster token. With the accessKey
// setting it expires when the API key does; without it, or when the key
// cannot be read, it does not expire.
func (p *Provider) token(ctx context.Context, c *apiclient.Client, acct provider.Account) (provider.Credential, error) {
	access := acct.ConfigString(ConfigAccessKey)
	if access == "" {
		return provider.Token(c.Token, provider.NoExpiry), nil
	}
	var k apiKey
	if err := c.Do(ctx, http.MethodGet, "/iam/v1alpha1/api-keys/"+url.PathEscape(access), nil, &k); err != nil {
		if ctx.Err() != nil {
			return provider.Credential{}, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "scaleway: not reading when API key %s expires: %v\n", access, err)
		return provider.Token(c.Token, provider.NoExpiry), nil
	}
	if k.ExpiresAt == nil {
		return provider.Token(c.Token, provider.NoExpiry), nil
	}
	return provider.Token(c.Token, *k.ExpiresAt), nil
}

// Credential checks that the cluster exists and the secret key may read
// it, then returns the secret key as its token.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	region, id, err := splitID(req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	var cl kapsuleCluster
	if err := c.Do(ctx, http.MethodGet, clusterPath(region, id), nil, &cl); err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from Scaleway; run tug sync")
		}
		return provider.Credential{}, apiError(err)
	}
	return p.token(ctx, c, req.Account)
}
