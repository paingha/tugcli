package cli

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/alias"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/render"
)

func newAliasCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Name your own shortcuts for tug and kubectl commands",
		Long: `Name your own shortcuts for tug and kubectl commands. An alias stands for the
words it was set to, followed by whatever comes after it on the command line:
with tug alias set wide 'get -o wide', tug wide pods runs tug get -o wide pods.

Only the first word of a command line is an alias, and an expansion is never
expanded again. An alias cannot be named after one of tug's own commands, and
one that tug later gains a command for is ignored. An alias that may write
says what it runs on stderr before it runs, and goes through the read-only
guard as the command it stands for would.

Aliases are kept in aliases.yaml in tug's config directory.`,
		Example: `  tug alias set wide 'get -o wide'
  tug alias set errs -- logs --since 10m --json
  tug alias ls
  tug alias rm wide`,
	}
	cmd.AddCommand(newAliasSetCmd(a), newAliasLsCmd(a), newAliasRmCmd(a))
	return cmd
}

func newAliasSetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set <name> <expansion>",
		Short: "Set an alias, replacing any of the same name",
		Long: `Set an alias. The expansion is one quoted string, split into words as a shell
would, or several words; put -- before words that start with a dash.`,
		Example: `  tug alias set wide 'get -o wide'
  tug alias set errs -- logs --since 10m --json
  tug alias set rs restart                 # tug rs api restarts deploy/api`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if builtinCommand(cmd.Root())(name) {
				return fmt.Errorf("%s is one of tug's own commands, which an alias cannot hide; choose another name", name)
			}
			words := args[1:]
			if len(words) == 1 {
				var err error
				if words, err = alias.Split(words[0]); err != nil {
					return err
				}
			}
			store := alias.New(a.paths.Config)
			all, err := store.Load()
			if err != nil {
				return err
			}
			if err := store.Set(name, words); err != nil {
				return err
			}
			if _, isAlias := all[words[0]]; isAlias && words[0] != name {
				fmt.Fprintf(a.stderr, "%s %s starts with alias %s, which is not expanded again inside an alias\n",
					a.colors().Yellow("warning:"), name, words[0])
			}
			_, err = fmt.Fprintf(a.stdout, "Alias %s runs tug %s\n", name, alias.Join(words))
			return err
		},
	}
}

// aliasView is an alias as tug alias ls -o json and yaml show it.
type aliasView struct {
	Name      string   `json:"name"`
	Expansion []string `json:"expansion"`
	// Hidden is true when tug has a command of the same name, which wins.
	Hidden bool `json:"hidden,omitempty"`
}

func newAliasLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List aliases",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			all, err := alias.New(a.paths.Config).Load()
			if err != nil {
				return err
			}
			builtin := builtinCommand(cmd.Root())
			views := make([]aliasView, 0, len(all))
			for _, name := range alias.Names(all) {
				views = append(views, aliasView{Name: name, Expansion: all[name], Hidden: builtin(name)})
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
				fmt.Fprintln(a.stderr, "No aliases; set one with `tug alias set <name> <expansion>`")
				return nil
			}
			rows := make([][]string, 0, len(views))
			for _, v := range views {
				line := "tug " + alias.Join(v.Expansion)
				if v.Hidden {
					line += a.colors().Dim("  (hidden by tug's own " + v.Name + " command)")
				}
				rows = append(rows, []string{v.Name, line})
			}
			return render.Table(a.stdout, []string{"ALIAS", "RUNS"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newAliasRmCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>...",
		Aliases: []string{"remove"},
		Short:   "Remove aliases",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			removed, err := alias.New(a.paths.Config).Remove(args...)
			for _, name := range removed {
				fmt.Fprintf(a.stdout, "Removed alias %s\n", name)
			}
			return err
		},
	}
}

// builtinCommand reports whether a word names one of tug's own commands,
// which an alias never hides.
func builtinCommand(root *cobra.Command) func(string) bool {
	return func(word string) bool {
		switch word {
		case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
		for _, c := range root.Commands() {
			if c.Name() == word || c.HasAlias(word) {
				return true
			}
		}
		return false
	}
}

// expandAlias expands the alias that starts a command line, if one does,
// and says what it runs on stderr when that may write, so that an alias
// never hides a change. The expanded command then runs as if typed, through
// the same guard. aliases.yaml that cannot be read only costs the aliases.
func (a *app) expandAlias(root *cobra.Command, args []string) []string {
	builtin := builtinCommand(root)
	if _, ok := alias.Candidate(args, builtin); !ok {
		return args
	}
	paths, err := config.DefaultPaths()
	if err != nil {
		return args
	}
	all, err := alias.New(paths.Config).Load()
	if err != nil {
		fmt.Fprintf(a.stderr, "%s ignoring aliases: %v\n", render.ColorsFor(a.stderr).Yellow("warning:"), err)
		return args
	}
	e, ok := alias.Expand(args, all, builtin)
	if !ok {
		return args
	}
	if !aliasReads(root, e.Args) {
		fmt.Fprintf(a.stderr, "%s tug %s\n", render.ColorsFor(a.stderr).Yellow("alias "+e.Name+":"), alias.Join(e.Args))
	}
	return e.Args
}

// readingCommands are tug's commands that change nothing, on the cluster
// or in the session's guard, beyond those that run across clusters.
var readingCommands = []string{
	"tug status", "tug ls", "tug version", "tug doctor", "tug audit", "tug pf", "tug pick", "tug open",
	"tug bookmark ls", "tug bookmark open", "tug alias ls", "tug tunnel ls",
}

// aliasReads reports whether an expanded command line only reads: a kubectl
// command as passthrough.IsRead judges it, or one of tug's commands that
// runs across clusters or is a readingCommand. Anything else may write.
func aliasReads(root *cobra.Command, args []string) bool {
	if isPassthrough(root, args) {
		return passthrough.IsRead(args)
	}
	cmd, _, err := root.Find(args)
	if err != nil || cmd == root {
		return false
	}
	return cmd.Annotations[annotationAcrossClusters] != "" || slices.Contains(readingCommands, cmd.CommandPath())
}
