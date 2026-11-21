// Package ibm is tug's first-party provider plugin for IBM Cloud Kubernetes
// Service (IKS) and Red Hat OpenShift on IBM Cloud. It exchanges the
// account's API key for IAM tokens at iam.cloud.ibm.com and lists clusters
// with the Kubernetes Service API. Kubernetes clusters accept the IAM ID
// token that IAM issues to its kube client, so that is their credential,
// with the CA from the kubeconfig the API generates. OpenShift clusters take
// a token from their own OAuth server, which accepts the API key as
// `oc login -u apikey` sends it.
package ibm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
	"github.com/paingha/tugcli/plugins/internal/kubeconfig"
)

// Name and version of the plugin.
const (
	Name    = "ibm"
	Version = "0.1.0"
)

// Default endpoints: the global Kubernetes Service API and IAM.
const (
	DefaultAPIURL = "https://containers.cloud.ibm.com/global"
	DefaultIAMURL = "https://iam.cloud.ibm.com"
)

// SecretAPIKey is the IBM Cloud API key of a user or service ID.
const SecretAPIKey = "api_key"

// Account settings.
const (
	// ConfigIAMURL points the plugin at another IAM endpoint.
	ConfigIAMURL = "iamURL"
	// ConfigResourceGroup is the ID of the resource group to list
	// clusters in. Without it, the plugin lists every cluster the API key
	// can see.
	ConfigResourceGroup = "resourceGroup"
)

// IAM clients. The Kubernetes Service API documents the bx client for its
// own calls and the kube client for tokens that clusters accept; neither
// has a secret.
var (
	apiClient  = "Basic " + base64.StdEncoding.EncodeToString([]byte("bx:bx"))
	kubeClient = "Basic " + base64.StdEncoding.EncodeToString([]byte("kube:kube"))
)

// apiKeyGrant is IAM's grant type for API keys.
const apiKeyGrant = "urn:ibm:params:oauth:grant-type:apikey"

// openShiftClient is the OpenShift OAuth client for clients that answer
// WWW-Authenticate challenges, which takes basic credentials.
const openShiftClient = "openshift-challenging-client"

// tokenLifetime is how long IAM tokens last when the response does not say.
const tokenLifetime = time.Hour

// Hints for errors a person can fix.
const (
	keyHint    = "create an API key with `ibmcloud iam api-key-create` or under Manage > Access (IAM) > API keys in the IBM Cloud console; IBM Cloud shows it only once"
	deniedHint = "the API key's identity needs at least the Viewer platform role for Kubernetes Service (containers-kubernetes) in IAM"
	rateHint   = "wait a minute, then run tug sync"
)

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
		Description:  "IBM Cloud Kubernetes Service and Red Hat OpenShift on IBM Cloud",
		Capabilities: provider.CapList | provider.CapCredential,
		Secrets:      []provider.SecretSpec{{Name: SecretAPIKey, Prompt: "IBM Cloud API key"}},
	}
}

func (p *Provider) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// tokens is IAM's answer to a token request.
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	// Expiration is when the access token expires, in Unix seconds.
	Expiration int64 `json:"expiration"`
}

// token exchanges the API key for tokens issued to an IAM client.
func (p *Provider) token(ctx context.Context, acct provider.Account, apiKey, client string) (*tokens, error) {
	base := DefaultIAMURL
	if raw := acct.ConfigString(ConfigIAMURL); raw != "" {
		var err error
		if base, err = apiclient.CheckURL(ConfigIAMURL, raw); err != nil {
			return nil, err
		}
	}
	c := &apiclient.Client{
		BaseURL: base,
		Sign: func(req *http.Request) error {
			req.Header.Set("Authorization", client)
			return nil
		},
		HTTP:      p.http,
		UserAgent: "tug-provider-ibm/" + Version,
		Message:   iamMessage,
		Code:      iamCode,
	}
	form := url.Values{"grant_type": {apiKeyGrant}, "apikey": {apiKey}}
	body := apiclient.RawBody{ContentType: "application/x-www-form-urlencoded", Data: []byte(form.Encode())}
	var t tokens
	if err := c.Do(ctx, http.MethodPost, "/identity/token", body, &t); err != nil {
		return nil, apiError(err)
	}
	if t.AccessToken == "" {
		return nil, provider.Errorf(provider.Internal, "IAM returned no access token")
	}
	return &t, nil
}

// iamMessage reads {"errorCode": "BXNIM0415E", "errorMessage": "..."}
// bodies.
func iamMessage(body []byte) string {
	var e struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(body, &e) != nil || e.ErrorMessage == "" {
		return ""
	}
	if e.ErrorCode == "" {
		return e.ErrorMessage
	}
	return e.ErrorCode + ": " + e.ErrorMessage
}

// iamCode treats a 400 from the token endpoint as a bad API key: IAM
// answers an unknown or malformed key with 400 and BXNIM0415E, and the key
// is the only parameter a user chooses.
func iamCode(status int, _ []byte) provider.Code {
	if status == http.StatusBadRequest {
		return provider.Unauthenticated
	}
	return ""
}

// apiMessage reads the Kubernetes Service API's ErrorResponse bodies.
func apiMessage(body []byte) string {
	var e struct {
		Code        string `json:"code"`
		Description string `json:"description"`
		Terse       string `json:"terseDescription"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	msg := e.Description
	if msg == "" {
		msg = e.Terse
	}
	if msg == "" || e.Code == "" {
		return msg
	}
	return e.Code + ": " + msg
}

// apiError adds hints to the errors a person can fix.
func apiError(err error) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint("IBM Cloud did not accept the API key; " + keyHint)
	case provider.PermissionDenied:
		return provider.AsError(err).WithHint(deniedHint)
	case provider.RateLimited:
		return provider.AsError(err).WithHint(rateHint)
	default:
		return err
	}
}

// session is an account's API client and the API key it was made with.
type session struct {
	api    *apiclient.Client
	apiKey string
	acct   provider.Account
}

// session exchanges the API key for tokens and returns a client for the
// Kubernetes Service API that sends them as the API reference says.
func (p *Provider) session(ctx context.Context, acct provider.Account, secrets map[string]string) (*session, error) {
	apiKey := secrets[SecretAPIKey]
	if apiKey == "" {
		return nil, provider.Errorf(provider.Unauthenticated, "no %s", SecretAPIKey).WithHint(keyHint)
	}
	base, err := apiclient.BaseURL(acct, DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	t, err := p.token(ctx, acct, apiKey, apiClient)
	if err != nil {
		return nil, err
	}
	group := acct.ConfigString(ConfigResourceGroup)
	api := &apiclient.Client{
		BaseURL: base,
		Sign: func(req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+t.AccessToken)
			if t.RefreshToken != "" {
				req.Header.Set("X-Auth-Refresh-Token", t.RefreshToken)
			}
			if group != "" {
				req.Header.Set("X-Auth-Resource-Group", group)
			}
			return nil
		},
		HTTP:      p.http,
		UserAgent: "tug-provider-ibm/" + Version,
		Message:   apiMessage,
	}
	return &session{api: api, apiKey: apiKey, acct: acct}, nil
}

// Kubernetes Service API types, trimmed to the fields the plugin uses.
type (
	serviceEndpoints struct {
		PrivateServiceEndpointURL string `json:"privateServiceEndpointURL"`
		PublicServiceEndpointURL  string `json:"publicServiceEndpointURL"`
	}
	ksCluster struct {
		ID                string           `json:"id"`
		Name              string           `json:"name"`
		Region            string           `json:"region"`
		MasterKubeVersion string           `json:"masterKubeVersion"`
		MasterURL         string           `json:"masterURL"`
		Provider          string           `json:"provider"`
		ResourceGroupName string           `json:"resourceGroupName"`
		ServiceEndpoints  serviceEndpoints `json:"serviceEndpoints"`
		State             string           `json:"state"`
		Type              string           `json:"type"`
	}
)

// openShift reports whether the cluster runs Red Hat OpenShift.
func (c ksCluster) openShift() bool { return strings.EqualFold(c.Type, "openshift") }

// endpoints returns the cluster's public and private master URLs. masterURL
// is the public one when the public endpoint is enabled.
func (c ksCluster) endpoints() provider.Endpoints {
	e := provider.Endpoints{
		Public:  c.ServiceEndpoints.PublicServiceEndpointURL,
		Private: c.ServiceEndpoints.PrivateServiceEndpointURL,
	}
	if e.Public == "" && c.MasterURL != "" && c.MasterURL != e.Private {
		e.Public = c.MasterURL
	}
	return e
}

// server is the endpoint tug connects to.
func (c ksCluster) server() string {
	e := c.endpoints()
	if e.Public != "" {
		return e.Public
	}
	return e.Private
}

// notReady reports whether a cluster in state has no master to reach: it
// is being ordered, its master is being deployed, or it is going away.
func notReady(state string) bool {
	switch strings.ToLower(state) {
	case "requested", "deploying", "deploy_failed", "aborted", "deleting", "deleted", "delete_failed":
		return true
	default:
		return false
	}
}

// version returns the version in a masterKubeVersion such as 1.31.4_1530
// or 4.16.21_1544_openshift.
func version(v string) string {
	v, _, _ = strings.Cut(v, "_")
	return v
}

// listClusters lists the classic and VPC clusters the API key can see.
func listClusters(ctx context.Context, s *session) ([]ksCluster, error) {
	var out []ksCluster
	seen := map[string]bool{}
	for _, path := range []string{"/v2/classic/getClusters", "/v2/vpc/getClusters?provider=vpc-gen2"} {
		var page []ksCluster
		if err := s.api.Do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, apiError(err)
		}
		for _, cl := range page {
			if cl.ID != "" && !seen[cl.ID] {
				seen[cl.ID] = true
				out = append(out, cl)
			}
		}
	}
	return out, nil
}

// List returns the account's IKS and OpenShift clusters. Clusters whose
// master is not deployed yet, or that are being deleted, are skipped.
// Kubernetes clusters come with their CA, from the kubeconfig the API
// generates, which also maps the API key's IAM roles to RBAC in the
// cluster, and with an IAM ID token inline.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	s, err := p.session(ctx, req.Account, req.Secrets)
	if err != nil {
		return nil, err
	}
	clusters, err := listClusters(ctx, s)
	if err != nil {
		return nil, err
	}
	var idToken *provider.Credential
	out := make([]provider.Cluster, 0, len(clusters))
	for _, cl := range clusters {
		if notReady(cl.State) || cl.server() == "" {
			fmt.Fprintf(os.Stderr, "ibm: skipping %s: its master is not ready (state %s)\n", cl.Name, cl.State)
			continue
		}
		pc := provider.Cluster{
			ID:        cl.ID,
			Name:      cl.Name,
			Region:    cl.Region,
			Version:   version(cl.MasterKubeVersion),
			Endpoints: cl.endpoints(),
			Labels:    labels(cl),
		}
		if cl.openShift() {
			// The OAuth server mints a token per request, so the plugin
			// asks for one only when kubectl needs it.
			out = append(out, pc)
			continue
		}
		acc, err := clusterConfig(ctx, s, cl.ID)
		switch {
		case err == nil:
			pc.CAData = acc.CAData
		case provider.CodeOf(err) == provider.NotFound || provider.CodeOf(err) == provider.Unavailable:
			fmt.Fprintf(os.Stderr, "ibm: %s has no kubeconfig yet (state %s); run tug sync when it is ready\n", cl.Name, cl.State)
		default:
			return nil, err
		}
		if idToken == nil {
			c, err := p.idToken(ctx, s)
			if err != nil {
				return nil, err
			}
			idToken = &c
		}
		cred := *idToken
		pc.Credential = &cred
		out = append(out, pc)
	}
	return out, nil
}

// labels marks OpenShift clusters and names the resource group and
// infrastructure.
func labels(cl ksCluster) map[string]string {
	l := map[string]string{}
	if cl.openShift() {
		l["openshift"] = "true"
	}
	if cl.ResourceGroupName != "" {
		l["resource-group"] = cl.ResourceGroupName
	}
	if cl.Provider != "" {
		l["infrastructure"] = cl.Provider
	}
	return l
}

// clusterConfig fetches the cluster's kubeconfig as JSON. Fetching it
// applies the identity's IAM roles to the cluster as RBAC.
func clusterConfig(ctx context.Context, s *session, id string) (*kubeconfig.Access, error) {
	resp, err := s.api.Send(ctx, http.MethodGet, "/v2/getKubeconfig?"+url.Values{"cluster": {id}, "format": {"json"}}.Encode(), nil)
	if err != nil {
		return nil, apiError(err)
	}
	acc, err := kubeconfig.Parse(resp.Body)
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", id, err)
	}
	return acc, nil
}

// idToken returns the IAM ID token that Kubernetes Service clusters
// accept, valid until the token's own expiry.
func (p *Provider) idToken(ctx context.Context, s *session) (provider.Credential, error) {
	t, err := p.token(ctx, s.acct, s.apiKey, kubeClient)
	if err != nil {
		return provider.Credential{}, err
	}
	if t.IDToken == "" {
		return provider.Credential{}, provider.Errorf(provider.Internal, "IAM returned no ID token for the kube client")
	}
	expires := jwtExpiry(t.IDToken)
	switch {
	case !expires.IsZero():
	case t.Expiration > 0:
		expires = time.Unix(t.Expiration, 0)
	case t.ExpiresIn > 0:
		expires = p.clock().Add(time.Duration(t.ExpiresIn) * time.Second)
	default:
		expires = p.clock().Add(tokenLifetime)
	}
	return provider.Token(t.IDToken, expires), nil
}

// jwtExpiry reads the exp claim of a JWT without checking its signature,
// which the cluster does. It returns the zero time when there is none.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// Credential returns an IAM ID token for a Kubernetes cluster, or a token
// from the cluster's OAuth server for an OpenShift one.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	s, err := p.session(ctx, req.Account, req.Secrets)
	if err != nil {
		return provider.Credential{}, err
	}
	var cl ksCluster
	if err := s.api.Do(ctx, http.MethodGet, "/v2/getCluster?"+url.Values{"cluster": {req.ClusterID}}.Encode(), nil, &cl); err != nil {
		return provider.Credential{}, apiError(err)
	}
	if !cl.openShift() {
		return p.idToken(ctx, s)
	}
	server := cl.server()
	if server == "" {
		return provider.Credential{}, provider.Errorf(provider.Unavailable, "cluster %s has no master URL yet (state %s)", cl.Name, cl.State).
			WithHint("try again when the cluster's master is deployed")
	}
	return p.openShiftToken(ctx, server, s.apiKey)
}

// openShiftToken signs in to the cluster's OAuth server with the API key,
// as `oc login -u apikey` does: it finds the server through the master's
// metadata, then asks for a token with basic credentials and reads it from
// the redirect, as the OAuth implicit grant returns it.
func (p *Provider) openShiftToken(ctx context.Context, server, apiKey string) (provider.Credential, error) {
	meta := &apiclient.Client{
		BaseURL:   strings.TrimSuffix(server, "/"),
		Sign:      func(*http.Request) error { return nil },
		HTTP:      p.http,
		UserAgent: "tug-provider-ibm/" + Version,
	}
	var md struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if err := meta.Do(ctx, http.MethodGet, "/.well-known/oauth-authorization-server", nil, &md); err != nil {
		return provider.Credential{}, err
	}
	authorize, err := apiclient.CheckURL("the OAuth authorization endpoint", md.AuthorizationEndpoint)
	if err != nil {
		return provider.Credential{}, err
	}
	q := url.Values{"client_id": {openShiftClient}, "response_type": {"token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authorize+"?"+q.Encode(), nil)
	if err != nil {
		return provider.Credential{}, err
	}
	req.SetBasicAuth("apikey", apiKey)
	// The OAuth server sends basic challenges only to requests with this
	// header, which it requires to be non-empty.
	req.Header.Set("X-CSRF-Token", "1")
	req.Header.Set("User-Agent", "tug-provider-ibm/"+Version)
	hc := &http.Client{Timeout: 30 * time.Second}
	if p.http != nil {
		c := *p.http
		hc = &c
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return provider.Credential{}, ctx.Err()
		}
		return provider.Credential{}, provider.Errorf(provider.Unavailable, "signing in to the cluster's OAuth server: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		e := provider.StatusError(resp.StatusCode, "the cluster's OAuth server answered "+resp.Status, resp.Header)
		return provider.Credential{}, apiError(e)
	}
	loc, err := resp.Location()
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "the cluster's OAuth server redirected nowhere: %v", err)
	}
	frag, err := url.ParseQuery(loc.Fragment)
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "reading the OAuth redirect: %v", err)
	}
	token := frag.Get("access_token")
	if token == "" {
		msg := frag.Get("error_description")
		if msg == "" {
			msg = frag.Get("error")
		}
		return provider.Credential{}, provider.Errorf(provider.Unauthenticated, "the cluster's OAuth server issued no token: %s", msg).
			WithHint("the API key's identity needs an IAM role for the cluster; " + deniedHint)
	}
	lifetime := tokenLifetime
	if n, err := strconv.Atoi(frag.Get("expires_in")); err == nil && n > 0 {
		lifetime = time.Duration(n) * time.Second
	}
	return provider.Token(token, p.clock().Add(lifetime)), nil
}
