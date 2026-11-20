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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

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

// targetPod picks the pod to act on: the first ready one.
func (a *app) targetPod(ctx context.Context, env *opsEnv, t resolve.Target) (*corev1.Pod, error) {
	pods, err := resolve.Pods(ctx, env.cs, t)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning {
			return &pods[i], nil
		}
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("%s has no pods", t)
	}
	return nil, fmt.Errorf("%s has no running pod (first is %s, %s)", t, pods[0].Name, pods[0].Status.Phase)
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
		Use:   "logs <target>",
		Short: "Tail logs from every pod of a workload, with coloured prefixes",
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

func newShCmd(a *app) *cobra.Command {
	var namespace, container string
	cmd := &cobra.Command{
		Use:   "sh <target> [-- command...]",
		Short: "Open a shell (bash, else sh) in a pod, or run a command",
		Example: `  tug sh api
  tug sh api -c sidecar
  tug sh api -- env`,
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
			env, err := a.opsEnv(ctx, namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			pod, err := a.targetPod(ctx, env, t)
			if err != nil {
				return err
			}
			c := container
			if c == "" {
				c = resolve.DefaultContainer(pod)
			}
			if err := a.ensureWritable(env, fmt.Sprintf("Run a command in pod/%s", pod.Name)); err != nil {
				return err
			}
			return a.exec(ctx, env, pod.Name, c, command)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringVarP(&container, "container", "c", "", "container (default: the pod's default container)")
	return cmd
}

// exec runs a command in a container, with a raw TTY when interactive.
func (a *app) exec(ctx context.Context, env *opsEnv, pod, container string, command []string) error {
	in, inIsFile := a.stdin.(*os.File)
	out, outIsFile := a.stdout.(*os.File)
	tty := inIsFile && outIsFile && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))

	o := ops.ExecOptions{
		Namespace: env.namespace,
		Pod:       pod,
		Container: container,
		Command:   command,
		Stdout:    a.stdout,
		Stderr:    a.stderr,
		TTY:       tty,
	}
	if tty || !inIsFile || !term.IsTerminal(int(in.Fd())) {
		o.Stdin = a.stdin // a terminal, or piped input
	}
	if tty {
		fmt.Fprintf(a.stderr, "%s\n", a.colors().Dim(fmt.Sprintf("pod/%s, container %s; exit to leave", pod, container)))
		state, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(in.Fd()), state) //nolint:errcheck // best effort on the way out
		sizes, stop := watchTerminalSize(out)
		defer stop()
		o.Sizes = sizes
	}
	err := ops.Exec(ctx, env.cfg, env.cs, o)
	if code, ok := ops.ExitCode(err); ok {
		return &exitError{code: code}
	}
	return err
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
			pod, err := a.targetPod(ctx, env, t)
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
		Use:   "top [target]",
		Short: "Show CPU and memory of a workload's pods (needs metrics-server)",
		Args:  cobra.MaximumNArgs(1),
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
