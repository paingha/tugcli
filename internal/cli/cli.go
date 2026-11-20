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
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/builtin/aws"
	"github.com/paingha/tugcli/internal/builtin/google"
	"github.com/paingha/tugcli/internal/builtin/kubeconfig"
	"github.com/paingha/tugcli/internal/builtin/local"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
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
	root := newRootCmd(a)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if isPassthrough(root, args) {
		if err := a.setup(false); err != nil {
			return a.fail(err)
		}
		code, err := a.passthrough(ctx, args)
		if err != nil {
			return a.fail(err)
		}
		return code
	}
	if err := root.ExecuteContext(ctx); err != nil {
		return a.fail(err)
	}
	return 0
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

	// mu guards the pieces below, which load on first use. tugd asks for
	// them from concurrent proxy requests.
	mu       sync.Mutex
	store    credentials.Store
	reg      *registry.Registry
	problems []plugin.Problem
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
		google.New(),
	}
	for _, p := range builtins {
		if err := r.Add(p, registry.KindBuiltin, ""); err != nil {
			a.log.Warn("registering built-in provider", "err", err)
		}
	}
	plugins, problems := plugin.NewDirs(a.paths.Data).Discover()
	a.problems = problems
	for _, prob := range problems {
		a.log.Debug("skipping plugin", "location", prob.Location, "err", prob.Err)
	}
	for _, p := range plugins {
		runner := &plugin.Runner{
			Bin:  p.Bin,
			Name: p.Name(),
			Env:  p.Manifest.Spec.Env,
			Timeouts: map[string]time.Duration{
				provider.OpList:       a.cfg.Timeout(provider.OpList, plugin.DefaultListTimeout),
				provider.OpCredential: a.cfg.Timeout(provider.OpCredential, plugin.DefaultCredentialTimeout),
			},
			Log: a.log,
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

// services returns the importer and credential broker for a catalog.
func (a *app) services(cat *catalog.Catalog) (*importer.Service, *credentials.Broker, error) {
	store, err := a.credentialStore()
	if err != nil {
		return nil, nil, err
	}
	svc := &importer.Service{Registry: a.registry(), Store: store, Now: a.now}
	broker := &credentials.Broker{Store: store, Now: a.now, Fetch: svc.Fetcher(cat)}
	svc.Broker = broker
	return svc, broker, nil
}

func (a *app) catalogPath() string { return catalog.Path(a.paths.Data) }

func (a *app) loadCatalog() (*catalog.Catalog, error) { return catalog.Load(a.catalogPath()) }

func (a *app) sessions() *session.Manager { return session.NewManager(a.paths.Runtime) }

// currentSession returns this terminal's session with a helpful error.
func (a *app) currentSession() (*session.Session, error) {
	s, err := a.sessions().Current()
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
