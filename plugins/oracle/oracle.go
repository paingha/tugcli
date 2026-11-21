// Package oracle is tug's first-party provider plugin for Oracle Kubernetes
// Engine (OKE). It calls the OCI REST API directly, signing each request
// with the API key of an OCI CLI profile in ~/.oci/config. The private key
// stays in the file the profile names, as it does for the OCI CLI, so the
// plugin declares no secrets. It lists clusters per compartment and region,
// and gives each the credential that `oci ce cluster create-kubeconfig`
// writes: a delegate that runs `oci ce cluster generate-token`, since OKE
// tokens are signed requests that the CLI makes.
package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/internal/apiclient"
	"github.com/paingha/tugcli/plugins/internal/kubeconfig"
)

// Name and version of the plugin.
const (
	Name    = "oracle"
	Version = "0.1.0"
)

// DefaultAPIURL is where OCI's services are in the commercial realm, oc1.
// {service} and {region} stand for the service's and region's names.
const DefaultAPIURL = "https://{service}.{region}.oci.oraclecloud.com"

// Account settings.
const (
	// ConfigProfile is the OCI CLI profile whose API key signs requests.
	ConfigProfile = "profile"
	// ConfigFile is the OCI CLI config file.
	ConfigFile = "configFile"
	// ConfigRegions lists the regions to look for clusters in, separated by
	// commas.
	ConfigRegions = "regions"
	// ConfigCompartments lists the OCIDs of the compartments to look for
	// clusters in, separated by commas.
	ConfigCompartments = "compartments"
)

// Environment variables the OCI CLI reads, which the plugin honours too.
const (
	EnvConfigFile = "OCI_CLI_CONFIG_FILE"
	EnvProfile    = "OCI_CLI_PROFILE"
)

// DefaultProfile is the OCI CLI's default profile, from which the others
// inherit.
const DefaultProfile = "DEFAULT"

// CLI is the OCI CLI's executable, which the credential runs.
const CLI = "oci"

const installHint = "install the OCI CLI (https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm) and set it up with `oci setup config`"

// execAPIVersion is the ExecCredential version generate-token prints and
// create-kubeconfig writes.
const execAPIVersion = "client.authentication.k8s.io/v1beta1"

// API versions in the request paths.
const (
	containerEngine = "/20180222"
	identity        = "/20160918"
)

// pageSize is how many items to ask for per call, and parallel how many
// compartments and regions to list at once.
const (
	pageSize = "100"
	parallel = 8
)

// Hints for errors a person can fix.
const (
	keyHint    = "check that the user, tenancy and fingerprint of the profile match an API key uploaded to the user in the OCI console, and that this computer's clock is right"
	deniedHint = "the user needs a policy that lets it use clusters in the compartment, such as `allow group <group> to use clusters in compartment <name>`; or set compartments to the ones it may see"
	rateHint   = "wait a minute, then run tug sync"
)

// regionID matches region identifiers such as us-ashburn-1.
var regionID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Provider implements the plugin.
type Provider struct {
	http   *http.Client
	now    func() time.Time
	getenv func(string) string
	home   func() (string, error)
}

var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.AccountLister = (*Provider)(nil)
)

// New returns the plugin.
func New() *Provider {
	return &Provider{now: time.Now, getenv: os.Getenv, home: os.UserHomeDir}
}

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "Oracle Kubernetes Engine (OKE), with an OCI CLI profile's API key",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// configPath returns the OCI config file the account uses, and whether it
// is the CLI's default.
func (p *Provider) configPath(acct provider.Account) (string, bool, error) {
	if path := acct.ConfigString(ConfigFile); path != "" {
		home, _ := p.home()
		path = expandPath(path, home, ".")
		// The credential passes the path to the CLI, which kubectl may run
		// from another directory.
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return path, false, nil
	}
	if path := p.getenv(EnvConfigFile); path != "" {
		return path, false, nil
	}
	home, err := p.home()
	if err != nil {
		return "", false, provider.Errorf(provider.InvalidConfig, "finding the OCI config file: %v", err).
			WithHint("set " + ConfigFile + " to the OCI config file")
	}
	return filepath.Join(home, ".oci", "config"), true, nil
}

// profileName returns the profile the account uses.
func (p *Provider) profileName(acct provider.Account) string {
	if name := acct.ConfigString(ConfigProfile); name != "" {
		return name
	}
	if name := p.getenv(EnvProfile); name != "" {
		return name
	}
	return DefaultProfile
}

// Accounts returns one account per profile with an API key in the OCI
// config file.
func (p *Provider) Accounts(_ context.Context) ([]provider.Account, error) {
	path, isDefault, err := p.configPath(provider.Account{})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return []provider.Account{}, nil
	}
	cfg, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	out := []provider.Account{}
	for _, name := range cfg.order {
		entries, _ := cfg.profile(name)
		if entries["security_token_file"] != "" || entries["user"] == "" || entries["key_file"] == "" ||
			entries["fingerprint"] == "" || entries["tenancy"] == "" {
			continue
		}
		desc := "OCI CLI profile " + name
		if r := entries["region"]; r != "" {
			desc += " (" + r + ")"
		}
		acct := provider.Account{ID: name, Description: desc, Config: map[string]any{ConfigProfile: name}}
		if !isDefault {
			acct.Config[ConfigFile] = path
		}
		out = append(out, acct)
	}
	return out, nil
}

// session is what an account's settings and profile resolve to.
type session struct {
	profile       string
	configPath    string
	defaultConfig bool
	key           *apiKey
	sign          func(*http.Request) error
	regions       []string
	// compartments is nil when the plugin is to find them.
	compartments []string
	apiURL       string
}

func (p *Provider) session(acct provider.Account) (*session, error) {
	path, isDefault, err := p.configPath(acct)
	if err != nil {
		return nil, err
	}
	cfg, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	home, _ := p.home()
	s := &session{profile: p.profileName(acct), configPath: path, defaultConfig: isDefault}
	if s.key, err = loadAPIKey(cfg, s.profile, home); err != nil {
		return nil, err
	}
	now := p.now
	if now == nil {
		now = time.Now
	}
	s.sign = (&signer{key: s.key, now: now}).sign

	s.regions = list(acct, ConfigRegions)
	if len(s.regions) == 0 && s.key.region != "" {
		s.regions = []string{s.key.region}
	}
	if len(s.regions) == 0 {
		return nil, provider.Errorf(provider.InvalidConfig, "profile %s has no region, and the account sets no %s", s.profile, ConfigRegions).
			WithHint(fmt.Sprintf("set %s, such as --set %s=us-ashburn-1,eu-frankfurt-1", ConfigRegions, ConfigRegions))
	}
	for _, r := range s.regions {
		if !regionID.MatchString(r) {
			return nil, provider.Errorf(provider.InvalidConfig, "%q is not an OCI region identifier", r).
				WithHint("regions are identifiers such as us-ashburn-1")
		}
	}
	s.compartments = list(acct, ConfigCompartments)
	for _, c := range s.compartments {
		if !strings.HasPrefix(c, "ocid1.") {
			return nil, provider.Errorf(provider.InvalidConfig, "%q is not a compartment OCID", c).
				WithHint("compartments takes OCIDs such as ocid1.compartment.oc1..aaaa..., separated by commas")
		}
	}
	s.apiURL = acct.ConfigString(apiclient.ConfigAPIURL)
	if s.apiURL == "" {
		s.apiURL = DefaultAPIURL
	}
	if _, err := s.endpoint("containerengine", s.regions[0]); err != nil {
		return nil, err
	}
	return s, nil
}

// list reads a setting that holds a list: commas between the values, or a
// JSON array from an accounts listing.
func list(acct provider.Account, key string) []string {
	var raw []string
	switch v := acct.Config[key].(type) {
	case string:
		raw = strings.Split(v, ",")
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	var out []string
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// endpoint returns the base URL of a service in a region.
func (s *session) endpoint(service, region string) (string, error) {
	raw := strings.NewReplacer("{service}", service, "{region}", region).Replace(s.apiURL)
	return apiclient.CheckURL(apiclient.ConfigAPIURL, raw)
}

// client returns an API client for a service in a region.
func (p *Provider) client(s *session, service, region string) (*apiclient.Client, error) {
	base, err := s.endpoint(service, region)
	if err != nil {
		return nil, err
	}
	return &apiclient.Client{
		BaseURL:   base,
		Sign:      s.sign,
		HTTP:      p.http,
		UserAgent: "tug-provider-oracle/" + Version,
		Message:   errorMessage,
	}, nil
}

// errorMessage reads {"code": "NotAuthenticated", "message": "..."} bodies.
func errorMessage(body []byte) string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil || (e.Code == "" && e.Message == "") {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// apiError adds hints to the errors a person can fix. denied is the hint
// for NotFound, since OCI answers 404 NotAuthorizedOrNotFound both for what
// does not exist and for what the user may not see.
func apiError(err error, profile, denied string) error {
	switch provider.CodeOf(err) {
	case provider.Unauthenticated:
		return provider.AsError(err).WithHint(fmt.Sprintf("OCI did not accept the API key of profile %s; %s", profile, keyHint))
	case provider.NotFound, provider.PermissionDenied:
		return provider.AsError(err).WithHint(denied)
	case provider.RateLimited:
		return provider.AsError(err).WithHint(rateHint)
	default:
		return err
	}
}

// listAll follows opc-next-page through every page of a listing.
func listAll[T any](ctx context.Context, c *apiclient.Client, path string, q url.Values) ([]T, error) {
	var out []T
	for {
		resp, err := c.Send(ctx, http.MethodGet, path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, provider.Errorf(provider.Internal, "decoding %s: %v", path, err)
		}
		out = append(out, items...)
		next := resp.Header.Get("opc-next-page")
		if next == "" {
			return out, nil
		}
		if next == q.Get("page") {
			return nil, provider.Errorf(provider.Internal, "%s returned the same page token twice", path)
		}
		q.Set("page", next)
	}
}

// OCI API types, trimmed to the fields the plugin uses.
type (
	compartment struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		LifecycleState string `json:"lifecycleState"`
	}
	clusterSummary struct {
		ID                string            `json:"id"`
		Name              string            `json:"name"`
		CompartmentID     string            `json:"compartmentId"`
		KubernetesVersion string            `json:"kubernetesVersion"`
		LifecycleState    string            `json:"lifecycleState"`
		FreeformTags      map[string]string `json:"freeformTags"`
		Endpoints         *struct {
			Kubernetes      string `json:"kubernetes"`
			PublicEndpoint  string `json:"publicEndpoint"`
			PrivateEndpoint string `json:"privateEndpoint"`
		} `json:"endpoints"`
	}
	nodePool struct {
		ClusterID string `json:"clusterId"`
		NodeShape string `json:"nodeShape"`
	}
)

// ready reports whether a cluster has an API server to reach.
func (c clusterSummary) ready() bool {
	switch c.LifecycleState {
	case "ACTIVE", "UPDATING":
	default:
		return false
	}
	return c.Endpoints != nil && (c.Endpoints.PublicEndpoint != "" || c.Endpoints.PrivateEndpoint != "" || c.Endpoints.Kubernetes != "")
}

// compartments returns the compartments to list, and whether the plugin
// found them rather than the account naming them.
func (p *Provider) compartments(ctx context.Context, s *session) ([]string, bool, error) {
	if s.compartments != nil {
		return s.compartments, false, nil
	}
	c, err := p.client(s, "identity", s.regions[0])
	if err != nil {
		return nil, false, err
	}
	q := url.Values{
		"compartmentId":          {s.key.tenancy},
		"compartmentIdInSubtree": {"true"},
		"accessLevel":            {"ACCESSIBLE"},
		"lifecycleState":         {"ACTIVE"},
		"limit":                  {"1000"},
	}
	found, err := listAll[compartment](ctx, c, identity+"/compartments", q)
	if err != nil {
		return nil, false, apiError(err, s.profile,
			"the user cannot list the tenancy's compartments; give it `inspect compartments in tenancy`, or set "+ConfigCompartments+" to the OCIDs of the compartments to list")
	}
	out := []string{s.key.tenancy}
	for _, c := range found {
		if c.LifecycleState == "ACTIVE" && !slices.Contains(out, c.ID) {
			out = append(out, c.ID)
		}
	}
	return out, true, nil
}

// List returns the OKE clusters in each of the account's compartments and
// regions. Clusters being created, deleted or that failed are skipped.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	s, err := p.session(req.Account)
	if err != nil {
		return nil, err
	}
	comps, discovered, err := p.compartments(ctx, s)
	if err != nil {
		return nil, err
	}
	type place struct{ region, compartment string }
	var places []place
	for _, r := range s.regions {
		for _, c := range comps {
			places = append(places, place{r, c})
		}
	}
	found := make([][]provider.Cluster, len(places))
	err = forEach(ctx, len(places), parallel, func(ctx context.Context, i int) error {
		pl := places[i]
		clusters, err := p.listIn(ctx, s, pl.region, pl.compartment)
		if discovered && provider.CodeOf(err) == provider.NotFound {
			// The user can see the compartment, perhaps through a
			// subcompartment, but not the clusters in it.
			fmt.Fprintf(os.Stderr, "oracle: skipping compartment %s in %s: %v\n", pl.compartment, pl.region, err)
			return nil
		}
		found[i] = clusters
		return err
	})
	if err != nil {
		return nil, err
	}
	out := []provider.Cluster{}
	for _, cl := range found {
		out = append(out, cl...)
	}
	return out, nil
}

// listIn returns the clusters in one compartment and region.
func (p *Provider) listIn(ctx context.Context, s *session, region, comp string) ([]provider.Cluster, error) {
	c, err := p.client(s, "containerengine", region)
	if err != nil {
		return nil, err
	}
	q := url.Values{"compartmentId": {comp}, "limit": {pageSize}}
	summaries, err := listAll[clusterSummary](ctx, c, containerEngine+"/clusters", q)
	if err != nil {
		return nil, apiError(err, s.profile, deniedHint)
	}
	var ready []clusterSummary
	for _, cl := range summaries {
		if !cl.ready() {
			if cl.LifecycleState != "DELETED" {
				fmt.Fprintf(os.Stderr, "oracle: skipping %s in %s: it is %s\n", cl.Name, region, cl.LifecycleState)
			}
			continue
		}
		ready = append(ready, cl)
	}
	if len(ready) == 0 {
		return nil, nil
	}
	pools, err := listAll[nodePool](ctx, c, containerEngine+"/nodePools", url.Values{"compartmentId": {comp}, "limit": {pageSize}})
	if err != nil {
		return nil, apiError(err, s.profile, deniedHint)
	}
	gpu := map[string]bool{}
	for _, np := range pools {
		if strings.Contains(strings.ToUpper(np.NodeShape), ".GPU") {
			gpu[np.ClusterID] = true
		}
	}
	out := make([]provider.Cluster, 0, len(ready))
	for _, cl := range ready {
		pc, err := p.cluster(ctx, s, c, region, cl)
		if err != nil {
			return nil, err
		}
		if gpu[cl.ID] {
			pc.Labels["gpu"] = "true"
		}
		out = append(out, pc)
	}
	return out, nil
}

// cluster converts a cluster summary, with the CA from the kubeconfig the
// API generates for it.
func (p *Provider) cluster(ctx context.Context, s *session, c *apiclient.Client, region string, cl clusterSummary) (provider.Cluster, error) {
	ep := cl.Endpoints
	pc := provider.Cluster{
		ID:      cl.ID,
		Name:    cl.Name,
		Region:  region,
		Version: strings.TrimPrefix(cl.KubernetesVersion, "v"),
		Endpoints: provider.Endpoints{
			Public:  endpointURL(ep.PublicEndpoint),
			Private: endpointURL(ep.PrivateEndpoint),
		},
		Labels: map[string]string{},
	}
	// A cluster without native VCN networking has only the legacy endpoint.
	kind := "PUBLIC_ENDPOINT"
	switch {
	case ep.PublicEndpoint == "" && ep.PrivateEndpoint == "":
		pc.Endpoints.Public, kind = endpointURL(ep.Kubernetes), "LEGACY_KUBERNETES"
	case ep.PublicEndpoint == "":
		kind = "PRIVATE_ENDPOINT"
	}
	body := map[string]string{"tokenVersion": "2.0.0", "endpoint": kind}
	resp, err := c.Send(ctx, http.MethodPost, containerEngine+"/clusters/"+url.PathEscape(cl.ID)+"/kubeconfig/content", body)
	if err != nil {
		return provider.Cluster{}, apiError(err, s.profile, deniedHint)
	}
	acc, err := kubeconfig.Parse(resp.Body)
	if err != nil {
		return provider.Cluster{}, provider.Errorf(provider.Internal, "the kubeconfig of cluster %s: %v", cl.Name, err)
	}
	pc.CAData = acc.CAData
	if acc.Server != "" {
		if kind == "PRIVATE_ENDPOINT" {
			pc.Endpoints.Private = acc.Server
		} else {
			pc.Endpoints.Public = acc.Server
		}
	}
	for k, v := range cl.FreeformTags {
		// tug sets the region label from Region.
		if !strings.EqualFold(k, "region") && k != "" && v != "" {
			pc.Labels[k] = v
		}
	}
	// The credential is a command, not a secret. Returning it here lets
	// sync replace it when the profile or config file setting changes.
	cred := s.delegate(cl.ID, region)
	pc.Credential = &cred
	return pc, nil
}

// endpointURL turns an endpoint such as 203.0.113.7:6443 into the URL the
// Kubernetes API is served at.
func endpointURL(s string) string {
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return s
	}
	return "https://" + s
}

// Credential returns the delegate that runs `oci ce cluster generate-token`
// for the cluster. With one region it needs no API call; with several it
// asks each for the cluster until one has it.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	if !strings.HasPrefix(req.ClusterID, "ocid1.cluster.") {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "%q is not an OKE cluster OCID", req.ClusterID)
	}
	s, err := p.session(req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	if len(s.regions) == 1 {
		return s.delegate(req.ClusterID, s.regions[0]), nil
	}
	for _, r := range s.regions {
		c, err := p.client(s, "containerengine", r)
		if err != nil {
			return provider.Credential{}, err
		}
		err = c.Do(ctx, http.MethodGet, containerEngine+"/clusters/"+url.PathEscape(req.ClusterID), nil, nil)
		if err == nil {
			return s.delegate(req.ClusterID, r), nil
		}
		if provider.CodeOf(err) != provider.NotFound {
			return provider.Credential{}, apiError(err, s.profile, deniedHint)
		}
	}
	return provider.Credential{}, provider.Errorf(provider.NotFound, "no region of %s has cluster %s", strings.Join(s.regions, ", "), req.ClusterID).
		WithHint("run tug sync; the cluster may have been deleted")
}

// delegate is the exec entry `oci ce cluster create-kubeconfig` writes, with
// the region, profile and config file pinned so that kubectl uses the same
// ones whatever its environment.
func (s *session) delegate(clusterID, region string) provider.Credential {
	args := []string{"ce", "cluster", "generate-token", "--cluster-id", clusterID, "--region", region, "--profile", s.profile}
	if !s.defaultConfig {
		args = append(args, "--config-file", s.configPath)
	}
	c := provider.Delegate(CLI, args...)
	c.Exec.APIVersion = execAPIVersion
	c.Exec.InstallHint = installHint
	return c
}

// forEach calls fn for 0 to n-1, at most limit at a time, and returns the
// first error, cancelling the calls still running.
func forEach(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				errs[i] = err
				cancel()
			}
		})
	}
	wg.Wait()
	// Report the error that stopped the others, not the cancellations it
	// caused.
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return errors.Join(errs...)
}
