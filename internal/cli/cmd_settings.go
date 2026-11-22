package cli

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/intelligence"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/policy"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/pkg/provider"
)

func newSettingsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Read and change tug's settings without editing config.yaml",
		Long: `Read and change the settings in config.yaml without editing it. A setting
holds one value (intelligence.enabled, sessions.maxAge), is one entry of a map
(plugins.timeouts.list, plugins.pins.<plugin>), or is a list that add and
remove change an item at a time: a list of names (plugins.sandbox.exclude),
of label sets (guard.readOnlyLabels), or of entries with named fields
(plugins.indexes, intelligence.prices, policy.packs). Give a label set or an
entry as key=value arguments; adding an entry with the name, model, path or
url of one already there replaces it.

tug changes only what you name, so the rest of the file, comments included,
stays as you wrote it, and it refuses a value it could not use. A change that
makes tug trust something or protect less (a plugin index, a plugin outside
the sandbox, a cluster no longer read-only) is shown and asked about first;
--yes makes it without asking.

Secrets are never settings: API keys go in the keychain with
tug intelligence key set, and providers' secrets with tug import.`,
		Example: `  tug settings list
  tug settings set intelligence.monthlySpendCapUSD 20
  tug settings get intelligence.model
  tug settings unset intelligence.model           # back to the provider's default
  tug settings add guard.readOnlyLabels team=payments tier=critical
  tug settings add plugins.indexes name=acme url=https://plugins.acme.example/index.yaml publicKey=<key>
  tug settings add intelligence.prices model=llama4 input=0 output=0
  tug settings set plugins.timeouts.list 60s
  tug settings unset plugins.pins.digitalocean    # unpin a plugin
  tug settings path                               # where config.yaml is`,
	}
	cmd.AddCommand(newSettingsListCmd(a), newSettingsGetCmd(a), newSettingsSetCmd(a), newSettingsUnsetCmd(a),
		newSettingsAddCmd(a), newSettingsRemoveCmd(a), newSettingsPathCmd(a))
	return cmd
}

// settingView is one setting as tug settings list shows it.
type settingView struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Value is the value tug uses; for a list or a map, its items, one a
	// line, as in Values.
	Value  string   `json:"value"`
	Values []string `json:"values,omitempty"`
	// Source is where the value comes from: config.yaml, the variable that
	// overrides it, or tug's default; "" when it has none.
	Source string `json:"source"`
}

// settingViews returns every setting with the value tug uses and where it
// comes from.
func (a *app) settingViews() ([]settingView, error) {
	inFile, err := config.SetInFile(a.paths.Config)
	if err != nil {
		return nil, err
	}
	var out []settingView
	for _, s := range config.Settings() {
		out = append(out, a.settingView(s, inFile))
	}
	return out, nil
}

// settingView returns one setting with the value tug uses and where it
// comes from.
func (a *app) settingView(s config.Setting, inFile map[string]bool) settingView {
	defaults := intelligenceDefaults(a.cfg.Intelligence)
	maps.Copy(defaults, otherDefaults)
	values := s.Values(a.cfg)
	v := settingView{Key: s.Key, Kind: s.Kind, Value: strings.Join(values, "\n")}
	if s.IsList() || s.Kind == config.KindMap {
		v.Values = values
	}
	switch {
	case s.Env != "" && os.Getenv(s.Env) != "":
		v.Source = s.Env
	case inFile[s.Key]:
		v.Source = "config.yaml"
	case defaults[s.Key] != "" && (v.Value == "" || v.Value == "false"):
		v.Value, v.Source = defaults[s.Key], "default"
	case v.Value != "" && v.Value != "false":
		v.Source = "default"
	}
	return v
}

// otherDefaults are defaults that the code using a setting applies, rather
// than config.Defaults: the sandbox is on unless a setting turns it off.
var otherDefaults = map[string]string{
	"plugins.sandbox.enabled":     "true",
	"plugins.wasm.memoryLimitMiB": "256",
}

// intelligenceDefaults are the values the intelligence package fills in
// for the settings config.yaml leaves out.
func intelligenceDefaults(c config.IntelligenceConfig) map[string]string {
	s, err := intelligence.SettingsFrom(c, intelligence.Overrides{})
	if err != nil {
		return map[string]string{}
	}
	return map[string]string{
		"intelligence.provider":        s.Provider,
		"intelligence.model":           s.Model,
		"intelligence.baseURL":         s.BaseURL,
		"intelligence.timeout":         s.Timeout.String(),
		"intelligence.maxOutputTokens": fmt.Sprint(s.MaxOutputTokens),
		"intelligence.effort":          s.Effort,
	}
}

func newSettingsListCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the settings, the values tug uses and where they come from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			views, err := a.settingViews()
			if err != nil {
				return err
			}
			switch format {
			case render.FormatName:
				for _, v := range views {
					fmt.Fprintln(a.stdout, v.Key)
				}
				return nil
			case render.FormatTable:
				rows := make([][]string, 0, len(views))
				for _, v := range views {
					value := v.Value
					if v.Values != nil {
						value = strings.Join(v.Values, "; ")
					}
					rows = append(rows, []string{v.Key, dash(value), dash(v.Source)})
				}
				return render.Table(a.stdout, []string{"SETTING", "VALUE", "FROM"}, rows)
			default:
				return render.Structured(a.stdout, format, views)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

// lookupSetting finds a setting, or says what to do instead.
func lookupSetting(key string) (config.Setting, error) {
	if s, ok := config.LookupSetting(key); ok {
		return s, nil
	}
	lower := strings.ToLower(key)
	if slices.ContainsFunc([]string{"apikey", "api-key", "api_key", "token", "secret", "password"}, func(w string) bool { return strings.Contains(lower, w) }) {
		return config.Setting{}, fmt.Errorf("%q is not a setting, and secrets never go in config.yaml; store a model provider's API key with `tug intelligence key set <provider>`", key)
	}
	if strings.HasPrefix(lower, "teams") {
		return config.Setting{}, errors.New("team catalogs are followed with `tug team add` and `tug team remove`")
	}
	return config.Setting{}, fmt.Errorf("%q is not a setting tug settings can change; see `tug settings list`", key)
}

// completeSettings offers setting keys for the first argument.
func completeSettings(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var keys []string
	for _, s := range config.Settings() {
		if s.Kind == config.KindMap {
			keys = append(keys, s.Key+".")
			continue
		}
		keys = append(keys, s.Key)
	}
	return keys, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

func newSettingsGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "get <setting>",
		Short:             "Print the value tug uses for a setting; a list's items, one a line",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			inFile, err := config.SetInFile(a.paths.Config)
			if err != nil {
				return err
			}
			if v := a.settingView(s, inFile); v.Value != "" {
				fmt.Fprintln(a.stdout, v.Value)
			}
			return nil
		},
	}
}

// changeOptions are the flags of the commands that change config.yaml.
type changeOptions struct {
	yes bool
}

func (o *changeOptions) flags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "make a change that trusts something or protects less without asking")
}

func newSettingsSetCmd(a *app) *cobra.Command {
	var o changeOptions
	cmd := &cobra.Command{
		Use:               "set <setting> <value>",
		Short:             "Change a setting, or an entry of a map, in config.yaml",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			if s.IsList() {
				return fmt.Errorf("%s is a list; change it with `tug settings add` and `tug settings remove`", s.Key)
			}
			return a.changeSettings(s, []config.Change{{Setting: s, Op: config.OpSet, Value: args[1]}}, o, func([]string) {
				fmt.Fprintf(a.stdout, "Set %s to %s in %s.\n", s.Key, strings.TrimSpace(args[1]), a.configFile())
			})
		},
	}
	o.flags(cmd)
	return cmd
}

func newSettingsUnsetCmd(a *app) *cobra.Command {
	var o changeOptions
	cmd := &cobra.Command{
		Use:               "unset <setting>",
		Short:             "Remove a setting, a whole list or an entry of a map from config.yaml, so tug uses its default",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			return a.changeSettings(s, []config.Change{{Setting: s, Op: config.OpUnset}}, o, func(effects []string) {
				if effects[0] == config.EffectUnchanged {
					fmt.Fprintf(a.stdout, "%s was not set in %s.\n", s.Key, a.configFile())
					return
				}
				fmt.Fprintf(a.stdout, "Removed %s from %s.\n", s.Key, a.configFile())
			})
		},
	}
	o.flags(cmd)
	return cmd
}

func newSettingsAddCmd(a *app) *cobra.Command {
	var o changeOptions
	cmd := &cobra.Command{
		Use:   "add <setting> <item...>",
		Short: "Add an item to a list setting in config.yaml",
		Long: `Add an item to a list setting: a name to a list of names, or a label set or
an entry given as key=value arguments. An entry with the name, model, path or
url of one already there replaces it.

The first label set added to guard.readOnlyLabels keeps env=prod with it, so
that clusters labelled env=prod, which tug opens read-only by default, stay
read-only.`,
		Example: `  tug settings add plugins.sandbox.exclude acme
  tug settings add guard.readOnlyLabels team=payments tier=critical
  tug settings add plugins.indexes name=acme url=https://plugins.acme.example/index.yaml publicKey=<key>
  tug settings add policy.packs url=https://policy.acme.example/pack.yaml sha256=<sha256>
  tug settings add intelligence.prices model=llama4 input=0 output=0`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			if !s.IsList() {
				return fmt.Errorf("%s is not a list; change it with `tug settings set`", s.Key)
			}
			if s.Kind == config.KindList && len(args) > 2 {
				return fmt.Errorf("%s takes one item at a time", s.Key)
			}
			item := args[1:]
			changes := []config.Change{{Setting: s, Op: config.OpAdd, Item: item}}
			kept, err := a.keepDefaults(s)
			if err != nil {
				return err
			}
			changes = append(kept, changes...)
			return a.changeSettings(s, changes, o, func(effects []string) {
				for i, c := range changes[:len(kept)] {
					if effects[i] == config.EffectAdded {
						fmt.Fprintf(a.stdout, "Kept %s in %s: tug's default, which a list of your own replaces.\n", strings.Join(c.Item, " "), s.Key)
					}
				}
				what := strings.Join(item, " ")
				switch effects[len(effects)-1] {
				case config.EffectAdded:
					fmt.Fprintf(a.stdout, "Added %s to %s in %s.\n", what, s.Key, a.configFile())
				case config.EffectReplaced:
					fmt.Fprintf(a.stdout, "Replaced the entry in %s with %s, in %s.\n", s.Key, what, a.configFile())
				default:
					fmt.Fprintf(a.stdout, "%s already has %s.\n", s.Key, what)
				}
			})
		},
	}
	o.flags(cmd)
	return cmd
}

// keepDefaults returns the changes that write a list's default into
// config.yaml before the first item of the user's own is added, for a list
// whose default a list of the user's replaces: guard.readOnlyLabels.
func (a *app) keepDefaults(s config.Setting) ([]config.Change, error) {
	if s.Key != "guard.readOnlyLabels" {
		return nil, nil
	}
	inFile, err := config.SetInFile(a.paths.Config)
	if err != nil || inFile[s.Key] {
		return nil, err
	}
	var defaults config.Config
	defaults.Defaults()
	var kept []config.Change
	for _, labels := range s.Values(&defaults) {
		kept = append(kept, config.Change{Setting: s, Op: config.OpAdd, Item: strings.Fields(labels)})
	}
	return kept, nil
}

func newSettingsRemoveCmd(a *app) *cobra.Command {
	var o changeOptions
	cmd := &cobra.Command{
		Use:   "remove <setting> <item...>",
		Short: "Remove an item from a list setting in config.yaml",
		Long: `Remove an item from a list setting: a name, a label set given as key=value
arguments, or an entry given by its name, model, path or url, or by key=value
arguments that all match. A list left empty is removed, so tug uses its
default again.`,
		Example: `  tug settings remove plugins.sandbox.exclude acme
  tug settings remove guard.readOnlyLabels team=payments tier=critical
  tug settings remove plugins.indexes acme
  tug settings remove intelligence.prices llama4`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			if !s.IsList() {
				return fmt.Errorf("%s is not a list; remove it with `tug settings unset`", s.Key)
			}
			item := args[1:]
			return a.changeSettings(s, []config.Change{{Setting: s, Op: config.OpRemove, Item: item}}, o, func(effects []string) {
				what := strings.Join(item, " ")
				if effects[0] == config.EffectUnchanged {
					fmt.Fprintf(a.stdout, "%s in %s has no %s.\n", s.Key, a.configFile(), what)
					return
				}
				fmt.Fprintf(a.stdout, "Removed %s from %s in %s.\n", what, s.Key, a.configFile())
			})
		},
	}
	o.flags(cmd)
	return cmd
}

func newSettingsPathCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print where config.yaml is",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(a.stdout, a.configFile())
			return nil
		},
	}
}

func (a *app) configFile() string { return filepath.Join(a.paths.Config, "config.yaml") }

// errNotChanged is returned by a check when the user declines a change.
var errNotChanged = errors.New("nothing was changed")

// changeSettings makes changes to config.yaml once they pass checkSettings
// and, for a change that trusts something or protects less, the user
// agrees. done reports the effects. Afterwards it warns when a variable
// overrides the setting.
func (a *app) changeSettings(s config.Setting, changes []config.Change, o changeOptions, done func(effects []string)) error {
	check := func(c *config.Config) error {
		if err := a.checkSettings(c); err != nil {
			return err
		}
		return a.agreeToLoosen(loosened(a.cfg, c), o.yes)
	}
	effects, err := config.EditSettings(a.paths.Config, changes, check)
	if errors.Is(err, errNotChanged) {
		fmt.Fprintln(a.stderr, "Nothing was changed.")
		return nil
	}
	if err != nil {
		return err
	}
	done(effects)
	if s.Env != "" && os.Getenv(s.Env) != "" {
		fmt.Fprintf(a.stderr, "%s %s is set, and overrides %s\n", a.colors().Yellow("warning:"), s.Env, s.Key)
	}
	return nil
}

// agreeToLoosen shows what a change would loosen and asks before making
// it. Without a terminal it refuses unless yes is set.
func (a *app) agreeToLoosen(effects []string, yes bool) error {
	if len(effects) == 0 {
		return nil
	}
	fmt.Fprintln(a.stderr, "This change:")
	for _, e := range effects {
		fmt.Fprintf(a.stderr, "  - %s\n", e)
	}
	switch {
	case yes:
		return nil
	case !a.interactive():
		return errors.New("this change trusts something or protects less; pass --yes to make it without a terminal")
	case !a.confirm("Make it?", false):
		return errNotChanged
	}
	return nil
}

// loosened says what changing before into after trusts or stops
// protecting: clusters no longer read-only, plugins outside the sandbox or
// allowed to replace a built-in provider, unsigned plugins allowed, plugin
// indexes trusted, and plugin limits raised.
func loosened(before, after *config.Config) []string {
	after = cloneConfig(after)
	after.Defaults()
	var out []string
	for _, labels := range before.Guard.ReadOnlyLabels {
		if !slices.ContainsFunc(after.Guard.ReadOnlyLabels, func(m map[string]string) bool { return maps.Equal(m, labels) }) {
			out = append(out, fmt.Sprintf("stops opening clusters labelled %s read-only", labelText(labels)))
		}
	}
	on := func(c *config.Config) bool { return c.Plugins.Sandbox.Enabled == nil || *c.Plugins.Sandbox.Enabled }
	switch {
	case on(before) && !on(after):
		out = append(out, "runs every plugin without the sandbox")
	case on(after):
		for _, p := range after.Plugins.Sandbox.Exclude {
			if !slices.Contains(before.Plugins.Sandbox.Exclude, p) {
				out = append(out, fmt.Sprintf("runs the %s plugin without the sandbox", p))
			}
		}
	}
	for _, p := range after.Plugins.AllowOverride {
		if !slices.Contains(before.Plugins.AllowOverride, p) {
			out = append(out, fmt.Sprintf("lets a plugin replace the built-in %s provider", p))
		}
	}
	if before.Plugins.RequireSignatures && !after.Plugins.RequireSignatures {
		out = append(out, "lets tug plugin install install plugins that have no Sigstore signature")
	}
	for _, ix := range after.Plugins.Indexes {
		if !slices.Contains(before.Plugins.Indexes, ix) {
			out = append(out, fmt.Sprintf("trusts the plugin index %s at %s, signed by the key %s: tug plugin install installs its plugins by name", ix.Name, ix.URL, shortKey(ix.PublicKey)))
		}
	}
	raised := func(name string, was, is int64) {
		if was > 0 && (is == 0 || is > was) {
			out = append(out, fmt.Sprintf("raises plugins.limits.%s, so plugins may use more", name))
		}
	}
	raised("cpuSeconds", int64(before.Plugins.Limits.CPUSeconds), int64(after.Plugins.Limits.CPUSeconds))
	raised("memoryMiB", int64(before.Plugins.Limits.MemoryMiB), int64(after.Plugins.Limits.MemoryMiB))
	raised("timeout", int64(before.Plugins.Limits.Timeout.Duration), int64(after.Plugins.Limits.Timeout.Duration))
	return out
}

// cloneConfig copies a config deeply enough for Defaults to change it.
func cloneConfig(c *config.Config) *config.Config {
	out := *c
	out.Guard.ReadOnlyLabels = slices.Clone(c.Guard.ReadOnlyLabels)
	return &out
}

func labelText(m map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// shortKey shortens a public key for a prompt.
func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12] + "…"
	}
	return k
}

// keyringBackends are the values keyring.backend takes.
var keyringBackends = []string{"auto", "keychain", "file"}

// timedOps are the plugin operations plugins.timeouts may set.
var timedOps = []string{provider.OpDescribe, provider.OpAccounts, provider.OpList, provider.OpCredential, provider.OpRotate, provider.OpCheckAccess}

// checkSettings refuses settings tug could read but not use.
func (a *app) checkSettings(c *config.Config) error {
	if b := c.Keyring.Backend; b != "" && !slices.Contains(keyringBackends, strings.ToLower(b)) {
		return fmt.Errorf("keyring.backend %q is not one of %s", b, strings.Join(keyringBackends, ", "))
	}
	for _, op := range slices.Sorted(maps.Keys(c.Plugins.Timeouts)) {
		switch {
		case !slices.Contains(timedOps, op):
			return fmt.Errorf("plugins.timeouts.%s is not an operation; use %s", op, strings.Join(timedOps, ", "))
		case c.Plugins.Timeouts[op].Duration <= 0:
			return fmt.Errorf("plugins.timeouts.%s must be a positive duration such as 30s", op)
		}
	}
	names := map[string]bool{}
	for _, ix := range c.Plugins.Indexes {
		switch {
		case ix.Name == "" || ix.URL == "" || ix.PublicKey == "":
			return errors.New("every entry in plugins.indexes needs a name, a url and a publicKey")
		case names[ix.Name]:
			return fmt.Errorf("plugins.indexes has two indexes named %s", ix.Name)
		}
		names[ix.Name] = true
		if _, err := plugin.ParsePublicKey(ix.PublicKey); err != nil {
			return fmt.Errorf("plugins.indexes %s: %w", ix.Name, err)
		}
	}
	for _, src := range c.Policy.Packs {
		if err := policy.CheckSource(src); err != nil {
			return fmt.Errorf("policy.packs: %w", err)
		}
	}
	for _, addr := range c.OIDC.ListenAddresses {
		if err := checkListenAddress(addr); err != nil {
			return err
		}
	}
	return intelligence.Validate(c.Intelligence)
}

// checkListenAddress refuses an address the browser could not come back to
// after a sign-in: one that is not host:port on this machine.
func checkListenAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	n, perr := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if err != nil || perr != nil || n < 1 || n > 65535 || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return fmt.Errorf("oidc.listenAddresses %q must be a loopback address and port, such as 127.0.0.1:8000", cmp.Or(addr, `""`))
	}
	return nil
}
