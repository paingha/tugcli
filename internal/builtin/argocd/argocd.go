// Package argocd is the built-in provider for Argo CD. It lists the
// clusters Argo CD deploys to from its declarative cluster Secrets, on a
// cluster already in tug's catalog, and turns each Secret's config into a
// tug credential.
package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/paingha/tugcli/internal/builtin/management"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "argocd"

// Account settings.
const (
	// ConfigCluster names the cluster Argo CD runs on, in tug's catalog.
	ConfigCluster = management.ConfigCluster
	// ConfigNamespace is Argo CD's namespace, DefaultNamespace by default.
	ConfigNamespace = management.ConfigNamespace
	// ConfigAllowExec, set to true, lets execProviderConfig run its command
	// on this machine. Anyone who can write Secrets in Argo CD's namespace
	// chooses that command, so it is off unless the user turns it on.
	ConfigAllowExec = "allowExec"
)

// allowExec reports whether the account lets cluster Secrets run commands.
func allowExec(acct provider.Account) bool {
	v := acct.Config[ConfigAllowExec]
	return v == true || v == "true"
}

// DefaultNamespace is where Argo CD is installed unless the namespace
// setting says otherwise.
const DefaultNamespace = "argocd"

// Argo CD's cluster Secret format.
const (
	secretTypeLabel = "argocd.argoproj.io/secret-type"
	secretTypeValue = "cluster"
	// inClusterServer is the cluster Argo CD runs on, which is the
	// management cluster already in the catalog and not reachable at this
	// address from outside it.
	inClusterServer = "https://kubernetes.default.svc"
)

// LabelProject is set on clusters that belong to an Argo CD project, to
// the project's name.
const LabelProject = "argocd-project"

// credentialTTL is how long a credential read from a cluster Secret is used
// before tug reads the Secret again, so that one rotated in place is picked
// up.
const credentialTTL = time.Hour

// awsCLIHint is shown when the AWS CLI, which runs for awsAuthConfig, is
// missing.
const awsCLIHint = "install the AWS CLI: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"

// Provider lists Argo CD clusters.
type Provider struct {
	clusterConfig management.ClusterConfig
	now           func() time.Time
}

var _ provider.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithClusterConfig sets how the provider reaches the cluster Argo CD runs
// on.
func WithClusterConfig(f management.ClusterConfig) Option {
	return func(p *Provider) { p.clusterConfig = f }
}

// WithNow replaces time.Now, for tests.
func WithNow(f func() time.Time) Option { return func(p *Provider) { p.now = f } }

// New returns the Argo CD provider.
func New(opts ...Option) *Provider {
	p := &Provider{now: time.Now}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      version.String(),
		Description:  "Clusters Argo CD deploys to, read from its cluster Secrets on a cluster in the catalog",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// clusterConfig is the "config" JSON of a cluster Secret, as Argo CD's
// ClusterConfig type defines it.
type clusterConfig struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	BearerToken     string `json:"bearerToken"`
	TLSClientConfig struct {
		Insecure   bool   `json:"insecure"`
		ServerName string `json:"serverName"`
		CertData   []byte `json:"certData"`
		KeyData    []byte `json:"keyData"`
		CAData     []byte `json:"caData"`
	} `json:"tlsClientConfig"`
	AWSAuthConfig *struct {
		ClusterName string `json:"clusterName"`
		RoleARN     string `json:"roleARN"`
		Profile     string `json:"profile"`
	} `json:"awsAuthConfig"`
	ExecProviderConfig *struct {
		Command            string            `json:"command"`
		Args               []string          `json:"args"`
		Env                map[string]string `json:"env"`
		APIVersion         string            `json:"apiVersion"`
		InstallHint        string            `json:"installHint"`
		ProvideClusterInfo bool              `json:"provideClusterInfo"`
	} `json:"execProviderConfig"`
	ProxyURL string `json:"proxyUrl"`
}

// cluster is a parsed cluster Secret.
type cluster struct {
	secret string // namespace/name, for messages
	id     string
	name   string
	server string
	config clusterConfig
	labels map[string]string
	// namespaces are the namespaces Argo CD may use, when it is limited.
	namespaces []string
}

func parse(s *corev1.Secret) (*cluster, error) {
	c := &cluster{
		secret: s.Namespace + "/" + s.Name,
		id:     s.Name,
		name:   string(s.Data["name"]),
		server: strings.TrimSuffix(string(s.Data["server"]), "/"),
		labels: maps.Clone(s.Labels),
	}
	if c.name == "" {
		c.name = s.Name
	}
	delete(c.labels, secretTypeLabel)
	if project := string(s.Data["project"]); project != "" {
		if c.labels == nil {
			c.labels = map[string]string{}
		}
		c.labels[LabelProject] = project
	}
	for ns := range strings.SplitSeq(string(s.Data["namespaces"]), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			c.namespaces = append(c.namespaces, ns)
		}
	}
	if raw := s.Data["config"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &c.config); err != nil {
			return nil, provider.Errorf(provider.InvalidConfig, "Argo CD cluster Secret %s has a config that is not valid JSON: %v", c.secret, err)
		}
	}
	return c, nil
}

// List returns one cluster per cluster Secret in Argo CD's namespace.
// Argo CD's own cluster, https://kubernetes.default.svc, is left out: it is
// the cluster tug read the Secrets from.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	mc, secrets, err := p.connect(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	ns := namespace(req.Account)
	list, err := secrets.Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: secretTypeLabel + "=" + secretTypeValue})
	if err != nil {
		return nil, mc.Error(err, "listing Argo CD cluster Secrets in "+ns)
	}
	var out []provider.Cluster
	for i := range list.Items {
		c, err := parse(&list.Items[i])
		if err != nil {
			return nil, err
		}
		if c.server == "" || c.server == inClusterServer {
			continue
		}
		pc := provider.Cluster{
			ID:                    c.id,
			Name:                  c.name,
			Endpoints:             provider.Endpoints{Public: c.server},
			CAData:                c.config.TLSClientConfig.CAData,
			TLSServerName:         c.config.TLSClientConfig.ServerName,
			InsecureSkipTLSVerify: c.config.TLSClientConfig.Insecure,
			ProxyURL:              c.config.ProxyURL,
			Labels:                c.labels,
		}
		if len(c.namespaces) == 1 {
			pc.DefaultNamespace = c.namespaces[0]
		}
		if cred, err := p.credential(req.Account, c); err == nil {
			pc.Credential = &cred
		}
		out = append(out, pc)
	}
	return out, nil
}

// Credential reads the cluster's Secret again and converts its config.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	mc, secrets, err := p.connect(ctx, req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	ns := namespace(req.Account)
	s, err := secrets.Secrets(ns).Get(ctx, req.ClusterID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "Argo CD has no cluster Secret %s/%s on %s", ns, req.ClusterID, mc.Name).
			WithHint("run `tug sync` to see the clusters Argo CD has now")
	}
	if err != nil {
		return provider.Credential{}, mc.Error(err, fmt.Sprintf("reading Secret %s/%s", ns, req.ClusterID))
	}
	// Only cluster Secrets: a request must not turn another Secret in
	// Argo CD's namespace, such as a repository's, into a credential.
	if s.Labels[secretTypeLabel] != secretTypeValue {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "Secret %s/%s on %s is not an Argo CD cluster Secret", ns, req.ClusterID, mc.Name)
	}
	c, err := parse(s)
	if err != nil {
		return provider.Credential{}, err
	}
	return p.credential(req.Account, c)
}

func (p *Provider) connect(ctx context.Context, acct provider.Account) (*management.Cluster, corev1client.SecretsGetter, error) {
	mc, err := management.Connect(ctx, p.clusterConfig, Name, acct)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := corev1client.NewForConfig(mc.Config)
	if err != nil {
		return nil, nil, provider.Errorf(provider.InvalidConfig, "management cluster %s: %v", mc.Name, err)
	}
	return mc, secrets, nil
}

func namespace(acct provider.Account) string {
	if ns := acct.ConfigString(ConfigNamespace); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// credential converts a cluster's config, trying its methods in the order
// Argo CD does: awsAuthConfig, then execProviderConfig, then the bearer
// token or client certificate.
func (p *Provider) credential(acct provider.Account, c *cluster) (provider.Credential, error) {
	cfg := c.config
	tls := cfg.TLSClientConfig
	switch {
	case cfg.AWSAuthConfig != nil:
		return awsCredential(c)
	case cfg.ExecProviderConfig != nil:
		e := cfg.ExecProviderConfig
		if e.Command == "" {
			return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "Argo CD cluster Secret %s has an execProviderConfig without a command", c.secret)
		}
		if e.ProvideClusterInfo {
			return provider.Credential{}, provider.Errorf(provider.Unimplemented,
				"Argo CD cluster Secret %s asks for provideClusterInfo, and tug cannot pass cluster information to exec plugins", c.secret)
		}
		if !allowExec(acct) {
			return provider.Credential{}, provider.Errorf(provider.PermissionDenied,
				"Argo CD cluster Secret %s gets its credential by running %q, and tug runs commands from a cluster only when the account allows it", c.secret, e.Command).
				WithHint(fmt.Sprintf("if you trust everyone who can write Secrets in namespace %s, import the account again with --set %s=true", namespace(acct), ConfigAllowExec))
		}
		cred := provider.Delegate(e.Command, e.Args...)
		cred.Exec.Env = maps.Clone(e.Env)
		cred.Exec.APIVersion = e.APIVersion
		cred.Exec.InstallHint = e.InstallHint
		return cred, nil
	case cfg.BearerToken != "":
		return provider.Token(cfg.BearerToken, p.now().Add(credentialTTL)), nil
	case len(tls.CertData) > 0 && len(tls.KeyData) > 0:
		return provider.ClientCert(tls.CertData, tls.KeyData, management.Expiry(p.now(), credentialTTL, tls.CertData)), nil
	case len(tls.CertData) > 0 || len(tls.KeyData) > 0:
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "Argo CD cluster Secret %s has a client certificate or key but not both", c.secret)
	case cfg.Username != "" || cfg.Password != "":
		return provider.Credential{}, provider.Errorf(provider.Unimplemented, "Argo CD cluster Secret %s uses basic authentication, which tug does not support", c.secret)
	default:
		return provider.Credential{}, provider.Errorf(provider.NotFound, "Argo CD cluster Secret %s has no credentials tug can use", c.secret)
	}
}

// awsCredential runs `aws eks get-token` for an awsAuthConfig, as Argo CD
// runs `argocd-k8s-auth aws`, with the cluster name and role. The profile
// in the config names one in Argo CD's environment, so it is not passed;
// the AWS CLI uses this machine's default credentials or AWS_PROFILE.
func awsCredential(c *cluster) (provider.Credential, error) {
	aws := c.config.AWSAuthConfig
	if aws.ClusterName == "" {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "Argo CD cluster Secret %s has an awsAuthConfig without a clusterName", c.secret)
	}
	args := []string{"eks", "get-token", "--cluster-name", aws.ClusterName}
	if aws.RoleARN != "" {
		args = append(args, "--role-arn", aws.RoleARN)
	}
	if region := eksRegion(c.server); region != "" {
		args = append(args, "--region", region)
	}
	cred := provider.Delegate("aws", args...)
	cred.Exec.APIVersion = "client.authentication.k8s.io/v1beta1"
	cred.Exec.InstallHint = awsCLIHint
	return cred, nil
}

// eksRegion returns the region in an EKS endpoint such as
// https://0123ABCD.gr7.eu-west-1.eks.amazonaws.com, or "" for any other
// server. The AWS CLI needs a region to sign the token request.
func eksRegion(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	labels := strings.Split(u.Hostname(), ".")
	for i := 1; i+2 < len(labels); i++ {
		if labels[i] == "eks" && labels[i+1] == "amazonaws" {
			return labels[i-1]
		}
	}
	return ""
}
