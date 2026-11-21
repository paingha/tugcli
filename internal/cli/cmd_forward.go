package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/profile"
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
				rows = append(rows, []string{f.Name, cmp.Or(f.Group, "-"), f.Cluster, f.Namespace, f.Target, forwardPorts(f),
					a.forwardState(profileForward{ForwardState: f, InProfile: true}), strconv.Itoa(f.Restarts), shortDuration(a.now().Sub(f.Since))})
			}
			if err := render.Table(a.stdout, []string{"NAME", "PROFILE", "CLUSTER", "NAMESPACE", "TARGET", "PORTS", "STATE", "RESTARTS", "SINCE"}, rows); err != nil {
				return err
			}
			if len(forwards) > 0 {
				a.explainRestarts()
			}
			return nil
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

// forwardStopped is the state shown for a profile's forward that tugd is
// not running.
const forwardStopped = "stopped"

// forwardState describes how a forward is doing: forwarding, starting,
// reconnecting with when it tries again and why it dropped, or stopped.
func (a *app) forwardState(f profileForward) string {
	state := f.State
	if f.State == daemon.ForwardReconnecting && !f.RetryAt.IsZero() {
		if d := f.RetryAt.Sub(a.now()); d > 0 {
			state += ", retry in " + shortDuration(d)
		} else {
			state += ", retrying"
		}
	}
	if f.LastError != "" && f.State != daemon.ForwardUp {
		state += ": " + f.LastError
	}
	if !f.InProfile {
		state += " (no longer in the profile)"
	}
	return state
}

// forwardPorts are the local:remote ports a forward listens on, or the
// ones it asks for when it is not listening.
func forwardPorts(f daemon.ForwardState) string {
	ports := f.Bound
	if ports == nil {
		ports = f.Ports
	}
	if len(ports) == 0 {
		return "-"
	}
	return strings.Join(ports, ",")
}

// explainRestarts says on stderr how tugd treats a forward that drops.
func (a *app) explainRestarts() {
	fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf(
		"tugd starts a forward that drops again on a running pod, on the same local ports, after %s and then up to %s apart; "+
			"tug pf stop <name> ends one, tug pf profile down <profile> a profile's",
		daemon.ForwardMinBackoff, daemon.ForwardMaxBackoff)))
}

// profileForward is a forward of a profile as tug pf profile ls shows it.
type profileForward struct {
	daemon.ForwardState
	// InProfile is false for a forward tugd runs for the profile that the
	// profile's file no longer has.
	InProfile bool `json:"inProfile"`
}

// declaredForward is the tugd forward for forward f of profile p, without
// its cluster.
func declaredForward(p *profile.Profile, f profile.Forward) daemon.Forward {
	addresses := f.Addresses
	if len(addresses) == 0 {
		addresses = []string{"localhost"}
	}
	return daemon.Forward{
		Name:      p.ForwardName(f),
		Group:     p.Name,
		Namespace: cmp.Or(f.Namespace, p.Namespace),
		Target:    f.Target,
		Ports:     f.Ports,
		Addresses: addresses,
	}
}

// profileForwards are the tugd forwards of profile p on cluster cl. tugd
// finds each target's pod itself on every attempt, so a forward follows
// its workload across restarts.
func profileForwards(p *profile.Profile, cl *catalog.Cluster) []daemon.Forward {
	out := make([]daemon.Forward, 0, len(p.Forwards))
	for _, f := range p.Forwards {
		fw := declaredForward(p, f)
		fw.ClusterID, fw.Cluster = cl.ID, cl.Name
		fw.Namespace = cmp.Or(fw.Namespace, cl.DefaultNamespace, "default")
		out = append(out, fw)
	}
	return out
}

// profileForwardStates joins the forwards profile p declares with the ones
// tugd runs for it: declared ones in the profile's order, stopped when tugd
// does not run them, then any tugd runs that the profile no longer has.
func (a *app) profileForwardStates(p *profile.Profile, running []daemon.ForwardState) []profileForward {
	byName := map[string]daemon.ForwardState{}
	for _, st := range running {
		byName[st.Name] = st
	}
	out := make([]profileForward, 0, len(p.Forwards)+len(running))
	for _, f := range p.Forwards {
		fw := declaredForward(p, f)
		st, ok := byName[fw.Name]
		if !ok {
			st = daemon.ForwardState{Forward: fw, State: forwardStopped}
		}
		delete(byName, fw.Name)
		out = append(out, profileForward{ForwardState: st, InProfile: true})
	}
	for _, st := range running {
		if _, ok := byName[st.Name]; ok {
			out = append(out, profileForward{ForwardState: st})
		}
	}
	return out
}

// startProfileForwards has tugd start the forwards of profile p on cluster
// cl that are not running, starting tugd when it is not running. tugd
// checks their local ports first and starts none when one is taken.
func (a *app) startProfileForwards(ctx context.Context, p *profile.Profile, cl *catalog.Cluster) error {
	if len(p.Forwards) == 0 {
		fmt.Fprintln(a.stderr, a.colors().Dim("profile "+p.Name+" has no forwards; add some under forwards: with `tug profile edit "+p.Name+"`"))
		return nil
	}
	c, err := a.ensureDaemon(ctx)
	if err != nil {
		return err
	}
	res, err := c.StartGroup(ctx, p.Name, profileForwards(p, cl))
	if err != nil {
		return fmt.Errorf("starting the forwards of profile %s: %w", p.Name, err)
	}
	for _, st := range res.Started {
		if st.State != daemon.ForwardUp {
			fmt.Fprintf(a.stdout, "Starting %s to %s; tug pf profile ls %s shows how it is doing\n", st.Name, st.Target, p.Name)
			continue
		}
		fmt.Fprintf(a.stdout, "Forwarding %s to %s (pod/%s) as %s\n", strings.Join(localPorts(st.Bound), ", "), st.Target, st.Pod, st.Name)
	}
	for _, st := range res.Running {
		fmt.Fprintf(a.stdout, "Already forwarding %s to %s as %s\n", strings.Join(localPorts(st.Bound), ", "), st.Target, st.Name)
	}
	if len(res.Failed) > 0 {
		errs := make([]error, 0, len(res.Failed)+1)
		for _, f := range res.Failed {
			errs = append(errs, fmt.Errorf("%s did not start: %s", f.Name, f.Error))
		}
		errs = append(errs, fmt.Errorf("running this again starts only what is not running; tug pf profile down %s stops the rest", p.Name))
		return errors.Join(errs...)
	}
	fmt.Fprintln(a.stderr, a.colors().Dim("tugd keeps them open across pod restarts; tug pf profile ls "+p.Name+" shows them, tug profile down "+p.Name+" ends them"))
	return nil
}

// stopProfileForwards has tugd end every forward of profile name, whatever
// its file says now, and says what it stopped.
func (a *app) stopProfileForwards(ctx context.Context, name string) error {
	if !profile.ValidName(name) {
		return fmt.Errorf("profile name %q: names are lower-case letters, digits, dashes and underscores, up to 31", name)
	}
	stopped, err := daemon.NewClient(a.paths.Runtime).StopGroup(ctx, name)
	if errors.Is(err, daemon.ErrNotRunning) {
		stopped, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("stopping the forwards of profile %s: %w", name, err)
	}
	if len(stopped) == 0 {
		_, err := fmt.Fprintf(a.stdout, "No forwards of profile %s are running\n", name)
		return err
	}
	for _, n := range stopped {
		fmt.Fprintf(a.stdout, "Stopped %s\n", n)
	}
	return nil
}

func newPfProfileCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Start, stop and list a profile's port-forwards on their own",
		Long: `A profile's forwards are a group that tugd runs together, named
<profile>.<forward>. These commands start and stop the group without
connecting the terminal; tug profile up and down do both.`,
	}
	up := &cobra.Command{
		Use:   "up <name>",
		Short: "Start a profile's port-forwards that are not running",
		Long: `Start a profile's port-forwards that are not running, through tugd. Every
local port they ask for is checked first, against each other, tugd's other
forwards and other programs, and none start when one is taken. Running it
again leaves running forwards alone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.profiles().Load(args[0])
			if err != nil {
				return err
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, err := a.chooseCluster(cat, p.Args())
			if err != nil {
				return fmt.Errorf("profile %s: %w", p.Name, err)
			}
			return a.startProfileForwards(cmd.Context(), p, cl)
		},
		ValidArgsFunction: a.completeProfiles,
	}
	down := &cobra.Command{
		Use:               "down <name>",
		Short:             "Stop a profile's port-forwards",
		Args:              cobra.ExactArgs(1),
		RunE:              func(cmd *cobra.Command, args []string) error { return a.stopProfileForwards(cmd.Context(), args[0]) },
		ValidArgsFunction: a.completeProfiles,
	}
	var output string
	ls := &cobra.Command{
		Use:     "ls [name]",
		Aliases: []string{"list"},
		Short:   "List a profile's port-forwards and how they are doing, or every profile's",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			groups, err := a.groupForwards(cmd.Context())
			if err != nil {
				return err
			}
			var states []profileForward
			if len(args) == 1 {
				p, err := a.profiles().Load(args[0])
				if err != nil {
					return err
				}
				states = a.profileForwardStates(p, groups[p.Name])
			} else {
				ps, listErr := a.profiles().List()
				if listErr != nil {
					fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), listErr)
				}
				for _, p := range ps {
					states = append(states, a.profileForwardStates(p, groups[p.Name])...)
					delete(groups, p.Name)
				}
				// Forwards of profiles whose files are gone.
				for _, running := range groups {
					for _, st := range running {
						states = append(states, profileForward{ForwardState: st})
					}
				}
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				if states == nil {
					states = []profileForward{}
				}
				return render.Structured(a.stdout, format, states)
			case render.FormatName:
				for _, st := range states {
					fmt.Fprintln(a.stdout, st.Name)
				}
				return nil
			}
			rows := make([][]string, 0, len(states))
			for _, st := range states {
				since := "-"
				if st.State != forwardStopped {
					since = shortDuration(a.now().Sub(st.Since))
				}
				rows = append(rows, []string{st.Name, st.Target, cmp.Or(st.Namespace, "-"), forwardPorts(st.ForwardState),
					a.forwardState(st), strconv.Itoa(st.Restarts), since})
			}
			if err := render.Table(a.stdout, []string{"NAME", "TARGET", "NAMESPACE", "PORTS", "STATE", "RESTARTS", "SINCE"}, rows); err != nil {
				return err
			}
			if len(states) > 0 {
				a.explainRestarts()
			}
			return nil
		},
		ValidArgsFunction: a.completeProfiles,
	}
	ls.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	cmd.AddCommand(up, down, ls)
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
