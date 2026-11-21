package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
)

// opsEnv is what the daily commands share: clients for the session's cluster
// in the namespace to act on.
type opsEnv struct {
	session   *session.Session
	factory   *kube.Factory
	cfg       *rest.Config
	cs        kubernetes.Interface
	namespace string
}

func (a *app) opsEnv(ctx context.Context, namespace string) (*opsEnv, error) {
	s, err := a.currentSession()
	if err != nil {
		return nil, err
	}
	if !s.State.Connected() {
		return nil, errors.New("not connected; run `tug connect` first")
	}
	f, err := a.factory(s)
	if err != nil {
		return nil, err
	}
	if namespace != "" {
		f.Namespace = namespace
	}
	cfg, err := f.RESTConfig(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &opsEnv{session: s, factory: f, cfg: cfg, cs: cs, namespace: f.Namespace}, nil
}

// resolveTarget turns a target argument into one workload or pod, asking when
// several match equally well.
func (a *app) resolveTarget(ctx context.Context, env *opsEnv, arg string) (resolve.Target, error) {
	q, err := resolve.ParseQuery(arg)
	if err != nil {
		return resolve.Target{}, err
	}
	candidates, err := resolve.Workloads(ctx, env.cs, env.namespace, q.Kind)
	if err != nil {
		return resolve.Target{}, fmt.Errorf("listing workloads in %s: %w", env.namespace, err)
	}
	matches := resolve.MatchTargets(q, candidates)
	switch len(matches) {
	case 0:
		return resolve.Target{}, fmt.Errorf("nothing in namespace %s matches %q; use -n for another namespace", env.namespace, arg)
	case 1:
		return matches[0], nil
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.String()
	}
	i, err := a.pick("Matches for "+arg+":", names)
	if err != nil {
		return resolve.Target{}, err
	}
	return matches[i], nil
}

// ensureWritable asks once, before any request, whether to write on a
// read-only session. Asking up front keeps prompts out of raw-mode shells.
func (a *app) ensureWritable(env *opsEnv, action string) error {
	policy := env.factory.Guard
	if policy.Writable() {
		return nil
	}
	if a.confirm(fmt.Sprintf("%s is read-only. %s anyway?", env.session.State.ClusterName, action), false) {
		policy.Confirm = func(string) bool { return true }
		return nil
	}
	return &guard.DeniedError{Cluster: env.session.State.ClusterName, Action: action}
}

type logsOptions struct {
	namespace  string
	container  string
	follow     bool
	previous   bool
	timestamps bool
	pretty     bool
	since      time.Duration
	tail       int64
}

func newLogsCmd(a *app) *cobra.Command {
	var o logsOptions
	cmd := &cobra.Command{
		Use:         "logs <target>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Tail logs from every pod of a workload, with coloured prefixes",
		Example: `  tug logs api -f            # deploy/api-server, every pod, new pods as they start
  tug logs sts/db -c postgres --since 10m
  tug logs api --json        # pretty-print JSON lines`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, o.namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			tail := o.tail
			if !cmd.Flags().Changed("tail") && t.IsWorkload() {
				tail = 10 // as kubectl does for selectors
			}
			prefix := t.IsWorkload() || o.container == ""
			c := a.colors()
			sink := ops.LogSink{
				Line: func(l ops.LogLine) {
					text := l.Text
					if o.pretty {
						text = render.PrettyJSON(text)
					}
					if prefix {
						label := l.Pod
						if o.container == "" {
							label += "/" + l.Container
						}
						fmt.Fprintf(a.stdout, "%s %s\n", c.Pick(l.Pod, label), text)
						return
					}
					fmt.Fprintln(a.stdout, text)
				},
				Error: func(pod, container string, err error) {
					fmt.Fprintf(a.stderr, "%s %s/%s: %v\n", c.Yellow("warning:"), pod, container, err)
				},
			}
			err = ops.Logs(ctx, env.cs, t, ops.LogOptions{
				Container:  o.container,
				Follow:     o.follow,
				Previous:   o.previous,
				Since:      o.since,
				Tail:       tail,
				Timestamps: o.timestamps,
			}, sink)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.StringVarP(&o.container, "container", "c", "", "only this container")
	f.BoolVarP(&o.follow, "follow", "f", false, "keep streaming, including pods that start later")
	f.BoolVarP(&o.previous, "previous", "p", false, "logs of the previous container instance")
	f.BoolVar(&o.timestamps, "timestamps", false, "include timestamps")
	f.BoolVar(&o.pretty, "json", false, "pretty-print lines that are JSON objects")
	f.DurationVar(&o.since, "since", 0, "only lines newer than this, e.g. 10m")
	f.Int64Var(&o.tail, "tail", -1, "lines from the end of each log (default all; 10 for workloads)")
	return cmd
}

// debugStartTimeout is how long tug sh waits for a debug container to start.
const debugStartTimeout = 2 * time.Minute

type shOptions struct {
	namespace string
	container string
	debug     bool
	image     string
}

func newShCmd(a *app) *cobra.Command {
	var o shOptions
	cmd := &cobra.Command{
		Use:   "sh <target> [-- command...]",
		Short: "Open a shell (bash, else sh) in a pod, or run a command",
		Long: `Open a shell in a pod's container: bash when the image has it, else sh. With a
command after --, run that instead.

When the container has no shell, as in distroless images, tug offers to add
an ephemeral debug container that shares the container's processes, and
opens the shell there; --debug-container does so without asking. The
container's files are under /proc/1/root. A debug container stays in the pod
until the pod is replaced, and adding one is a write.`,
		Example: `  tug sh api
  tug sh api -c sidecar
  tug sh api -- env
  tug sh api --debug-container              # a busybox shell beside the app's processes
  tug sh api --debug-container --image nicolaka/netshoot -- tcpdump -i any`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash > 1 || (dash < 0 && len(args) > 1) {
				return errors.New("put the command after --, for example: tug sh api -- ls /")
			}
			var command []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				command = args[dash:]
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, o.namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			pod, err := resolve.RunningPod(ctx, env.cs, t)
			if err != nil {
				return err
			}
			c := o.container
			if c == "" {
				c = resolve.DefaultContainer(pod)
			}
			if o.debug {
				return a.debugShell(ctx, env, pod.Name, c, o.image, command)
			}
			if err := a.ensureWritable(env, fmt.Sprintf("Run a command in pod/%s", pod.Name)); err != nil {
				return err
			}
			err = a.exec(ctx, env, a.console(), pod.Name, c, command)
			if len(command) > 0 || !ops.NoShell(err) {
				return remoteExit(err)
			}
			if !a.confirm(fmt.Sprintf("Container %s of pod/%s has no shell. Add an ephemeral debug container (%s) to the pod?", c, pod.Name, o.image), false) {
				return fmt.Errorf("container %s of pod/%s has no shell; to open one in an ephemeral debug container, run `%s`",
					c, pod.Name, debugCommand(cmd, args[0], o))
			}
			return a.debugShell(ctx, env, pod.Name, c, o.image, command)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.StringVarP(&o.container, "container", "c", "", "container (default: the pod's default container)")
	f.BoolVar(&o.debug, "debug-container", false, "open the shell in an ephemeral debug container that shares the container's processes")
	f.StringVar(&o.image, "image", ops.DefaultDebugImage, "image of the debug container")
	return cmd
}

// debugCommand is the tug sh command line that adds a debug container.
func debugCommand(cmd *cobra.Command, target string, o shOptions) string {
	line := "tug sh " + target + " --debug-container"
	if o.namespace != "" {
		line += " -n " + o.namespace
	}
	if o.container != "" {
		line += " -c " + o.container
	}
	if cmd.Flags().Changed("image") {
		line += " --image " + o.image
	}
	return line
}

// debugShell asks before writing on a read-only session, then opens a
// shell in a debug container on tug's own streams.
func (a *app) debugShell(ctx context.Context, env *opsEnv, pod, target, image string, command []string) error {
	if err := a.ensureWritable(env, fmt.Sprintf("Add a debug container to pod/%s", pod)); err != nil {
		return err
	}
	return remoteExit(a.debugContainer(ctx, env, a.console(), pod, target, image, command))
}

// debugContainer adds an ephemeral debug container beside a pod's
// container, waits for it to start and attaches con to it. A command it
// runs may finish before tug can attach; its logs are shown then. The
// caller has dealt with the guard.
func (a *app) debugContainer(ctx context.Context, env *opsEnv, con console, pod, target, image string, command []string) error {
	name, err := ops.AddDebugContainer(ctx, env.cs, ops.DebugOptions{
		Namespace: env.namespace, Pod: pod, Target: target, Image: image, Command: command, TTY: con.tty(),
	})
	if err != nil {
		return err
	}
	c := con.colors()
	fmt.Fprintf(con.stderr, "%s\n", c.Dim(fmt.Sprintf(
		"Added debug container %s (%s) to pod/%s, sharing the processes of %s; waiting for it to start", name, image, pod, target)))

	wait, cancel := context.WithTimeout(ctx, debugStartTimeout)
	defer cancel()
	state, err := ops.WaitForEphemeralContainer(wait, env.cs, env.namespace, pod, name)
	var start *ops.StartError
	switch {
	case errors.As(err, &start) && start.Image():
		return fmt.Errorf("%w; choose another image with --image", err)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return fmt.Errorf("debug container %s did not start within %s; `tug why pod/%s` may say why", name, debugStartTimeout, pod)
	case err != nil:
		return err
	}

	if t := state.Terminated; t != nil {
		err := ops.Logs(ctx, env.cs, resolve.Target{Kind: resolve.KindPod, Name: pod, Namespace: env.namespace},
			ops.LogOptions{Container: name, Tail: -1}, ops.LogSink{
				Line: func(l ops.LogLine) { fmt.Fprintln(con.stdout, l.Text) },
				Error: func(pod, container string, err error) {
					fmt.Fprintf(con.stderr, "%s %s/%s: %v\n", c.Yellow("warning:"), pod, container, err)
				},
			})
		if err != nil {
			return err
		}
		if t.ExitCode != 0 {
			return &exitError{code: int(t.ExitCode)}
		}
		return nil
	}
	banner := fmt.Sprintf("pod/%s, debug container %s; exit to leave, and press Enter if there is no prompt", pod, name)
	return con.interact(banner, func(r remoteIO) error {
		return ops.Attach(ctx, env.cfg, env.cs, ops.AttachOptions{
			Namespace: env.namespace, Pod: pod, Container: name,
			Stdin: r.stdin, Stdout: con.stdout, Stderr: con.stderr, TTY: r.tty, Sizes: r.sizes,
		})
	})
}

// exec runs a command in a container on con, with a raw TTY when con is a
// terminal. A command that exits non-zero returns an error ops.ExitCode
// reads.
func (a *app) exec(ctx context.Context, env *opsEnv, con console, pod, container string, command []string) error {
	banner := fmt.Sprintf("pod/%s, container %s; exit to leave", pod, container)
	return con.interact(banner, func(r remoteIO) error {
		return ops.Exec(ctx, env.cfg, env.cs, ops.ExecOptions{
			Namespace: env.namespace, Pod: pod, Container: container, Command: command,
			Stdin: r.stdin, Stdout: con.stdout, Stderr: con.stderr, TTY: r.tty, Sizes: r.sizes,
		})
	})
}

// remoteExit makes a remote command's exit status tug's own.
func remoteExit(err error) error {
	if code, ok := ops.ExitCode(err); ok {
		return &exitError{code: code}
	}
	return err
}

// console is the local end of an exec or attach: tug's own streams, or the
// terminal the TUI hands over while it is suspended.
type console struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// console returns tug's own streams.
func (a *app) console() console { return console{stdin: a.stdin, stdout: a.stdout, stderr: a.stderr} }

func (c console) colors() render.Colors { return render.ColorsFor(c.stdout) }

// remoteIO is the local end of an exec or attach.
type remoteIO struct {
	stdin io.Reader
	tty   bool
	sizes remotecommand.TerminalSizeQueue
}

// tty reports whether stdin and stdout are both terminals, so that a remote
// shell can have one too.
func (c console) tty() bool {
	in, inIsFile := c.stdin.(*os.File)
	out, outIsFile := c.stdout.(*os.File)
	return inIsFile && outIsFile && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// interact runs an exec or attach with the console's streams: in raw mode
// with resizes when both ends are terminals, else with piped input.
func (c console) interact(banner string, run func(remoteIO) error) error {
	r := remoteIO{tty: c.tty()}
	in, inIsFile := c.stdin.(*os.File)
	out, _ := c.stdout.(*os.File)
	if r.tty || !inIsFile || !term.IsTerminal(int(in.Fd())) {
		r.stdin = c.stdin // a terminal, or piped input
	}
	if r.tty {
		fmt.Fprintf(c.stderr, "%s\n", c.colors().Dim(banner))
		state, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(in.Fd()), state) //nolint:errcheck // best effort on the way out
		sizes, stop := watchTerminalSize(out)
		defer stop()
		r.sizes = sizes
	}
	return run(r)
}

func newPfCmd(a *app) *cobra.Command {
	var namespace, name string
	var addresses []string
	cmd := &cobra.Command{
		Use:   "pf <target> [[local:]port...]",
		Short: "Port-forward to a workload by short name",
		Long: `Port-forward to a running pod of a workload until Ctrl-C. With --name, tugd
runs the forward in the background instead, keeps the same local ports, and
starts it again on a new pod when the old one goes away. tug pf ls lists
named forwards and tug pf stop ends them; a workload called ls or stop is
deploy/ls or deploy/stop.`,
		Example: `  tug pf api 8080          # localhost:8080 -> container port 8080
  tug pf api 9000:http     # localhost:9000 -> the port named http
  tug pf db                # every declared port to the same local port
  tug pf db 5432 --name db # in the background, through tugd`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			pod, err := resolve.RunningPod(ctx, env.cs, t)
			if err != nil {
				return err
			}
			ports, err := ops.ResolvePorts(pod, args[1:])
			if err != nil {
				return err
			}
			if name != "" {
				return a.startNamedForward(ctx, env, t, name, args[1:], addresses)
			}
			fmt.Fprintf(a.stderr, "%s\n", a.colors().Dim(fmt.Sprintf("%s via pod/%s; Ctrl-C to stop", t, pod.Name)))
			err = ops.PortForward(ctx, env.cfg, env.cs, ops.ForwardOptions{
				Namespace: env.namespace,
				Pod:       pod.Name,
				Ports:     ports,
				Addresses: addresses,
				Out:       a.stdout,
				ErrOut:    a.stderr,
			})
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringSliceVar(&addresses, "address", []string{"localhost"}, "addresses to listen on")
	cmd.Flags().StringVar(&name, "name", "", "run in the background through tugd under this name, reconnecting when the pod goes away")
	cmd.AddCommand(newPfLsCmd(a), newPfStopCmd(a))
	return cmd
}

// newWriteCmd builds a command that resolves a target and changes it.
func newWriteCmd(a *app, use, short, example string, nargs cobra.PositionalArgs,
	run func(ctx context.Context, env *opsEnv, t resolve.Target, args []string, w io.Writer) error,
) *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:     use,
		Short:   short,
		Example: example,
		Args:    nargs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			if err := a.ensureWritable(env, fmt.Sprintf("%s %s", cmd.Name(), t)); err != nil {
				return err
			}
			return run(ctx, env, t, args[1:], a.stdout)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: the session's)")
	return cmd
}

func newRestartCmd(a *app) *cobra.Command {
	cmd := newWriteCmd(a, "restart <target>", "Rolling-restart a deployment, statefulset or daemonset",
		"  tug restart api", cobra.ExactArgs(1),
		func(ctx context.Context, env *opsEnv, t resolve.Target, _ []string, w io.Writer) error {
			if err := ops.Restart(ctx, env.cs, t, a.now()); err != nil {
				return err
			}
			_, err := fmt.Fprintf(w, "Restarted %s\n", t)
			return err
		})
	return cmd
}

func newScaleCmd(a *app) *cobra.Command {
	cmd := newWriteCmd(a, "scale <target> <replicas>", "Set the replicas of a deployment or statefulset",
		"  tug scale api 3", cobra.ExactArgs(2),
		func(ctx context.Context, env *opsEnv, t resolve.Target, args []string, w io.Writer) error {
			n, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil || n < 0 {
				return fmt.Errorf("replicas must be a number from 0, got %q", args[0])
			}
			if err := ops.Scale(ctx, env.cs, t, int32(n)); err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "Scaled %s to %d\n", t, n)
			return err
		})
	return cmd
}

func newRollbackCmd(a *app) *cobra.Command {
	var toRevision int64
	cmd := newWriteCmd(a, "rollback <target>", "Roll a deployment back to its previous revision",
		"  tug rollback api\n  tug rollback api --to-revision 3", cobra.ExactArgs(1),
		func(ctx context.Context, env *opsEnv, t resolve.Target, _ []string, w io.Writer) error {
			rev, err := ops.Rollback(ctx, env.cs, t, toRevision)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "Rolled %s back to revision %d\n", t, rev)
			return err
		})
	cmd.Flags().Int64Var(&toRevision, "to-revision", 0, "revision to return to (default: the previous one)")
	return cmd
}

func newTopCmd(a *app) *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:         "top [target]",
		Annotations: acrossClusters(acrossKubectl),
		Short:       "Show CPU and memory of a workload's pods (needs metrics-server)",
		Args:        cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, namespace)
			if err != nil {
				return err
			}
			kubectlArgs := []string{"top", "pod", "-n", env.namespace}
			if len(args) == 1 {
				t, err := a.resolveTarget(ctx, env, args[0])
				if err != nil {
					return err
				}
				if t.IsWorkload() {
					kubectlArgs = append(kubectlArgs, "-l", t.Selector.String())
				} else {
					kubectlArgs = append(kubectlArgs, t.Name)
				}
			}
			code, err := a.passthrough(ctx, kubectlArgs)
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: the session's)")
	return cmd
}
