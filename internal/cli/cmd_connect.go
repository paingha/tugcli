package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/tunnel"
	"github.com/paingha/tugcli/internal/version"
)

type connectOptions struct {
	namespace string
	readOnly  bool
	readWrite bool
	proxy     bool
}

func newConnectCmd(a *app) *cobra.Command {
	var o connectOptions
	cmd := &cobra.Command{
		Use:   "connect [cluster] [label=value...]",
		Short: "Point this terminal at a cluster",
		Long: `Point this terminal at a cluster. Other terminals and ~/.kube/config are not
touched. The cluster can be a name, a prefix, or a fuzzy match; label=value
arguments filter first, for example: tug connect gpu=true.

Clusters labelled env=prod open read-only, as do those a team policy makes
read-only. Use tug unlock to allow writes for a while, or --read-write to
connect without the guard where no policy limits unlocks.`,
		Aliases: []string{"c"},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			return a.connect(cmd.Context(), s, args, o)
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace for this session")
	cmd.Flags().BoolVar(&o.readOnly, "read-only", false, "open read-only whatever the cluster's labels")
	cmd.Flags().BoolVar(&o.readWrite, "read-write", false, "open read-write even on a production cluster")
	cmd.Flags().BoolVar(&o.proxy, "proxy", false, "go through tugd's local API proxy, so the guard applies to every client")
	cmd.MarkFlagsMutuallyExclusive("read-only", "read-write")
	return cmd
}

// connect points session s at the cluster chosen by args.
func (a *app) connect(ctx context.Context, s *session.Session, args []string, o connectOptions) error {
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	cl, err := a.chooseCluster(cat, args)
	if err != nil {
		return err
	}
	limits := a.limits(cl)
	if o.readWrite {
		if err := limits.CheckReadWrite(cl.Name); err != nil {
			return err
		}
	}
	readOnly, auth, err := a.connectByHand(ctx, s, cl, o)
	if err != nil {
		return err
	}

	c := a.colors()
	mode := c.Green("read-write")
	if readOnly {
		mode = c.Red("read-only") + c.Dim(" (tug unlock "+guard.ShortDuration(limits.Unlock(defaultUnlock))+" to allow writes)")
	}
	fmt.Fprintf(a.stdout, "Connected to %s, namespace %s, %s\n", c.Bold(cl.Name), s.State.Namespace, mode)
	if auth.ProxyURL != "" {
		fmt.Fprintln(a.stdout, c.Dim("Every client goes through tugd at "+auth.ProxyURL+", so the guard applies to kubectl, helm and k9s too"))
	}
	if cl.Stale {
		fmt.Fprintln(a.stderr, c.Yellow("warning: this cluster was not listed by its provider at the last sync"))
	}

	// Check credentials now so problems show here, not in the next kubectl.
	_, broker, err := a.services(cat)
	if err != nil {
		return err
	}
	if _, err := broker.Resolve(ctx, cl.ID, false); err != nil {
		fmt.Fprintf(a.stderr, "%s %v\n", c.Yellow("warning: credentials are not ready:"), err)
	}
	return nil
}

// connectByHand points session s at cl as the user chose, in tug connect or
// the TUI. It ends any pin: leaving the pinned directory must not undo the
// user's own choice.
func (a *app) connectByHand(ctx context.Context, s *session.Session, cl *catalog.Cluster, o connectOptions) (bool, session.AuthCommand, error) {
	readOnly, auth, err := a.connectTo(ctx, s, cl, o)
	if err != nil {
		return false, auth, err
	}
	if s.State.Pin != nil {
		if err := s.SetPin(nil); err != nil {
			return false, auth, err
		}
	}
	return readOnly, auth, nil
}

// connectTo points session s at cl: read-only by its labels unless o says
// otherwise, and through tugd when o asks or the cluster has a tunnel.
func (a *app) connectTo(ctx context.Context, s *session.Session, cl *catalog.Cluster, o connectOptions) (bool, session.AuthCommand, error) {
	limits := a.limits(cl)
	readOnly := limits.ReadOnly
	switch {
	case o.readOnly:
		readOnly = true
	case o.readWrite:
		// tug connect refuses --read-write where a policy limits unlocks;
		// putting a connection back, as leaving a pinned directory does,
		// opens read-only there instead.
		if err := limits.CheckReadWrite(cl.Name); err != nil {
			fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), err)
			break
		}
		readOnly = false
	}
	auth := session.AuthCommand{Command: selfPath()}
	if tun := cl.EffectiveTunnel(); tun != "" && !o.proxy {
		// kubectl cannot open tug's tunnels; tugd can, and keeps them open.
		fmt.Fprintln(a.stderr, a.colors().Dim(cl.Name+" is reached through "+tunnel.Redacted(tun)+", so this session goes through tugd"))
		o.proxy = true
	}
	if o.proxy {
		c, err := a.ensureDaemon(ctx)
		if err != nil {
			return false, auth, err
		}
		reg, err := c.Register(ctx, s.State.ID)
		if err != nil {
			return false, auth, err
		}
		auth.ProxyURL, auth.ProxyCA = reg.ProxyURL, reg.ProxyCA
	}
	return readOnly, auth, s.Connect(cl, o.namespace, readOnly, auth)
}

// chooseCluster resolves a target and label filters to one cluster.
func (a *app) chooseCluster(cat *catalog.Catalog, args []string) (*catalog.Cluster, error) {
	var query string
	sel := map[string]string{}
	for _, arg := range args {
		if k, v, ok := strings.Cut(arg, "="); ok {
			sel[k] = v
			continue
		}
		if query != "" {
			return nil, fmt.Errorf("one cluster at a time: got %q and %q", query, arg)
		}
		query = arg
	}
	if len(cat.Clusters) == 0 {
		return nil, errors.New("the catalog is empty; run `tug import` first")
	}
	candidates := cat.Select(sel)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no cluster has labels %v", sel)
	}
	byName := make(map[string]*catalog.Cluster, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, cl := range candidates {
		byName[cl.Name] = cl
		names = append(names, cl.Name)
	}
	if query != "" {
		if cl, err := cat.Get(query); err == nil && slices.Contains(candidates, cl) {
			return cl, nil
		}
		names = resolve.Best(query, names)
		if len(names) == 0 {
			return nil, fmt.Errorf("no cluster matches %q; `tug ls` lists them, `tug sync` refreshes", query)
		}
	}
	i, err := a.pick("Clusters:", names)
	if err != nil {
		return nil, err
	}
	return byName[names[i]], nil
}

func newDisconnectCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Stop pointing this terminal at a cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			return s.Disconnect()
		},
	}
}

// statusView is what status prints with -o json|yaml.
type statusView struct {
	Session       string     `json:"session"`
	Cluster       string     `json:"cluster,omitempty"`
	ClusterID     string     `json:"clusterID,omitempty"`
	Namespace     string     `json:"namespace,omitempty"`
	ReadOnly      bool       `json:"readOnly"`
	Writable      bool       `json:"writable"`
	UnlockedUntil *time.Time `json:"unlockedUntil,omitempty"`
	Kubeconfig    string     `json:"kubeconfig,omitempty"`
	ProxyURL      string     `json:"proxyURL,omitempty"`
	Env           string     `json:"env,omitempty"`
	Production    bool       `json:"production,omitempty"`
	Profile       string     `json:"profile,omitempty"`
}

func newStatusCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"current"},
		Short:   "Show this terminal's cluster, namespace and mode",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			st := s.State
			v := statusView{
				Session:    st.ID,
				Cluster:    st.ClusterName,
				ClusterID:  st.ClusterID,
				Namespace:  st.Namespace,
				ReadOnly:   st.ReadOnly,
				Writable:   st.Writable(a.now()),
				ProxyURL:   st.ProxyURL,
				Env:        st.Env,
				Production: st.Production,
				Profile:    st.Profile,
			}
			if st.Connected() {
				v.Kubeconfig = s.KubeconfigPath()
			}
			if a.now().Before(st.UnlockedUntil) {
				v.UnlockedUntil = &st.UnlockedUntil
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, v)
			case render.FormatName:
				_, err := fmt.Fprintln(a.stdout, st.ClusterName)
				return err
			}
			if !st.Connected() {
				_, err := fmt.Fprintln(a.stdout, "Not connected. Run `tug connect`.")
				return err
			}
			c := a.colors()
			mode := c.Green("read-write")
			switch {
			case v.UnlockedUntil != nil:
				mode = c.Yellow("unlocked until " + v.UnlockedUntil.Local().Format(time.Kitchen))
			case st.ReadOnly:
				mode = c.Red("read-only")
			}
			fmt.Fprintf(a.stdout, "cluster:    %s\nnamespace:  %s\nmode:       %s\nkubeconfig: %s\n",
				c.Bold(st.ClusterName), st.Namespace, mode, v.Kubeconfig)
			if st.ProxyURL != "" {
				fmt.Fprintf(a.stdout, "proxy:      %s (tugd guards every client)\n", st.ProxyURL)
			}
			if st.Env != "" || st.Production {
				env := st.Env
				if st.Production {
					env = c.Red(strings.TrimSpace(env + " (production)"))
				}
				fmt.Fprintf(a.stdout, "env:        %s\n", env)
			}
			if st.Profile != "" {
				fmt.Fprintf(a.stdout, "profile:    %s\n", st.Profile)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newNsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ns [namespace]",
		Short: "Switch this session's namespace (fuzzy)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			if !s.State.Connected() {
				return errors.New("not connected; run `tug connect` first")
			}
			names, listErr := a.namespaces(cmd.Context(), s)
			if len(args) == 0 {
				if listErr != nil {
					return listErr
				}
				if !a.interactive() {
					for _, n := range names {
						marker := "  "
						if n == s.State.Namespace {
							marker = "* "
						}
						fmt.Fprintln(a.stdout, marker+n)
					}
					return nil
				}
				i, err := a.pick("Namespaces:", names)
				if err != nil {
					return err
				}
				return a.setNamespace(s, names[i])
			}
			target := args[0]
			if listErr != nil {
				// Listing namespaces is often forbidden; trust the name given.
				a.log.Debug("listing namespaces", "err", listErr)
				return a.setNamespace(s, target)
			}
			matches := resolve.Best(target, names)
			switch len(matches) {
			case 0:
				return fmt.Errorf("no namespace matches %q", target)
			case 1:
				return a.setNamespace(s, matches[0])
			}
			i, err := a.pick("Namespaces:", matches)
			if err != nil {
				return err
			}
			return a.setNamespace(s, matches[i])
		},
	}
}

func (a *app) setNamespace(s *session.Session, ns string) error {
	if err := s.SetNamespace(ns); err != nil {
		return err
	}
	_, err := fmt.Fprintf(a.stdout, "Namespace %s on %s\n", a.colors().Bold(ns), s.State.ClusterName)
	return err
}

// factory returns a client factory for the session's cluster.
func (a *app) factory(s *session.Session) (*kube.Factory, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, err
	}
	cl, err := cat.Get(s.State.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("session cluster: %w; run `tug connect` again", err)
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return nil, err
	}
	policy := &guard.Policy{
		Cluster:       cl.Name,
		ClusterID:     cl.ID,
		Session:       s.State.ID,
		ReadOnly:      s.State.ReadOnly,
		UnlockedUntil: s.State.UnlockedUntil,
		Now:           a.now,
		Confirm: func(action string) bool {
			return a.confirm(fmt.Sprintf("%s is read-only. Allow %s?", cl.Name, action), false)
		},
		Audit: a.audit(),
	}
	return &kube.Factory{
		Cluster:   cl,
		Namespace: s.State.Namespace,
		Broker:    broker,
		Guard:     policy,
		CacheDir:  a.paths.Cache,
		UserAgent: "tug/" + version.String(),
		Tunnels:   a.tunnels(),
	}, nil
}

func (a *app) namespaces(ctx context.Context, s *session.Session) ([]string, error) {
	f, err := a.factory(s)
	if err != nil {
		return nil, err
	}
	cs, err := f.Clientset(ctx)
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("not allowed to list namespaces on %s: %w", s.State.ClusterName, err)
		}
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	slices.Sort(names)
	return names, nil
}

// defaultUnlock is how long tug unlock allows writes when no duration is
// given and no policy says less.
const defaultUnlock = 15 * time.Minute

func newUnlockCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock [duration]",
		Short: "Allow writes on a read-only session for a while (default 15m)",
		Long: `Allow writes on a read-only session for a while, 15 minutes by default. A
team policy can limit how long an unlock lasts; tug unlock then refuses a
longer one, and without a duration unlocks for the limit if it is shorter.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d := defaultUnlock
			if len(args) == 1 {
				var err error
				if d, err = time.ParseDuration(args[0]); err != nil || d <= 0 {
					return fmt.Errorf("invalid duration %q; use for example 15m or 1h", args[0])
				}
			}
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			if !s.State.Connected() {
				return errors.New("not connected")
			}
			if !s.State.ReadOnly {
				_, err := fmt.Fprintf(a.stdout, "%s is already read-write\n", s.State.ClusterName)
				return err
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			if cl, err := cat.Get(s.State.ClusterID); err == nil {
				limits := a.limits(cl)
				if len(args) == 1 {
					if err := limits.CheckUnlock(cl.Name, d); err != nil {
						return err
					}
				}
				d = limits.Unlock(d)
			}
			if err := s.Unlock(d); err != nil {
				return err
			}
			c := a.colors()
			_, err = fmt.Fprintf(a.stdout, "%s writes allowed on %s until %s\n", c.Yellow("Unlocked:"), c.Bold(s.State.ClusterName),
				s.State.UnlockedUntil.Local().Format(time.Kitchen))
			return err
		},
	}
}

func newLockCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Make a read-only session read-only again now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			return s.Lock()
		},
	}
}

func newShellCmd(a *app) *cobra.Command {
	var o connectOptions
	cmd := &cobra.Command{
		Use:   "shell [cluster] [label=value...]",
		Short: "Start a subshell with its own session (for shells without the hook)",
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr := a.sessions()
			parent := os.Getenv(session.EnvSession)
			pid := os.Getpid() // the shell replaces this process, keeping the PID
			s, err := mgr.Create(pid, parent)
			if err != nil {
				return err
			}
			if len(args) > 0 {
				if err := a.connect(cmd.Context(), s, args, o); err != nil {
					_ = mgr.Remove(s.State.ID)
					return err
				}
			}
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = defaultShell()
			}
			env := os.Environ()
			for _, kv := range sessionEnv(s, mgr, pid) {
				env = append(env, kv[0]+"="+kv[1])
			}
			if s.State.Connected() {
				env = append(env, "KUBECONFIG="+s.KubeconfigPath())
			}
			return a.replaceProcess(shell, nil, env)
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace for the new session")
	cmd.Flags().BoolVar(&o.readOnly, "read-only", false, "open read-only whatever the cluster's labels")
	cmd.Flags().BoolVar(&o.readWrite, "read-write", false, "open read-write even on a production cluster")
	return cmd
}

// completeClusters completes cluster names from the catalog.
func (a *app) completeClusters(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if a.cfg == nil {
		if err := a.setup(false); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, cl := range cat.Clusters {
		if strings.HasPrefix(cl.Name, toComplete) {
			out = append(out, cl.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
