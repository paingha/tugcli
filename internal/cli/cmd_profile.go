package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/profile"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
)

func newProfileCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Switch a terminal to a saved environment: cluster, namespace, safety, aliases and forwards",
		Long: `A profile is a YAML file in tug's config directory that bundles a cluster, a
namespace, a safety mode, command aliases and named port-forwards, so a team
can review it like any other file and a terminal can switch to all of it at
once. tug profile edit writes one; tug profile ls shows where they live.

A profile can make a session read-only (safety: read-only) but never
read-write: on a cluster whose labels make it read-only, such as env=prod,
writes still ask first or need tug unlock. Profiles are chosen only by name;
a .tug.yaml pin never picks one.

  cluster: payments-prod      # a name, prefix or fuzzy match, as for tug connect
  namespace: shop
  safety: read-only           # or auto (the default): the labels decide
  proxy: true                 # every client goes through tugd
  aliases:
    api-logs: logs api -f     # tug profile run api-logs
  forwards:
    - name: db
      target: sts/postgres
      ports: [5432]`,
	}
	cmd.AddCommand(newProfileUseCmd(a), newProfileUpCmd(a), newProfileDownCmd(a), newProfileLsCmd(a),
		newProfileShowCmd(a), newProfileEditCmd(a), newProfileRunCmd(a))
	return cmd
}

func (a *app) profiles() *profile.Store { return profile.NewStore(a.paths.Config) }

func newProfileUseCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Connect this terminal as a profile says",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.profiles().Load(args[0])
			if err != nil {
				return err
			}
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			_, err = a.useProfile(cmd.Context(), s, p)
			return err
		},
		ValidArgsFunction: a.completeProfiles,
	}
}

func newProfileUpCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "up <name>",
		Short: "Connect this terminal as a profile says and start its port-forwards",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.profiles().Load(args[0])
			if err != nil {
				return err
			}
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			cl, err := a.useProfile(cmd.Context(), s, p)
			if err != nil {
				return err
			}
			return a.startProfileForwards(cmd.Context(), p, cl)
		},
		ValidArgsFunction: a.completeProfiles,
	}
}

func newProfileDownCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "down <name>",
		Short: "Stop a profile's port-forwards",
		Long: `Stop the port-forwards tugd runs for a profile, including ones since removed
from the file or of a profile since deleted. The terminal stays connected.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.stopProfileForwards(cmd.Context(), args[0])
		},
		ValidArgsFunction: a.completeProfiles,
	}
}

// useProfile connects session s as profile p says and returns the cluster.
// The session is read-only when the cluster's labels say so or the profile
// asks; a profile never makes it read-write. Like tug connect, it ends any
// pin.
func (a *app) useProfile(ctx context.Context, s *session.Session, p *profile.Profile) (*catalog.Cluster, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, err
	}
	cl, err := a.chooseCluster(cat, p.Args())
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	readOnly, auth, err := a.connectByHand(ctx, s, cl, connectOptions{namespace: p.Namespace, readOnly: p.ForceReadOnly(), proxy: p.Proxy})
	if err != nil {
		return nil, err
	}
	if err := s.SetProfile(p.Name); err != nil {
		return nil, err
	}

	c := a.colors()
	mode := c.Green("read-write")
	if readOnly {
		mode = c.Red("read-only") + c.Dim(" (tug unlock "+guard.ShortDuration(a.limits(cl).SuggestedUnlock())+" to allow writes)")
	}
	fmt.Fprintf(a.stdout, "Profile %s: connected to %s, namespace %s, %s\n", c.Bold(p.Name), c.Bold(cl.Name), s.State.Namespace, mode)
	if auth.ProxyURL != "" {
		fmt.Fprintln(a.stdout, c.Dim("Every client goes through tugd at "+auth.ProxyURL+", so the guard applies to kubectl, helm and k9s too"))
	}
	if len(p.Aliases) > 0 {
		fmt.Fprintln(a.stderr, c.Dim("aliases: "+strings.Join(p.AliasNames(), ", ")+"; tug profile run <alias> runs one"))
	}
	if cl.Stale {
		fmt.Fprintln(a.stderr, c.Yellow("warning: this cluster was not listed by its provider at the last sync"))
	}
	// Check credentials now so problems show here, not in the next kubectl.
	_, broker, err := a.services(cat)
	if err != nil {
		return nil, err
	}
	if _, err := broker.Resolve(ctx, cl.ID, false); err != nil {
		fmt.Fprintf(a.stderr, "%s %v\n", c.Yellow("warning: credentials are not ready:"), err)
	}
	return cl, nil
}

// profileView is a profile as tug profile ls and show print it with
// -o json|yaml.
type profileView struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Current is set for the profile this terminal was connected with.
	Current bool `json:"current"`
	*profile.Profile
	// Running are the profile's forwards that tugd runs now.
	Running []daemon.ForwardState `json:"running"`
}

// profileViews describes profiles, with their running forwards when tugd
// is running.
func (a *app) profileViews(ctx context.Context, ps []*profile.Profile) ([]profileView, error) {
	forwards, err := a.groupForwards(ctx)
	if err != nil {
		return nil, err
	}
	current := ""
	if s, err := a.session(); err == nil {
		current = s.State.Profile
	}
	views := make([]profileView, 0, len(ps))
	for _, p := range ps {
		path, err := a.profiles().Path(p.Name)
		if err != nil {
			return nil, err
		}
		running := forwards[p.Name]
		if running == nil {
			running = []daemon.ForwardState{}
		}
		views = append(views, profileView{Name: p.Name, Path: path, Current: p.Name == current, Profile: p, Running: running})
	}
	return views, nil
}

// groupForwards returns tugd's forwards by group, or none when tugd is not
// running.
func (a *app) groupForwards(ctx context.Context) (map[string][]daemon.ForwardState, error) {
	forwards, err := daemon.NewClient(a.paths.Runtime).Forwards(ctx)
	if errors.Is(err, daemon.ErrNotRunning) {
		return map[string][]daemon.ForwardState{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]daemon.ForwardState{}
	for _, f := range forwards {
		if f.Group != "" {
			out[f.Group] = append(out[f.Group], f)
		}
	}
	return out, nil
}

func newProfileLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the profiles",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			ps, listErr := a.profiles().List()
			if listErr != nil {
				// The profiles that parsed are still listed.
				fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), listErr)
			}
			views, err := a.profileViews(cmd.Context(), ps)
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, views)
			case render.FormatName:
				for _, v := range views {
					fmt.Fprintln(a.stdout, v.Name)
				}
				return nil
			}
			if len(views) == 0 {
				fmt.Fprintf(a.stderr, "No profiles in %s; tug profile edit <name> writes one\n", a.profiles().Dir())
				return nil
			}
			rows := make([][]string, 0, len(views))
			for _, v := range views {
				current := ""
				if v.Current {
					current = "*"
				}
				forwards := "-"
				if n := len(v.Forwards); n > 0 {
					forwards = fmt.Sprintf("%d (%d running)", n, len(v.Running))
				}
				aliases := "-"
				if len(v.Aliases) > 0 {
					aliases = strings.Join(v.AliasNames(), ",")
				}
				rows = append(rows, []string{current, v.Name, strings.Join(v.Args(), " "), cmp.Or(v.Namespace, "-"), v.Safety, yesNo(v.Proxy), forwards, aliases})
			}
			return render.Table(a.stdout, []string{"CURRENT", "NAME", "CLUSTER", "NAMESPACE", "SAFETY", "PROXY", "FORWARDS", "ALIASES"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newProfileShowCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "show [name]",
		Short: "Show a profile, by default this terminal's, and how its forwards are doing",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			name, err := a.profileName(args)
			if err != nil {
				return err
			}
			p, err := a.profiles().Load(name)
			if err != nil {
				return err
			}
			views, err := a.profileViews(cmd.Context(), []*profile.Profile{p})
			if err != nil {
				return err
			}
			v := views[0]
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, v)
			case render.FormatName:
				_, err := fmt.Fprintln(a.stdout, v.Name)
				return err
			}
			return a.describeProfile(v)
		},
		ValidArgsFunction: a.completeProfiles,
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

// profileName is the profile named in args, or this terminal's.
func (a *app) profileName(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	s, err := a.currentSession()
	if err != nil {
		return "", err
	}
	if s.State.Profile == "" {
		return "", errors.New("this terminal is not using a profile; name one, or run `tug profile use <name>`")
	}
	return s.State.Profile, nil
}

func (a *app) describeProfile(v profileView) error {
	c := a.colors()
	head := c.Bold(v.Name) + "  " + c.Dim(v.Path)
	if v.Current {
		head += "  " + c.Dim("(this terminal's)")
	}
	safety := "auto: the cluster's labels decide"
	if v.ForceReadOnly() {
		safety = c.Red("read-only") + ", whatever the cluster's labels"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  cluster:    %s\n  namespace:  %s\n  safety:     %s\n  proxy:      %s\n",
		head, strings.Join(v.Args(), " "), cmp.Or(v.Namespace, "the cluster's default"), safety, yesNo(v.Proxy))
	if len(v.Aliases) > 0 {
		b.WriteString("  aliases:\n")
		width := 0
		for _, n := range v.AliasNames() {
			width = max(width, len(n))
		}
		for _, n := range v.AliasNames() {
			fmt.Fprintf(&b, "    %-*s  tug %s\n", width, n, v.Aliases[n])
		}
	}
	if len(v.Forwards) > 0 {
		b.WriteString("  forwards:\n")
		rows := make([][]string, 0, len(v.Forwards))
		for _, st := range a.profileForwardStates(v.Profile, v.Running) {
			rows = append(rows, []string{"   ", st.Name, st.Target, cmp.Or(st.Namespace, "-"), forwardPorts(st.ForwardState), a.forwardState(st)})
		}
		if err := render.Table(&b, []string{"   ", "NAME", "TARGET", "NAMESPACE", "PORTS", "STATE"}, rows); err != nil {
			return err
		}
	}
	_, err := io.WriteString(a.stdout, b.String())
	return err
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func newProfileEditCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <name>",
		Short: "Write or change a profile in your editor, or from stdin",
		Long: `Open a profile in $VISUAL or $EDITOR, starting a new one from a commented
template with this terminal's cluster and namespace. tug checks the result
before saving it and offers to edit it again when it does not parse; until
then the profile stays as it was.

Without a terminal, the profile is read from stdin, so scripts can write one:
  tug profile edit payments < payments.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			store := a.profiles()
			path, err := store.Path(name)
			if err != nil {
				return err
			}
			if !a.interactive() {
				b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
				if err != nil {
					return fmt.Errorf("reading the profile from stdin: %w", err)
				}
				if len(bytes.TrimSpace(b)) == 0 {
					return fmt.Errorf("%w; give the profile on stdin: tug profile edit %s < %s.yaml", errNotInteractive, name, name)
				}
				if _, err := store.Save(name, b); err != nil {
					return fmt.Errorf("profile %s not saved: %w", name, err)
				}
				_, err = fmt.Fprintf(a.stdout, "Saved %s\n", path)
				return err
			}
			return a.editProfile(cmd.Context(), store, name, path)
		},
		ValidArgsFunction: a.completeProfiles,
	}
}

// editProfile edits a copy of the profile and saves it only once it parses,
// so a mistake never leaves a broken profile behind.
func (a *app) editProfile(ctx context.Context, store *profile.Store, name, path string) error {
	original, err := os.ReadFile(path)
	exists := err == nil
	switch {
	case errors.Is(err, os.ErrNotExist):
		cluster, namespace := "", ""
		if s, err := a.session(); err == nil && s.State.Connected() {
			cluster, namespace = s.State.ClusterName, s.State.Namespace
		}
		original = profile.Template(name, cluster, namespace)
	case err != nil:
		return fmt.Errorf("reading profile %s: %w", name, err)
	}
	if err := os.MkdirAll(store.Dir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(store.Dir(), "."+name+".*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(original); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	for {
		if err := a.runEditor(ctx, tmp.Name()); err != nil {
			return err
		}
		b, err := os.ReadFile(tmp.Name())
		if err != nil {
			return err
		}
		if exists && bytes.Equal(b, original) {
			_, err := fmt.Fprintf(a.stdout, "No change to %s\n", path)
			return err
		}
		_, saveErr := store.Save(name, b)
		if saveErr == nil {
			_, err := fmt.Fprintf(a.stdout, "Saved %s\n", path)
			return err
		}
		fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("This profile does not parse:"), saveErr)
		if !a.confirm("Edit it again?", true) {
			if exists {
				return fmt.Errorf("profile %s not saved; it is as it was", name)
			}
			return fmt.Errorf("profile %s not saved", name)
		}
	}
}

// runEditor opens path in $VISUAL, $EDITOR or the platform's editor, on the
// terminal tug was started from.
func (a *app) runEditor(ctx context.Context, path string) error {
	words, err := profile.Words(cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), defaultEditor()))
	if err != nil || len(words) == 0 {
		return fmt.Errorf("reading $VISUAL or $EDITOR: %w", cmp.Or(err, errors.New("empty")))
	}
	cmd := exec.CommandContext(ctx, words[0], append(words[1:], path)...)
	// stderr is a terminal whenever this runs, even when stdout is a pipe.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stderr, a.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", words[0], err)
	}
	return nil
}

// aliasKey marks, in a context, a command line an alias expanded to.
type aliasKey struct{}

func newProfileRunCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "run <alias> [args...]",
		Short: "Run an alias of this terminal's profile",
		Long: `Run an alias of the profile this terminal was connected with. The alias's
command line runs as tug would run it, followed by any further arguments,
and is held to the same guard as when typed out.`,
		Example: `  tug profile run api-logs           # tug logs api -f, from the profile's aliases
  tug profile run psql -- -c 'select 1'`,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true, // flags after the alias belong to its command
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
				return cmd.Help()
			}
			ctx := cmd.Context()
			if ctx.Value(aliasKey{}) != nil {
				return errors.New("an alias cannot run another alias")
			}
			s, err := a.currentSession()
			if err != nil {
				return err
			}
			if s.State.Profile == "" {
				return errors.New("this terminal is not using a profile; run `tug profile use <name>` first")
			}
			p, err := a.profiles().Load(s.State.Profile)
			if err != nil {
				return err
			}
			words, err := p.Expand(args[0], args[1:])
			if err != nil {
				return err
			}
			a.log.Debug("running alias", "profile", p.Name, "alias", args[0], "args", words)
			return a.execute(context.WithValue(ctx, aliasKey{}, args[0]), words)
		},
		ValidArgsFunction: a.completeAliases,
	}
}

// completeProfiles completes profile names.
func (a *app) completeProfiles(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if a.cfg == nil {
		if err := a.setup(false); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
	}
	ps, _ := a.profiles().List()
	var out []string
	for _, p := range ps {
		if strings.HasPrefix(p.Name, toComplete) {
			out = append(out, p.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeAliases completes the aliases of this terminal's profile.
func (a *app) completeAliases(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveDefault
	}
	if a.cfg == nil {
		if err := a.setup(false); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
	}
	s, err := a.session()
	if err != nil || s.State.Profile == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	p, err := a.profiles().Load(s.State.Profile)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := slices.DeleteFunc(p.AliasNames(), func(n string) bool { return !strings.HasPrefix(n, toComplete) })
	return names, cobra.ShellCompDirectiveNoFileComp
}
