// Package kubeconfig is the built-in provider that imports clusters from
// kubeconfig files. Embedded tokens and keys are returned inline so that tug
// moves them to the keychain; the source files are only ever read.
package kubeconfig

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "kubeconfig"

// Account config keys.
const (
	// ConfigPath holds the kubeconfig file path.
	ConfigPath = "path"
	// ConfigSkipLocal, when true, leaves out contexts of local clusters
	// (kind, k3d, minikube, Docker Desktop, OrbStack, Rancher Desktop),
	// which the local provider imports instead. Accounts sets it on the
	// files it discovers; files named on the command line import everything.
	ConfigSkipLocal = "skipLocal"
)

// fileCredentialTTL bounds how long a credential read from a referenced file
// (tokenFile, client-certificate) is cached, so rotated files are picked up.
const fileCredentialTTL = time.Hour

// Provider imports kubeconfig files.
type Provider struct {
	home     string
	getenv   func(string) string
	skipDirs []string
	now      func() time.Time
}

// Option configures a Provider.
type Option func(*Provider)

// WithHome sets the home directory used to find ~/.kube.
func WithHome(dir string) Option { return func(p *Provider) { p.home = dir } }

// WithGetenv replaces os.Getenv, for tests.
func WithGetenv(f func(string) string) Option { return func(p *Provider) { p.getenv = f } }

// WithSkipDirs excludes kubeconfig files under these directories, such as
// tug's own session directory.
func WithSkipDirs(dirs ...string) Option {
	return func(p *Provider) { p.skipDirs = append(p.skipDirs, dirs...) }
}

// New returns the kubeconfig provider.
func New(opts ...Option) *Provider {
	p := &Provider{getenv: os.Getenv, now: time.Now}
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
		Description:  "Clusters in kubeconfig files (~/.kube/config, $KUBECONFIG, or files you name)",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// Accounts returns one account per file that Files finds.
func (p *Provider) Accounts(_ context.Context) ([]provider.Account, error) {
	var accounts []provider.Account
	used := map[string]bool{}
	for _, path := range p.Files() {
		id := AccountID(path, p.home)
		for base, i := id, 2; used[id]; i++ {
			id = fmt.Sprintf("%s-%d", base, i)
		}
		used[id] = true
		accounts = append(accounts, provider.Account{
			ID:          id,
			Description: path,
			Config:      map[string]any{ConfigPath: path, ConfigSkipLocal: true},
		})
	}
	return accounts, nil
}

// Files returns the kubeconfig files on this machine, as absolute paths:
// ~/.kube/config, the files in $KUBECONFIG, and other *.yaml, *.yml and
// *.kubeconfig files directly under ~/.kube. The local provider reads the same
// files, so a local context that skipLocal leaves out is always found there.
func (p *Provider) Files() []string {
	var paths []string
	add := func(path string) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil || slices.Contains(paths, abs) || p.skipped(abs) {
			return
		}
		if st, err := os.Stat(abs); err != nil || !st.Mode().IsRegular() {
			return
		}
		paths = append(paths, abs)
	}
	kubeDir := filepath.Join(p.home, ".kube")
	add(filepath.Join(kubeDir, "config"))
	for _, f := range p.envFiles() {
		add(f)
	}
	for _, pattern := range []string{"*.yaml", "*.yml", "*.kubeconfig"} {
		matches, _ := filepath.Glob(filepath.Join(kubeDir, pattern))
		for _, m := range matches {
			add(m)
		}
	}
	return paths
}

// envFiles returns the files $KUBECONFIG names. Inside a tug session it names
// the session's own file, in a skipped directory; that entry stands for the
// files in $TUG_ORIG_KUBECONFIG, the value the session replaced.
func (p *Provider) envFiles() []string {
	var out []string
	for _, f := range filepath.SplitList(p.getenv("KUBECONFIG")) {
		if abs, err := filepath.Abs(f); err == nil && p.skipped(abs) {
			out = append(out, filepath.SplitList(p.getenv(session.EnvOrigKubeconfig))...)
			continue
		}
		out = append(out, f)
	}
	return out
}

func (p *Provider) skipped(path string) bool {
	for _, d := range p.skipDirs {
		if rel, err := filepath.Rel(d, path); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

var nonID = regexp.MustCompile(`[^a-z0-9]+`)

// AccountID derives a short account ID from a kubeconfig path: "default" for
// ~/.kube/config, otherwise the file name.
func AccountID(path, home string) string {
	if home != "" && path == filepath.Join(home, ".kube", "config") {
		return "default"
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	id := strings.Trim(nonID.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if id == "" || id == "config" {
		id = "file"
	}
	return id
}

// List returns one cluster per context in the account's file.
func (p *Provider) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	path, cfg, err := load(req.Account)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	slices.Sort(names)

	skipLocal, _ := req.Account.Config[ConfigSkipLocal].(bool)
	var out []provider.Cluster
	for _, name := range names {
		if skipLocal && LocalDistro(cfg, name) != "" {
			continue
		}
		pc, ok, err := p.ContextCluster(cfg, name, path)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, pc)
		}
	}
	return out, nil
}

// ContextCluster converts one context of a kubeconfig into a cluster, with
// the credential inline when the file embeds it. path is the file the
// kubeconfig came from, for resolving relative paths; it may be "" for
// kubeconfigs with embedded data. ok is false for contexts tug skips: its own
// session files and contexts without a cluster.
func (p *Provider) ContextCluster(cfg *clientcmdapi.Config, name, path string) (provider.Cluster, bool, error) {
	kctx, ok := cfg.Contexts[name]
	if !ok {
		return provider.Cluster{}, false, nil
	}
	auth := cfg.AuthInfos[kctx.AuthInfo]
	if auth != nil && isTugExec(auth.Exec) {
		return provider.Cluster{}, false, nil
	}
	cl, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cl.Server == "" {
		return provider.Cluster{}, false, nil
	}
	pc := provider.Cluster{
		ID:                    name,
		Name:                  name,
		Endpoints:             provider.Endpoints{Public: cl.Server},
		TLSServerName:         cl.TLSServerName,
		InsecureSkipTLSVerify: cl.InsecureSkipTLSVerify,
		ProxyURL:              cl.ProxyURL,
		DefaultNamespace:      kctx.Namespace,
	}
	ca, err := caData(cl, path)
	if err != nil {
		return provider.Cluster{}, false, provider.Errorf(provider.InvalidConfig, "context %s: %v", name, err)
	}
	pc.CAData = ca
	if auth != nil {
		if c, inline, err := p.credential(auth, path); err == nil && inline {
			pc.Credential = &c
		}
	}
	return pc, true, nil
}

// ContextCredential returns the credential of one context's user.
func (p *Provider) ContextCredential(cfg *clientcmdapi.Config, name, path string) (provider.Credential, error) {
	kctx, ok := cfg.Contexts[name]
	if !ok {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "context %q is not in the kubeconfig", name)
	}
	auth, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "context %q has no user", name)
	}
	c, _, err := p.credential(auth, path)
	return c, err
}

// Local distributions recognised by LocalDistro.
const (
	DistroKind           = "kind"
	DistroK3d            = "k3d"
	DistroMinikube       = "minikube"
	DistroDockerDesktop  = "docker-desktop"
	DistroOrbStack       = "orbstack"
	DistroRancherDesktop = "rancher-desktop"
)

// LocalDistro names the local Kubernetes distribution behind a context, or
// returns "" for any other cluster. It goes by the names the tools give
// their contexts, kind's and k3d's loopback servers, and the cluster_info
// extension minikube writes.
func LocalDistro(cfg *clientcmdapi.Config, name string) string {
	kctx, ok := cfg.Contexts[name]
	if !ok {
		return ""
	}
	switch name {
	case DistroDockerDesktop, DistroOrbStack, DistroRancherDesktop:
		return name
	}
	cl := cfg.Clusters[kctx.Cluster]
	if cl == nil {
		return ""
	}
	switch {
	case strings.HasPrefix(name, "kind-") && loopback(cl.Server):
		return DistroKind
	case strings.HasPrefix(name, "k3d-") && loopback(cl.Server):
		return DistroK3d
	case isMinikube(cl.Extensions):
		return DistroMinikube
	}
	return ""
}

func loopback(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	switch host := u.Hostname(); host {
	case "localhost", "0.0.0.0", "host.docker.internal":
		return true
	default:
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
}

// isMinikube looks for the cluster_info extension minikube adds to the
// clusters it writes.
func isMinikube(ext map[string]runtime.Object) bool {
	obj, ok := ext["cluster_info"]
	if !ok {
		return false
	}
	u, ok := obj.(*runtime.Unknown)
	return ok && strings.Contains(string(u.Raw), "minikube.sigs.k8s.io")
}

// Credential reads the context's user from the file again.
func (p *Provider) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	path, cfg, err := load(req.Account)
	if err != nil {
		return provider.Credential{}, err
	}
	kctx, ok := cfg.Contexts[req.ClusterID]
	if !ok {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "context %q is no longer in %s", req.ClusterID, path)
	}
	auth, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return provider.Credential{}, provider.Errorf(provider.NotFound, "context %q has no user in %s", req.ClusterID, path)
	}
	c, _, err := p.credential(auth, path)
	return c, err
}

func load(acct provider.Account) (string, *clientcmdapi.Config, error) {
	path := acct.ConfigString(ConfigPath)
	if path == "" {
		return "", nil, provider.Errorf(provider.InvalidConfig, "account %q has no %s", acct.ID, ConfigPath)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, provider.Errorf(provider.NotFound, "%s no longer exists", path).
			WithHint("credentials imported earlier are still in the keychain; run `tug import kubeconfig <file>` to import it again")
	}
	if err != nil {
		return "", nil, provider.Errorf(provider.InvalidConfig, "reading %s: %v", path, err)
	}
	return path, cfg, nil
}

// credential converts a kubeconfig user. inline is true when the secret is
// embedded in the file and should be moved to the keychain at import.
func (p *Provider) credential(auth *clientcmdapi.AuthInfo, path string) (c provider.Credential, inline bool, err error) {
	switch {
	case auth.Exec != nil:
		return provider.Credential{Type: provider.CredentialDelegate, Exec: execConfig(auth.Exec, path)}, true, nil

	case auth.Token != "":
		return provider.Token(auth.Token, provider.NoExpiry), true, nil

	case auth.TokenFile != "":
		b, err := os.ReadFile(resolve(auth.TokenFile, path))
		if err != nil {
			return c, false, provider.Errorf(provider.InvalidConfig, "reading token file: %v", err)
		}
		return provider.Token(strings.TrimSpace(string(b)), p.now().Add(fileCredentialTTL)), false, nil

	case len(auth.ClientCertificateData) > 0 && len(auth.ClientKeyData) > 0:
		return provider.ClientCert(auth.ClientCertificateData, auth.ClientKeyData, certExpiry(auth.ClientCertificateData)), true, nil

	case auth.ClientCertificate != "" && auth.ClientKey != "":
		cert, err := os.ReadFile(resolve(auth.ClientCertificate, path))
		if err != nil {
			return c, false, provider.Errorf(provider.InvalidConfig, "reading client certificate: %v", err)
		}
		key, err := os.ReadFile(resolve(auth.ClientKey, path))
		if err != nil {
			return c, false, provider.Errorf(provider.InvalidConfig, "reading client key: %v", err)
		}
		exp := p.now().Add(fileCredentialTTL)
		if na := certExpiry(cert); !na.IsZero() && na.Before(exp) {
			exp = na
		}
		return provider.ClientCert(cert, key, exp), false, nil

	case auth.AuthProvider != nil:
		return c, false, provider.Errorf(provider.Unimplemented, "auth-provider %q is not supported; switch the kubeconfig to an exec plugin", auth.AuthProvider.Name)

	case auth.Username != "" || auth.Password != "":
		return c, false, provider.Errorf(provider.Unimplemented, "basic authentication is not supported")

	default:
		return c, false, provider.Errorf(provider.NotFound, "the kubeconfig user has no credentials")
	}
}

func execConfig(e *clientcmdapi.ExecConfig, path string) *provider.ExecConfig {
	cmd := e.Command
	if strings.ContainsRune(cmd, filepath.Separator) && !filepath.IsAbs(cmd) {
		cmd = resolve(cmd, path)
	}
	out := &provider.ExecConfig{
		Command:         cmd,
		Args:            slices.Clone(e.Args),
		APIVersion:      e.APIVersion,
		InteractiveMode: string(e.InteractiveMode),
		InstallHint:     e.InstallHint,
	}
	if len(e.Env) > 0 {
		out.Env = make(map[string]string, len(e.Env))
		for _, kv := range e.Env {
			out.Env[kv.Name] = kv.Value
		}
	}
	return out
}

// isTugExec reports whether a user runs `tug auth token`, which marks a
// kubeconfig that tug wrote itself.
func isTugExec(e *clientcmdapi.ExecConfig) bool {
	if e == nil {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(e.Command), ".exe")
	return base == "tug" && len(e.Args) >= 2 && e.Args[0] == "auth" && e.Args[1] == "token"
}

func caData(cl *clientcmdapi.Cluster, path string) ([]byte, error) {
	if len(cl.CertificateAuthorityData) > 0 {
		return cl.CertificateAuthorityData, nil
	}
	if cl.CertificateAuthority == "" {
		return nil, nil
	}
	b, err := os.ReadFile(resolve(cl.CertificateAuthority, path))
	if err != nil {
		return nil, fmt.Errorf("reading certificate-authority: %w", err)
	}
	return b, nil
}

// resolve makes a path from a kubeconfig file absolute, relative to the file.
func resolve(p, kubeconfigPath string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(kubeconfigPath), p)
}

// certExpiry returns the NotAfter of the first certificate in PEM data, or the
// zero time.
func certExpiry(certPEM []byte) time.Time {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}
