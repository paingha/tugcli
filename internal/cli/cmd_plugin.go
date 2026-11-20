package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

func newPluginCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plugin",
		Aliases: []string{"plugins"},
		Short:   "Install, write and test provider plugins",
	}
	cmd.AddCommand(
		newPluginLsCmd(a), newPluginInstallCmd(a), newPluginRemoveCmd(a),
		newPluginLinkCmd(a), newPluginUnlinkCmd(a), newPluginDescribeCmd(a),
		newPluginNewCmd(a), newPluginTestCmd(a), newPluginPackageCmd(a),
	)
	return cmd
}

func (a *app) pluginDirs() plugin.Dirs { return plugin.NewDirs(a.paths.Data) }

func newPluginLsCmd(a *app) *cobra.Command {
	return &cobra.Command{
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
}

type installOptions struct {
	sha256 string
	yes    bool
	force  bool
}

func newPluginInstallCmd(a *app) *cobra.Command {
	var o installOptions
	cmd := &cobra.Command{
		Use:   "install <file|url>",
		Short: "Install a plugin archive after checking its checksums",
		Long: `Install a plugin from a .tar.gz or .zip archive, such as one made by
tug plugin package. Every binary must match the sha256 in the archive's
manifest. Archives from a URL also need --sha256, which pins the archive
itself. Before installing, tug shows the publisher, the secrets the plugin
will receive and the hosts it talks to, and asks you to confirm.

Installing by name from the reviewed plugin index arrives in v1.`,
		Example: `  tug plugin install ./tug-provider-acme_0.1.0.tar.gz
  tug plugin install https://example.com/tug-provider-acme_0.1.0.tar.gz --sha256 9f2c...`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installPlugin(cmd.Context(), args[0], o)
		},
	}
	cmd.Flags().StringVar(&o.sha256, "sha256", "", "expected sha256 of the archive (required for URLs)")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "install without asking")
	cmd.Flags().BoolVar(&o.force, "force", false, "replace the same version if it is installed")
	return cmd
}

func (a *app) installPlugin(ctx context.Context, src string, o installOptions) error {
	dirs := a.pluginDirs()
	archive := src
	if u, err := url.Parse(src); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		if o.sha256 == "" {
			return fmt.Errorf("installing from a URL needs --sha256 so the download can be verified")
		}
		tmp, err := os.MkdirTemp("", "tug-plugin-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if archive, err = plugin.Fetch(ctx, src, tmp); err != nil {
			return err
		}
	} else if _, err := os.Stat(src); err != nil {
		if !strings.ContainsAny(src, `/\.`) {
			return fmt.Errorf("installing %q by name needs the plugin index, which arrives in v1; install from a file or URL", src)
		}
		return err
	}

	staged, err := dirs.Stage(archive, o.sha256)
	if err != nil {
		return err
	}
	defer staged.Discard()
	m := staged.Plugin.Manifest
	if e, ok := a.registry().Entry(m.Metadata.Name); ok && e.Kind == registry.KindBuiltin &&
		!slices.Contains(a.cfg.Plugins.AllowOverride, m.Metadata.Name) {
		return fmt.Errorf("%s is a built-in provider; a plugin may replace it only if plugins.allowOverride in config.yaml lists it", m.Metadata.Name)
	}

	a.printManifest(m)
	if !o.yes {
		if !a.interactive() {
			return fmt.Errorf("review the plugin above, then pass --yes to install it without a terminal")
		}
		if !a.confirm(fmt.Sprintf("Install %s %s?", m.Metadata.Name, m.Metadata.Version), false) {
			return fmt.Errorf("not installed")
		}
	}
	p, err := dirs.Commit(staged, o.force)
	if err != nil {
		return err
	}
	c := a.colors()
	fmt.Fprintf(a.stdout, "%s %s %s in %s\n", c.Green("Installed"), p.Name(), m.Metadata.Version, p.Dir)
	fmt.Fprintf(a.stdout, "Next: tug import %s --account <id>\n", p.Name())
	return nil
}

// printManifest shows what a plugin will be able to see and reach.
func (a *app) printManifest(m *plugin.Manifest) {
	secrets := make([]string, 0, len(m.Spec.Secrets))
	for _, s := range m.Spec.Secrets {
		secrets = append(secrets, s.Name)
	}
	platforms := make([]string, 0, len(m.Spec.Platforms))
	for _, p := range m.Spec.Platforms {
		platforms = append(platforms, p.OS+"/"+p.Arch)
	}
	c := a.colors()
	fmt.Fprintf(a.stderr, "%s %s\n", c.Bold(m.Metadata.Name), m.Metadata.Version)
	fmt.Fprintf(a.stderr, "  publisher:     %s\n", dash(m.Metadata.Publisher))
	fmt.Fprintf(a.stderr, "  homepage:      %s\n", dash(m.Metadata.Homepage))
	fmt.Fprintf(a.stderr, "  capabilities:  %s\n", m.Spec.Capabilities)
	fmt.Fprintf(a.stderr, "  secrets:       %s\n", dash(strings.Join(secrets, ", ")))
	fmt.Fprintf(a.stderr, "  environment:   %s\n", dash(strings.Join(m.Spec.Env, ", ")))
	fmt.Fprintf(a.stderr, "  network hosts: %s\n", dash(strings.Join(m.Spec.Network, ", ")))
	fmt.Fprintf(a.stderr, "  platforms:     %s\n", strings.Join(platforms, ", "))
}

func newPluginRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm", "uninstall"},
		Short:   "Remove every installed version of a plugin",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs := a.pluginDirs()
			versions, err := dirs.Remove(args[0])
			if err != nil {
				if links, lerr := dirs.Links(); lerr == nil && links[args[0]] != "" {
					return fmt.Errorf("%s is linked, not installed; use tug plugin unlink %s", args[0], args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Removed %s %s\n", args[0], strings.Join(versions, ", "))
			return err
		},
	}
}

func newPluginLinkCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "link <dir>",
		Short: "Use a plugin directory under development (no checksum; marked dev)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.pluginDirs().Link(args[0])
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
}

func newPluginUnlinkCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <name>",
		Short: "Stop using a linked plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.pluginDirs().Unlink(args[0])
		},
	}
}

func newPluginDescribeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <name>",
		Short: "Run a plugin's describe operation and show its manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.registry().Get(args[0])
			if err != nil {
				return err
			}
			pp, ok := p.(*plugin.Provider)
			if !ok {
				return fmt.Errorf("%s is a built-in provider", args[0])
			}
			d, err := pp.Describe(cmd.Context())
			if err != nil {
				return err
			}
			a.printManifest(pp.Plugin().Manifest)
			fmt.Fprintf(a.stderr, "  protocols:     %s\n  executable:    %s\n", strings.Join(d.Protocols, ", "), pp.Plugin().Bin)
			if d.Name != pp.Plugin().Name() {
				fmt.Fprintf(a.stderr, "%s describe says %q but the manifest says %q\n", a.colors().Yellow("warning:"), d.Name, pp.Plugin().Name())
			}
			return nil
		},
	}
}

func newPluginNewCmd(a *app) *cobra.Command {
	var lang, dir, module, sdkReplace string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Start a new plugin with the SDK, tests and a release workflow",
		Example: `  tug plugin new acme
  tug plugin new acme --lang python
  tug plugin new acme --module github.com/acme/tug-provider-acme`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := plugin.ParseLang(lang)
			if err != nil {
				return err
			}
			files, err := plugin.Scaffold(plugin.ScaffoldOptions{
				Name: args[0], Lang: l, Dir: dir, Module: module, SDKReplace: sdkReplace,
				SDKVersion: sdkVersion(),
			})
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Fprintln(a.stdout, "  created "+f)
			}
			root := filepath.Dir(files[0])
			fmt.Fprintf(a.stdout, "\nNext:\n  cd %s\n", root)
			if l == plugin.LangGo {
				fmt.Fprintln(a.stdout, "  go mod tidy && make test && make conformance")
				if sdkReplace == "" {
					// The SDK lives in this repository, which may not be
					// reachable from everywhere yet.
					fmt.Fprintln(a.stderr, a.colors().Dim("go mod tidy fetches the SDK, github.com/paingha/tugcli. If you cannot reach it, set GOPRIVATE=github.com/paingha/tugcli with access to the repository, or scaffold again with --sdk-replace <tugcli checkout>."))
				}
			} else {
				fmt.Fprintf(a.stdout, "  python3 -m unittest && tug plugin link . && TUG_SECRET_API_TOKEN=demo_test tug plugin test %s --set clusters=dev=https://127.0.0.1:6443\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&lang, "lang", "go", "language: go or python")
	cmd.Flags().StringVar(&dir, "dir", "", "directory to create (default ./tug-provider-<name>)")
	cmd.Flags().StringVar(&module, "module", "", "Go module path (default example.com/tug-provider-<name>)")
	cmd.Flags().StringVar(&sdkReplace, "sdk-replace", "", "use a local tugcli checkout for the SDK")
	_ = cmd.Flags().MarkHidden("sdk-replace")
	return cmd
}

// sdkVersion pins new plugins to this tug's SDK when tug is a release;
// development builds leave the version to `go mod tidy`.
func sdkVersion() string {
	if v := version.String(); version.IsRelease(v) {
		return v
	}
	return ""
}

func newPluginPackageCmd(a *app) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "package [dir]",
		Short: "Write an installable archive, recording each binary's sha256",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			res, err := plugin.Package(dir, out)
			if err != nil {
				return err
			}
			c := a.colors()
			for _, p := range res.Missing {
				fmt.Fprintf(a.stderr, "%s %s/%s: %s is not built\n", c.Yellow("left out"), p.OS, p.Arch, p.Bin)
			}
			for _, p := range res.Mismatched {
				fmt.Fprintf(a.stderr, "%s %s/%s: %s was built for another platform\n", c.Yellow("left out"), p.OS, p.Arch, p.Bin)
			}
			platforms := make([]string, 0, len(res.Manifest.Spec.Platforms))
			for _, p := range res.Manifest.Spec.Platforms {
				platforms = append(platforms, p.OS+"/"+p.Arch)
			}
			_, err = fmt.Fprintf(a.stdout, "%s\n  platforms: %s\n  sha256:    %s\n", res.Path, strings.Join(platforms, ", "), res.SHA256)
			return err
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "dist", "directory to write the archive to")
	return cmd
}

type pluginTestOptions struct {
	account string
	set     []string
	output  string
}

func newPluginTestCmd(a *app) *cobra.Command {
	var o pluginTestOptions
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Run the protocol conformance suite against a plugin or built-in",
		Long: `Run tug's conformance suite against a provider: describe is valid, stdout
carries only JSON, invalid secrets give Unauthenticated, list is stable,
credentials are unexpired, SIGTERM is honoured, and more.

Checks that need real data use --account (an imported account) or --set
settings, with secrets from the keychain or TUG_SECRET_<NAME> variables.
Without them those checks are skipped.`,
		Example: `  tug plugin test acme --account prod
  TUG_SECRET_API_TOKEN=demo_test tug plugin test acme --set clusters=dev=https://127.0.0.1:6443
  tug plugin test kubeconfig --account default`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.testPlugin(cmd.Context(), args[0], o)
		},
	}
	cmd.Flags().StringVar(&o.account, "account", "", "imported account to test against")
	cmd.Flags().StringArrayVar(&o.set, "set", nil, "account setting as key=value (repeatable)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table or json")
	return cmd
}

func (a *app) testPlugin(ctx context.Context, name string, o pluginTestOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	e, ok := a.registry().Entry(name)
	if !ok {
		return fmt.Errorf("unknown provider %q; link a plugin under development with tug plugin link <dir>", name)
	}
	info := e.Provider.Info()

	var target conformance.Target
	switch p := e.Provider.(type) {
	case *plugin.Provider:
		runner := &plugin.Runner{Bin: p.Plugin().Bin, Env: p.Plugin().Manifest.Spec.Env}
		target = conformance.Target{Command: p.Plugin().Bin, Env: runner.Environ()}
	default:
		// Built-ins run through the same process interface as plugins.
		target = conformance.Target{Command: selfPath(), Args: []string{"provider-exec", name}, Env: os.Environ()}
	}

	acct, err := a.conformanceAccount(name, o)
	if err != nil {
		return err
	}
	store, err := a.credentialStore()
	if err != nil {
		a.log.Debug("no credential store; secrets come from the environment only", "err", err)
		store = credentials.NewMemoryStore()
	}
	svc := &importer.Service{Registry: a.registry(), Store: store}
	secrets, err := svc.Secrets(info, acct.ID)
	if err != nil && o.account != "" {
		return err
	}

	report := conformance.Run(ctx, target, conformance.Options{
		Name:            name,
		Account:         acct,
		Secrets:         secrets,
		DeclaredSecrets: info.Secrets,
	})
	if format == render.FormatJSON || format == render.FormatYAML {
		if err := render.Structured(a.stdout, format, report); err != nil {
			return err
		}
	} else {
		a.printReport(report)
	}
	if report.Failed() {
		return &exitError{code: 1}
	}
	return nil
}

// conformanceAccount picks the account the suite lists: an imported one, or
// one built from --set settings.
func (a *app) conformanceAccount(name string, o pluginTestOptions) (provider.Account, error) {
	settings, err := parseSettings(o.set)
	if err != nil {
		return provider.Account{}, err
	}
	if o.account == "" {
		if len(settings) == 0 {
			return provider.Account{}, nil
		}
		return provider.Account{ID: "conformance", Config: settings}, nil
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return provider.Account{}, err
	}
	src, ok := cat.Source(name, o.account)
	if !ok {
		return provider.Account{}, fmt.Errorf("no imported %s account %q; run tug import %s --account %s", name, o.account, name, o.account)
	}
	acct := src.ProviderAccount()
	for k, v := range settings {
		if acct.Config == nil {
			acct.Config = map[string]any{}
		}
		acct.Config[k] = v
	}
	return acct, nil
}

func (a *app) printReport(r conformance.Report) {
	c := a.colors()
	counts := map[conformance.Status]int{}
	rows := make([][]string, 0, len(r.Results))
	for _, x := range r.Results {
		counts[x.Status]++
		status := string(x.Status)
		switch x.Status {
		case conformance.Pass:
			status = c.Green("pass")
		case conformance.Fail:
			status = c.Red("FAIL")
		case conformance.Skip:
			status = c.Dim("skip")
		}
		rows = append(rows, []string{status, x.Name, x.Detail})
	}
	_ = render.Table(a.stdout, []string{"RESULT", "CHECK", "DETAIL"}, rows)
	fmt.Fprintf(a.stdout, "\n%d passed, %d skipped, %d failed\n", counts[conformance.Pass], counts[conformance.Skip], counts[conformance.Fail])
}

// newProviderExecCmd serves a provider over the plugin protocol. It lets the
// conformance suite test built-ins exactly as it tests plugins.
func newProviderExecCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "provider-exec <provider> <operation>",
		Short:  "Serve a provider over the plugin protocol (for conformance testing)",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.registry().Get(args[0])
			if err != nil {
				return err
			}
			if code := provider.Run(cmd.Context(), registry.ToSDK(p), args[1:], a.stdin, a.stdout, a.stderr); code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
}
