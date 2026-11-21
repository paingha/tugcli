package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/tui"
)

func newUICmd(a *app) *cobra.Command {
	var k9s bool
	cmd := &cobra.Command{
		Use:   "ui [-- k9s arguments...]",
		Short: "Open a full-screen terminal UI on this terminal's session",
		Long: `Open a full-screen UI on this terminal's session. It lists the catalog's
clusters, and the workloads of the session's namespace with their ready
counts and health, refreshed every few seconds. On a workload, l streams
its logs, w says why it is not ready, t shows what it is made of, s opens a
shell, p port-forwards through tugd, r restarts it and S scales it. Enter
on a cluster and n for a namespace change this terminal's session, as tug
connect and tug ns do. On a read-only session, shells, restarts and scaling
ask first. ? shows every key and q quits.

tug ui needs a terminal. With --k9s it opens k9s on the session's cluster
instead, with any arguments after --.`,
		Example: `  tug ui
  tug ui --k9s
  tug ui --k9s -- --readonly   # arguments after -- go to k9s`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 && !k9s {
				return errors.New("arguments are for k9s; run `tug ui --k9s -- <arguments>`")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if k9s {
				return a.k9s(args)
			}
			return a.ui(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&k9s, "k9s", false, "open k9s with the session's kubeconfig instead")
	return cmd
}

// k9s replaces tug with k9s on the session's kubeconfig.
func (a *app) k9s(args []string) error {
	s, err := a.currentSession()
	if err != nil {
		return err
	}
	if !s.State.Connected() {
		return errors.New("not connected; run `tug connect` first")
	}
	k9s, err := exec.LookPath("k9s")
	if err != nil {
		return errors.New("k9s is not installed; see https://k9scli.io")
	}
	env := append(os.Environ(), "KUBECONFIG="+s.KubeconfigPath())
	return a.replaceProcess(k9s, args, env)
}

// ui runs the TUI on this terminal's session. The TUI owns the terminal
// while it runs, so what tug would write on stderr meanwhile, its log
// included, is held and written after. With stderr held, tug's own prompts
// see no terminal and do not ask: the TUI asks its own questions.
func (a *app) ui(ctx context.Context) error {
	if !a.console().tty() {
		return errors.New("tug ui needs a terminal on stdin and stdout; in scripts, use tug ls, tug status and the other commands")
	}
	s, err := a.currentSession()
	if err != nil {
		return err
	}
	held := &lockedBuffer{}
	stderr, log := a.stderr, a.log
	a.stderr, a.log = held, a.logger(held)
	defer func() {
		a.stderr, a.log = stderr, log
		_, _ = held.WriteTo(stderr) // best effort on the way out
	}()
	return tui.Run(ctx, &tuiBackend{a: a, session: s.State.ID}, a.stdin, a.stdout)
}

// lockedBuffer collects writes from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) WriteTo(w io.Writer) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.WriteTo(w)
}

// tuiBackend gives the TUI the code tug's commands run, so the TUI
// connects, switches namespace, opens shells and starts named forwards as
// tug connect, tug ns, tug sh and tug pf --name do. It never asks anything
// itself: the TUI has asked before it calls.
type tuiBackend struct {
	a *app
	// session is the ID of this terminal's session.
	session string
}

var _ tui.Backend = (*tuiBackend)(nil)

func (b *tuiBackend) Catalog() (*catalog.Catalog, error) { return b.a.loadCatalog() }

func (b *tuiBackend) Session() (session.State, error) {
	s, err := b.a.sessions().Open(b.session)
	if err != nil {
		return session.State{}, err
	}
	return s.State, nil
}

func (b *tuiBackend) Connect(ctx context.Context, clusterID string) (session.State, error) {
	s, err := b.a.sessions().Open(b.session)
	if err != nil {
		return session.State{}, err
	}
	cat, err := b.a.loadCatalog()
	if err != nil {
		return session.State{}, err
	}
	cl, err := cat.Get(clusterID)
	if err != nil {
		return session.State{}, err
	}
	if _, _, err := b.a.connectByHand(ctx, s, cl, connectOptions{}); err != nil {
		return session.State{}, err
	}
	return s.State, nil
}

func (b *tuiBackend) Namespaces(ctx context.Context) ([]string, error) {
	s, err := b.a.sessions().Open(b.session)
	if err != nil {
		return nil, err
	}
	return b.a.namespaces(ctx, s)
}

func (b *tuiBackend) SetNamespace(ns string) (session.State, error) {
	s, err := b.a.sessions().Open(b.session)
	if err != nil {
		return session.State{}, err
	}
	if err := s.SetNamespace(ns); err != nil {
		return session.State{}, err
	}
	return s.State, nil
}

// env returns clients for the session's cluster, in namespace when it is
// not "". Their guard never asks: on a read-only session it refuses writes,
// unless allowWrites says the user agreed in the TUI, as ensureWritable
// allows them after asking.
func (b *tuiBackend) env(ctx context.Context, namespace string, allowWrites bool) (*opsEnv, error) {
	env, err := b.a.opsEnv(ctx, namespace)
	if err != nil {
		return nil, err
	}
	env.factory.Guard.Confirm = nil
	if allowWrites {
		env.factory.Guard.Confirm = func(string) bool { return true }
	}
	if env.cfg.ExecProvider != nil {
		// A credential plugin must not read the terminal the TUI reads.
		cfg := rest.CopyConfig(env.cfg)
		ep := *cfg.ExecProvider
		ep.InteractiveMode = clientcmdapi.NeverExecInteractiveMode
		cfg.ExecProvider = &ep
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		env.cfg, env.cs = cfg, cs
	}
	return env, nil
}

func (b *tuiBackend) Kube(ctx context.Context, allowWrites bool) (kubernetes.Interface, error) {
	env, err := b.env(ctx, "", allowWrites)
	if err != nil {
		return nil, err
	}
	return env.cs, nil
}

func (b *tuiBackend) Forward(ctx context.Context, t resolve.Target, name string, ports []string) (daemon.ForwardState, error) {
	env, err := b.env(ctx, t.Namespace, false)
	if err != nil {
		return daemon.ForwardState{}, err
	}
	// Ports are checked against a pod now, as tug pf does, so a mistake
	// is not left to tugd to find.
	pod, err := resolve.RunningPod(ctx, env.cs, t)
	if err != nil {
		return daemon.ForwardState{}, err
	}
	if _, err := ops.ResolvePorts(pod, ports); err != nil {
		return daemon.ForwardState{}, err
	}
	return b.a.namedForward(ctx, env, t, name, ports, []string{"localhost"})
}

func (b *tuiBackend) Shell(ctx context.Context, r tui.ShellRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	env, err := b.env(ctx, r.Namespace, r.AllowWrites)
	if err != nil {
		return err
	}
	con := console{stdin: stdin, stdout: stdout, stderr: stderr}
	if r.Debug {
		return b.a.debugContainer(ctx, env, con, r.Pod, r.Container, ops.DefaultDebugImage, nil)
	}
	return b.a.exec(ctx, env, con, r.Pod, r.Container, nil)
}
