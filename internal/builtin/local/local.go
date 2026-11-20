// Package local is the built-in provider for Kubernetes clusters running on
// this machine. kind and k3d clusters come from their own CLIs, so they are
// found even when no kubeconfig mentions them and their certificates are
// fresh after a recreate. minikube, Docker Desktop, OrbStack and Rancher
// Desktop clusters come from the contexts those tools write. Each tool is an
// account of its own, so when Docker is stopped only kind's and k3d's
// sources fail, and their clusters stay in the catalog.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/builtin/kubeconfig"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "local"

// ConfigDistro is the account setting naming the tool; the account ID is
// used when it is absent.
const ConfigDistro = "distro"

// Labels set on every local cluster.
const (
	LabelLocal  = "local"
	LabelDistro = "distro"
)

// fileDistros are the tools found through the contexts they write.
var fileDistros = []string{
	kubeconfig.DistroMinikube, kubeconfig.DistroDockerDesktop,
	kubeconfig.DistroOrbStack, kubeconfig.DistroRancherDesktop,
}

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Provider lists local clusters.
type Provider struct {
	kc       *kubeconfig.Provider
	run      Runner
	lookPath func(string) (string, error)
	getenv   func(string) string
	home     string
	skipDirs []string
}

var _ provider.AccountLister = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithRunner replaces running commands, for tests.
func WithRunner(r Runner) Option { return func(p *Provider) { p.run = r } }

// WithLookPath replaces exec.LookPath, for tests.
func WithLookPath(f func(string) (string, error)) Option {
	return func(p *Provider) { p.lookPath = f }
}

// WithHome sets the home directory used to find ~/.kube and ~/.orbstack.
func WithHome(dir string) Option { return func(p *Provider) { p.home = dir } }

// WithGetenv replaces os.Getenv, for tests.
func WithGetenv(f func(string) string) Option { return func(p *Provider) { p.getenv = f } }

// WithSkipDirs excludes kubeconfig files under these directories, such as
// tug's own session directory, as kubeconfig.WithSkipDirs does.
func WithSkipDirs(dirs ...string) Option {
	return func(p *Provider) { p.skipDirs = append(p.skipDirs, dirs...) }
}

// New returns the local provider.
func New(opts ...Option) *Provider {
	p := &Provider{run: runCommand, lookPath: exec.LookPath, getenv: os.Getenv}
	for _, o := range opts {
		o(p)
	}
	if p.home == "" {
		p.home, _ = os.UserHomeDir()
	}
	p.kc = kubeconfig.New(kubeconfig.WithHome(p.home), kubeconfig.WithGetenv(p.getenv), kubeconfig.WithSkipDirs(p.skipDirs...))
	return p
}

// runCommand runs a command, turning a failure into an error that carries
// the first line of its stderr.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		line, _, _ := strings.Cut(strings.TrimSpace(string(exitErr.Stderr)), "\n")
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), line)
	}
	return out, err
}

// Info implements registry.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      version.String(),
		Description:  "Clusters on this machine: kind, k3d, minikube, Docker Desktop, OrbStack, Rancher Desktop",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// Accounts returns one account per local tool found: kind and k3d when
// their CLIs are installed, and the others when a kubeconfig has their
// contexts.
func (p *Provider) Accounts(_ context.Context) ([]provider.Account, error) {
	var out []provider.Account
	add := func(distro, description string) {
		if !slices.ContainsFunc(out, func(a provider.Account) bool { return a.ID == distro }) {
			out = append(out, provider.Account{ID: distro, Description: description, Config: map[string]any{ConfigDistro: distro}})
		}
	}
	for _, tool := range []string{kubeconfig.DistroKind, kubeconfig.DistroK3d} {
		if path, err := p.lookPath(tool); err == nil {
			add(tool, tool+" clusters, through "+path)
		}
	}
	for _, f := range p.kubeconfigFiles() {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			continue
		}
		for name := range cfg.Contexts {
			if d := kubeconfig.LocalDistro(cfg, name); slices.Contains(fileDistros, d) {
				add(d, d+" contexts in "+f)
			}
		}
	}
	slices.SortFunc(out, func(a, b provider.Account) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// kubeconfigFiles are the files to look for local contexts in: every file the
// kubeconfig provider finds, whose imports leave local contexts to this
// provider, and OrbStack's own file.
func (p *Provider) kubeconfigFiles() []string {
	files := p.kc.Files()
	orb := filepath.Join(p.home, ".orbstack", "k8s", "config.yml")
	if st, err := os.Stat(orb); err == nil && st.Mode().IsRegular() && !slices.Contains(files, orb) {
		files = append(files, orb)
	}
	return files
}

func distro(acct provider.Account) string {
	if d := acct.ConfigString(ConfigDistro); d != "" {
		return d
	}
	return acct.ID
}

// List returns the account's clusters.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	d := distro(req.Account)
	var (
		clusters []provider.Cluster
		err      error
	)
	switch d {
	case kubeconfig.DistroKind, kubeconfig.DistroK3d:
		clusters, err = p.listCLI(ctx, d)
	case kubeconfig.DistroMinikube, kubeconfig.DistroDockerDesktop, kubeconfig.DistroOrbStack, kubeconfig.DistroRancherDesktop:
		clusters, err = p.listFiles(d)
	default:
		return nil, provider.Errorf(provider.InvalidConfig, "unknown local distribution %q", d).
			WithHint("use one of kind, k3d, minikube, docker-desktop, orbstack, rancher-desktop")
	}
	if err != nil {
		return nil, err
	}
	for i := range clusters {
		labels := clusters[i].Labels
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelLocal] = "true"
		labels[LabelDistro] = d
		labels["env"] = "dev"
		clusters[i].Labels = labels
	}
	return clusters, nil
}

// clusterNames lists the clusters a CLI knows.
func (p *Provider) clusterNames(ctx context.Context, tool string) ([]string, error) {
	switch tool {
	case kubeconfig.DistroKind:
		out, err := p.run(ctx, "kind", "get", "clusters")
		if err != nil {
			return nil, err
		}
		var names []string
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				names = append(names, line)
			}
		}
		return names, nil
	default: // k3d
		out, err := p.run(ctx, "k3d", "cluster", "list", "-o", "json")
		if err != nil {
			return nil, err
		}
		var list []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, fmt.Errorf("decoding k3d cluster list: %w", err)
		}
		names := make([]string, 0, len(list))
		for _, c := range list {
			names = append(names, c.Name)
		}
		return names, nil
	}
}

// cliKubeconfig asks a CLI for one cluster's kubeconfig.
func (p *Provider) cliKubeconfig(ctx context.Context, tool, name string) (*clientcmdapi.Config, error) {
	var (
		out []byte
		err error
	)
	if tool == kubeconfig.DistroKind {
		out, err = p.run(ctx, "kind", "get", "kubeconfig", "--name", name)
	} else {
		out, err = p.run(ctx, "k3d", "kubeconfig", "get", name)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		return nil, fmt.Errorf("parsing %s's kubeconfig for %s: %w", tool, name, err)
	}
	return cfg, nil
}

func unavailable(tool string, err error) error {
	return provider.Errorf(provider.Unavailable, "listing %s clusters: %v", tool, err).
		WithHint(tool + " needs Docker (or Podman) running; start it, then run `tug sync`")
}

// listCLI lists kind or k3d clusters with their kubeconfigs. The contexts
// are named kind-<name> and k3d-<name>, as the tools name them.
func (p *Provider) listCLI(ctx context.Context, tool string) ([]provider.Cluster, error) {
	names, err := p.clusterNames(ctx, tool)
	if err != nil {
		return nil, unavailable(tool, err)
	}
	out := make([]provider.Cluster, 0, len(names))
	for _, n := range names {
		cfg, err := p.cliKubeconfig(ctx, tool, n)
		if err != nil {
			return nil, unavailable(tool, err)
		}
		pc, ok, err := p.kc.ContextCluster(cfg, tool+"-"+n, "")
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, pc)
		}
	}
	return out, nil
}

// listFiles lists a tool's contexts across the kubeconfig files. A context
// in more than one file is listed once, from the first. Files that are not
// kubeconfigs, such as a kind cluster config saved in ~/.kube, are skipped,
// as Accounts and Credential skip them.
func (p *Provider) listFiles(d string) ([]provider.Cluster, error) {
	var out []provider.Cluster
	for _, f := range p.kubeconfigFiles() {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(cfg.Contexts))
		for name := range cfg.Contexts {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if kubeconfig.LocalDistro(cfg, name) != d || slices.ContainsFunc(out, func(c provider.Cluster) bool { return c.ID == name }) {
				continue
			}
			pc, ok, err := p.kc.ContextCluster(cfg, name, f)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, pc)
			}
		}
	}
	return out, nil
}

// Credential reads the context's credential again from its tool or file.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	d := distro(req.Account)
	switch d {
	case kubeconfig.DistroKind, kubeconfig.DistroK3d:
		name, ok := strings.CutPrefix(req.ClusterID, d+"-")
		if !ok {
			return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "%q is not a %s context", req.ClusterID, d)
		}
		cfg, err := p.cliKubeconfig(ctx, d, name)
		if err != nil {
			return provider.Credential{}, unavailable(d, err)
		}
		return p.kc.ContextCredential(cfg, req.ClusterID, "")
	default:
		for _, f := range p.kubeconfigFiles() {
			cfg, err := clientcmd.LoadFromFile(f)
			if err != nil {
				continue
			}
			if kubeconfig.LocalDistro(cfg, req.ClusterID) == d {
				return p.kc.ContextCredential(cfg, req.ClusterID, f)
			}
		}
		return provider.Credential{}, provider.Errorf(provider.NotFound, "no %s context %q in %s", d, req.ClusterID, strings.Join(p.kubeconfigFiles(), ", ")).
			WithHint("start the cluster so " + d + " writes its context again, then run `tug sync`")
	}
}
