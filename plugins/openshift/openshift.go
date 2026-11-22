// Package openshift is tug's first-party provider plugin for the OpenShift
// clusters that Red Hat OpenShift Cluster Manager (OCM) runs, such as Red
// Hat OpenShift Service on AWS (ROSA) and OpenShift Dedicated (OSD). It
// exchanges the account's offline token at Red Hat's single sign-on for an
// access token, as the ocm and rosa CLIs do, and lists clusters with the
// OCM API.
//
// A cluster that authenticates users with an external OIDC issuer gets an
// oidc credential for that issuer, so that tug signs the user in itself.
// The others authenticate through OpenShift's own OAuth server, whose
// tokens are opaque and which has no exec credential plugin. For those the
// plugin uses the token that `oc login --web` keeps in the user's
// kubeconfig, and asks the cluster when it expires.
package openshift

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
)

// Name and version of the plugin.
const (
	Name    = "openshift"
	Version = "0.1.1"
)

// Endpoints of Red Hat's services.
const (
	// DefaultAPIURL is the OCM API.
	DefaultAPIURL = "https://api.openshift.com"
	// DefaultTokenURL is the token endpoint of Red Hat's single sign-on.
	DefaultTokenURL = "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/token"
)

// ssoClientID is the public client that OCM's own tools, ocm and rosa,
// exchange offline tokens with.
const ssoClientID = "cloud-services"

// SecretOfflineToken is the OCM offline token from
// https://console.redhat.com/openshift/token.
const SecretOfflineToken = "offline_token"

// Account settings, besides apiURL.
const (
	// ConfigTokenURL overrides DefaultTokenURL.
	ConfigTokenURL = "tokenURL"
	// ConfigSearch narrows the listing with an OCM search expression, such
	// as product.id = 'rosa'.
	ConfigSearch = "search"
	// ConfigOIDCClientID is the client tug signs in with on clusters that
	// use an external OIDC issuer, when the cluster does not name one for
	// the CLI.
	ConfigOIDCClientID = "oidcClientID"
	// ConfigOIDCScopes adds scopes, comma or space separated, to those
	// sign-ins.
	ConfigOIDCScopes = "oidcScopes"
)

// InstallHint tells users where to get oc.
const InstallHint = "install the OpenShift CLI, oc, from https://console.redhat.com/openshift/downloads"

const (
	tokenHint = "get an offline token at https://console.redhat.com/openshift/token and give it to tug; an offline token lapses after 30 days without use"
	// clustersPath is the OCM collection of clusters.
	clustersPath = "/api/clusters_mgmt/v1/clusters"
	// defaultPageSize is how many clusters each page of the listing asks for.
	defaultPageSize = 100
	// fallbackLifetime is how long the plugin trusts an oc token whose
	// expiry the cluster does not say.
	fallbackLifetime = time.Hour
	// oauthTokenPrefix starts the tokens of OpenShift's OAuth server.
	oauthTokenPrefix = "sha256~"
)

// Provider implements the plugin.
type Provider struct {
	http *http.Client
	// now returns the current time; tests replace it.
	now func() time.Time
	// pageSize is how many clusters to ask for at a time; tests lower it.
	pageSize int

	// mu guards tokens, the access tokens exchanged so far, so that one
	// serve process exchanges an offline token once while it lasts.
	mu     sync.Mutex
	tokens map[string]accessToken
}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider {
	return &Provider{now: time.Now, pageSize: defaultPageSize, tokens: map[string]accessToken{}}
}

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "OpenShift clusters in Red Hat OpenShift Cluster Manager: ROSA and OpenShift Dedicated",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets: []provider.SecretSpec{{
			Name:   SecretOfflineToken,
			Prompt: "OpenShift Cluster Manager offline token (https://console.redhat.com/openshift/token)",
		}},
	}
}

// accessToken is a short-lived OCM access token.
type accessToken struct {
	token   string
	expires time.Time
}

// tokenResponse is the token endpoint's answer, or its OAuth 2.0 error.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// offlineToken returns the account's offline token. Offline tokens are
// JWTs, so checking the shape catches anything else pasted in their place
// without asking Red Hat.
func offlineToken(secrets map[string]string) (string, error) {
	offline := strings.TrimSpace(secrets[SecretOfflineToken])
	if offline == "" {
		return "", provider.Errorf(provider.Unauthenticated, "no %s", SecretOfflineToken).WithHint(tokenHint)
	}
	if strings.Count(offline, ".") != 2 {
		return "", provider.Errorf(provider.Unauthenticated, "the %s is not an offline token", SecretOfflineToken).WithHint(tokenHint)
	}
	return offline, nil
}

// accessToken exchanges the offline token for an access token with the
// refresh token grant, or reuses one from earlier that is still valid.
func (p *Provider) accessToken(ctx context.Context, acct provider.Account, offline string) (string, error) {
	tokenURL, err := apiclient.CheckURL(ConfigTokenURL, cmp.Or(acct.ConfigString(ConfigTokenURL), DefaultTokenURL))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(offline))
	key := tokenURL + " " + hex.EncodeToString(sum[:])
	p.mu.Lock()
	cached, ok := p.tokens[key]
	p.mu.Unlock()
	if ok && p.now().Add(time.Minute).Before(cached.expires) {
		return cached.token, nil
	}

	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {ssoClientID}, "refresh_token": {offline}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tug-provider-openshift/"+Version)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", provider.Errorf(provider.Unavailable, "exchanging the offline token: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", provider.Errorf(provider.Unavailable, "reading the token response: %v", err)
	}
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr) // an error page is reported by status below
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		msg := cmp.Or(tr.ErrorDescription, tr.Error, strings.TrimSpace(string(body)), resp.Status)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		switch tr.Error {
		// Keycloak answers 400 invalid_grant for an offline token that is
		// unknown, revoked or lapsed.
		case "invalid_grant", "invalid_client", "unauthorized_client":
			return "", provider.Errorf(provider.Unauthenticated, "Red Hat single sign-on refused the offline token: %s", msg).WithHint(tokenHint)
		}
		err := provider.StatusError(resp.StatusCode, "exchanging the offline token: "+msg, resp.Header)
		if err.Code == provider.Unauthenticated {
			return "", err.WithHint(tokenHint)
		}
		return "", err
	}
	if tr.AccessToken == "" {
		return "", provider.Errorf(provider.Internal, "Red Hat single sign-on returned no access token")
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	p.mu.Lock()
	p.tokens[key] = accessToken{token: tr.AccessToken, expires: p.now().Add(lifetime)}
	p.mu.Unlock()
	return tr.AccessToken, nil
}

func (p *Provider) httpClient() *http.Client {
	if p.http != nil {
		return p.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// client returns an OCM API client with a fresh access token. The secret
// and settings are checked before anything goes over the network.
func (p *Provider) client(ctx context.Context, acct provider.Account, secrets map[string]string) (*apiclient.Client, error) {
	offline, err := offlineToken(secrets)
	if err != nil {
		return nil, err
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	token, err := p.accessToken(ctx, acct, offline)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Token:     token,
		HTTP:      p.http,
		UserAgent: "tug-provider-openshift/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads OCM's {"kind": "Error", "code": ..., "reason": ...}
// bodies.
func errorMessage(body []byte) string {
	var e struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return cmp.Or(e.Reason, e.Code)
}

// apiError adds the next step to the OCM errors a user can fix.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint(tokenHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint("your Red Hat account cannot see these clusters; ask an administrator of your organisation for access in OpenShift Cluster Manager")
	case provider.RateLimited:
		return provider.AsError(err).WithHint("wait a minute, then run tug sync")
	default:
		return err
	}
}

// OCM API types, trimmed to the fields the plugin uses.
type (
	cluster struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		State            string `json:"state"`
		OpenshiftVersion string `json:"openshift_version"`
		API              struct {
			URL       string `json:"url"`
			Listening string `json:"listening"`
		} `json:"api"`
		Region        link `json:"region"`
		CloudProvider link `json:"cloud_provider"`
		Product       link `json:"product"`
		Version       struct {
			RawID string `json:"raw_id"`
		} `json:"version"`
		Hypershift struct {
			Enabled bool `json:"enabled"`
		} `json:"hypershift"`
		ExternalAuthConfig struct {
			Enabled bool `json:"enabled"`
			// State is "enabled" on ARO HCP clusters, which do not set
			// Enabled.
			State string `json:"state"`
		} `json:"external_auth_config"`
	}
	link struct {
		ID string `json:"id"`
	}
	clusterList struct {
		Items []cluster `json:"items"`
		Total int       `json:"total"`
	}
	externalAuth struct {
		ID     string `json:"id"`
		Issuer struct {
			URL       string   `json:"url"`
			Audiences []string `json:"audiences"`
			CA        string   `json:"ca"`
		} `json:"issuer"`
		Clients []authClient `json:"clients"`
		Claim   struct {
			Mappings struct {
				Username struct {
					Claim string `json:"claim"`
				} `json:"username"`
			} `json:"mappings"`
		} `json:"claim"`
	}
	authClient struct {
		ID          string   `json:"id"`
		Secret      string   `json:"secret"`
		ExtraScopes []string `json:"extra_scopes"`
		Component   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"component"`
	}
	externalAuthList struct {
		Items []externalAuth `json:"items"`
	}
)

// externalAuth reports whether the cluster authenticates users with an
// external OIDC issuer rather than OpenShift's OAuth server.
func (cl cluster) externalAuth() bool {
	return cl.ExternalAuthConfig.Enabled || cl.ExternalAuthConfig.State == "enabled"
}

// listClusters reads every page of the listing.
func (p *Provider) listClusters(ctx context.Context, c *apiclient.Client, search string) ([]cluster, error) {
	size := cmp.Or(p.pageSize, defaultPageSize)
	var out []cluster
	for page := 1; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "size": {strconv.Itoa(size)}}
		if search != "" {
			q.Set("search", search)
		}
		var resp clusterList
		if err := c.Do(ctx, http.MethodGet, clustersPath+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if len(resp.Items) < size || len(out) >= resp.Total {
			return out, nil
		}
	}
}

// List returns the clusters the offline token's user can see that have an
// API server to reach.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	c, err := p.client(ctx, req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	clusters, err := p.listClusters(ctx, c, req.Account.ConfigString(ConfigSearch))
	if err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return nil, provider.AsError(err).WithHint(fmt.Sprintf("check that %s is the OpenShift Cluster Manager API", c.BaseURL))
		}
		return nil, apiError(err)
	}
	out := make([]provider.Cluster, 0, len(clusters))
	for _, cl := range clusters {
		switch {
		case cl.State == "uninstalling":
			fmt.Fprintf(os.Stderr, "openshift: skipping %s: it is being uninstalled\n", cl.Name)
			continue
		case cl.API.URL == "":
			fmt.Fprintf(os.Stderr, "openshift: skipping %s: it is %s and has no API server yet\n", cl.Name, cmp.Or(cl.State, "not ready"))
			continue
		}
		pc := provider.Cluster{
			ID:   cl.ID,
			Name: cmp.Or(cl.Name, cl.ID),
			// OCM gives the OpenShift version, not Kubernetes'.
			Version: cmp.Or(cl.OpenshiftVersion, cl.Version.RawID),
			Region:  cl.Region.ID,
			Labels:  clusterLabels(cl),
		}
		if cl.API.Listening == "internal" {
			pc.Endpoints.Private = cl.API.URL
		} else {
			pc.Endpoints.Public = cl.API.URL
		}
		if cl.externalAuth() {
			// The OIDC credential is a sign-in to make, not a secret.
			// Returning it here lets import and sync replace a cached one
			// when the issuer or client changes.
			cred, err := p.oidcCredential(ctx, c, req.Account, cl)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "openshift: listing %s without a credential: %v\n", cl.Name, err)
			} else {
				pc.Credential = &cred
			}
		}
		out = append(out, pc)
	}
	return out, nil
}

// clusterLabels describes the cluster's product (rosa, osd, ...), cloud and
// whether its control plane is hosted.
func clusterLabels(cl cluster) map[string]string {
	labels := map[string]string{}
	if cl.Product.ID != "" {
		labels["product"] = strings.ToLower(cl.Product.ID)
	}
	if cl.CloudProvider.ID != "" {
		labels["cloud"] = strings.ToLower(cl.CloudProvider.ID)
	}
	if cl.Hypershift.Enabled {
		labels["hcp"] = "true"
	}
	return labels
}

// Credential returns an OIDC sign-in for a cluster with an external issuer,
// and otherwise the token oc keeps for the cluster's OAuth server.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	c, err := p.client(ctx, req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	var cl cluster
	if err := c.Do(ctx, http.MethodGet, clustersPath+"/"+url.PathEscape(req.ClusterID), nil, &cl); err != nil {
		if provider.CodeOf(err) == provider.NotFound {
			return provider.Credential{}, provider.AsError(err).WithHint("the cluster is gone from OpenShift Cluster Manager, or you can no longer see it; run tug sync")
		}
		return provider.Credential{}, apiError(err)
	}
	if cl.externalAuth() {
		return p.oidcCredential(ctx, c, req.Account, cl)
	}
	if cl.API.URL == "" {
		return provider.Credential{}, provider.Errorf(provider.Unavailable, "cluster %s has no API server; it is %s", cl.Name, cl.State)
	}
	return p.ocToken(ctx, cl.API.URL)
}

// oidcCredential returns the sign-in for a cluster with an external OIDC
// issuer: the issuer, the client the cluster names for oc (or the
// oidcClientID setting), and the scopes that client and the username claim
// need.
func (p *Provider) oidcCredential(ctx context.Context, c *apiclient.Client, acct provider.Account, cl cluster) (provider.Credential, error) {
	var resp externalAuthList
	path := clustersPath + "/" + url.PathEscape(cl.ID) + "/external_auth_config/external_auths"
	if err := c.Do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return provider.Credential{}, apiError(err)
	}
	// A cluster has at most one external authentication provider.
	if len(resp.Items) == 0 || resp.Items[0].Issuer.URL == "" {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster %s uses external authentication but has no provider", cl.Name).
			WithHint(fmt.Sprintf("add one with `rosa create external-auth-provider --cluster=%s`", cl.Name))
	}
	ea := resp.Items[0]
	client, err := cliClient(acct, cl.Name, ea)
	if err != nil {
		return provider.Credential{}, err
	}
	var scopes []string
	add := func(s string) {
		if s != "" && s != "openid" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	for _, s := range client.ExtraScopes {
		add(s)
	}
	add(claimScope(ea.Claim.Mappings.Username.Claim))
	for s := range strings.FieldsFuncSeq(acct.ConfigString(ConfigOIDCScopes), func(r rune) bool { return r == ',' || r == ' ' }) {
		add(s)
	}
	if ea.Issuer.CA != "" {
		fmt.Fprintf(os.Stderr, "openshift: %s: tug checks the certificate of %s against the system's trusted roots, not the issuer CA the cluster has\n", cl.Name, ea.Issuer.URL)
	}
	cred := provider.OIDC(ea.Issuer.URL, client.ID, scopes...)
	cred.ClientSecret = client.Secret
	return cred, nil
}

// cliClient picks the client to sign in with: the oidcClientID setting, or
// the client the cluster configures for the CLI, or the issuer's only
// audience that no other client of the cluster uses. The issuer accepts
// tokens only for its audiences, and every client's ID is among them.
func cliClient(acct provider.Account, name string, ea externalAuth) (authClient, error) {
	if id := acct.ConfigString(ConfigOIDCClientID); id != "" {
		for _, c := range ea.Clients {
			if c.ID == id {
				return c, nil
			}
		}
		return authClient{ID: id}, nil
	}
	for _, c := range ea.Clients {
		if c.Component.Name == "cli" {
			return c, nil
		}
	}
	var free []string
	for _, aud := range ea.Issuer.Audiences {
		if !slices.ContainsFunc(ea.Clients, func(c authClient) bool { return c.ID == aud }) {
			free = append(free, aud)
		}
	}
	if len(free) == 1 {
		return authClient{ID: free[0]}, nil
	}
	hint := "set oidcClientID to the client ID registered for oc in your identity provider; it must be one of the issuer's audiences"
	if len(free) > 1 {
		hint += ": " + strings.Join(free, ", ")
	}
	return authClient{}, provider.Errorf(provider.InvalidConfig, "cannot tell which OIDC client of %s to sign in to %s with", ea.Issuer.URL, name).WithHint(hint)
}

// claimScope returns the standard OpenID Connect scope that puts claim in
// the ID token, or "".
func claimScope(claim string) string {
	switch claim {
	case "email", "email_verified":
		return "email"
	case "preferred_username", "name", "given_name", "family_name", "middle_name", "nickname":
		return "profile"
	default:
		return ""
	}
}

// kubeconfig is the merged kubeconfig that oc config view prints, trimmed
// to what finds a token for a server.
type kubeconfig struct {
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
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token string `json:"token"`
		} `json:"user"`
	} `json:"users"`
}

// tokenFor returns a context whose cluster is server and whose user has a
// token, and that token. It prefers the current context, and otherwise
// takes the first, in the order oc config view prints them.
func (kc kubeconfig) tokenFor(server string) (contextName, token string) {
	want := normaliseServer(server)
	servers := map[string]string{}
	for _, c := range kc.Clusters {
		servers[c.Name] = normaliseServer(c.Cluster.Server)
	}
	tokens := map[string]string{}
	for _, u := range kc.Users {
		tokens[u.Name] = u.User.Token
	}
	for _, c := range kc.Contexts {
		if servers[c.Context.Cluster] != want || tokens[c.Context.User] == "" {
			continue
		}
		if contextName == "" || c.Name == kc.CurrentContext {
			contextName, token = c.Name, tokens[c.Context.User]
		}
	}
	return contextName, token
}

// normaliseServer makes API server URLs comparable: lower-case scheme and
// host, an explicit port, and no trailing slash.
func normaliseServer(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = "443"
		if scheme == "http" {
			port = "80"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port + strings.TrimSuffix(u.EscapedPath(), "/")
}

// oauthToken is a UserOAuthAccessToken, trimmed to when it expires.
type oauthToken struct {
	Metadata struct {
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	// ExpiresIn is in seconds from creation; 0 means it does not expire.
	ExpiresIn int64 `json:"expiresIn"`
	// InactivityTimeoutSeconds is in seconds from creation too, and grows
	// as the token is used.
	InactivityTimeoutSeconds int64 `json:"inactivityTimeoutSeconds"`
}

// ocToken returns the token oc keeps for the API server, valid until the
// cluster's OAuth server says it expires. Reading when it expires also
// checks that the cluster still accepts it.
func (p *Provider) ocToken(ctx context.Context, server string) (provider.Credential, error) {
	login := loginHint(server)
	out, err := oc(ctx, "config", "view", "--raw", "-o", "json")
	if err != nil {
		return provider.Credential{}, err
	}
	var kc kubeconfig
	if err := json.Unmarshal(out, &kc); err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "reading oc config view: %v", err)
	}
	contextName, token := kc.tokenFor(server)
	if token == "" {
		return provider.Credential{}, provider.Errorf(provider.Unauthenticated, "oc has no token for %s", server).WithHint(login)
	}
	if !strings.HasPrefix(token, oauthTokenPrefix) {
		// Not from the OAuth server, such as a service account token given
		// to oc login --token, so the cluster cannot say when it expires.
		fmt.Fprintf(os.Stderr, "openshift: the token of context %s is not an OAuth token; using it for %s\n", contextName, fallbackLifetime)
		return provider.Token(token, p.now().Add(fallbackLifetime)), nil
	}
	out, err = oc(ctx, "get", "useroauthaccesstokens", tokenObjectName(token), "-o", "json", "--context="+contextName)
	switch {
	case provider.CodeOf(err) == provider.NotFound:
		fmt.Fprintf(os.Stderr, "openshift: the cluster does not say when the token of context %s expires; using it for %s\n", contextName, fallbackLifetime)
		return provider.Token(token, p.now().Add(fallbackLifetime)), nil
	case provider.CodeOf(err) == provider.Unauthenticated:
		return provider.Credential{}, provider.AsError(err).WithHint("the token oc keeps has expired or was revoked; " + login)
	case err != nil:
		return provider.Credential{}, err
	}
	var t oauthToken
	if err := json.Unmarshal(out, &t); err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "reading the OAuth token: %v", err)
	}
	return p.oauthCredential(token, t, server)
}

// oauthCredential returns token until t, its UserOAuthAccessToken, says it
// expires.
func (p *Provider) oauthCredential(token string, t oauthToken, server string) (provider.Credential, error) {
	now := p.now()
	created := t.Metadata.CreationTimestamp
	expires := now.Add(fallbackLifetime)
	if !created.IsZero() {
		if t.ExpiresIn > 0 {
			expires = created.Add(time.Duration(t.ExpiresIn) * time.Second)
		}
		if idle := created.Add(time.Duration(t.InactivityTimeoutSeconds) * time.Second); t.InactivityTimeoutSeconds > 0 && idle.Before(expires) {
			expires = idle
		}
	}
	if !expires.After(now) {
		return provider.Credential{}, provider.Errorf(provider.Unauthenticated, "the token oc keeps for %s expired at %s", server, expires.Format(time.RFC3339)).
			WithHint(loginHint(server))
	}
	return provider.Token(token, expires), nil
}

// tokenObjectName is the name of the OAuthAccessToken that stands for an
// OAuth token: its prefix and the unpadded base64url SHA-256 of the rest,
// as OpenShift's oauth-apiserver and oc logout compute it.
func tokenObjectName(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimPrefix(token, oauthTokenPrefix)))
	return oauthTokenPrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

func loginHint(server string) string {
	return fmt.Sprintf("sign in with `oc login --web %s`; tug then uses the token oc keeps", server)
}

// oc runs oc and returns what it printed on stdout. oc gets no standard
// input, so it fails rather than waiting for an answer. A failure gets the
// code that oc's message names.
func oc(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "oc", args...)
	var stderr bytes.Buffer
	// oc's warnings go to the plugin's stderr too, which tug logs.
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	out, err := cmd.Output()
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, exec.ErrNotFound):
		return nil, provider.Errorf(provider.Unavailable, "oc is not installed or not on PATH").WithHint(InstallHint)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("running oc %s: %w", args[0], ctx.Err())
	}
	msg := toolMessage(stderr.Bytes(), err)
	return nil, provider.Errorf(ocCode(msg), "oc %s %s: %s", args[0], args[1], msg)
}

// ocCode returns the code for oc's error message, which names the API
// server's status.
func ocCode(msg string) provider.Code {
	switch {
	case strings.Contains(msg, "(Unauthorized)"):
		return provider.Unauthenticated
	case strings.Contains(msg, "(Forbidden)"):
		return provider.PermissionDenied
	case strings.Contains(msg, "(NotFound)"), strings.Contains(msg, "the server doesn't have a resource type"):
		return provider.NotFound
	case strings.Contains(msg, "(TooManyRequests)"):
		return provider.RateLimited
	default:
		return provider.Unavailable
	}
}

// toolMessage returns the reason oc gave for failing: its error line, or
// else the last line it printed, or else how it exited.
func toolMessage(stderr []byte, err error) string {
	var last string
	for line := range strings.Lines(string(stderr)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "error: ") || strings.HasPrefix(line, "Error from server") {
			return line
		}
		if line != "" {
			last = line
		}
	}
	return cmp.Or(last, err.Error())
}
