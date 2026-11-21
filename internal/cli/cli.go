// Package cli defines tug's commands. Commands parse flags and call into the
// other internal packages; they hold no logic of their own beyond prompting.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/browser"
	"github.com/paingha/tugcli/internal/builtin/argocd"
	"github.com/paingha/tugcli/internal/builtin/aws"
	"github.com/paingha/tugcli/internal/builtin/azure"
	"github.com/paingha/tugcli/internal/builtin/capi"
	"github.com/paingha/tugcli/internal/builtin/clusterprofile"
	"github.com/paingha/tugcli/internal/builtin/google"
	"github.com/paingha/tugcli/internal/builtin/kubeconfig"
	"github.com/paingha/tugcli/internal/builtin/local"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/oidc"
	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/tunnel"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Main runs tug with the process's arguments and streams and returns the exit
// code. SIGINT and SIGTERM cancel the command's context.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// Run runs tug with explicit arguments and streams and returns the exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{
		stdin:  stdin,
		in:     bufio.NewReader(stdin),
		stdout: stdout,
		stderr: stderr,
		now:    time.Now,
	}
	defer a.closeTunnels()
	defer a.closePlugins()
	if err := a.execute(ctx, args); err != nil {
		return a.fail(err)
	}
	return 0
}

// execute runs one command line: on every cluster that --cluster-selector
// picks, as kubectl when tug has no such command, or as one of tug's own.
func (a *app) execute(ctx context.Context, args []string) error {
	fan, args, err := splitFanoutFlags(args)
	if err != nil {
		return err
	}
	root := newRootCmd(a)
	root.SetArgs(args)
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)

	if fan != nil {
		return a.fanout(ctx, root, args, fan)
	}
	if isPassthrough(root, args) {
		if err := a.setup(false); err != nil {
			return err
		}
		code, err := a.passthrough(ctx, args)
		if err != nil {
			return err
		}
		if code != 0 {
			return &exitError{code: code}
		}
		return nil
	}
	return root.ExecuteContext(ctx)
}

// app holds what commands share. Expensive pieces load on first use.
type app struct {
	stdin  io.Reader
	in     *bufio.Reader
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
	debug  bool

	paths config.Paths
	cfg   *config.Config
	log   *slog.Logger

	// fixed, when set, is the session commands run in instead of this
	// terminal's: the temporary one a run across clusters made.
	fixed *session.Session

	// mu guards the pieces below, which load on first use. tugd asks for
	// them from concurrent proxy requests.
	mu         sync.Mutex
	tunnelPool *tunnel.Pool
	store      credentials.Store
	reg        *registry.Registry
	problems   []plugin.Problem
}

// exitError carries a specific exit code without printing anything more.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (a *app) fail(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	fmt.Fprintf(a.stderr, "tug: %v\n", err)
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Hint != "" {
		fmt.Fprintf(a.stderr, "hint: %s\n", pe.Hint)
	}
	return 1
}

// setup resolves paths, loads config and the logger.
func (a *app) setup(debug bool) error {
	a.debug = a.debug || debug
	level := slog.LevelWarn
	if a.debug {
		level = slog.LevelDebug
	}
	a.log = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	a.paths = paths
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return err
	}
	a.cfg = cfg
	return nil
}

// credentialStore opens the keychain or encrypted file on first use.
func (a *app) credentialStore() (credentials.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		s, err := credentials.Open(a.cfg.Keyring.Backend, a.paths)
		if err != nil {
			return nil, err
		}
		a.store = s
	}
	return a.store, nil
}

// registry builds the provider registry: built-ins first, then plugins.
func (a *app) registry() *registry.Registry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reg != nil {
		return a.reg
	}
	r := registry.New(a.cfg.Plugins.AllowOverride)
	builtins := []registry.Provider{
		kubeconfig.New(kubeconfig.WithSkipDirs(a.paths.Runtime)),
		local.New(local.WithSkipDirs(a.paths.Runtime)),
		aws.New(),
		azure.New(),
		google.New(),
		registry.FromSDK(capi.New(capi.WithClusterConfig(a.managementConfig))),
		registry.FromSDK(argocd.New(argocd.WithClusterConfig(a.managementConfig))),
		registry.FromSDK(clusterprofile.New(clusterprofile.WithClusterConfig(a.managementConfig))),
	}
	for _, p := range builtins {
		if err := r.Add(p, registry.KindBuiltin, ""); err != nil {
			a.log.Warn("registering built-in provider", "err", err)
		}
	}
	plugins, problems := a.pluginDirs().Discover()
	a.problems = problems
	for _, prob := range problems {
		a.log.Debug("skipping plugin", "location", prob.Location, "err", prob.Err)
	}
	for _, p := range plugins {
		runner := &plugin.Runner{
			Bin:      p.Bin,
			Name:     p.Name(),
			Env:      p.Manifest.Spec.Env,
			Timeouts: a.cfg.PluginTimeouts(),
			Serve:    p.Manifest.Spec.Capabilities.Has(provider.CapServe),
			Log:      a.log,
		}
		kind := registry.KindPlugin
		if p.Dev {
			kind = registry.KindDevPlugin
		}
		if err := r.Add(plugin.NewProvider(p, runner), kind, p.Dir); err != nil {
			a.problems = append(a.problems, plugin.Problem{Location: p.Dir, Err: err})
		}
	}
	a.reg = r
	return r
}

// managementConfig returns a client config for a catalog cluster that a
// built-in provider reads other clusters from: a Cluster API management
// cluster, the cluster Argo CD runs on or a ClusterProfile hub. The
// providers only read, so the config refuses writes.
func (a *app) managementConfig(ctx context.Context, cluster string) (*rest.Config, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, err
	}
	cl, err := cat.Get(cluster)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, provider.Errorf(provider.NotFound, "%s is not in the catalog", cluster).
			WithHint("import the management cluster first, for example with `tug import kubeconfig <file>`; `tug ls` lists the names")
	}
	if err != nil {
		return nil, err
	}
	// A management cluster whose own credential comes from a provider that
	// reads it from that same cluster would recurse forever.
	if slices.Contains(managing(ctx), cl.ID) {
		return nil, provider.Errorf(provider.InvalidConfig, "%s gets its credential through itself", cl.Name).
			WithHint("set the account's cluster setting to a cluster imported from another source")
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return nil, err
	}
	f := &kube.Factory{
		Cluster:   cl,
		Broker:    broker,
		Guard:     &guard.Policy{Cluster: cl.Name, ReadOnly: true, Now: a.now},
		CacheDir:  a.paths.Cache,
		UserAgent: "tug/" + version.String(),
		// A management cluster may itself be private.
		Tunnels: a.tunnels(),
	}
	return f.RESTConfig(context.WithValue(ctx, managingKey{}, append(managing(ctx), cl.ID)))
}

// managingKey holds, in a context, the IDs of the management clusters whose
// configs are being built.
type managingKey struct{}

func managing(ctx context.Context) []string {
	ids, _ := ctx.Value(managingKey{}).([]string)
	return slices.Clip(ids)
}

// services returns the importer and credential broker for a catalog.
func (a *app) services(cat *catalog.Catalog) (*importer.Service, *credentials.Broker, error) {
	store, err := a.credentialStore()
	if err != nil {
		return nil, nil, err
	}
	svc := &importer.Service{Registry: a.registry(), Store: store, Now: a.now}
	broker := &credentials.Broker{
		Store:    store,
		Now:      a.now,
		Fetch:    svc.Fetcher(cat),
		Clusters: importer.ClusterLookup(cat),
		OIDC:     a.oidcClient(store, a.implicitSignIn()),
	}
	svc.Broker = broker
	return svc, broker, nil
}

// tunnels returns the pool that keeps this process's tunnels open.
func (a *app) tunnels() *tunnel.Pool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tunnelPool == nil {
		a.tunnelPool = tunnel.NewPool(tunnel.WithLogger(a.log))
	}
	return a.tunnelPool
}

// closePlugins stops plugins running in serve mode.
func (a *app) closePlugins() {
	a.mu.Lock()
	reg := a.reg
	a.mu.Unlock()
	if reg == nil {
		return
	}
	for _, name := range reg.Names() {
		if p, err := reg.Get(name); err == nil {
			if c, ok := p.(io.Closer); ok {
				_ = c.Close() // best effort on the way out
			}
		}
	}
}

// closeTunnels ends every tunnel this process opened.
func (a *app) closeTunnels() {
	a.mu.Lock()
	p := a.tunnelPool
	a.mu.Unlock()
	if p != nil {
		_ = p.Close() // best effort on the way out
	}
}

// oidcClient returns the client that signs in to OpenID Connect issuers.
// With ui, it may sign in on its own when there is no token to use.
func (a *app) oidcClient(store credentials.Store, ui *oidc.UI) *oidc.Client {
	opts := []oidc.Option{
		oidc.WithNow(a.now),
		oidc.WithLogger(a.log),
		oidc.WithLockDir(filepath.Join(a.paths.Cache, "oidc")),
		oidc.WithListenAddresses(a.cfg.OIDC.ListenAddresses...),
	}
	if ui != nil {
		opts = append(opts, oidc.WithLogin(*ui))
	}
	return oidc.New(store, opts...)
}

// implicitSignIn lets a command that needs a token, such as `tug auth token`
// run by kubectl, sign in on its own, but only where someone can see the
// address to open: when stderr is a terminal.
func (a *app) implicitSignIn() *oidc.UI {
	if !a.stderrTerminal() {
		return nil
	}
	ui := a.signInUI(false)
	return &ui
}

// signInUI shows sign-in steps on stderr and opens the system browser.
// Over SSH, where a browser here is no use, it prefers a device code.
func (a *app) signInUI(device bool) oidc.UI {
	return oidc.UI{
		Tell: func(msg string) { fmt.Fprintln(a.stderr, msg) },
		OpenBrowser: func(ctx context.Context, u string) error {
			return browser.Open(ctx, u, os.Getenv)
		},
		Device: device || os.Getenv("SSH_CONNECTION") != "",
	}
}

func (a *app) catalogPath() string { return catalog.Path(a.paths.Data) }

func (a *app) loadCatalog() (*catalog.Catalog, error) { return catalog.Load(a.catalogPath()) }

func (a *app) sessions() *session.Manager { return session.NewManager(a.paths.Runtime) }

// session returns the session commands run in: this terminal's, or the
// temporary one a run across clusters made for this app.
func (a *app) session() (*session.Session, error) {
	if a.fixed != nil {
		return a.fixed, nil
	}
	return a.sessions().Current()
}

// currentSession returns this terminal's session with a helpful error.
func (a *app) currentSession() (*session.Session, error) {
	s, err := a.session()
	if errors.Is(err, session.ErrNoSession) {
		return nil, fmt.Errorf("%w; add `eval \"$(tug init zsh)\"` (or bash, fish, powershell) to your shell's rc file, or run `tug shell`", err)
	}
	return s, err
}

func (a *app) colors() render.Colors { return render.ColorsFor(a.stdout) }

// isPassthrough reports whether args name a command tug does not have, which
// then runs as kubectl.
func isPassthrough(root *cobra.Command, args []string) bool {
	words := passthrough.Words(args)
	if len(words) == 0 {
		return false
	}
	first := words[0]
	switch first {
	case "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return false
	}
	for _, c := range root.Commands() {
		if c.Name() == first || c.HasAlias(first) {
			return false
		}
	}
	return true
}
