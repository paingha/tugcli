// Package ovhcloud is tug's first-party provider plugin for OVHcloud
// Managed Kubernetes. It lists a Public Cloud project's clusters with the
// OVHcloud API, signing each request with the account's application key,
// application secret and consumer key. The credential is the client
// certificate in the kubeconfig the API generates for each cluster, which
// listing returns inline; tug asks again when the certificate expires.
package ovhcloud

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
	"github.com/paingha/tugcli/plugins/internal/kubeconfig"
)

// Name and version of the plugin.
const (
	Name    = "ovhcloud"
	Version = "0.1.1"
)

// Endpoints are the OVHcloud API's regional endpoints, by the name the
// endpoint setting takes, as OVHcloud's own API clients list them.
var Endpoints = map[string]string{
	"eu": "https://api.eu.ovhcloud.com/1.0",
	"ca": "https://api.ca.ovhcloud.com/1.0",
	"us": "https://api.us.ovhcloud.com/1.0",
}

// DefaultEndpoint is the endpoint for accounts that do not choose one.
const DefaultEndpoint = "eu"

// Secrets: the three keys of an OVHcloud API token.
const (
	SecretApplicationKey    = "application_key"
	SecretApplicationSecret = "application_secret"
	SecretConsumerKey       = "consumer_key"
)

// Account settings.
const (
	// ConfigEndpoint is the API endpoint the account belongs to: eu, ca or
	// us.
	ConfigEndpoint = "endpoint"
	// ConfigProject is the Public Cloud project (its service name) to list
	// clusters in. It can be left out when the keys see one project.
	ConfigProject = "project"
)

// rights are the API calls the consumer key needs.
const rights = "GET /cloud/project, GET /cloud/project/* and POST /cloud/project/*/kube/*/kubeconfig"

const rateHint = "wait a minute, then run tug sync"

// Provider implements the plugin.
type Provider struct {
	http *http.Client
	now  func() time.Time
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{now: time.Now} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "OVHcloud Managed Kubernetes",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{
			{Name: SecretApplicationKey, Prompt: "OVHcloud API application key"},
			{Name: SecretApplicationSecret, Prompt: "OVHcloud API application secret"},
			{Name: SecretConsumerKey, Prompt: "OVHcloud API consumer key"},
		},
	}
}

func (p *Provider) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// endpoint returns the account's endpoint name and API URL.
func endpoint(acct provider.Account) (string, string, error) {
	name := strings.TrimPrefix(strings.ToLower(acct.ConfigString(ConfigEndpoint)), "ovh-")
	if name == "" {
		name = DefaultEndpoint
	}
	def, ok := Endpoints[name]
	if !ok {
		return "", "", provider.Errorf(provider.InvalidConfig, "%s must be eu, ca or us, got %q", ConfigEndpoint, acct.ConfigString(ConfigEndpoint)).
			WithHint("use the endpoint of the OVHcloud account the keys belong to: eu for OVHcloud Europe, ca for Canada and Asia-Pacific, us for OVHcloud US")
	}
	base, err := apiclient.BaseURL(acct, def)
	return name, base, err
}

// keyHint says where to create keys for an endpoint.
func keyHint(name string) string {
	return fmt.Sprintf("create an application key, application secret and consumer key at https://auth.%s.ovhcloud.com/api/createToken with the rights %s; "+
		"check that %s matches the account they belong to", name, rights, ConfigEndpoint)
}

// session is an account's signed API client.
type session struct {
	api      *apiclient.Client
	endpoint string
}

// session reads the API's clock, so that signatures carry its time, and
// returns a client that signs with the account's keys.
func (p *Provider) session(ctx context.Context, acct provider.Account, secrets map[string]string) (*session, error) {
	ak, as, ck := secrets[SecretApplicationKey], secrets[SecretApplicationSecret], secrets[SecretConsumerKey]
	name, base, err := endpoint(acct)
	if err != nil {
		return nil, err
	}
	if ak == "" || as == "" || ck == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s, %s and %s", SecretApplicationKey, SecretApplicationSecret, SecretConsumerKey).
			WithHint(keyHint(name))
	}
	unsigned := &apiclient.Client{
		BaseURL:   base,
		Sign:      func(*http.Request) error { return nil },
		HTTP:      p.http,
		UserAgent: "tug-provider-ovhcloud/" + Version,
		Message:   errorMessage,
	}
	var serverTime int64
	if err := unsigned.Do(ctx, http.MethodGet, "/auth/time", nil, &serverTime); err != nil {
		return nil, err
	}
	s := &signer{appKey: ak, appSecret: as, consumerKey: ck, delta: p.clock().Sub(time.Unix(serverTime, 0)), now: p.clock}
	api := *unsigned
	api.Sign = s.sign
	api.Code = errorCode
	return &session{api: &api, endpoint: name}, nil
}

// apiError is an OVHcloud API error body.
type apiError struct {
	Class     string `json:"class"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorCode"`
}

func errorMessage(body []byte) string {
	var e apiError
	if json.Unmarshal(body, &e) != nil || e.Message == "" {
		return ""
	}
	if e.ErrorCode != "" {
		return e.ErrorCode + ": " + e.Message
	}
	return e.Message
}

// errorCode classifies the API's 400s and 403s by their errorCode, as
// OVHcloud's own clients do: a 403 covers both keys the API does not know
// and calls the consumer key may not make, and a signature made with the
// wrong application secret is a 400. 460 means the project has expired.
func errorCode(status int, body []byte) provider.Code {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden:
		var e apiError
		_ = json.Unmarshal(body, &e)
		switch e.ErrorCode {
		case "INVALID_KEY", "INVALID_CREDENTIAL", "NOT_CREDENTIAL", "INVALID_SIGNATURE":
			return provider.Unauthenticated
		}
	case 460:
		return provider.NotFound
	}
	return ""
}

// withHint adds the next step to the errors a person can fix.
func (s *session) withHint(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the API does not accept the keys, or the consumer key has not been validated; " + keyHint(s.endpoint))
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint("the consumer key's rights do not cover this call; create one with the rights " + rights)
	case provider.NotFound:
		return provider.AsError(err).WithHint("check " + ConfigProject + ": it is the project ID that the Public Cloud section of the OVHcloud Control Panel shows, and the project must be active")
	case provider.RateLimited:
		return provider.AsError(err).WithHint(rateHint)
	default:
		return err
	}
}

// project returns the account's project, or the only one the keys see.
func (s *session) project(ctx context.Context, acct provider.Account) (string, error) {
	if id := acct.ConfigString(ConfigProject); id != "" {
		return id, nil
	}
	var ids []string
	if err := s.api.Do(ctx, http.MethodGet, "/cloud/project", nil, &ids); err != nil {
		return "", s.withHint(err)
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", provider.Errorf(provider.NotFound, "the keys see no Public Cloud projects").
			WithHint("create a Public Cloud project in the OVHcloud Control Panel, or use keys of the account that has one")
	default:
		return "", provider.Errorf(provider.InvalidConfig, "the keys see %d Public Cloud projects", len(ids)).
			WithHint("set " + ConfigProject + " to one of " + strings.Join(ids, ", "))
	}
}

func kubePath(project, id string) string {
	return "/cloud/project/" + url.PathEscape(project) + "/kube/" + url.PathEscape(id)
}

// kubeCluster is cloud.kube.Cluster, trimmed to the fields the plugin uses.
type kubeCluster struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Region  string `json:"region"`
	URL     string `json:"url"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Plan    string `json:"plan"`
}

// notReady reports whether a cluster in status has no API server to
// reach: it is being installed or reset, is suspended, or is going away.
func notReady(status string) bool {
	switch status {
	case "INSTALLING", "RESETTING", "DELETING", "DELETED", "SUSPENDING", "SUSPENDED", "REOPENING":
		return true
	default:
		return false
	}
}

// List returns the project's clusters, each with its CA and client
// certificate from the kubeconfig the API generates.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	s, err := p.session(ctx, req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	project, err := s.project(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := s.api.Do(ctx, http.MethodGet, "/cloud/project/"+url.PathEscape(project)+"/kube", nil, &ids); err != nil {
		return nil, s.withHint(err)
	}
	out := make([]provider.Cluster, 0, len(ids))
	for _, id := range ids {
		var cl kubeCluster
		if err := s.api.Do(ctx, http.MethodGet, kubePath(project, id), nil, &cl); err != nil {
			return nil, s.withHint(err)
		}
		if notReady(cl.Status) || cl.URL == "" {
			fmt.Fprintf(os.Stderr, "ovhcloud: skipping %s: it is %s\n", cl.Name, cl.Status)
			continue
		}
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Name,
			Region:    cl.Region,
			Version:   cl.Version,
			Endpoints: provider.Endpoints{Public: endpointURL(cl.URL)},
			Labels:    map[string]string{},
		}
		if cl.Plan != "" {
			pc.Labels["plan"] = cl.Plan
		}
		acc, err := s.kubeconfig(ctx, project, cl.ID)
		switch {
		case err == nil:
			cred, err := credential(cl.Name, acc)
			if err != nil {
				return nil, err
			}
			pc.CAData = acc.CAData
			if acc.Server != "" {
				pc.Endpoints.Public = acc.Server
			}
			pc.Credential = &cred
		case provider.CodeOf(err) == provider.Unavailable || provider.CodeOf(err) == provider.InvalidConfig:
			// A cluster in maintenance or redeploying may not hand out a
			// kubeconfig. Listing it without one keeps it from being
			// marked stale; the next sync picks up the CA.
			fmt.Fprintf(os.Stderr, "ovhcloud: %s has no kubeconfig now (it is %s): %v\n", cl.Name, cl.Status, err)
		default:
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

// endpointURL turns the cluster's URL, a host name, into the URL the
// Kubernetes API is served at.
func endpointURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return s
	}
	return "https://" + s
}

// kubeconfig asks the API to generate the cluster's kubeconfig and reads
// it.
func (s *session) kubeconfig(ctx context.Context, project, id string) (*kubeconfig.Access, error) {
	var resp struct {
		Content string `json:"content"`
	}
	if err := s.api.Do(ctx, http.MethodPost, kubePath(project, id)+"/kubeconfig", nil, &resp); err != nil {
		return nil, s.withHint(err)
	}
	acc, err := kubeconfig.Parse([]byte(resp.Content))
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", id, err)
	}
	return acc, nil
}

// credential returns the kubeconfig's client certificate, which tug uses
// until shortly before it expires, or its token.
func credential(cluster string, acc *kubeconfig.Access) (provider.Credential, error) {
	switch {
	case len(acc.CertData) > 0 && len(acc.KeyData) > 0:
		expires := provider.NoExpiry
		if block, _ := pem.Decode(acc.CertData); block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				expires = cert.NotAfter
			}
		}
		return provider.ClientCert(acc.CertData, acc.KeyData, expires), nil
	case acc.Token != "":
		return provider.Token(acc.Token, provider.NoExpiry), nil
	default:
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s has neither a client certificate nor a token", cluster)
	}
}

// Credential generates the cluster's kubeconfig and returns its client
// certificate.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	s, err := p.session(ctx, req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	project, err := s.project(ctx, req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	acc, err := s.kubeconfig(ctx, project, req.ClusterID)
	if err != nil {
		return provider.Credential{}, err
	}
	return credential(req.ClusterID, acc)
}
