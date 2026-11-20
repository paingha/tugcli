// Package google is the built-in provider for Google Kubernetes Engine. It
// lists clusters per project through the Container API and mints short-lived
// OAuth tokens from Application Default Credentials, so gcloud is optional.
package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/oauth2"
	googleauth "golang.org/x/oauth2/google"

	"github.com/paingha/tugcli/internal/ini"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "google"

// ConfigProject is the account config key for the Google Cloud project.
const ConfigProject = "project"

// DefaultAPIURL is the Container API.
const DefaultAPIURL = "https://container.googleapis.com/v1"

// authPlugin is Google's own kubectl exec plugin, the fallback when there are
// no Application Default Credentials.
const authPlugin = "gke-gcloud-auth-plugin"

var scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
}

// TokenSource returns the OAuth token source for API calls and credentials.
type TokenSource func(ctx context.Context) (oauth2.TokenSource, error)

// Provider lists GKE clusters.
type Provider struct {
	apiURL   string
	base     http.RoundTripper
	tokens   TokenSource
	lookPath func(string) (string, error)
	getenv   func(string) string
	home     string
}

// Option configures a Provider.
type Option func(*Provider)

// WithAPIURL points the provider at another Container API endpoint, for tests.
func WithAPIURL(u string) Option { return func(p *Provider) { p.apiURL = strings.TrimSuffix(u, "/") } }

// WithTransport sets the HTTP transport under the OAuth layer, for tests.
func WithTransport(rt http.RoundTripper) Option { return func(p *Provider) { p.base = rt } }

// WithTokenSource replaces Application Default Credentials, for tests.
func WithTokenSource(ts TokenSource) Option { return func(p *Provider) { p.tokens = ts } }

// WithLookPath replaces exec.LookPath, for tests.
func WithLookPath(f func(string) (string, error)) Option { return func(p *Provider) { p.lookPath = f } }

// WithHome sets the home directory used to find gcloud's configuration.
func WithHome(dir string) Option { return func(p *Provider) { p.home = dir } }

// WithGetenv replaces os.Getenv, for tests.
func WithGetenv(f func(string) string) Option { return func(p *Provider) { p.getenv = f } }

// New returns the GKE provider.
func New(opts ...Option) *Provider {
	p := &Provider{
		apiURL: DefaultAPIURL,
		base:   http.DefaultTransport,
		tokens: func(ctx context.Context) (oauth2.TokenSource, error) {
			return googleauth.DefaultTokenSource(ctx, scopes...)
		},
		lookPath: exec.LookPath,
		getenv:   os.Getenv,
	}
	for _, o := range opts {
		o(p)
	}
	if p.home == "" {
		p.home, _ = os.UserHomeDir()
	}
	return p
}

// Info implements registry.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      version.String(),
		Description:  "Google Kubernetes Engine clusters, per Google Cloud project",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// gcloudDir is where gcloud keeps its configurations.
func (p *Provider) gcloudDir() string {
	if d := p.getenv("CLOUDSDK_CONFIG"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if appData := p.getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "gcloud")
		}
	}
	return filepath.Join(p.home, ".config", "gcloud")
}

// Accounts returns one account per project named in gcloud's configurations.
func (p *Provider) Accounts(_ context.Context) ([]provider.Account, error) {
	files, err := filepath.Glob(filepath.Join(p.gcloudDir(), "configurations", "config_*"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []provider.Account
	for _, f := range files {
		cfg, err := ini.ParseFile(f)
		if err != nil {
			continue
		}
		project := cfg.Get("core", "project")
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		name := strings.TrimPrefix(filepath.Base(f), "config_")
		out = append(out, provider.Account{
			ID:          project,
			Description: "gcloud configuration " + name,
			Config:      map[string]any{ConfigProject: project},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func project(acct provider.Account) string {
	if v := acct.ConfigString(ConfigProject); v != "" {
		return v
	}
	return acct.ID
}

// Container API response, trimmed to the fields tug uses.
type (
	listResponse struct {
		Clusters     []gkeCluster `json:"clusters"`
		MissingZones []string     `json:"missingZones"`
	}
	gkeCluster struct {
		Name                 string            `json:"name"`
		Location             string            `json:"location"`
		Endpoint             string            `json:"endpoint"`
		CurrentMasterVersion string            `json:"currentMasterVersion"`
		Status               string            `json:"status"`
		ResourceLabels       map[string]string `json:"resourceLabels"`
		MasterAuth           struct {
			ClusterCACertificate string `json:"clusterCaCertificate"`
		} `json:"masterAuth"`
		PrivateClusterConfig *struct {
			EnablePrivateEndpoint bool   `json:"enablePrivateEndpoint"`
			PrivateEndpoint       string `json:"privateEndpoint"`
		} `json:"privateClusterConfig"`
		NodePools []struct {
			Config struct {
				Accelerators []struct {
					AcceleratorType string `json:"acceleratorType"`
				} `json:"accelerators"`
			} `json:"config"`
		} `json:"nodePools"`
	}
	apiError struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
)

// List returns every cluster in the account's project, in all locations.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	proj := project(req.Account)
	if proj == "" {
		return nil, provider.Errorf(provider.InvalidConfig, "no Google Cloud project for account %q", req.Account.ID).
			WithHint("tug import google --account <project-id>")
	}
	ts, err := p.tokens(ctx)
	if err != nil {
		return nil, noCredentials(err)
	}
	client := &http.Client{Transport: &oauth2.Transport{Source: ts, Base: p.base}}
	u := fmt.Sprintf("%s/projects/%s/locations/-/clusters", p.apiURL, url.PathEscape(proj))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return nil, noCredentials(err)
		}
		return nil, provider.Errorf(provider.Unavailable, "Container API: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, provider.MaxMessageBytes))
	if err != nil {
		return nil, provider.Errorf(provider.Unavailable, "reading Container API response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, containerError(resp, body, proj)
	}
	var lr listResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, provider.Errorf(provider.Internal, "decoding Container API response: %v", err)
	}
	if len(lr.MissingZones) > 0 {
		fmt.Fprintf(os.Stderr, "google: these locations did not answer and may be missing: %s\n", strings.Join(lr.MissingZones, ", "))
	}
	out := make([]provider.Cluster, 0, len(lr.Clusters))
	for _, c := range lr.Clusters {
		pc, err := toCluster(proj, c)
		if err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

func toCluster(proj string, c gkeCluster) (provider.Cluster, error) {
	out := provider.Cluster{
		ID:      c.Location + "/" + c.Name,
		Name:    c.Name,
		Region:  c.Location,
		Version: c.CurrentMasterVersion,
		Labels:  map[string]string{"gcp-project": proj},
	}
	if pc := c.PrivateClusterConfig; pc != nil && pc.PrivateEndpoint != "" {
		out.Endpoints.Private = "https://" + pc.PrivateEndpoint
	}
	if pc := c.PrivateClusterConfig; (pc == nil || !pc.EnablePrivateEndpoint) && c.Endpoint != "" {
		out.Endpoints.Public = "https://" + c.Endpoint
	}
	if ca := c.MasterAuth.ClusterCACertificate; ca != "" {
		pem, err := base64.StdEncoding.DecodeString(ca)
		if err != nil {
			return provider.Cluster{}, provider.Errorf(provider.Internal, "cluster %s: bad CA certificate: %v", c.Name, err)
		}
		out.CAData = pem
	}
	for k, v := range c.ResourceLabels {
		if k == "environment" {
			k = "env"
		}
		out.Labels[k] = v
	}
	for _, np := range c.NodePools {
		if len(np.Config.Accelerators) > 0 {
			out.Labels["gpu"] = "true"
		}
	}
	return out, nil
}

func containerError(resp *http.Response, body []byte, proj string) error {
	var ae apiError
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &ae) == nil && ae.Error.Message != "" {
		msg = ae.Error.Message
	}
	e := provider.StatusError(resp.StatusCode, msg, resp.Header)
	switch {
	case resp.StatusCode == http.StatusForbidden && (ae.Error.Status == "PERMISSION_DENIED" && strings.Contains(msg, "has not been used") || strings.Contains(msg, "SERVICE_DISABLED")):
		return e.WithHint("enable the Kubernetes Engine API: gcloud services enable container.googleapis.com --project " + proj)
	case e.Code == provider.Unauthenticated:
		return e.WithHint("gcloud auth application-default login")
	}
	return e
}

// Credential returns a short-lived OAuth token, which GKE accepts as a bearer
// token. Without Application Default Credentials it delegates to
// gke-gcloud-auth-plugin when that is installed.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	ts, err := p.tokens(ctx)
	if err == nil {
		var tok *oauth2.Token
		if tok, err = ts.Token(); err == nil {
			return provider.Token(tok.AccessToken, tok.Expiry), nil
		}
	}
	if _, lerr := p.lookPath(authPlugin); lerr == nil {
		c := provider.Delegate(authPlugin)
		c.Exec.APIVersion = "client.authentication.k8s.io/v1beta1"
		c.Exec.InstallHint = "gcloud components install gke-gcloud-auth-plugin"
		return c, nil
	}
	return provider.Credential{}, noCredentials(err)
}

func noCredentials(err error) error {
	return provider.Errorf(provider.Unauthenticated, "no Google credentials: %v", err).
		WithHint("gcloud auth application-default login")
}
