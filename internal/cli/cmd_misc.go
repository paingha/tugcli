package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/version"
)

func newAuthCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Credential commands used by kubectl and other clients",
	}
	var clusterID string
	var refresh bool
	token := &cobra.Command{
		Use:   "token --cluster <id>",
		Short: "Print an ExecCredential for a cluster (the exec entry in session kubeconfigs)",
		Long: `Print a client.authentication.k8s.io/v1 ExecCredential for a cluster. Every
kubeconfig tug writes calls this, so secrets stay in the keychain and never in a
file. It answers from the cache, or asks the cluster's provider on a miss.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, err := cat.Get(clusterID)
			if err != nil {
				return err
			}
			_, broker, err := a.services(cat)
			if err != nil {
				return err
			}
			ec, err := broker.ExecCredential(cmd.Context(), cl.ID, refresh)
			if err != nil {
				return fmt.Errorf("credentials for %s: %w", cl.Name, err)
			}
			return json.NewEncoder(a.stdout).Encode(ec)
		},
	}
	token.Flags().StringVar(&clusterID, "cluster", "", "catalog cluster ID or name")
	token.Flags().BoolVar(&refresh, "refresh", false, "ignore the cache and ask the provider")
	_ = token.MarkFlagRequired("cluster")
	cmd.AddCommand(token)
	return cmd
}

func newPluginCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plugin",
		Aliases: []string{"plugins"},
		Short:   "List, link and inspect provider plugins",
	}

	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List providers, built in and plugins",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg := a.registry()
			rows := make([][]string, 0, len(reg.Names()))
			for _, name := range reg.Names() {
				e, _ := reg.Entry(name)
				info := e.Provider.Info()
				rows = append(rows, []string{name, dash(info.Version), e.Kind.String(), info.Capabilities.String(), dash(e.Location)})
			}
			if err := render.Table(a.stdout, []string{"NAME", "VERSION", "KIND", "CAPABILITIES", "LOCATION"}, rows); err != nil {
				return err
			}
			for _, p := range a.problems {
				fmt.Fprintf(a.stderr, "%s %s: %v\n", a.colors().Yellow("skipped"), p.Location, p.Err)
			}
			return nil
		},
	}

	link := &cobra.Command{
		Use:   "link <dir>",
		Short: "Use a plugin directory under development (no checksum; marked dev)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := plugin.NewDirs(a.paths.Data).Link(args[0])
			if err != nil {
				return err
			}
			if e, ok := a.registry().Entry(p.Name()); ok && e.Kind == registry.KindBuiltin {
				fmt.Fprintf(a.stderr, "%s %s is a built-in provider name; the plugin is ignored unless plugins.allowOverride lists it\n",
					a.colors().Yellow("warning:"), p.Name())
			}
			_, err = fmt.Fprintf(a.stdout, "Linked %s %s from %s %s\n", p.Name(), p.Manifest.Metadata.Version, p.Dir,
				a.colors().Yellow("(dev: not checksummed)"))
			return err
		},
	}

	unlink := &cobra.Command{
		Use:   "unlink <name>",
		Short: "Stop using a linked plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return plugin.NewDirs(a.paths.Data).Unlink(args[0])
		},
	}

	describe := &cobra.Command{
		Use:   "describe <name>",
		Short: "Run a plugin's describe operation and show its manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.describePlugin(cmd.Context(), args[0])
		},
	}

	cmd.AddCommand(ls, link, unlink, describe)
	return cmd
}

func (a *app) describePlugin(ctx context.Context, name string) error {
	p, err := a.registry().Get(name)
	if err != nil {
		return err
	}
	pp, ok := p.(*plugin.Provider)
	if !ok {
		return fmt.Errorf("%s is a built-in provider", name)
	}
	d, err := pp.Describe(ctx)
	if err != nil {
		return err
	}
	m := pp.Plugin().Manifest
	secrets := make([]string, 0, len(m.Spec.Secrets))
	for _, s := range m.Spec.Secrets {
		secrets = append(secrets, s.Name)
	}
	fmt.Fprintf(a.stdout, "name:          %s\nversion:       %s\npublisher:     %s\nprotocols:     %s\ncapabilities:  %s\nsecrets:       %s\nenv:           %s\nnetwork:       %s\nexecutable:    %s\n",
		d.Name, d.Version, dash(m.Metadata.Publisher), strings.Join(d.Protocols, ", "), d.Capabilities,
		dash(strings.Join(secrets, ", ")), dash(strings.Join(m.Spec.Env, ", ")), dash(strings.Join(m.Spec.Network, ", ")), pp.Plugin().Bin)
	if d.Name != m.Metadata.Name {
		fmt.Fprintf(a.stderr, "%s describe says %q but the manifest says %q\n", a.colors().Yellow("warning:"), d.Name, m.Metadata.Name)
	}
	return nil
}

func newVersionCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print tug's version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			v := version.Get()
			if format == render.FormatJSON || format == render.FormatYAML {
				return render.Structured(a.stdout, format, v)
			}
			_, err = fmt.Fprintf(a.stdout, "tug %s (%s, %s, %s)\n", v.Version, dash(shortCommit(v.Commit)), v.GoVersion, v.Platform)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// passthrough runs kubectl with this terminal's session. On a read-only
// session, commands that may write are refused unless the user confirms.
func (a *app) passthrough(_ context.Context, args []string) (int, error) {
	kubeconfigPath := ""
	s, err := a.sessions().Current()
	switch {
	case err == nil && s.State.Connected():
		kubeconfigPath = s.KubeconfigPath()
		if !s.State.Writable(a.now()) && !passthrough.IsRead(args) {
			question := fmt.Sprintf("%s is read-only. Run `kubectl %s` anyway?", s.State.ClusterName, strings.Join(args, " "))
			if !a.confirm(question, false) {
				return 1, fmt.Errorf("refused: %s is read-only and `kubectl %s` may write; run `tug unlock 15m` to allow writes",
					s.State.ClusterName, strings.Join(args, " "))
			}
		}
	case err == nil, errors.Is(err, session.ErrNoSession):
		// Not connected: behave like plain kubectl.
	default:
		return 1, err
	}
	cmd, err := passthrough.Command(a.cfg.Kubectl, args, kubeconfigPath)
	if err != nil {
		return 1, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	if err := cmd.Run(); err != nil {
		code := passthrough.ExitCode(err)
		var ee interface{ ExitCode() int }
		if !errors.As(err, &ee) {
			return code, err
		}
		return code, nil
	}
	return 0, nil
}
