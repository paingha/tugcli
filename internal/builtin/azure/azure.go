// Package azure is the built-in provider for Azure Kubernetes Service. It
// lists managed clusters per subscription through Azure Resource Manager and
// signs in with the Azure SDK's DefaultAzureCredential, which covers
// environment variables, workload identity, managed identity and the Azure
// CLI. Clusters with Microsoft Entra ID integration get short-lived Entra
// tokens, so kubelogin is not needed; clusters with local accounts get the
// user credential AKS issues for them.
package azure

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/builtin/kubeconfig"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "azure"

// Account config keys.
const (
	// ConfigSubscription holds the subscription ID. The account ID is used
	// when it is absent.
	ConfigSubscription = "subscription"
	// ConfigTenant holds the subscription's Microsoft Entra tenant, which
	// tokens for Azure Resource Manager are requested from. Without it they
	// come from the signed-in identity's default tenant.
	ConfigTenant = "tenant"
)

// DefaultARMURL is Azure Resource Manager in Azure's public cloud.
const DefaultARMURL = "https://management.azure.com"

// API versions, as Microsoft's REST API reference documents them: the
// Microsoft.Resources subscriptions API and the stable AKS managed clusters
// API.
const (
	subscriptionsAPIVersion = "2022-12-01"
	aksAPIVersion           = "2025-10-01"
)

// aksScope asks Microsoft Entra ID for a token for the AKS server
// application, the audience every AKS API server with Entra integration
// accepts. Microsoft documents the application ID as the same in every
// environment.
const aksScope = "6dae42f8-4368-4678-94ff-3960e28e3630/.default"

// localCredentialTTL bounds how long a local account's credential is cached,
// so that rotated cluster certificates and withdrawn access are picked up.
const localCredentialTTL = time.Hour

// Provider lists AKS clusters.
type Provider struct {
	armURL string
	client *http.Client
	cred   func() (azcore.TokenCredential, error)
	now    func() time.Time
	kc     *kubeconfig.Provider
}

var _ registry.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithARMURL points the provider at another Azure Resource Manager endpoint,
// for tests or another Azure cloud. Tokens for it are requested with the
// scope <url>/.default.
func WithARMURL(u string) Option {
	return func(p *Provider) { p.armURL = strings.TrimSuffix(u, "/") }
}

// WithHTTPClient sets the client for calls to Azure Resource Manager, for
// tests. Tokens are still fetched by the token credential.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.client = c } }

// WithTokenCredential replaces DefaultAzureCredential, for tests.
func WithTokenCredential(c azcore.TokenCredential) Option {
	return func(p *Provider) {
		p.cred = func() (azcore.TokenCredential, error) { return c, nil }
	}
}

// WithNow replaces time.Now, for tests.
func WithNow(f func() time.Time) Option { return func(p *Provider) { p.now = f } }

// New returns the AKS provider. DefaultAzureCredential is created on first
// use and then shared, so a long-running process remembers which source
// signed it in instead of probing every source again.
func New(opts ...Option) *Provider {
	p := &Provider{
		armURL: DefaultARMURL,
		client: http.DefaultClient,
		cred: sync.OnceValues(func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		}),
		now: time.Now,
		kc:  kubeconfig.New(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Info implements registry.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      version.String(),
		Description:  "Azure Kubernetes Service clusters, per Azure subscription",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// Azure Resource Manager responses, trimmed to the fields tug uses.
type (
	subscriptionList struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			DisplayName    string `json:"displayName"`
			State          string `json:"state"`
			TenantID       string `json:"tenantId"`
		} `json:"value"`
		NextLink string `json:"nextLink"`
	}
	clusterList struct {
		Value    []managedCluster `json:"value"`
		NextLink string           `json:"nextLink"`
	}
	managedCluster struct {
		ID         string            `json:"id"`
		Name       string            `json:"name"`
		Location   string            `json:"location"`
		Tags       map[string]string `json:"tags"`
		Properties struct {
			KubernetesVersion        string `json:"kubernetesVersion"`
			CurrentKubernetesVersion string `json:"currentKubernetesVersion"`
			FQDN                     string `json:"fqdn"`
			PrivateFQDN              string `json:"privateFQDN"`
			DisableLocalAccounts     bool   `json:"disableLocalAccounts"`
			APIServerAccessProfile   *struct {
				EnablePrivateCluster bool `json:"enablePrivateCluster"`
			} `json:"apiServerAccessProfile"`
			AADProfile *struct {
				Managed  bool   `json:"managed"`
				TenantID string `json:"tenantID"`
			} `json:"aadProfile"`
		} `json:"properties"`
	}
	credentialResults struct {
		Kubeconfigs []struct {
			Name  string `json:"name"`
			Value []byte `json:"value"`
		} `json:"kubeconfigs"`
	}
	errorResponse struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
)

// Accounts returns the subscriptions the signed-in identity can see in its
// default tenant, leaving out disabled and deleted ones.
func (p *Provider) Accounts(ctx context.Context) ([]provider.Account, error) {
	tok, err := p.token(ctx, p.armScope(), "")
	if err != nil {
		return nil, err
	}
	var out []provider.Account
	next := p.armURL + "/subscriptions?api-version=" + subscriptionsAPIVersion
	for next != "" {
		var page subscriptionList
		if err := p.call(ctx, tok.Token, http.MethodGet, next, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Value {
			// Disabled and deleted subscriptions have no clusters to reach.
			if s.SubscriptionID == "" || s.State == "Disabled" || s.State == "Deleted" {
				continue
			}
			acct := provider.Account{
				ID:          s.SubscriptionID,
				Description: "Azure subscription " + cmp.Or(s.DisplayName, s.SubscriptionID),
				Config:      map[string]any{ConfigSubscription: s.SubscriptionID},
			}
			if s.TenantID != "" {
				acct.Config[ConfigTenant] = s.TenantID
			}
			out = append(out, acct)
		}
		if next, err = p.nextPage(page.NextLink); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b provider.Account) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// subscription returns the account's subscription ID.
func subscription(acct provider.Account) (string, error) {
	sub := cmp.Or(acct.ConfigString(ConfigSubscription), acct.ID)
	if !isUUID(sub) {
		return "", provider.Errorf(provider.InvalidConfig, "Azure account %q names no subscription ID", acct.ID).
			WithHint("tug import azure --account <subscription-id>, or --set subscription=<subscription-id>")
	}
	return sub, nil
}

// isUUID reports whether s has the form of a subscription ID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdefABCDEF", r):
			return false
		}
	}
	return true
}

// List returns the account's AKS clusters. Clusters still being created,
// which have no API server address yet, are left out until the next sync, as
// are clusters whose user credential the identity may not list: without it
// tug has no CA to verify the API server with. So are clusters with local
// accounts disabled and no Entra integration, which nobody can sign in to.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	sub, err := subscription(req.Account)
	if err != nil {
		return nil, err
	}
	tok, err := p.token(ctx, p.armScope(), req.Account.ConfigString(ConfigTenant))
	if err != nil {
		return nil, err
	}
	var out []provider.Cluster
	next := fmt.Sprintf("%s/subscriptions/%s/providers/Microsoft.ContainerService/managedClusters?api-version=%s",
		p.armURL, url.PathEscape(sub), aksAPIVersion)
	for next != "" {
		var page clusterList
		if err := p.call(ctx, tok.Token, http.MethodGet, next, &page); err != nil {
			return nil, err
		}
		for _, mc := range page.Value {
			pc, ok, err := p.toCluster(ctx, tok.Token, sub, mc)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, pc)
			}
		}
		if next, err = p.nextPage(page.NextLink); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// toCluster converts a managed cluster, fetching its CA from the user
// kubeconfig. ok is false for clusters List leaves out.
func (p *Provider) toCluster(ctx context.Context, token, sub string, mc managedCluster) (provider.Cluster, bool, error) {
	props := mc.Properties
	if (props.FQDN == "" && props.PrivateFQDN == "") || noSignIn(mc) {
		return provider.Cluster{}, false, nil
	}
	rg := resourceGroup(mc.ID)
	if rg == "" || mc.Name == "" {
		return provider.Cluster{}, false, provider.Errorf(provider.Internal, "managed cluster %q has no resource group or name", mc.ID)
	}
	cfg, kctx, err := p.userKubeconfig(ctx, token, sub, rg, mc.Name)
	if err != nil {
		if code := provider.CodeOf(err); code == provider.PermissionDenied || code == provider.NotFound {
			return provider.Cluster{}, false, nil
		}
		return provider.Cluster{}, false, err
	}
	ca, err := clusterCA(cfg, kctx)
	if err != nil {
		return provider.Cluster{}, false, provider.Errorf(provider.Internal, "cluster %s: %v", mc.Name, err)
	}

	out := provider.Cluster{
		// Resource group names are case-insensitive and ARM does not always
		// return them in the same case, so the ID uses lower case to stay
		// stable.
		ID:      strings.ToLower(rg) + "/" + mc.Name,
		Name:    mc.Name,
		Region:  mc.Location,
		Version: cmp.Or(props.CurrentKubernetesVersion, props.KubernetesVersion),
		CAData:  ca,
		Labels:  map[string]string{},
	}
	if ap := props.APIServerAccessProfile; (ap != nil && ap.EnablePrivateCluster) || props.FQDN == "" {
		// As `az aks get-credentials` does, prefer the private FQDN, which
		// the cluster's private DNS zone resolves.
		out.Endpoints.Private = serverURL(cmp.Or(props.PrivateFQDN, props.FQDN))
	} else {
		out.Endpoints.Public = serverURL(props.FQDN)
	}
	for k, v := range mc.Tags {
		key := strings.ToLower(k)
		if key == "environment" {
			key = "env"
		}
		out.Labels[key] = v
	}
	out.Labels["azure-subscription"] = sub
	out.Labels["azure-resource-group"] = rg
	return out, true, nil
}

// noSignIn reports whether a cluster has local accounts disabled and no
// Entra integration, which leaves no way to authenticate to it.
func noSignIn(mc managedCluster) bool {
	return mc.Properties.DisableLocalAccounts && mc.Properties.AADProfile == nil
}

// serverURL turns an API server FQDN into the URL AKS kubeconfigs use.
func serverURL(fqdn string) string {
	if fqdn == "" {
		return ""
	}
	return "https://" + fqdn + ":443"
}

// resourceGroup returns the resource group named in an ARM resource ID such
// as /subscriptions/<id>/resourcegroups/<group>/providers/....
func resourceGroup(id string) string {
	parts := strings.Split(id, "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

// Credential returns a credential for one cluster. Clusters with Microsoft
// Entra ID integration get an Entra token for the AKS server application from
// the cluster's tenant; clusters with local accounts get the credential in
// their user kubeconfig.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	sub, err := subscription(req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	rg, name, ok := strings.Cut(req.ClusterID, "/")
	if !ok || rg == "" || name == "" || strings.Contains(name, "/") {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster id %q is not resource-group/name", req.ClusterID)
	}
	tenant := req.Account.ConfigString(ConfigTenant)
	armTok, err := p.token(ctx, p.armScope(), tenant)
	if err != nil {
		return provider.Credential{}, err
	}
	var mc managedCluster
	if err := p.call(ctx, armTok.Token, http.MethodGet, p.clusterURL(sub, rg, name, ""), &mc); err != nil {
		return provider.Credential{}, err
	}

	if aad := mc.Properties.AADProfile; aad != nil {
		if !aad.Managed {
			return provider.Credential{}, provider.Errorf(provider.Unimplemented, "cluster %s uses legacy Azure AD integration, which tug does not support", name).
				WithHint(fmt.Sprintf("migrate it to AKS-managed Microsoft Entra integration: az aks update -g %s -n %s --enable-aad --aad-admin-group-object-ids <group-id>", rg, name))
		}
		tok, err := p.token(ctx, aksScope, cmp.Or(aad.TenantID, tenant))
		if err != nil {
			return provider.Credential{}, err
		}
		return provider.Token(tok.Token, tok.ExpiresOn), nil
	}
	if noSignIn(mc) {
		return provider.Credential{}, provider.Errorf(provider.PermissionDenied, "cluster %s has local accounts disabled and no Microsoft Entra integration, so nothing can sign in to it", name).
			WithHint(fmt.Sprintf("enable Entra integration (az aks update -g %s -n %s --enable-aad) or local accounts (--enable-local-accounts)", rg, name))
	}

	cfg, kctx, err := p.userKubeconfig(ctx, armTok.Token, sub, rg, name)
	if err != nil {
		return provider.Credential{}, err
	}
	c, err := p.kc.ContextCredential(cfg, kctx, "")
	if err != nil {
		return provider.Credential{}, err
	}
	if limit := p.now().Add(localCredentialTTL); c.ExpiresAt == nil || c.ExpiresAt.After(limit) {
		c.ExpiresAt = &limit
	}
	return c, nil
}

// clusterURL is the ARM URL of a managed cluster, or of an action on it.
func (p *Provider) clusterURL(sub, rg, name, action string) string {
	u := fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s",
		p.armURL, url.PathEscape(sub), url.PathEscape(rg), url.PathEscape(name))
	if action != "" {
		u += "/" + action
	}
	return u + "?api-version=" + aksAPIVersion
}

// userKubeconfig fetches a cluster's user kubeconfig with
// listClusterUserCredential and returns it with the name of its context.
func (p *Provider) userKubeconfig(ctx context.Context, token, sub, rg, name string) (*clientcmdapi.Config, string, error) {
	var res credentialResults
	if err := p.call(ctx, token, http.MethodPost, p.clusterURL(sub, rg, name, "listClusterUserCredential"), &res); err != nil {
		return nil, "", err
	}
	if len(res.Kubeconfigs) == 0 {
		return nil, "", provider.Errorf(provider.Internal, "cluster %s: listClusterUserCredential returned no kubeconfig", name)
	}
	cfg, err := clientcmd.Load(res.Kubeconfigs[0].Value)
	if err != nil {
		return nil, "", provider.Errorf(provider.Internal, "cluster %s: parsing the user kubeconfig: %v", name, err)
	}
	kctx := cfg.CurrentContext
	if kctx == "" && len(cfg.Contexts) == 1 {
		for n := range cfg.Contexts {
			kctx = n
		}
	}
	if _, ok := cfg.Contexts[kctx]; !ok {
		return nil, "", provider.Errorf(provider.Internal, "cluster %s: the user kubeconfig has no current context", name)
	}
	return cfg, kctx, nil
}

// clusterCA returns the CA certificate of a kubeconfig context's cluster.
func clusterCA(cfg *clientcmdapi.Config, kctx string) ([]byte, error) {
	cl, ok := cfg.Clusters[cfg.Contexts[kctx].Cluster]
	if !ok || len(cl.CertificateAuthorityData) == 0 {
		return nil, fmt.Errorf("the user kubeconfig has no certificate-authority-data")
	}
	return cl.CertificateAuthorityData, nil
}

// armScope is the token scope for Azure Resource Manager.
func (p *Provider) armScope() string { return p.armURL + "/.default" }

// token gets a Microsoft Entra access token for scope, from tenant when it is
// set and from the identity's default tenant otherwise.
func (p *Provider) token(ctx context.Context, scope, tenant string) (azcore.AccessToken, error) {
	cred, err := p.cred()
	if err != nil {
		return azcore.AccessToken{}, noCredentials(err, tenant)
	}
	tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}, TenantID: tenant})
	if err != nil {
		if ctx.Err() != nil {
			return azcore.AccessToken{}, ctx.Err()
		}
		return azcore.AccessToken{}, noCredentials(err, tenant)
	}
	return tok, nil
}

// call sends one request to Azure Resource Manager and decodes the JSON
// answer into out.
func (p *Provider) call(ctx context.Context, token, method, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return provider.Errorf(provider.Internal, "building Azure Resource Manager request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tug/"+version.String())
	resp, err := p.client.Do(req)
	if err != nil {
		return provider.Errorf(provider.Unavailable, "Azure Resource Manager: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, provider.MaxMessageBytes))
	if err != nil {
		return provider.Errorf(provider.Unavailable, "reading Azure Resource Manager response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return armError(resp, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return provider.Errorf(provider.Internal, "decoding Azure Resource Manager response: %v", err)
	}
	return nil
}

// nextPage checks that a nextLink stays on Azure Resource Manager, so that
// the bearer token is never sent anywhere else.
func (p *Provider) nextPage(link string) (string, error) {
	if link == "" {
		return "", nil
	}
	base, err := url.Parse(p.armURL)
	if err != nil {
		return "", provider.Errorf(provider.InvalidConfig, "Azure Resource Manager URL %q: %v", p.armURL, err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return "", provider.Errorf(provider.Internal, "Azure Resource Manager returned a next page outside %s: %q", p.armURL, link)
	}
	return link, nil
}

// armError converts an ARM error answer into a protocol error.
func armError(resp *http.Response, body []byte) error {
	var er errorResponse
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &er) == nil && er.Error.Message != "" {
		msg = er.Error.Message
		if er.Error.Code != "" {
			msg = er.Error.Code + ": " + msg
		}
	}
	e := provider.StatusError(resp.StatusCode, msg, resp.Header)
	switch {
	case e.Code == provider.Unauthenticated:
		return e.WithHint(signInHint(""))
	case er.Error.Code == "SubscriptionNotFound":
		return e.WithHint("check the subscription ID; `az account list` shows the subscriptions you can use")
	}
	return e
}

func noCredentials(err error, tenant string) error {
	return provider.Errorf(provider.Unauthenticated, "no Azure credentials: %v", err).WithHint(signInHint(tenant))
}

func signInHint(tenant string) string {
	login := "az login"
	if tenant != "" {
		login += " --tenant " + tenant
	}
	return "sign in with `" + login + "`, or set AZURE_TENANT_ID and AZURE_CLIENT_ID for a service principal or workload identity"
}
