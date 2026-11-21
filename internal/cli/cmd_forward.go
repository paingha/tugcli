package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/version"
)

// startNamedForward hands a port-forward to tugd and says where it listens.
func (a *app) startNamedForward(ctx context.Context, env *opsEnv, t resolve.Target, name string, ports, addresses []string) error {
	st, err := a.namedForward(ctx, env, t, name, ports, addresses)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, a.colors().Dim("tugd keeps it open across pod restarts; tug pf ls shows it, tug pf stop "+name+" ends it"))
	_, err = fmt.Fprintf(a.stdout, "Forwarding %s to %s (pod/%s) as %s\n", strings.Join(localPorts(st.Bound), ", "), st.Target, st.Pod, st.Name)
	return err
}

// namedForward hands a port-forward to tugd, starting tugd when it is not
// running. tugd keeps the forward open and starts it again on a new pod
// when the old one goes away.
func (a *app) namedForward(ctx context.Context, env *opsEnv, t resolve.Target, name string, ports, addresses []string) (daemon.ForwardState, error) {
	c, err := a.ensureDaemon(ctx)
	if err != nil {
		return daemon.ForwardState{}, err
	}
	st, err := c.StartForward(ctx, daemon.Forward{
		Name:      name,
		ClusterID: env.session.State.ClusterID,
		Cluster:   env.session.State.ClusterName,
		Namespace: env.namespace,
		Target:    t.String(),
		Ports:     ports,
		Addresses: addresses,
	})
	if err != nil {
		return daemon.ForwardState{}, fmt.Errorf("starting the forward %s: %w", name, err)
	}
	return st, nil
}

// localPorts turns local:remote pairs into localhost:local -> remote.
func localPorts(bound []string) []string {
	out := make([]string, len(bound))
	for i, b := range bound {
		local, remote, _ := strings.Cut(b, ":")
		out[i] = "localhost:" + local + " -> " + remote
	}
	return out
}

func newPfLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the named port-forwards tugd keeps open",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			forwards, err := daemon.NewClient(a.paths.Runtime).Forwards(cmd.Context())
			if errors.Is(err, daemon.ErrNotRunning) {
				forwards, err = nil, nil
			}
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				if forwards == nil {
					forwards = []daemon.ForwardState{}
				}
				return render.Structured(a.stdout, format, forwards)
			case render.FormatName:
				for _, f := range forwards {
					fmt.Fprintln(a.stdout, f.Name)
				}
				return nil
			}
			rows := make([][]string, 0, len(forwards))
			for _, f := range forwards {
				state := f.State
				if f.LastError != "" && f.State != daemon.ForwardUp {
					state += ": " + f.LastError
				}
				rows = append(rows, []string{f.Name, f.Cluster, f.Namespace, f.Target, strings.Join(f.Bound, ","), state,
					strconv.Itoa(f.Restarts), shortDuration(a.now().Sub(f.Since))})
			}
			return render.Table(a.stdout, []string{"NAME", "CLUSTER", "NAMESPACE", "TARGET", "PORTS", "STATE", "RESTARTS", "SINCE"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newPfStopCmd(a *app) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop <name>... | --all",
		Short: "End named port-forwards",
		Args: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the forwards to stop, or pass --all")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c := daemon.NewClient(a.paths.Runtime)
			if all {
				forwards, err := c.Forwards(cmd.Context())
				if errors.Is(err, daemon.ErrNotRunning) {
					return nil
				}
				if err != nil {
					return err
				}
				for _, f := range forwards {
					args = append(args, f.Name)
				}
			}
			var errs []error
			for _, name := range args {
				if err := c.StopForward(cmd.Context(), name); err != nil {
					errs = append(errs, fmt.Errorf("stopping %s: %w", name, err))
					continue
				}
				fmt.Fprintf(a.stdout, "Stopped %s\n", name)
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every named forward")
	return cmd
}

// daemonForward runs one attempt of a named forward inside tugd: it finds
// a running pod of the target now, so a restarted workload is followed.
func (a *app) daemonForward(ctx context.Context, f daemon.Forward, pinned []string, up func(pod string, bound []string)) error {
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	cl, err := cat.Get(f.ClusterID)
	if err != nil {
		return err
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return err
	}
	fac := &kube.Factory{Cluster: cl, Namespace: f.Namespace, Broker: broker, CacheDir: a.paths.Cache,
		UserAgent: "tugd/" + version.String(), Tunnels: a.tunnels()}
	cfg, err := fac.RESTConfig(ctx)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	env := &opsEnv{factory: fac, cfg: cfg, cs: cs, namespace: f.Namespace}
	t, err := a.resolveTarget(ctx, env, f.Target)
	if err != nil {
		return err
	}
	pod, err := resolve.RunningPod(ctx, env.cs, t)
	if err != nil {
		return err
	}
	ports := pinned
	if ports == nil {
		if ports, err = ops.ResolvePorts(pod, f.Ports); err != nil {
			return err
		}
	}
	return ops.PortForward(ctx, cfg, cs, ops.ForwardOptions{
		Namespace: f.Namespace,
		Pod:       pod.Name,
		Ports:     ports,
		Addresses: f.Addresses,
		Bound:     func(bound []string) { up(pod.Name, bound) },
		Out:       io.Discard,
		ErrOut:    io.Discard, // failures come back as the error
	})
}

// shortDuration prints 45s, 12m or 3h.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
}
