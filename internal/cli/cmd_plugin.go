package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/internal/sigstore"
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
		newPluginLinkCmd(a), newPluginUnlinkCmd(a), newPluginDescribeCmd(a), newPluginSearchCmd(a), newPluginIndexCmd(a),
		newPluginNewCmd(a), newPluginTestCmd(a), newPluginPackageCmd(a),
	)
	return cmd
}

// pluginDirs locates plugins for this tug, which refuses plugins that need
// a newer one.
func (a *app) pluginDirs() plugin.Dirs {
	d := plugin.NewDirs(a.paths.Data)
	d.TugVersion = version.String()
	d.PreferWASM = a.cfg.Plugins.WASM.Prefer
	d.Pins = a.cfg.Plugins.Pins
	return d
}

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
				pin, _ := a.cfg.Plugins.PluginPin(name)
				rows = append(rows, []string{name, dash(info.Version), dash(pin), e.Kind.String(), info.Capabilities.String(), dash(e.Location)})
			}
			if err := render.Table(a.stdout, []string{"NAME", "VERSION", "PINNED", "KIND", "CAPABILITIES", "LOCATION"}, rows); err != nil {
				return err
			}
			for _, p := range a.problems {
				fmt.Fprintf(a.stderr, "%s %s: %v\n", a.colors().Yellow("skipped"), p.Location, p.Err)
			}
			a.warnRevoked(cmd.Context(), reg)
			return nil
		},
	}
}

// warnRevoked warns about installed plugins that a configured index has
// revoked.
func (a *app) warnRevoked(ctx context.Context, reg *registry.Registry) {
	var installed []*plugin.Plugin
	for _, name := range reg.Names() {
		e, _ := reg.Entry(name)
		if pp, ok := e.Provider.(*plugin.Provider); ok && !pp.Plugin().Dev {
			installed = append(installed, pp.Plugin())
		}
	}
	if len(installed) == 0 || len(a.cfg.Plugins.Indexes) == 0 {
		return
	}
	c := a.colors()
	loaded, errs, err := a.loadIndexes(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, err := range errs {
		fmt.Fprintf(a.stderr, "%s cannot check for revoked plugins: %v\n", c.Yellow("warning:"), err)
	}
	dirs := a.pluginDirs()
	for _, p := range installed {
		if r, by, revoked := revokedInstall(dirs, loaded, p); revoked {
			fmt.Fprintf(a.stderr, "%s index %s has revoked %s %s: %s; remove it with tug plugin remove %s, or install another version\n",
				c.Red("revoked:"), by, p.Name(), p.Manifest.Metadata.Version, dash(r.Reason), p.Name())
		}
	}
}

// revokedInstall returns the revocation of an installed plugin: of the
// archive it was installed from or, for one installed before tug kept
// receipts, of its name and version.
func revokedInstall(dirs plugin.Dirs, loaded []plugin.Loaded, p *plugin.Plugin) (plugin.Revocation, string, bool) {
	if rc, ok := dirs.Receipt(p); ok {
		return plugin.FindRevoked(loaded, rc.SHA256)
	}
	return plugin.FindRevokedVersion(loaded, p.Name(), p.Manifest.Metadata.Version)
}

type installOptions struct {
	sha256 string
	yes    bool
	force  bool
	// pin pins the plugin to the version installed, replacing any pin it
	// has, rather than refusing a version other than its pin.
	pin bool
	// wantName is the plugin an index promised the archive holds, and
	// wantVersion its version.
	wantName    string
	wantVersion string
	// index is the index the archive is listed in, and sig its Sigstore
	// signature there, if it has one.
	index string
	sig   *plugin.IndexSigstore
	// indexes are the indexes already loaded, which say what is revoked;
	// loaded is set once they have been.
	indexes []plugin.Loaded
	loaded  bool
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

A plain name, or name@version, installs from the signed plugin indexes in
plugins.indexes: the index's signature, the archive's sha256 and every
binary's sha256 are all checked.

A plugin pinned in plugins.pins in config.yaml installs at its pinned
version, and any other version is refused. --pin installs the version given
and pins the plugin to it, replacing its pin. When the index names a Sigstore signature
for the archive, tug downloads the bundle and checks it first: the Fulcio
certificate chain, the Rekor transparency log entry and that the signer is
the one the index names. With plugins.requireSignatures set in config.yaml,
tug refuses index entries without a signature. An archive from a file or URL
has no index to name its signer, so tug checks no signature on it and warns
that it is unsigned.`,
		Example: `  tug plugin install acme
  tug plugin install acme@0.1.0
  tug plugin install acme@0.1.0 --pin   # and keep it at 0.1.0
  tug plugin install ./tug-provider-acme_0.1.0.tar.gz
  tug plugin install https://example.com/tug-provider-acme_0.1.0.tar.gz --sha256 9f2c...`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installPlugin(cmd.Context(), args[0], o)
		},
	}
	cmd.Flags().StringVar(&o.sha256, "sha256", "", "expected sha256 of the archive (required for URLs)")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "install without asking")
	cmd.Flags().BoolVar(&o.force, "force", false, "replace the same version if it is installed")
	cmd.Flags().BoolVar(&o.pin, "pin", false, "pin the plugin to the version installed, in plugins.pins in config.yaml")
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
		if strings.ContainsAny(src, `/\`) || strings.HasSuffix(src, ".gz") || strings.HasSuffix(src, ".zip") {
			return err
		}
		// A bare name, or name@version, comes from a signed index, which
		// pins the archive's sha256.
		ref, err := a.pinnedRef(src, o)
		if err != nil {
			return err
		}
		if len(a.cfg.Plugins.Indexes) == 0 {
			return errNoIndex
		}
		loaded, errs, err := a.loadIndexes(ctx)
		if err != nil {
			return err
		}
		o.indexes, o.loaded = loaded, true
		m, v, err := resolveIndexed(loaded, errs, ref)
		if err != nil {
			return err
		}
		if v.Sigstore == nil && a.cfg.Plugins.RequireSignatures {
			return fmt.Errorf("index %s has no Sigstore signature for %s %s, and plugins.requireSignatures in config.yaml refuses unsigned plugins; not installed",
				m.Index, m.Plugin.Name, v.Version)
		}
		fmt.Fprintf(a.stderr, "%s %s from index %s\n", m.Plugin.Name, v.Version, m.Index)
		o.sha256 = v.SHA256
		o.wantName, o.wantVersion = m.Plugin.Name, v.Version
		o.index, o.sig = m.Index, v.Sigstore
		return a.installPlugin(ctx, v.URL, o)
	}

	// The signature comes first, then the archive's sha256 and every
	// binary's.
	signer, err := a.checkSignature(ctx, archive, o)
	if err != nil {
		return err
	}
	staged, err := dirs.Stage(archive, o.sha256)
	if err != nil {
		return err
	}
	defer staged.Discard()
	if err := a.checkRevoked(ctx, staged.Receipt.SHA256, o); err != nil {
		return err
	}
	staged.Receipt.Source, staged.Receipt.Index = src, o.index
	if abs, err := filepath.Abs(src); err == nil && archive == src {
		staged.Receipt.Source = abs
	}
	if signer != nil {
		staged.Receipt.Signer = signer.Subject
	}
	m := staged.Plugin.Manifest
	if o.wantName != "" && m.Metadata.Name != o.wantName {
		return fmt.Errorf("the index lists %s, but the archive holds %s; not installed", o.wantName, m.Metadata.Name)
	}
	if o.wantVersion != "" && strings.TrimPrefix(m.Metadata.Version, "v") != strings.TrimPrefix(o.wantVersion, "v") {
		return fmt.Errorf("the index lists %s %s, but the archive holds %s; not installed", o.wantName, o.wantVersion, m.Metadata.Version)
	}
	if !o.pin {
		if err := dirs.CheckPin(m); err != nil {
			return fmt.Errorf("%w; not installed. Pass --pin to install it and move the pin", err)
		}
	}
	if e, ok := a.registry().Entry(m.Metadata.Name); ok && e.Kind == registry.KindBuiltin &&
		!slices.Contains(a.cfg.Plugins.AllowOverride, m.Metadata.Name) {
		return fmt.Errorf("%s is a built-in provider; a plugin may replace it only if plugins.allowOverride in config.yaml lists it", m.Metadata.Name)
	}

	if staged.Plugin.WASM {
		// Compile now: a module tug cannot run is refused before it is
		// installed, and the first call finds it compiled.
		runner := a.pluginRunner(staged.Plugin)
		err := runner.Prepare()
		_ = runner.Close() // the compiled module stays in the cache
		if err != nil {
			return fmt.Errorf("not installed: %w", err)
		}
	}
	a.printManifest(staged.Plugin)
	c := a.colors()
	switch {
	case signer != nil:
		fmt.Fprintf(a.stderr, "  signed by:     %s\n", sigstore.Identity{Subject: signer.Subject, Issuer: signer.Issuer})
	case o.index != "":
		fmt.Fprintf(a.stderr, "%s index %s has no Sigstore signature for %s %s, so tug cannot check who built it\n",
			c.Yellow("warning:"), o.index, o.wantName, o.wantVersion)
	default:
		fmt.Fprintf(a.stderr, "%s %s has no Sigstore signature, so tug cannot check who built it; install it only if you trust where it came from\n",
			c.Yellow("warning:"), src)
	}
	a.warnUnknownFields(m, "which it ignores")
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
	fmt.Fprintf(a.stdout, "%s %s %s in %s\n", c.Green("Installed"), p.Name(), m.Metadata.Version, p.Dir)
	if o.pin {
		was, err := config.SetPluginPin(a.paths.Config, p.Name(), m.Metadata.Version)
		if err != nil {
			return fmt.Errorf("installed, but not pinned: %w", err)
		}
		if was != "" && was != m.Metadata.Version {
			fmt.Fprintf(a.stdout, "Pinned %s to %s in config.yaml, which pinned it to %s\n", p.Name(), m.Metadata.Version, was)
		} else {
			fmt.Fprintf(a.stdout, "Pinned %s to %s in config.yaml\n", p.Name(), m.Metadata.Version)
		}
	}
	fmt.Fprintf(a.stdout, "Next: tug import %s --account <id>\n", p.Name())
	return nil
}

// checkRevoked refuses an archive that any configured index has revoked.
// An index that cannot be loaded is a warning, since the archive's own
// checks have passed.
func (a *app) checkRevoked(ctx context.Context, sha256 string, o installOptions) error {
	loaded := o.indexes
	if !o.loaded {
		var errs []error
		var err error
		if loaded, errs, err = a.loadIndexes(ctx); err != nil {
			return err
		}
		for _, err := range errs {
			fmt.Fprintf(a.stderr, "%s cannot check for revoked plugins: %v\n", a.colors().Yellow("warning:"), err)
		}
	}
	if r, by, revoked := plugin.FindRevoked(loaded, sha256); revoked {
		return fmt.Errorf("index %s has revoked this archive, %s; not installed", by, r)
	}
	return nil
}

// pinnedRef returns the name[@version] to look up in the indexes for ref:
// a pinned plugin named without a version means its pinned version, and a
// version other than its pin is refused unless --pin moves the pin.
func (a *app) pinnedRef(ref string, o installOptions) (string, error) {
	name, version, _ := strings.Cut(ref, "@")
	pin, ok := a.cfg.Plugins.PluginPin(name)
	switch {
	case !ok:
		return ref, nil
	case version == "" && !o.pin:
		fmt.Fprintf(a.stderr, "%s is pinned to %s in config.yaml\n", name, pin)
		return name + "@" + pin, nil
	case version != "" && strings.TrimPrefix(version, "v") != strings.TrimPrefix(pin, "v") && !o.pin:
		return "", fmt.Errorf("%s is %w: plugins.pins in config.yaml pins it to %s; not installed. Pass --pin to install %s and move the pin",
			name, plugin.ErrPinned, pin, version)
	default:
		return ref, nil
	}
}

// checkSignature checks the archive against the Sigstore signature its
// index names and returns the signer, or nil when there is no signature to
// check.
func (a *app) checkSignature(ctx context.Context, archive string, o installOptions) (*sigstore.Signer, error) {
	if o.sig == nil {
		return nil, nil
	}
	bundle, err := plugin.FetchBundle(ctx, o.sig.Bundle)
	if err != nil {
		return nil, fmt.Errorf("the Sigstore bundle for %s %s: %w", o.wantName, o.wantVersion, err)
	}
	v, err := a.sigstoreVerifier(ctx)
	if err != nil {
		return nil, err
	}
	s, err := v.Verify(archive, bundle, o.sig.Signer())
	if err != nil {
		return nil, fmt.Errorf("checking the Sigstore signature of %s %s: %w", o.wantName, o.wantVersion, err)
	}
	return s, nil
}

// sigstoreVerifier checks plugin signatures against plugins.sigstoreTrustedRoot
// when it is set, and against the public-good Sigstore deployment otherwise.
func (a *app) sigstoreVerifier(ctx context.Context) (*sigstore.Verifier, error) {
	if path := a.cfg.Plugins.SigstoreTrustedRoot; path != "" {
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.paths.Config, path)
		}
		return sigstore.FromFile(path)
	}
	v, err := sigstore.PublicGood(ctx, filepath.Join(a.paths.Cache, "sigstore"))
	if err != nil {
		return nil, err
	}
	if err := v.Stale(); err != nil {
		a.log.Debug("using the Sigstore trust root built into tug", "err", err)
	}
	return v, nil
}

// printManifest shows what a plugin will be able to see and reach, and how
// tug runs it.
func (a *app) printManifest(p *plugin.Plugin) {
	m := p.Manifest
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
	if p.WASM {
		fmt.Fprintf(a.stderr, "  runs as:       WebAssembly in tug's sandbox: https to the network hosts only, no files\n")
	} else {
		fmt.Fprintf(a.stderr, "  runs as:       %s\n", a.describeSandbox(m))
	}
	for i, s := range m.Settings() {
		label := ""
		if i == 0 {
			label = "settings:"
		}
		fmt.Fprintf(a.stderr, "  %-15s%s\n", label, describeSetting(s))
	}
}

// describeSandbox says how a native plugin runs: what it may reach in the
// operating system's sandbox beyond its own directory, or why it runs
// without one.
func (a *app) describeSandbox(m *plugin.Manifest) string {
	const unconfined = "a process with your user's access"
	if !a.cfg.Plugins.Sandbox.Confines(m.Metadata.Name) {
		return unconfined + ": plugins.sandbox in config.yaml turns the sandbox off for it"
	}
	if st := sandbox.Check(); !st.Any() {
		return unconfined + ": there is no sandbox here; " + strings.TrimPrefix(st.Missing, "everything: ")
	}
	ports := []string{"443"}
	for _, p := range m.SandboxPorts() {
		if p != 443 {
			ports = append(ports, strconv.Itoa(int(p)))
		}
	}
	parts := []string{"TCP ports " + strings.Join(ports, ", ")}
	s := m.Spec.Sandbox
	for _, f := range []struct {
		verb  string
		items []string
	}{{"runs", s.Tools}, {"reads", s.Read}, {"writes", s.Write}, {"connects to", s.Sockets}} {
		if len(f.items) > 0 {
			parts = append(parts, f.verb+" "+strings.Join(f.items, ", "))
		}
	}
	return "a process in the operating system's sandbox: " + strings.Join(parts, "; ")
}

// describeSetting shows an account setting as `tug import --set` takes it.
func describeSetting(s plugin.Setting) string {
	out := s.Name
	if len(s.Enum) > 0 {
		out += " (" + strings.Join(s.Enum, ", ") + ")"
	}
	if s.Description != "" {
		out += ": " + s.Description
	}
	return out
}

// warnUnknownFields names the plugin.yaml fields this tug does not know,
// which are usually typos, and says what became of them.
func (a *app) warnUnknownFields(m *plugin.Manifest, fate string) {
	if len(m.Unknown) == 0 {
		return
	}
	fmt.Fprintf(a.stderr, "%s %s's plugin.yaml has fields this tug does not know, %s: %s\n",
		a.colors().Yellow("warning:"), m.Metadata.Name, fate, strings.Join(m.Unknown, ", "))
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
			a.warnUnknownFields(p.Manifest, "which it ignores")
			build := ""
			if p.WASM {
				build = " as WebAssembly"
				// Compile now, so a module tug cannot run says so here; the
				// link stands, ready for a rebuilt module.
				runner := a.pluginRunner(p)
				if err := runner.Prepare(); err != nil {
					fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), err)
				}
				_ = runner.Close() // the compiled module stays in the cache
			}
			_, err = fmt.Fprintf(a.stdout, "Linked %s %s%s from %s %s\n", p.Name(), p.Manifest.Metadata.Version, build, p.Dir,
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
			dir, err := a.pluginDirs().Unlink(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Unlinked %s (%s)\n", args[0], dir)
			return err
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
			a.printManifest(pp.Plugin())
			fmt.Fprintf(a.stderr, "  description:   %s\n", dash(d.Description))
			bin := "executable:"
			if pp.Plugin().WASM {
				bin = "module:"
			}
			fmt.Fprintf(a.stderr, "  protocols:     %s\n  %-15s%s\n", strings.Join(d.Protocols, ", "), bin, pp.Plugin().Bin)
			a.warnUnknownFields(pp.Plugin().Manifest, "which it ignores")
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
			a.warnUnknownFields(res.Manifest, "which are left out of the archive")
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
		if pl := p.Plugin(); pl.WASM {
			// tug runs the module, so the suite runs tug, which gives the
			// module the plugin's sandbox.
			target = conformance.Target{Command: selfPath(), Args: []string{"provider-exec", name}, Env: os.Environ(), Module: pl.Bin}
			// Compile once into the cache, rather than in each process.
			warm := a.pluginRunner(pl)
			err := warm.Prepare()
			_ = warm.Close() // the compiled module stays in the cache
			if err != nil {
				return err
			}
		} else {
			// The plugin runs as it would for any other command: scrubbed
			// environment and sandbox included.
			c, err := a.pluginRunner(pl).Command()
			if err != nil {
				return err
			}
			defer c.Cleanup()
			target = conformance.Target{Command: c.Path, Args: c.Args, Env: c.Env}
		}
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

// newPluginSandboxExecCmd confines its own process with the sandbox policy
// tug passes in its environment, then runs the plugin in its place, so the
// plugin starts restricted. tug starts plugins through it on Linux.
func newPluginSandboxExecCmd(*app) *cobra.Command {
	return &cobra.Command{
		Use:    sandbox.ExecCommand + " <plugin> [args]",
		Short:  "Run a plugin in tug's sandbox (tug runs this itself)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		// Everything after the command name is the plugin's.
		DisableFlagParsing: true,
		// The plugin's scrubbed environment has no TUG_HOME, so this command
		// must not read config.yaml or anything else of tug's.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			// The plugin decides what SIGINT and SIGTERM do; one that came
			// before this point has already cancelled the context.
			signal.Reset(os.Interrupt, syscall.SIGTERM)
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			return sandbox.Exec(args)
		},
	}
}

// newProviderExecCmd serves a provider over the plugin protocol. It lets the
// conformance suite test built-ins exactly as it tests plugins, and reach a
// WebAssembly plugin, which only tug can run, through a process.
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
			if pp, ok := p.(*plugin.Provider); ok {
				// The plugin itself answers, bytes and exit status as they are.
				code, err := pp.Exec(cmd.Context(), args[1:], a.stdin, a.stdout, a.stderr)
				if err != nil {
					return err
				}
				if code != 0 {
					return &exitError{code: code}
				}
				return nil
			}
			if code := provider.Run(cmd.Context(), registry.ToSDK(p), args[1:], a.stdin, a.stdout, a.stderr); code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
}
