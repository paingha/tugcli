// Package capi is the built-in provider for Cluster API. It lists the
// cluster.x-k8s.io Cluster objects on a management cluster already in tug's
// catalog, and takes each workload cluster's credential from the
// <cluster>-kubeconfig Secret that the control plane provider writes next to
// it, as the Cluster API contract requires.
package capi

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/builtin/management"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "capi"

// Account settings.
const (
	// ConfigCluster names the management cluster in tug's catalog.
	ConfigCluster = management.ConfigCluster
	// ConfigNamespace limits the provider to one namespace; by default it
	// reads Clusters in every namespace.
	ConfigNamespace = management.ConfigNamespace
)

// Group is the Cluster API group.
const Group = "cluster.x-k8s.io"

// versions are the Cluster API versions tug reads, in order of preference.
// v1beta1 is served by every Cluster API release still in use, through
// conversion since v1.11 made v1beta2 the storage version; v1beta2 is the
// fallback for releases that no longer serve v1beta1. The fields tug reads
// are the same in both.
var versions = []string{"v1beta1", "v1beta2"}

// The kubeconfig Secret, as the control plane contract defines it.
const (
	kubeconfigSuffix = "-kubeconfig"
	kubeconfigKey    = "value"
)

// How long a credential read from a kubeconfig Secret is used before tug
// reads the Secret again. Control plane providers rotate client
// certificates long before they expire, and providers for managed
// Kubernetes, such as CAPA for EKS, write short-lived tokens that they
// refresh every few minutes.
const (
	certTTL  = time.Hour
	tokenTTL = 5 * time.Minute
)

// Provider lists Cluster API clusters.
type Provider struct {
	clusterConfig management.ClusterConfig
	now           func() time.Time
}

var _ provider.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithClusterConfig sets how the provider reaches management clusters in
// tug's catalog.
func WithClusterConfig(f management.ClusterConfig) Option {
	return func(p *Provider) { p.clusterConfig = f }
}

// WithNow replaces time.Now, for tests.
func WithNow(f func() time.Time) Option { return func(p *Provider) { p.now = f } }

// New returns the Cluster API provider.
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
		Description:  "Cluster API workload clusters, read from a management cluster in the catalog",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// cluster is the part of a Cluster object tug reads.
type cluster struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		ControlPlaneEndpoint struct {
			Host string `json:"host"`
			Port int32  `json:"port"`
		} `json:"controlPlaneEndpoint"`
		Topology *struct {
			Version string `json:"version"`
		} `json:"topology"`
	} `json:"spec"`
}

// List returns the Clusters whose control plane has an endpoint and whose
// kubeconfig Secret exists. A cluster still being provisioned has neither;
// the next sync picks it up.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	mc, err := management.Connect(ctx, p.clusterConfig, Name, req.Account)
	if err != nil {
		return nil, err
	}
	v, err := mc.ServedVersion(ctx, Group, versions...)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(mc.Config)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "management cluster %s: %v", mc.Name, err)
	}
	secrets, err := corev1client.NewForConfig(mc.Config)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "management cluster %s: %v", mc.Name, err)
	}
	gvr := schema.GroupVersionResource{Group: Group, Version: v, Resource: "clusters"}
	list, err := dyn.Resource(gvr).Namespace(req.Account.ConfigString(ConfigNamespace)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, mc.Error(err, "listing Cluster API clusters")
	}
	var out []provider.Cluster
	for i := range list.Items {
		var c cluster
		if err := management.Decode(&list.Items[i], &c); err != nil {
			return nil, provider.Errorf(provider.Internal, "decoding Cluster %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
		ep := c.Spec.ControlPlaneEndpoint
		if ep.Host == "" {
			continue
		}
		// Only a failure to read the Secret fails the listing. A Secret
		// that is not there yet leaves the cluster out, and one tug cannot
		// parse leaves it without a CA, so that Credential reports why.
		kc, err := readKubeconfig(ctx, mc, secrets, c.Metadata.Namespace, c.Metadata.Name)
		if err != nil {
			switch provider.CodeOf(err) {
			case provider.NotFound:
				continue
			case provider.InvalidConfig:
				kc = nil
			default:
				return nil, err
			}
		}
		pc := provider.Cluster{
			ID:        c.Metadata.Namespace + "/" + c.Metadata.Name,
			Name:      c.Metadata.Name,
			Endpoints: provider.Endpoints{Public: endpoint(ep.Host, ep.Port)},
			CAData:    caData(kc),
			Labels:    c.Metadata.Labels,
		}
		if c.Spec.Topology != nil {
			pc.Version = c.Spec.Topology.Version
		}
		if kc != nil {
			if cred, err := p.credential(kc); err == nil {
				pc.Credential = &cred
			}
		}
		out = append(out, pc)
	}
	return out, nil
}

// endpoint turns a control plane endpoint into a URL. Cluster API v1beta1
// allows a zero port, which means the HTTPS default.
func endpoint(host string, port int32) string {
	if port == 0 {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// Credential reads the cluster's kubeconfig Secret again and returns the
// client certificate or token in it.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	ns, name, ok := strings.Cut(req.ClusterID, "/")
	if !ok || ns == "" || name == "" {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster id %q is not namespace/name", req.ClusterID)
	}
	mc, err := management.Connect(ctx, p.clusterConfig, Name, req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	secrets, err := corev1client.NewForConfig(mc.Config)
	if err != nil {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "management cluster %s: %v", mc.Name, err)
	}
	kc, err := readKubeconfig(ctx, mc, secrets, ns, name)
	if err != nil {
		return provider.Credential{}, err
	}
	return p.credential(kc)
}

// readKubeconfig reads and parses a cluster's kubeconfig Secret. It fails
// with NotFound when the Secret does not exist yet.
func readKubeconfig(ctx context.Context, mc *management.Cluster, secrets corev1client.SecretsGetter, ns, name string) (*kubeconfig, error) {
	secretName := name + kubeconfigSuffix
	s, err := secrets.Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, provider.Errorf(provider.NotFound, "Cluster %s/%s has no Secret %s on %s yet", ns, name, secretName, mc.Name).
			WithHint("the cluster may still be provisioning; run `tug sync` once its control plane is up")
	}
	if err != nil {
		return nil, mc.Error(err, fmt.Sprintf("reading Secret %s/%s", ns, secretName))
	}
	data := s.Data[kubeconfigKey]
	if len(data) == 0 {
		return nil, provider.Errorf(provider.NotFound, "Secret %s/%s on %s has no %q key", ns, secretName, mc.Name, kubeconfigKey)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "Secret %s/%s on %s does not hold a kubeconfig: %v", ns, secretName, mc.Name, err)
	}
	return &kubeconfig{secret: ns + "/" + secretName, cfg: cfg}, nil
}

// kubeconfig is a parsed kubeconfig Secret.
type kubeconfig struct {
	secret string
	cfg    *clientcmdapi.Config
}

// context returns the kubeconfig's current context, or its only one.
func (k *kubeconfig) context() (*clientcmdapi.Context, error) {
	name := k.cfg.CurrentContext
	if name == "" && len(k.cfg.Contexts) == 1 {
		for n := range k.cfg.Contexts {
			name = n
		}
	}
	kctx, ok := k.cfg.Contexts[name]
	if !ok {
		return nil, provider.Errorf(provider.InvalidConfig, "the kubeconfig in Secret %s has no current context", k.secret)
	}
	return kctx, nil
}

func caData(k *kubeconfig) []byte {
	if k == nil {
		return nil
	}
	kctx, err := k.context()
	if err != nil {
		return nil
	}
	if cl, ok := k.cfg.Clusters[kctx.Cluster]; ok {
		return cl.CertificateAuthorityData
	}
	return nil
}

// credential returns the token or client certificate embedded in the
// kubeconfig. The kubeconfig was written for the management cluster's
// controllers, so tug refuses anything that would run a command or read a
// file on this machine.
func (p *Provider) credential(k *kubeconfig) (provider.Credential, error) {
	kctx, err := k.context()
	if err != nil {
		return provider.Credential{}, err
	}
	auth, ok := k.cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "the kubeconfig in Secret %s has no user", k.secret)
	}
	now := p.now()
	switch {
	case auth.Token != "":
		return provider.Token(auth.Token, now.Add(tokenTTL)), nil
	case len(auth.ClientCertificateData) > 0 && len(auth.ClientKeyData) > 0:
		return provider.ClientCert(auth.ClientCertificateData, auth.ClientKeyData,
			management.Expiry(now, certTTL, auth.ClientCertificateData)), nil
	case auth.Exec != nil:
		return provider.Credential{}, provider.Errorf(provider.Unimplemented,
			"the kubeconfig in Secret %s runs %s; tug does not run commands read from a management cluster", k.secret, auth.Exec.Command)
	case auth.AuthProvider != nil:
		return provider.Credential{}, provider.Errorf(provider.Unimplemented, "the kubeconfig in Secret %s uses auth-provider %q, which tug does not support", k.secret, auth.AuthProvider.Name)
	case auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "":
		return provider.Credential{}, provider.Errorf(provider.Unimplemented, "the kubeconfig in Secret %s refers to files on another machine; tug needs embedded data", k.secret)
	case auth.Username != "" || auth.Password != "":
		return provider.Credential{}, provider.Errorf(provider.Unimplemented, "the kubeconfig in Secret %s uses basic authentication, which tug does not support", k.secret)
	default:
		return provider.Credential{}, provider.Errorf(provider.NotFound, "the kubeconfig in Secret %s has no credentials", k.secret)
	}
}
