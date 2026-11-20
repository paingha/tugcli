// Package crusoe is tug's first-party provider plugin for Crusoe Managed
// Kubernetes (CMK). It lists clusters with the Crusoe Cloud REST API,
// signing each request with the account's API access key and secret key.
// Like Crusoe's own tooling, it gives a cluster set up for OpenID Connect an
// oidc credential, so that tug signs the user in to the cluster's issuer,
// and any other cluster the admin client certificate from its kubeconfig.
package crusoe

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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "crusoe"
	Version = "0.1.0"
)

// DefaultAPIURL is version 1 of the Crusoe Cloud API.
const DefaultAPIURL = "https://api.cloud.crusoe.ai/v1"

// Secrets: the two halves of a Crusoe Cloud API key.
const (
	SecretAccessKeyID = "access_key_id"
	SecretSecretKey   = "secret_key"
)

// Account settings.
const (
	// ConfigProject is the project to list clusters in, by ID or name. It
	// can be left out when the API key can see only one project.
	ConfigProject = "project"
	// ConfigAuth chooses the credential: AuthAuto, AuthOIDC or
	// AuthAdminCert.
	ConfigAuth = "auth"
)

// Values of ConfigAuth, named after the API's auth_type.
const (
	// AuthAuto, the default, uses OIDC for clusters set up for it and the
	// admin client certificate for the others.
	AuthAuto = "auto"
	// AuthOIDC uses OIDC only, and leaves out clusters without it, for
	// users who may not fetch admin certificates.
	AuthOIDC = "oidc"
	// AuthAdminCert uses the admin client certificate for every cluster.
	AuthAdminCert = "admin_cert"
)

// Hints for errors a person can fix.
const (
	keyHint        = "create a Cloud API key in the Crusoe Cloud console under Admin > Security > Cloud API keys (https://console.crusoecloud.com/security/cloud-api-keys); the console shows its secret key only once"
	clustersDenied = "the API key's user needs access to the project's Kubernetes clusters"
	adminDenied    = "fetching a cluster's admin certificate needs the admin role in the Crusoe organisation; ask an admin, or set up OIDC on the cluster and set auth=oidc"
)

// cpuFamily matches the VM type families that have no GPUs: c1a and c2a
// (general purpose) and s1a and s2a (storage optimised).
var cpuFamily = regexp.MustCompile(`^[cs]\d+[a-z]*$`)

// projectID matches a project ID, as opposed to a project name.
var projectID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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
		Description:  "Crusoe Managed Kubernetes (CMK)",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{
			{Name: SecretAccessKeyID, Prompt: "Crusoe Cloud API access key ID"},
			{Name: SecretSecretKey, Prompt: "Crusoe Cloud API secret key"},
		},
	}
}

func (p *Provider) client(acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	keyID, secretKey := secrets[SecretAccessKeyID], secrets[SecretSecretKey]
	if keyID == "" || secretKey == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s and %s", SecretAccessKeyID, SecretSecretKey).WithHint(keyHint)
	}
	now := p.now
	if now == nil {
		now = time.Now
	}
	s, err := newSigner(keyID, secretKey, now)
	if err != nil {
		return nil, err
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Sign:      s.sign,
		HTTP:      p.http,
		UserAgent: "tug-provider-crusoe/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"code": "401", "message": "bad_credential"} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Message
}

// apiError adds hints to the errors a person can fix. denied is the hint for
// PermissionDenied, which depends on what was asked for.
func apiError(err error, denied string) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("the API key is wrong, expired or deleted; " + keyHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint(denied)
	case provider.RateLimited:
		return provider.AsError(err).WithHint("wait a minute, then run tug sync")
	default:
		return err
	}
}

func authMode(acct provider.Account) (string, error) {
	switch mode := acct.ConfigString(ConfigAuth); mode {
	case "", AuthAuto:
		return AuthAuto, nil
	case AuthOIDC, AuthAdminCert:
		return mode, nil
	default:
		return "", provider.Errorf(provider.InvalidConfig, "%s must be %s, %s or %s, got %q", ConfigAuth, AuthAuto, AuthOIDC, AuthAdminCert, mode)
	}
}

// Crusoe Cloud API types, trimmed to the fields the plugin uses.
type (
	project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	projectsResponse struct {
		Items []project `json:"items"`
	}
	oidcConfig struct {
		IssuerURL string `json:"issuer_url"`
		ClientID  string `json:"client_id"`
		CACert    string `json:"ca_cert"`
	}
	cmkCluster struct {
		ID            string            `json:"id"`
		Name          string            `json:"name"`
		Location      string            `json:"location"`
		Version       string            `json:"version"`
		State         string            `json:"state"`
		Private       bool              `json:"private"`
		KubeloginArgs map[string]string `json:"kubelogin_args"`
		AuthConfig    *struct {
			OIDC *oidcConfig `json:"oidc"`
		} `json:"auth_config"`
	}
	clustersResponse struct {
		Items []cmkCluster `json:"items"`
	}
	nodePool struct {
		ClusterID string `json:"cluster_id"`
		Type      string `json:"type"`
	}
	nodePoolsResponse struct {
		Items []nodePool `json:"items"`
	}
	// credentialsResponse is KubernetesAuthenticationDetails: with an
	// auth_type, a kubeconfig; without one, the legacy PEM fields.
	credentialsResponse struct {
		KubeConfig            string `json:"kube_config"`
		ClusterAddress        string `json:"cluster_address"`
		ClusterCACertificate  string `json:"cluster_ca_certificate"`
		UserClientCertificate string `json:"user_client_certificate"`
		UserClientKey         string `json:"user_client_key"`
	}
)

// oidc returns the cluster's OIDC configuration, or nil when it has none.
func (cl cmkCluster) oidc() *oidcConfig {
	if cl.AuthConfig == nil || cl.AuthConfig.OIDC == nil || cl.AuthConfig.OIDC.IssuerURL == "" || cl.AuthConfig.OIDC.ClientID == "" {
		return nil
	}
	return cl.AuthConfig.OIDC
}

// useOIDC reports whether the cluster's credential is OIDC under mode.
func useOIDC(mode string, cl cmkCluster) bool {
	return mode != AuthAdminCert && cl.oidc() != nil
}

// notReady reports whether a cluster in state has no API server to reach.
func notReady(state string) bool {
	switch state {
	case "STATE_PROVISIONING", "STATE_DELETING", "STATE_DELETED", "STATE_FAILED":
		return true
	default:
		return false
	}
}

func clustersPath(proj string) string {
	return "/projects/" + url.PathEscape(proj) + "/kubernetes/clusters"
}

// project returns the ID of the account's project: the project setting when
// it is an ID, else the project of that name, else the only project the key
// can see.
func (p *Provider) project(ctx context.Context, c *apiclient.Client, acct provider.Account) (string, error) {
	want := acct.ConfigString(ConfigProject)
	if projectID.MatchString(want) {
		return want, nil
	}
	q := url.Values{"skip_loading_project_resources": {"true"}}
	if want != "" {
		q.Set("project_name", want)
	}
	var resp projectsResponse
	if err := c.Do(ctx, http.MethodGet, "/organizations/projects?"+q.Encode(), nil, &resp); err != nil {
		return "", apiError(err, "the API key's user cannot list projects; set project to the project's ID")
	}
	found := slices.DeleteFunc(resp.Items, func(pr project) bool { return want != "" && pr.Name != want })
	switch {
	case len(found) == 1:
		return found[0].ID, nil
	case want != "" && len(found) == 0:
		return "", provider.Errorf(provider.NotFound, "no project named %q", want).
			WithHint("set project to a project's ID or name from the Projects page of the Crusoe Cloud console")
	case want != "":
		return "", provider.Errorf(provider.InvalidConfig, "%d projects are named %q", len(found), want).
			WithHint("set project to the ID of the one to use")
	case len(found) == 0:
		return "", provider.Errorf(provider.NotFound, "the API key can see no projects").
			WithHint("ask an admin of your Crusoe organisation to add you to a project")
	default:
		names := make([]string, 0, len(found))
		for _, pr := range found {
			names = append(names, fmt.Sprintf("%s (%s)", pr.Name, pr.ID))
		}
		return "", provider.Errorf(provider.InvalidConfig, "the API key can see %d projects", len(found)).
			WithHint("set project to one of " + strings.Join(names, ", "))
	}
}

// List returns the project's CMK clusters, each with the credential it
// uses. Clusters being created or deleted are skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	mode, err := authMode(req.Account)
	if err != nil {
		return nil, err
	}
	proj, err := p.project(ctx, c, req.Account)
	if err != nil {
		return nil, err
	}
	var clusters clustersResponse
	if err := c.Do(ctx, http.MethodGet, clustersPath(proj), nil, &clusters); err != nil {
		return nil, apiError(err, clustersDenied)
	}
	var pools nodePoolsResponse
	if err := c.Do(ctx, http.MethodGet, "/projects/"+url.PathEscape(proj)+"/kubernetes/nodepools", nil, &pools); err != nil {
		return nil, apiError(err, clustersDenied)
	}
	gpu := map[string]bool{}
	for _, np := range pools.Items {
		if gpuType(np.Type) {
			gpu[np.ClusterID] = true
		}
	}
	out := make([]provider.Cluster, 0, len(clusters.Items))
	for _, cl := range clusters.Items {
		if notReady(cl.State) {
			fmt.Fprintf(os.Stderr, "crusoe: skipping %s: it is %s\n", cl.Name, cl.State)
			continue
		}
		oidc := useOIDC(mode, cl)
		if mode == AuthOIDC && !oidc {
			fmt.Fprintf(os.Stderr, "crusoe: skipping %s: it is not set up for OIDC, and auth is %s\n", cl.Name, AuthOIDC)
			continue
		}
		authType := AuthAdminCert
		if oidc {
			authType = AuthOIDC
		}
		// The kubeconfig is where the API server address and CA come
		// from, whichever credential the cluster uses.
		acc, err := p.fetchAccess(ctx, c, proj, cl.ID, authType)
		if err != nil {
			return nil, err
		}
		pc := provider.Cluster{
			ID:      cl.ID,
			Name:    cl.Name,
			Region:  cl.Location,
			Version: cl.Version,
			CAData:  acc.ca,
			Labels:  map[string]string{},
		}
		if cl.Private {
			pc.Endpoints.Private = acc.server
		} else {
			pc.Endpoints.Public = acc.server
		}
		if gpu[cl.ID] {
			pc.Labels["gpu"] = "true"
		}
		// Returning the credential inline lets import and sync replace the
		// cached one, also when the cluster or the auth setting moves
		// between OIDC and the admin certificate.
		var cred provider.Credential
		if oidc {
			cred = oidcCredential(cl)
		} else if cred, err = certCredential(cl.Name, acc); err != nil {
			return nil, err
		}
		pc.Credential = &cred
		out = append(out, pc)
	}
	return out, nil
}

// gpuType reports whether a VM type such as h100-80gb-sxm-ib.8x has GPUs.
func gpuType(t string) bool {
	family, _, _ := strings.Cut(t, ".")
	return family != "" && !cpuFamily.MatchString(family)
}

// Credential returns the cluster's OIDC issuer when it is set up for OIDC,
// and otherwise its admin client certificate.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	mode, err := authMode(req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	proj, err := p.project(ctx, c, req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	var cl cmkCluster
	if err := c.Do(ctx, http.MethodGet, clustersPath(proj)+"/"+url.PathEscape(req.ClusterID), nil, &cl); err != nil {
		return provider.Credential{}, apiError(err, clustersDenied)
	}
	switch {
	case useOIDC(mode, cl):
		return oidcCredential(cl), nil
	case mode == AuthOIDC:
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster %s is not set up for OIDC", cl.Name).
			WithHint("set auth=auto to use its admin certificate, or set up OIDC on the cluster")
	}
	acc, err := p.fetchAccess(ctx, c, proj, req.ClusterID, AuthAdminCert)
	if err != nil {
		return provider.Credential{}, err
	}
	return certCredential(cl.Name, acc)
}

// oidcCredential returns the cluster's OIDC issuer and client, with the
// extra scopes and client secret that Crusoe passes to kubelogin.
func oidcCredential(cl cmkCluster) provider.Credential {
	o := cl.oidc()
	var scopes []string
	split := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	for _, s := range strings.FieldsFunc(cl.KubeloginArgs["oidc-extra-scope"], split) {
		if s != "openid" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	cred := provider.OIDC(o.IssuerURL, o.ClientID, scopes...)
	cred.ClientSecret = cl.KubeloginArgs["oidc-client-secret"]
	if o.CACert != "" {
		fmt.Fprintf(os.Stderr, "crusoe: %s: tug checks the certificate of %s against the system's trusted roots, not the cluster's OIDC ca_cert\n", cl.Name, o.IssuerURL)
	}
	return cred
}

// clusterAccess is what a cluster's kubeconfig says about reaching it.
type clusterAccess struct {
	server    string
	ca        []byte
	cert, key []byte
}

// fetchAccess fetches the cluster's kubeconfig for authType and reads it.
func (p *Provider) fetchAccess(ctx context.Context, c *apiclient.Client, proj, clusterID, authType string) (*clusterAccess, error) {
	path := clustersPath(proj) + "/" + url.PathEscape(clusterID) + "/get-credentials?" + url.Values{"auth_type": {authType}}.Encode()
	denied := adminDenied
	if authType == AuthOIDC {
		denied = clustersDenied
	}
	var resp credentialsResponse
	if err := c.Do(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return nil, apiError(err, denied)
	}
	var acc *clusterAccess
	if resp.KubeConfig != "" {
		var err error
		if acc, err = parseKubeconfig(resp.KubeConfig); err != nil {
			return nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", clusterID, err)
		}
	} else {
		// The legacy form, without a kubeconfig, has the address and PEM
		// data in fields of their own.
		acc = &clusterAccess{
			server: resp.ClusterAddress,
			ca:     pemData(resp.ClusterCACertificate),
			cert:   pemData(resp.UserClientCertificate),
			key:    pemData(resp.UserClientKey),
		}
	}
	if acc.server == "" {
		return nil, provider.Errorf(provider.Internal, "the credentials of cluster %s have no API server address", clusterID)
	}
	acc.server = endpointURL(acc.server)
	return acc, nil
}

// parseKubeconfig reads the server, CA and client certificate of the
// kubeconfig's current context, or of its first entries.
func parseKubeconfig(raw string) (*clusterAccess, error) {
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
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			Name string `json:"name"`
			User struct {
				CertData string `json:"client-certificate-data"`
				KeyData  string `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal([]byte(raw), &kc); err != nil {
		return nil, err
	}
	if len(kc.Clusters) == 0 {
		return nil, errors.New("it has no clusters")
	}
	clusterName, userName := kc.Clusters[0].Name, ""
	if len(kc.Users) > 0 {
		userName = kc.Users[0].Name
	}
	for _, kctx := range kc.Contexts {
		if kctx.Name == kc.CurrentContext {
			clusterName, userName = kctx.Context.Cluster, kctx.Context.User
		}
	}
	acc := &clusterAccess{}
	for _, cl := range kc.Clusters {
		if cl.Name == clusterName {
			acc.server = cl.Cluster.Server
			acc.ca, _ = base64.StdEncoding.DecodeString(cl.Cluster.CAData)
		}
	}
	for _, u := range kc.Users {
		if u.Name == userName {
			acc.cert, _ = base64.StdEncoding.DecodeString(u.User.CertData)
			acc.key, _ = base64.StdEncoding.DecodeString(u.User.KeyData)
		}
	}
	return acc, nil
}

// pemData returns PEM text as bytes, decoding it first if it came base64
// encoded.
func pemData(s string) []byte {
	if s == "" {
		return nil
	}
	if !strings.Contains(s, "-----BEGIN") {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil && strings.Contains(string(b), "-----BEGIN") {
			return b
		}
	}
	return []byte(s)
}

// certCredential returns the admin client certificate, which tug uses until
// shortly before the certificate expires.
func certCredential(cluster string, acc *clusterAccess) (provider.Credential, error) {
	if len(acc.cert) == 0 || len(acc.key) == 0 {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s has no client certificate", cluster)
	}
	expires := provider.NoExpiry
	if block, _ := pem.Decode(acc.cert); block != nil {
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			expires = cert.NotAfter
		}
	}
	return provider.ClientCert(acc.cert, acc.key, expires), nil
}

// endpointURL adds https:// to a bare host name.
func endpointURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return s
	}
	return "https://" + strings.TrimPrefix(s, "//")
}
