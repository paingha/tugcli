// Package clusterprofile is the built-in provider for the SIG Multicluster
// cluster inventory: ClusterProfile objects (multicluster.x-k8s.io) on a hub
// cluster already in tug's catalog. Each profile's status lists access
// providers, each with the cluster's server and CA. As KEP-5339 describes
// (since merged into KEP-4322), tug takes the first access provider that a
// local providers file names and runs the exec plugin that file configures
// for it. The hub says where a cluster is but never which command to run;
// it adds arguments or environment variables only where the file allows.
// A plugin that sets provideClusterInfo is told about the access provider's
// cluster, with its client.authentication.k8s.io/exec extension as
// spec.cluster.config, as the library does.
//
// The API and the plugin protocol are alpha. This package follows the
// v1alpha2 types and the providers file read by the cluster-inventory-api
// library's access package.
package clusterprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/builtin/management"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "clusterprofile"

// Account settings.
const (
	// ConfigCluster names the hub cluster in tug's catalog.
	ConfigCluster = management.ConfigCluster
	// ConfigNamespace limits the provider to one namespace, which the
	// cluster inventory uses for a ClusterSet; by default it reads every
	// namespace.
	ConfigNamespace = management.ConfigNamespace
	// ConfigProviderFile is the absolute path of the providers file, the
	// JSON that cluster-inventory-api consumers read through their
	// --clusterprofile-provider-file flag.
	ConfigProviderFile = "providerFile"
)

// Group is the cluster inventory API group.
const Group = "multicluster.x-k8s.io"

// versions are the ClusterProfile versions tug reads, in order of
// preference. v1alpha1 is deprecated in favour of v1alpha2; it has the
// same fields, plus the deprecated status.credentialProviders.
var versions = []string{"v1alpha2", "v1alpha1"}

// Extensions on an access provider's cluster that add to the exec plugin's
// arguments and environment, when the providers file allows it.
const (
	extensionArgs = "clusterprofiles.multicluster.x-k8s.io/exec/additional-args"
	extensionEnvs = "clusterprofiles.multicluster.x-k8s.io/exec/additional-envs"
)

// extensionExec is the extension that client-go reserves for an exec
// plugin's settings for one cluster. A plugin that sets provideClusterInfo
// gets it as spec.cluster.config.
const extensionExec = "client.authentication.k8s.io/exec"

// Policies for what a ClusterProfile may add to an exec plugin. Both
// default to Ignore, so the hub cannot change the command line unless the
// providers file says so.
const (
	PolicyIgnore            = "Ignore"
	PolicyAppend            = "Append"
	PolicyAppendIfNotExists = "AppendIfNotExists"
	PolicyReplace           = "Replace"
)

// Provider lists clusters from ClusterProfiles.
type Provider struct {
	clusterConfig management.ClusterConfig
}

var _ provider.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithClusterConfig sets how the provider reaches hub clusters in tug's
// catalog.
func WithClusterConfig(f management.ClusterConfig) Option {
	return func(p *Provider) { p.clusterConfig = f }
}

// New returns the ClusterProfile provider.
func New(opts ...Option) *Provider {
	p := &Provider{}
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
		Description:  "Clusters in a SIG Multicluster ClusterProfile inventory, on a hub cluster in the catalog",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// providersFile maps access provider names to exec plugins, in order of
// preference.
type providersFile struct {
	Providers []accessConfig `json:"providers"`
}

type accessConfig struct {
	Name                        string      `json:"name"`
	ExecConfig                  *execConfig `json:"execConfig"`
	ProfileSourcedCLIArgsPolicy string      `json:"profileSourcedCLIArgsPolicy"`
	ProfileSourcedEnvVarsPolicy string      `json:"profileSourcedEnvVarsPolicy"`
}

// execConfig is a kubeconfig exec section, with clientcmd's field names.
type execConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
	APIVersion         string `json:"apiVersion"`
	InstallHint        string `json:"installHint"`
	ProvideClusterInfo bool   `json:"provideClusterInfo"`
	InteractiveMode    string `json:"interactiveMode"`
}

// loadProviders reads and checks the account's providers file. It returns
// the path for messages.
func loadProviders(acct provider.Account) ([]accessConfig, string, error) {
	path := acct.ConfigString(ConfigProviderFile)
	if path == "" {
		return nil, "", provider.Errorf(provider.InvalidConfig, "%s account %q has no %s setting", Name, acct.ID, ConfigProviderFile).
			WithHint(fmt.Sprintf("name the JSON file that maps access providers to exec plugins: --set %s=/absolute/path/providers.json", ConfigProviderFile))
	}
	if !filepath.IsAbs(path) {
		return nil, path, provider.Errorf(provider.InvalidConfig, "%s %q is not an absolute path", ConfigProviderFile, path).
			WithHint("tug reads it from wherever kubectl runs, so give the full path")
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, provider.Errorf(provider.NotFound, "providers file %s does not exist", path)
	}
	if err != nil {
		return nil, path, provider.Errorf(provider.InvalidConfig, "reading providers file: %v", err)
	}
	var f providersFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, path, provider.Errorf(provider.InvalidConfig, "parsing providers file %s: %v", path, err)
	}
	for i, ac := range f.Providers {
		switch {
		case ac.Name == "":
			return nil, path, provider.Errorf(provider.InvalidConfig, "provider %d in %s has no name", i, path)
		case ac.ExecConfig == nil || ac.ExecConfig.Command == "":
			return nil, path, provider.Errorf(provider.InvalidConfig, "provider %q in %s has no execConfig.command", ac.Name, path)
		case !slices.Contains([]string{"", PolicyIgnore, PolicyAppend}, ac.ProfileSourcedCLIArgsPolicy):
			return nil, path, provider.Errorf(provider.InvalidConfig, "provider %q in %s: profileSourcedCLIArgsPolicy %q is not %s or %s",
				ac.Name, path, ac.ProfileSourcedCLIArgsPolicy, PolicyAppend, PolicyIgnore)
		case !slices.Contains([]string{"", PolicyIgnore, PolicyAppendIfNotExists, PolicyReplace}, ac.ProfileSourcedEnvVarsPolicy):
			return nil, path, provider.Errorf(provider.InvalidConfig, "provider %q in %s: profileSourcedEnvVarsPolicy %q is not %s, %s or %s",
				ac.Name, path, ac.ProfileSourcedEnvVarsPolicy, PolicyAppendIfNotExists, PolicyReplace, PolicyIgnore)
		}
	}
	return f.Providers, path, nil
}

// profile is the part of a ClusterProfile tug reads.
type profile struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Status   struct {
		Version struct {
			Kubernetes string `json:"kubernetes"`
		} `json:"version"`
		AccessProviders []access `json:"accessProviders"`
		// CredentialProviders is the deprecated v1alpha1 name.
		CredentialProviders []access `json:"credentialProviders"`
	} `json:"status"`
}

type access struct {
	Name    string `json:"name"`
	Cluster struct {
		Server                   string `json:"server"`
		TLSServerName            string `json:"tls-server-name"`
		InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify"`
		CertificateAuthorityData []byte `json:"certificate-authority-data"`
		ProxyURL                 string `json:"proxy-url"`
		Extensions               []struct {
			Name      string `json:"name"`
			Extension any    `json:"extension"`
		} `json:"extensions"`
	} `json:"cluster"`
}

func (pr *profile) id() string { return pr.Metadata.Namespace + "/" + pr.Metadata.Name }

func (pr *profile) access() []access {
	if len(pr.Status.AccessProviders) > 0 {
		return pr.Status.AccessProviders
	}
	return pr.Status.CredentialProviders
}

// match returns the first provider in the file that the profile offers,
// in the file's order, as the cluster-inventory-api library does.
func (pr *profile) match(providers []accessConfig) (*accessConfig, *access) {
	offered := pr.access()
	for i := range providers {
		if j := slices.IndexFunc(offered, func(a access) bool { return a.Name == providers[i].Name }); j >= 0 {
			return &providers[i], &offered[j]
		}
	}
	return nil, nil
}

// List returns one cluster per ClusterProfile that offers an access
// provider the providers file names. Profiles that offer none, or have no
// status yet, are left out; when every profile is left out, List fails
// and says which access providers the hub offers.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	providers, path, err := loadProviders(req.Account)
	if err != nil {
		return nil, err
	}
	mc, res, err := p.connect(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	list, err := res.Namespace(req.Account.ConfigString(ConfigNamespace)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, mc.Error(err, "listing ClusterProfiles")
	}
	var out []provider.Cluster
	offered := map[string]bool{}
	for i := range list.Items {
		var pr profile
		if err := management.Decode(&list.Items[i], &pr); err != nil {
			return nil, provider.Errorf(provider.Internal, "decoding ClusterProfile %s/%s: %v", list.Items[i].GetNamespace(), list.Items[i].GetName(), err)
		}
		ac, acc := pr.match(providers)
		if acc == nil || acc.Cluster.Server == "" {
			for _, a := range pr.access() {
				offered[a.Name] = true
			}
			continue
		}
		pc := provider.Cluster{
			ID:                    pr.id(),
			Name:                  pr.Metadata.Name,
			Version:               pr.Status.Version.Kubernetes,
			Endpoints:             provider.Endpoints{Public: acc.Cluster.Server},
			CAData:                acc.Cluster.CertificateAuthorityData,
			TLSServerName:         acc.Cluster.TLSServerName,
			InsecureSkipTLSVerify: acc.Cluster.InsecureSkipTLSVerify,
			ProxyURL:              acc.Cluster.ProxyURL,
			Labels:                pr.Metadata.Labels,
		}
		if cred, err := delegate(ac, acc, &pr); err == nil {
			pc.Credential = &cred
		}
		out = append(out, pc)
	}
	if len(out) == 0 && len(list.Items) > 0 {
		names := "none yet"
		if len(offered) > 0 {
			names = strings.Join(slices.Sorted(maps.Keys(offered)), ", ")
		}
		return nil, provider.Errorf(provider.InvalidConfig, "none of the %d ClusterProfiles on %s offers an access provider that %s configures; they offer %s",
			len(list.Items), mc.Name, path, names).
			WithHint("add an entry for one of those names to the providers file")
	}
	return out, nil
}

// Credential reads the ClusterProfile again and returns the exec plugin the
// providers file configures for its access provider.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	ns, name, ok := strings.Cut(req.ClusterID, "/")
	if !ok || ns == "" || name == "" {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster id %q is not namespace/name", req.ClusterID)
	}
	providers, path, err := loadProviders(req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	mc, res, err := p.connect(ctx, req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	u, err := res.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return provider.Credential{}, mc.Error(err, "reading ClusterProfile "+req.ClusterID)
	}
	var pr profile
	if err := management.Decode(u, &pr); err != nil {
		return provider.Credential{}, provider.Errorf(provider.Internal, "decoding ClusterProfile %s: %v", req.ClusterID, err)
	}
	ac, acc := pr.match(providers)
	if acc == nil {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "ClusterProfile %s on %s offers no access provider that %s configures", req.ClusterID, mc.Name, path)
	}
	return delegate(ac, acc, &pr)
}

func (p *Provider) connect(ctx context.Context, acct provider.Account) (*management.Cluster, dynamic.NamespaceableResourceInterface, error) {
	mc, err := management.Connect(ctx, p.clusterConfig, Name, acct)
	if err != nil {
		return nil, nil, err
	}
	v, err := mc.ServedVersion(ctx, Group, versions...)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(mc.Config)
	if err != nil {
		return nil, nil, provider.Errorf(provider.InvalidConfig, "hub cluster %s: %v", mc.Name, err)
	}
	return mc, dyn.Resource(schema.GroupVersionResource{Group: Group, Version: v, Resource: "clusterprofiles"}), nil
}

// delegate builds the exec plugin credential for an access provider: the
// command from the providers file, with the arguments and environment the
// profile's extensions add where the file's policies allow it, and the
// cluster's exec extension when the plugin asks for cluster information.
func delegate(ac *accessConfig, acc *access, pr *profile) (provider.Credential, error) {
	e := ac.ExecConfig
	args := slices.Clone(e.Args)
	env := make(map[string]string, len(e.Env))
	for _, kv := range e.Env {
		env[kv.Name] = kv.Value
	}
	var clusterConfig json.RawMessage
	for _, ext := range acc.Cluster.Extensions {
		switch {
		case ext.Name == extensionExec && e.ProvideClusterInfo:
			// The library reads the extensions into a map, so the last
			// one of a name wins there too.
			b, err := json.Marshal(ext.Extension)
			if err != nil {
				return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "ClusterProfile %s: encoding %s: %v", pr.id(), extensionExec, err)
			}
			clusterConfig = b
		case ext.Name == extensionArgs && ac.ProfileSourcedCLIArgsPolicy == PolicyAppend:
			var extra []string
			if err := convert(ext.Extension, &extra); err != nil {
				return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "ClusterProfile %s: %s is not a list of strings: %v", pr.id(), extensionArgs, err)
			}
			args = append(args, extra...)
		case ext.Name == extensionEnvs && ac.ProfileSourcedEnvVarsPolicy != "" && ac.ProfileSourcedEnvVarsPolicy != PolicyIgnore:
			var extra map[string]string
			if err := convert(ext.Extension, &extra); err != nil {
				return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "ClusterProfile %s: %s is not a map of strings: %v", pr.id(), extensionEnvs, err)
			}
			for k, v := range extra {
				if _, set := env[k]; !set || ac.ProfileSourcedEnvVarsPolicy == PolicyReplace {
					env[k] = v
				}
			}
		}
	}
	cred := provider.Delegate(e.Command, args...)
	if len(env) > 0 {
		cred.Exec.Env = env
	}
	cred.Exec.APIVersion = e.APIVersion
	cred.Exec.InteractiveMode = e.InteractiveMode
	cred.Exec.InstallHint = e.InstallHint
	cred.Exec.ProvideClusterInfo = e.ProvideClusterInfo
	cred.Exec.ClusterConfig = clusterConfig
	return cred, nil
}

// convert decodes an extension, which the API server returns as JSON, the
// way the library does: as YAML, which JSON is a subset of.
func convert(v any, out any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}
