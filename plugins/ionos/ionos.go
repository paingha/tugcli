// Package ionos is tug's first-party provider plugin for IONOS Cloud
// Managed Kubernetes. It lists clusters with the Cloud API v6, which keeps a
// cluster's API server address only in its kubeconfig, so listing fetches
// each cluster's kubeconfig for the address and CA and returns its
// credential inline, with the expiry the credential carries; tug moves it to
// the keychain and asks for a new one before it runs out.
package ionos

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
	Name    = "ionos"
	Version = "0.1.1"
)

// DefaultAPIURL is version 6 of the IONOS Cloud API.
const DefaultAPIURL = "https://api.ionos.com/cloudapi/v6"

// SecretToken is an IONOS Cloud API token.
const SecretToken = "api_token"

// ConfigContract is the account setting for users with several contracts:
// the contract number to act in, sent as X-Contract-Number.
const ConfigContract = "contract"

// fallbackLifetime is how long the plugin trusts a kubeconfig token whose
// expiry it cannot read. The API does not document how long kubeconfig
// tokens last; asking again after 12 hours costs one request.
const fallbackLifetime = 12 * time.Hour

const tokenHint = "create a token in the DCD under Menu > Management > Token Manager, or with `ionosctl token generate`, and give tug its value"

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
		Description:  "IONOS Cloud Managed Kubernetes",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   SecretToken,
			Prompt: "IONOS Cloud API token",
		}},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	token := secrets[SecretToken]
	if token == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s", SecretToken).WithHint(tokenHint)
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	contract := strings.TrimSpace(acct.ConfigString(ConfigContract))
	if contract != "" && strings.Trim(contract, "0123456789") != "" {
		return nil, provider.Errorf(provider.InvalidConfig, "%s %q is not a contract number", ConfigContract, contract)
	}
	return &apiclient.Client{
		BaseURL: base,
		Token:   token,
		Sign: func(req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			if contract != "" {
				req.Header.Set("X-Contract-Number", contract)
			}
			return nil
		},
		HTTP:      p.http,
		UserAgent: "tug-provider-ionos/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"httpStatus": ..., "messages": [{"errorCode": ...,
// "message": ...}]} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Messages []struct {
			ErrorCode string `json:"errorCode"`
			Message   string `json:"message"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	var msgs []string
	for _, m := range e.Messages {
		if m.Message != "" {
			msgs = append(msgs, m.Message)
		}
	}
	return strings.Join(msgs, "; ")
}

// apiError adds hints to the errors a person can fix.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the token is wrong, expired or deleted; " + tokenHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint("your IONOS Cloud user needs access to the cluster through a group with the Create Kubernetes Clusters privilege, or must be a contract administrator")
	case provider.RateLimited:
		return provider.AsError(err).WithHint("the Cloud API limits how many requests a contract may make a minute; wait a minute, then run tug sync")
	default:
		return err
	}
}

// API types, trimmed to the fields the plugin uses.
type (
	metadata struct {
		State string `json:"state"`
	}
	k8sCluster struct {
		ID         string   `json:"id"`
		Metadata   metadata `json:"metadata"`
		Properties struct {
			Name       string `json:"name"`
			K8sVersion string `json:"k8sVersion"`
			Public     *bool  `json:"public"`
			Location   string `json:"location"`
		} `json:"properties"`
	}
	nodePool struct {
		Properties struct {
			ServerType string `json:"serverType"`
		} `json:"properties"`
	}
	collection[T any] struct {
		Items []T `json:"items"`
	}
)

// notReady reports whether a cluster in state has no API server to reach.
func notReady(state string) bool {
	switch state {
	case "DEPLOYING", "DESTROYING", "FAILED_DESTROYING", "TERMINATED", "INACTIVE":
		return true
	default:
		return false
	}
}

func clusterPath(id string) string { return "/k8s/" + url.PathEscape(id) }

// List returns the account's clusters, each with the address, CA and
// credential from its kubeconfig. Clusters being created or deleted are
// skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	var clusters collection[k8sCluster]
	if err := c.Do(ctx, http.MethodGet, "/k8s?depth=1", nil, &clusters); err != nil {
		return nil, apiError(err)
	}
	out := []provider.Cluster{}
	for _, cl := range clusters.Items {
		name := cl.Properties.Name
		if notReady(cl.Metadata.State) {
			fmt.Fprintf(os.Stderr, "ionos: skipping %s: it is %s\n", name, cl.Metadata.State)
			continue
		}
		var pools collection[nodePool]
		if err := c.Do(ctx, http.MethodGet, clusterPath(cl.ID)+"/nodepools?depth=1", nil, &pools); err != nil {
			return nil, apiError(err)
		}
		kc, err := p.kubeconfig(ctx, c, cl.ID)
		if err != nil {
			switch code := provider.CodeOf(err); {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case code == provider.NotFound:
				fmt.Fprintf(os.Stderr, "ionos: skipping %s: it has no kubeconfig yet (state %s)\n", name, cl.Metadata.State)
				continue
			case len(pools.Items) == 0 && (code == provider.PermissionDenied || code == provider.InvalidConfig):
				// Only administrators may download the kubeconfig of a
				// cluster without node pools.
				fmt.Fprintf(os.Stderr, "ionos: skipping %s: only contract administrators can reach a cluster without node pools\n", name)
				continue
			default:
				return nil, apiError(err)
			}
		}
		pc := provider.Cluster{
			ID:         cl.ID,
			Name:       name,
			Region:     cl.Properties.Location,
			Version:    cl.Properties.K8sVersion,
			Endpoints:  provider.Endpoints{Public: kc.server},
			CAData:     kc.ca,
			Labels:     map[string]string{},
			Credential: &kc.cred,
		}
		if pc.Name == "" {
			pc.Name = cl.ID
		}
		if slices.ContainsFunc(pools.Items, func(np nodePool) bool { return strings.EqualFold(np.Properties.ServerType, "GPU") }) {
			pc.Labels["gpu"] = "true"
		}
		if cl.Properties.Public != nil && !*cl.Properties.Public {
			// Private clusters run their node pools behind a NAT gateway in
			// one location.
			pc.Labels["private-nodes"] = "true"
		}
		out = append(out, pc)
	}
	return out, nil
}

// clusterConfig is what a cluster's kubeconfig says about reaching it.
type clusterConfig struct {
	server string
	ca     []byte
	cred   provider.Credential
}

// kubeconfig fetches the cluster's kubeconfig and reads it.
func (p *Provider) kubeconfig(ctx context.Context, c *apiclient.Client, id string) (*clusterConfig, error) {
	var raw json.RawMessage
	if err := c.Do(ctx, http.MethodGet, clusterPath(id)+"/kubeconfig", nil, &raw); err != nil {
		return nil, err
	}
	kc, err := p.parseKubeconfig(raw)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", id, err)
	}
	return kc, nil
}

// parseKubeconfig reads the server, CA and credential of the kubeconfig's
// current context, or of its first entries. The API returns the kubeconfig
// in JSON when asked for JSON; a JSON string holding the YAML form is
// accepted too.
func (p *Provider) parseKubeconfig(raw []byte) (*clusterConfig, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = []byte(text)
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
				Token    string `json:"token"`
				CertData []byte `json:"client-certificate-data"`
				KeyData  []byte `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
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
	out := &clusterConfig{}
	for _, cl := range kc.Clusters {
		if cl.Name == clusterName {
			out.server, out.ca = cl.Cluster.Server, cl.Cluster.CAData
		}
	}
	if out.server == "" {
		return nil, errors.New("it has no API server address")
	}
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		switch {
		case u.User.Token != "":
			out.cred = provider.Token(u.User.Token, p.tokenExpiry(u.User.Token))
		case len(u.User.CertData) > 0 && len(u.User.KeyData) > 0:
			expires, err := certExpiry(u.User.CertData)
			if err != nil {
				return nil, err
			}
			out.cred = provider.ClientCert(u.User.CertData, u.User.KeyData, expires)
		}
	}
	if out.cred.Type == "" {
		return nil, errors.New("its user has neither a token nor a client certificate")
	}
	return out, nil
}

// tokenExpiry returns when a kubeconfig token expires: the exp claim of a
// JSON Web Token, or fallbackLifetime from now for a token without one.
// The claim is only read, never trusted for anything else; the API server
// checks the token.
func (p *Provider) tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); err == nil {
			var claims struct {
				Exp float64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Exp > 0 {
				return time.Unix(int64(claims.Exp), 0)
			}
		}
	}
	return p.now().Add(fallbackLifetime)
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

// Credential returns the credential from the cluster's kubeconfig.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	kc, err := p.kubeconfig(ctx, c, req.ClusterID)
	if err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from IONOS Cloud, or has no kubeconfig yet; run tug sync")
		}
		return provider.Credential{}, apiError(err)
	}
	return kc.cred, nil
}
