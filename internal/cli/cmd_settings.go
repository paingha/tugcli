package cli

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/intelligence"
	"github.com/paingha/tugcli/internal/render"
)

func newSettingsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Read and change tug's settings without editing config.yaml",
		Long: `Read and change the settings in config.yaml that hold one value: a boolean,
a number, a string or a duration, such as intelligence.enabled or
sessions.maxAge. tug changes only the setting you name, so the rest of the
file, comments included, stays as you wrote it, and it refuses a value it
could not use. Lists, such as plugins.indexes, are edited in the file or by
the commands that own them.

Secrets are never settings: API keys go in the keychain with
tug intelligence key set, and providers' secrets with tug import.`,
		Example: `  tug settings list
  tug settings set intelligence.monthlySpendCapUSD 20
  tug settings get intelligence.model
  tug settings unset intelligence.model    # back to the provider's default
  tug settings path                        # where config.yaml is`,
	}
	cmd.AddCommand(newSettingsListCmd(a), newSettingsGetCmd(a), newSettingsSetCmd(a), newSettingsUnsetCmd(a), newSettingsPathCmd(a))
	return cmd
}

// settingView is one setting as tug settings list shows it.
type settingView struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
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
	defaults := intelligenceDefaults(a.cfg.Intelligence)
	maps.Copy(defaults, otherDefaults)
	var out []settingView
	for _, s := range config.Settings() {
		v := settingView{Key: s.Key, Kind: s.Kind, Value: s.Value(a.cfg)}
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
		out = append(out, v)
	}
	return out, nil
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
					rows = append(rows, []string{v.Key, dash(v.Value), dash(v.Source)})
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
	if slices.ContainsFunc([]string{"key", "token", "secret", "password"}, func(w string) bool { return strings.Contains(lower, w) }) {
		return config.Setting{}, fmt.Errorf("%q is not a setting, and secrets never go in config.yaml; store a model provider's API key with `tug intelligence key set <provider>`", key)
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
		keys = append(keys, s.Key)
	}
	return keys, cobra.ShellCompDirectiveNoFileComp
}

func newSettingsGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "get <setting>",
		Short:             "Print the value tug uses for a setting",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			views, err := a.settingViews()
			if err != nil {
				return err
			}
			for _, v := range views {
				if v.Key == s.Key && v.Value != "" {
					fmt.Fprintln(a.stdout, v.Value)
				}
			}
			return nil
		},
	}
}

func newSettingsSetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "set <setting> <value>",
		Short:             "Change a setting in config.yaml",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			if _, err := config.EditSettings(a.paths.Config, []config.Change{{Setting: s, Value: args[1]}}, a.checkSettings); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Set %s to %s in %s.\n", s.Key, strings.TrimSpace(args[1]), a.configFile())
			a.warnOverridden(s)
			return nil
		},
	}
}

func newSettingsUnsetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "unset <setting>",
		Short:             "Remove a setting from config.yaml, so tug uses its default",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSettings,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := lookupSetting(args[0])
			if err != nil {
				return err
			}
			removed, err := config.EditSettings(a.paths.Config, []config.Change{{Setting: s, Unset: true}}, a.checkSettings)
			if err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Fprintf(a.stdout, "%s was not set in %s.\n", s.Key, a.configFile())
				return nil
			}
			fmt.Fprintf(a.stdout, "Removed %s from %s.\n", s.Key, a.configFile())
			a.warnOverridden(s)
			return nil
		},
	}
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

// warnOverridden says when a variable in the environment overrides the
// setting just changed.
func (a *app) warnOverridden(s config.Setting) {
	if s.Env != "" && os.Getenv(s.Env) != "" {
		fmt.Fprintf(a.stderr, "%s %s is set, and overrides %s\n", a.colors().Yellow("warning:"), s.Env, s.Key)
	}
}

// keyringBackends are the values keyring.backend takes.
var keyringBackends = []string{"auto", "keychain", "file"}

// checkSettings refuses settings tug could read but not use.
func (a *app) checkSettings(c *config.Config) error {
	if b := c.Keyring.Backend; b != "" && !slices.Contains(keyringBackends, strings.ToLower(b)) {
		return fmt.Errorf("keyring.backend %q is not one of %s", b, strings.Join(keyringBackends, ", "))
	}
	if err := intelligence.Validate(c.Intelligence); err != nil {
		return err
	}
	return nil
}
