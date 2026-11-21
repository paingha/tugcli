// Package exoscale is tug's first-party provider plugin for Exoscale
// Scalable Kubernetes Service (SKS). It lists the SKS clusters in each zone
// with the Exoscale API v2, signing every request with the account's API
// key and secret. Listing reads each cluster's control plane CA and mints
// nothing; when tug needs a credential, the plugin generates a kubeconfig
// as `exo compute sks kubeconfig` does, with a client certificate that
// lasts an hour, and tug asks for a new one before it runs out.
package exoscale

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
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "exoscale"
	Version = "0.1.0"
)

// DefaultAPIURL is the API of one zone; {zone} stands for the zone's name.
// The apiURL setting may use {zone} the same way.
const DefaultAPIURL = "https://api-{zone}.exoscale.com/v2"

// defaultZone is the zone asked for the list of zones.
const defaultZone = "ch-gva-2"

// Secrets: the two halves of an Exoscale API key.
const (
	SecretAPIKey    = "api_key"
	SecretAPISecret = "api_secret"
)

// Account settings.
const (
	// ConfigZones limits listing to some zones, comma separated, such as
	// ch-gva-2,de-fra-1. Without it the plugin lists every zone.
	ConfigZones = "zones"
	// ConfigUser is the user name in generated kubeconfigs, which becomes
	// the client certificate's common name (default: tug).
	ConfigUser = "user"
	// ConfigGroups are the Kubernetes groups of generated kubeconfigs,
	// comma separated, which become the certificate's organisations
	// (default: system:masters, as in Exoscale's quick start).
	ConfigGroups = "groups"
)

const (
	defaultUser  = "tug"
	defaultGroup = "system:masters"
)

// certLifetime is how long generated client certificates last. Exoscale's
// default is 30 days, and a certificate cannot be revoked short of rotating
// the cluster's operators CA, so the plugin asks for an hour.
const certLifetime = time.Hour

const keyHint = "create an API key for a role that allows the sks service, under IAM in the Exoscale Portal or with `exo iam api-key create`, and give tug its key and secret"

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
		Description:  "Exoscale Scalable Kubernetes Service (SKS)",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{
			{Name: SecretAPIKey, Prompt: "Exoscale API key (EXO...)"},
			{Name: SecretAPISecret, Prompt: "Exoscale API secret"},
		},
	}
}

// api makes clients for each zone of one account.
type api struct {
	p      *Provider
	base   string
	signer *signer
}

func (p *Provider) api(acct provider.Account, secrets map[string]string) (*api, error) {
	key, secret := secrets[SecretAPIKey], secrets[SecretAPISecret]
	if key == "" || secret == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s and %s", SecretAPIKey, SecretAPISecret).WithHint(keyHint)
	}
	base := acct.ConfigString(apiclient.ConfigAPIURL)
	if base == "" {
		base = DefaultAPIURL
	} else if _, err := zoneURL(base, defaultZone); err != nil {
		return nil, err
	}
	now := p.now
	if now == nil {
		now = time.Now
	}
	return &api{p: p, base: base, signer: &signer{key: key, secret: []byte(secret), now: now}}, nil
}

// zoneURL returns the API URL of a zone, checked as apiclient checks
// apiURL.
func zoneURL(base, zone string) (string, error) {
	return apiclient.CheckURL(apiclient.ConfigAPIURL, strings.ReplaceAll(base, "{zone}", url.PathEscape(zone)))
}

// client returns an API client for one zone.
func (a *api) client(zone string) (*apiclient.Client, error) {
	base, err := zoneURL(a.base, zone)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Sign:      a.signer.sign,
		HTTP:      a.p.http,
		UserAgent: "tug-provider-exoscale/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"message": ...} bodies, and the RFC 9457 title and
// detail some errors carry instead.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Title   string `json:"title"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	switch {
	case e.Message != "":
		return e.Message
	case e.Error != "":
		return e.Error
	case e.Detail != "":
		return e.Detail
	default:
		return e.Title
	}
}

// apiError adds hints to the errors a person can fix. Exoscale answers 403
// for a key or signature it does not accept, as its Python client says, and
// a key whose role does not allow SKS needs the same fix, so 401 and 403
// are both Unauthenticated.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated, provider.PermissionDenied:
		pe := *provider.AsError(err)
		pe.Code = provider.Unauthenticated
		return pe.WithHint("the API key is wrong, revoked, or for a role that does not allow the sks service; " + keyHint)
	case provider.RateLimited:
		return provider.AsError(err).WithHint("the Exoscale API limits how fast a key may call it; wait a minute, then run tug sync")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	zone struct {
		Name string `json:"name"`
	}
	sksNodepool struct {
		InstanceType struct {
			ID string `json:"id"`
		} `json:"instance-type"`
	}
	sksCluster struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		State     string            `json:"state"`
		Version   string            `json:"version"`
		Endpoint  string            `json:"endpoint"`
		Level     string            `json:"level"`
		Labels    map[string]string `json:"labels"`
		Nodepools []sksNodepool     `json:"nodepools"`
	}
	instanceType struct {
		ID     string `json:"id"`
		Family string `json:"family"`
		GPUs   int    `json:"gpus"`
	}
)

// zones returns the account's zones, or every zone.
func (a *api) zones(ctx context.Context, acct provider.Account) ([]string, error) {
	var out []string
	for z := range strings.SplitSeq(acct.ConfigString(ConfigZones), ",") {
		if z = strings.ToLower(strings.TrimSpace(z)); z != "" {
			out = append(out, z)
		}
	}
	if len(out) == 0 {
		c, err := a.client(defaultZone)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Zones []zone `json:"zones"`
		}
		if err := c.Do(ctx, http.MethodGet, "/zone", nil, &resp); err != nil {
			return nil, apiError(err)
		}
		for _, z := range resp.Zones {
			if z.Name != "" {
				out = append(out, z.Name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// List returns the SKS clusters in each of the account's zones, with their
// control plane CA. Clusters being created or deleted are skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	a, err := p.api(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	zones, err := a.zones(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	out := []provider.Cluster{}
	for _, z := range zones {
		c, err := a.client(z)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Clusters []sksCluster `json:"sks-clusters"`
		}
		if err := c.Do(ctx, http.MethodGet, "/sks-cluster", nil, &resp); err != nil {
			return nil, apiError(err)
		}
		gpus, err := gpuTypes(ctx, c, resp.Clusters)
		if err != nil {
			return nil, err
		}
		for _, cl := range resp.Clusters {
			endpoint := endpointURL(cl.Endpoint)
			if cl.State == "creating" || cl.State == "deleting" || endpoint == "" {
				fmt.Fprintf(os.Stderr, "exoscale: skipping %s: no API server endpoint yet (state %s)\n", cl.Name, cl.State)
				continue
			}
			ca, err := controlPlaneCA(ctx, c, cl.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, provider.Cluster{
				ID:        clusterID(z, cl.ID),
				Name:      cl.Name,
				Region:    z,
				Version:   cl.Version,
				Endpoints: provider.Endpoints{Public: endpoint},
				CAData:    ca,
				Labels:    clusterLabels(cl, gpus),
			})
		}
	}
	return out, nil
}

// clusterID joins the zone and the cluster's ID: the API finds a cluster
// only in its own zone, and neither ever changes.
func clusterID(zone, id string) string { return zone + "/" + id }

// splitID is the inverse of clusterID.
func splitID(id string) (zone, clusterID string, err error) {
	zone, clusterID, ok := strings.Cut(id, "/")
	if !ok || zone == "" || clusterID == "" {
		return "", "", provider.Errorf(provider.NotFound, "%q is not an Exoscale cluster ID of the form <zone>/<id>", id).
			WithHint("run tug sync to list the account's clusters again")
	}
	return zone, clusterID, nil
}

// endpointURL adds https:// to a bare host name.
func endpointURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err != nil || u.Hostname() == "" {
		return ""
	}
	return s
}

// clusterLabels keeps the cluster's labels, marks clusters with GPU node
// pools, and marks the Pro level, whose control plane is highly
// available.
func clusterLabels(cl sksCluster, gpus map[string]bool) map[string]string {
	labels := map[string]string{}
	for k, v := range cl.Labels {
		if k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v); k != "" && v != "" {
			labels[k] = v
		}
	}
	if cl.Level == "pro" {
		labels["ha"] = "true"
	}
	if slices.ContainsFunc(cl.Nodepools, func(np sksNodepool) bool { return gpus[np.InstanceType.ID] }) {
		labels["gpu"] = "true"
	}
	return labels
}

// gpuTypes returns the zone's instance types that have GPUs, when any of
// the clusters has node pools.
func gpuTypes(ctx context.Context, c *apiclient.Client, clusters []sksCluster) (map[string]bool, error) {
	if !slices.ContainsFunc(clusters, func(cl sksCluster) bool { return len(cl.Nodepools) > 0 }) {
		return nil, nil
	}
	var resp struct {
		InstanceTypes []instanceType `json:"instance-types"`
	}
	if err := c.Do(ctx, http.MethodGet, "/instance-type", nil, &resp); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "exoscale: listing without GPU labels: %v\n", err)
		return nil, nil
	}
	gpus := map[string]bool{}
	for _, it := range resp.InstanceTypes {
		if it.GPUs > 0 || strings.HasPrefix(it.Family, "gpu") {
			gpus[it.ID] = true
		}
	}
	return gpus, nil
}

// controlPlaneCA returns the cluster's control plane CA, which signs its API
// server certificate. When it cannot be read, the cluster is listed without
// one, and tug keeps the CA it has while the endpoint stays the same.
func controlPlaneCA(ctx context.Context, c *apiclient.Client, id string) ([]byte, error) {
	var resp struct {
		CACert string `json:"cacert"`
	}
	if err := c.Do(ctx, http.MethodGet, "/sks-cluster/"+url.PathEscape(id)+"/authority/control-plane/cert", nil, &resp); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "exoscale: listing cluster %s without its CA: %v\n", id, err)
		return nil, nil
	}
	return pemData(resp.CACert), nil
}

// pemData returns PEM that the API sent base64 encoded, or as is.
func pemData(s string) []byte {
	if s == "" {
		return nil
	}
	if !strings.Contains(s, "-----BEGIN") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s)); err == nil {
			return b
		}
	}
	return []byte(s)
}

// groups returns the Kubernetes groups for generated kubeconfigs.
func groups(acct provider.Account) []string {
	var out []string
	for g := range strings.SplitSeq(acct.ConfigString(ConfigGroups), ",") {
		if g = strings.TrimSpace(g); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return []string{defaultGroup}
	}
	return out
}

// Credential generates a kubeconfig for the cluster and returns its client
// certificate, which lasts certLifetime.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	a, err := p.api(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	z, id, err := splitID(req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	c, err := a.client(z)
	if err != nil {
		return provider.Credential{}, err
	}
	user := strings.TrimSpace(req.Account.ConfigString(ConfigUser))
	if user == "" {
		user = defaultUser
	}
	body := map[string]any{"user": user, "groups": groups(req.Account), "ttl": int64(certLifetime.Seconds())}
	var resp struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := c.Do(ctx, http.MethodPost, "/sks-cluster-kubeconfig/"+url.PathEscape(id), body, &resp); err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from Exoscale; run tug sync")
		}
		return provider.Credential{}, apiError(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(resp.Kubeconfig))
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s is not base64: %v", id, err)
	}
	cred, err := certCredential(raw)
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", id, err)
	}
	return cred, nil
}

// certCredential reads the client certificate of the kubeconfig's current
// context, or of its first user, which expires when the certificate does.
func certCredential(raw []byte) (provider.Credential, error) {
	var kc struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				User string `json:"user"`
			} `json:"context"`
		} `json:"contexts"`
		Users []struct {
			Name string `json:"name"`
			User struct {
				CertData []byte `json:"client-certificate-data"`
				KeyData  []byte `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return provider.Credential{}, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(kc.Users) == 0 {
		return provider.Credential{}, errors.New("it has no users")
	}
	name := kc.Users[0].Name
	for _, kctx := range kc.Contexts {
		if kctx.Name == kc.CurrentContext {
			name = kctx.Context.User
		}
	}
	for _, u := range kc.Users {
		if u.Name != name {
			continue
		}
		if len(u.User.CertData) == 0 || len(u.User.KeyData) == 0 {
			return provider.Credential{}, errors.New("it has no client certificate")
		}
		block, _ := pem.Decode(u.User.CertData)
		if block == nil || block.Type != "CERTIFICATE" {
			return provider.Credential{}, errors.New("its client certificate is not PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return provider.Credential{}, fmt.Errorf("parsing its client certificate: %w", err)
		}
		return provider.ClientCert(u.User.CertData, u.User.KeyData, cert.NotAfter), nil
	}
	return provider.Credential{}, fmt.Errorf("it has no user %q", name)
}
