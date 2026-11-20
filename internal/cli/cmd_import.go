package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/builtin/kubeconfig"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/pkg/provider"
)

type importOptions struct {
	account string
	set     []string
	yes     bool
}

func newImportCmd(a *app) *cobra.Command {
	var o importOptions
	cmd := &cobra.Command{
		Use:   "import [provider] [kubeconfig files...]",
		Short: "Find accounts on this machine and import their clusters",
		Long: `Import clusters into tug's catalog.

  tug import                          scan every provider for accounts on this machine
  tug import kubeconfig ./lke.yaml    import a downloaded kubeconfig; its token moves to the keychain
  tug import coreweave --account work --set zone=US-EAST-04A

Secrets that a provider declares are asked for once and kept in the keychain.
In CI, set TUG_SECRET_<NAME> (for example TUG_SECRET_API_TOKEN) instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runImport(cmd.Context(), args, o)
		},
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			if a.cfg == nil && a.setup(false) != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			return a.registry().Names(), cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().StringVar(&o.account, "account", "", "account ID to import")
	cmd.Flags().StringArrayVar(&o.set, "set", nil, "account setting as key=value (repeatable)")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "import everything found without asking")
	return cmd
}

func (a *app) runImport(ctx context.Context, args []string, o importOptions) error {
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	svc, _, err := a.services(cat)
	if err != nil {
		return err
	}
	sources, err := a.importSources(ctx, cat, args, o)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		_, err := fmt.Fprintln(a.stdout, "Nothing new to import. `tug sync` refreshes sources already imported.")
		return err
	}
	var failed int
	for _, src := range sources {
		p, err := a.registry().Get(src.Provider)
		if err != nil {
			return err
		}
		if err := a.askSecrets(svc, p.Info(), src.Account); err != nil {
			return err
		}
		res := svc.Import(ctx, cat, src)
		a.printResult(res)
		if res.Err != nil {
			failed++
		}
	}
	a.applyTeams(cat)
	if err := cat.Save(a.catalogPath()); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %s failed", failed, plural(len(sources), "source", "sources"))
	}
	return nil
}

// importSources works out which provider accounts to import.
func (a *app) importSources(ctx context.Context, cat *catalog.Catalog, args []string, o importOptions) ([]catalog.Source, error) {
	settings, err := parseSettings(o.set)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return a.discoverSources(ctx, cat, o.yes)
	}
	name, files := args[0], args[1:]
	p, err := a.registry().Get(name)
	if err != nil {
		return nil, err
	}
	a.warnUnknownSettings(p, settings)
	switch {
	case len(files) > 0:
		if name != kubeconfig.Name {
			return nil, fmt.Errorf("only the kubeconfig provider takes files; got %v", files)
		}
		return kubeconfigSources(cat, files, o.account)
	case o.account != "":
		return []catalog.Source{{Provider: name, Account: o.account, Config: settings}}, nil
	case p.Info().Capabilities.Has(provider.CapAccounts):
		found, err := a.providerAccounts(ctx, p)
		if err != nil {
			return nil, err
		}
		return a.chooseSources(cat, found, o.yes)
	default:
		return []catalog.Source{{Provider: name, Account: "default", Config: settings}}, nil
	}
}

// discoverSources asks every provider that can list local accounts.
func (a *app) discoverSources(ctx context.Context, cat *catalog.Catalog, yes bool) ([]catalog.Source, error) {
	var found []foundSource
	for _, name := range a.registry().Names() {
		p, _ := a.registry().Get(name)
		if !p.Info().Capabilities.Has(provider.CapAccounts) {
			continue
		}
		srcs, err := a.providerAccounts(ctx, p)
		if err != nil {
			a.log.Warn("listing accounts", "provider", name, "err", err)
			continue
		}
		found = append(found, srcs...)
	}
	return a.chooseSources(cat, found, yes)
}

// foundSource is an account a provider found, with its description for
// the picker; the catalog keeps only the source.
type foundSource struct {
	catalog.Source
	Description string
}

func (a *app) providerAccounts(ctx context.Context, p registry.Provider) ([]foundSource, error) {
	accts, err := p.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	name := p.Info().Name
	out := make([]foundSource, 0, len(accts))
	for _, acct := range accts {
		out = append(out, foundSource{Source: catalog.Source{Provider: name, Account: acct.ID, Config: acct.Config}, Description: acct.Description})
	}
	return out, nil
}

// chooseSources drops sources already in the catalog and asks which of the
// rest to import.
func (a *app) chooseSources(cat *catalog.Catalog, found []foundSource, yes bool) ([]catalog.Source, error) {
	var fresh []catalog.Source
	labels := make([]string, 0, len(found))
	for _, s := range found {
		if _, ok := cat.Source(s.Provider, s.Account); ok {
			continue
		}
		fresh = append(fresh, s.Source)
		label := s.Key()
		switch path, ok := s.Config[kubeconfig.ConfigPath].(string); {
		case ok:
			label += "  " + path
		case s.Description != "" && s.Description != s.Account:
			label += "  " + s.Description
		}
		labels = append(labels, label)
	}
	if len(fresh) == 0 || yes {
		return fresh, nil
	}
	picked, err := a.pickMany("Found these accounts:", labels)
	if errors.Is(err, errNotInteractive) {
		return nil, errors.New("found accounts to import; pass --yes to import them all without a terminal")
	}
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Source, 0, len(picked))
	for _, i := range picked {
		out = append(out, fresh[i])
	}
	return out, nil
}

// kubeconfigSources turns file arguments into kubeconfig sources.
func kubeconfigSources(cat *catalog.Catalog, files []string, account string) ([]catalog.Source, error) {
	if account != "" && len(files) > 1 {
		return nil, errors.New("--account names a single file's account")
	}
	var out []catalog.Source
	for _, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		id := account
		if id == "" {
			id = kubeconfig.AccountID(abs, "")
			// Keep IDs unique when two files share a name.
			for base, i := id, 2; ; i++ {
				existing, ok := cat.Source(kubeconfig.Name, id)
				if !ok || existing.Config[kubeconfig.ConfigPath] == abs {
					break
				}
				id = base + "-" + strconv.Itoa(i)
			}
		}
		out = append(out, catalog.Source{Provider: kubeconfig.Name, Account: id, Config: map[string]any{kubeconfig.ConfigPath: abs}})
	}
	return out, nil
}

// askSecrets prompts for declared secrets that are not stored yet.
func (a *app) askSecrets(svc *importer.Service, info provider.Info, account string) error {
	for _, spec := range svc.MissingSecrets(info, account) {
		prompt := spec.Prompt
		if prompt == "" {
			prompt = spec.Name
		}
		v, err := a.readSecret(fmt.Sprintf("%s (%s/%s)", prompt, info.Name, account))
		if errors.Is(err, errNotInteractive) {
			return fmt.Errorf("%s needs %s; set %s%s or run in a terminal", info.Name, spec.Name, importer.SecretEnvPrefix, strings.ToUpper(spec.Name))
		}
		if err != nil {
			return err
		}
		if v == "" {
			return fmt.Errorf("%s is required", spec.Name)
		}
		if err := svc.SetSecret(info.Name, account, spec.Name, v); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) printResult(r importer.Result) {
	c := a.colors()
	if r.Err != nil {
		fmt.Fprintf(a.stderr, "%s %s: %v\n", c.Red("✗"), r.Source.Key(), r.Err)
		var pe *provider.Error
		if errors.As(r.Err, &pe) && pe.Hint != "" {
			fmt.Fprintf(a.stderr, "  hint: %s\n", pe.Hint)
		}
		return
	}
	parts := []string{plural(r.Clusters, "cluster", "clusters")}
	if n := len(r.Merge.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new", n))
	}
	if n := len(r.Merge.Stale); n > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d stale", n)))
	}
	if r.Credentials > 0 {
		parts = append(parts, fmt.Sprintf("%s moved to %s", plural(r.Credentials, "credential", "credentials"), a.store.Name()))
	}
	fmt.Fprintf(a.stdout, "%s %s: %s\n", c.Green("✓"), r.Source.Key(), strings.Join(parts, ", "))
}

func parseSettings(kvs []string) (map[string]any, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set wants key=value, got %q", kv)
		}
		out[k] = v
	}
	return out, nil
}

func newSyncCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Refresh every imported source; clusters gone upstream are marked stale",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			if len(cat.Sources) == 0 {
				return errors.New("nothing imported yet; run `tug import`")
			}
			svc, _, err := a.services(cat)
			if err != nil {
				return err
			}
			results := svc.Sync(cmd.Context(), cat)
			failed := 0
			for _, r := range results {
				a.printResult(r)
				if r.Err != nil {
					failed++
				}
			}
			a.applyTeams(cat)
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("%d of %s failed", failed, plural(len(results), "source", "sources"))
			}
			return nil
		},
	}
}

// clusterView is what ls prints with -o json|yaml.
type clusterView struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Account  string            `json:"account"`
	Region   string            `json:"region,omitempty"`
	Version  string            `json:"version,omitempty"`
	Server   string            `json:"server"`
	Labels   map[string]string `json:"labels,omitempty"`
	Stale    bool              `json:"stale,omitempty"`
	LastSeen time.Time         `json:"lastSeen"`
	// Aliases and Team come from a team catalog that describes the
	// cluster.
	Aliases []string `json:"aliases,omitempty"`
	Team    string   `json:"team,omitempty"`
}

func newLsCmd(a *app) *cobra.Command {
	var output, selector string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"clusters", "list"},
		Short:   "List clusters in the catalog",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			sel, err := parseSelector(selector)
			if err != nil {
				return err
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			clusters := cat.Select(sel)
			current := ""
			if s, err := a.sessions().Current(); err == nil {
				current = s.State.ClusterID
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				views := make([]clusterView, 0, len(clusters))
				for _, cl := range clusters {
					views = append(views, clusterView{
						Name: cl.Name, ID: cl.ID, Provider: cl.Provider, Account: cl.Account,
						Region: cl.Region, Version: cl.Version, Server: cl.Server,
						Labels: cl.EffectiveLabels(), Stale: cl.Stale, LastSeen: cl.LastSeen,
						Aliases: cl.Aliases(), Team: teamOf(cl),
					})
				}
				return render.Structured(a.stdout, format, views)
			case render.FormatName:
				for _, cl := range clusters {
					fmt.Fprintln(a.stdout, cl.Name)
				}
				return nil
			}
			if len(clusters) == 0 {
				_, err := fmt.Fprintln(a.stdout, "No clusters. Run `tug import` to find some.")
				return err
			}
			c := a.colors()
			rows := make([][]string, 0, len(clusters))
			for _, cl := range clusters {
				name := cl.Name
				if cl.ID == current {
					name = "* " + name
				} else {
					name = "  " + name
				}
				status := ""
				if cl.Stale {
					status = c.Yellow("stale")
				}
				labels := cl.EffectiveLabels()
				env := labels[catalog.LabelEnv]
				if env == "prod" {
					env = c.Red(env)
				}
				rows = append(rows, []string{name, cl.Provider, cl.Account, dash(cl.Region), dash(cl.Version), dash(env), extraLabels(labels), status})
			}
			return render.Table(a.stdout, []string{"  NAME", "PROVIDER", "ACCOUNT", "REGION", "VERSION", "ENV", "LABELS", "STATUS"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "filter by labels, e.g. env=prod,gpu=true")
	return cmd
}

// teamOf names the team catalog that describes cl, or "".
func teamOf(cl *catalog.Cluster) string {
	if cl.Team == nil {
		return ""
	}
	return cl.Team.Team
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// extraLabels shows labels not already in their own column.
func extraLabels(labels map[string]string) string {
	var out []string
	for k, v := range labels {
		switch k {
		case catalog.LabelProvider, catalog.LabelAccount, catalog.LabelRegion, catalog.LabelEnv:
			continue
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

func parseSelector(s string) (map[string]string, error) {
	sel := map[string]string{}
	if s == "" {
		return sel, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("selector wants key=value pairs, got %q", part)
		}
		sel[k] = v
	}
	return sel, nil
}

func newLabelCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "label <cluster> key=value... | key-...",
		Short: "Set or remove your own labels on a cluster",
		Example: `  tug label staging-us env=staging team=payments
  tug label staging-us team-`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.completeClusters,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, err := cat.Get(args[0])
			if err != nil {
				return err
			}
			if cl.UserLabels == nil {
				cl.UserLabels = map[string]string{}
			}
			for _, arg := range args[1:] {
				if k, ok := strings.CutSuffix(arg, "-"); ok && !strings.Contains(arg, "=") {
					delete(cl.UserLabels, k)
					continue
				}
				k, v, ok := strings.Cut(arg, "=")
				if !ok || k == "" || v == "" {
					return fmt.Errorf("want key=value or key-, got %q", arg)
				}
				cl.UserLabels[k] = v
			}
			return cat.Save(a.catalogPath())
		},
	}
}

func newForgetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "forget <provider>/<account>",
		Short: "Remove an imported source, its clusters and their stored credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prov, acct, ok := strings.Cut(args[0], "/")
			if !ok {
				return fmt.Errorf("want <provider>/<account>, got %q", args[0])
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			if _, ok := cat.Source(prov, acct); !ok {
				keys := make([]string, 0, len(cat.Sources))
				for _, s := range cat.Sources {
					keys = append(keys, s.Key())
				}
				slices.Sort(keys)
				return fmt.Errorf("no source %q (sources: %s)", args[0], strings.Join(keys, ", "))
			}
			svc, _, err := a.services(cat)
			if err != nil {
				return err
			}
			removed, forgetErr := svc.Forget(cat, prov, acct)
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Removed %s and %s\n", args[0], plural(len(removed), "cluster", "clusters"))
			return forgetErr
		},
	}
}

// warnUnknownSettings names --set keys that a plugin's accountConfig schema
// does not describe, which are usually typos. The plugin may still accept
// them, so they are passed on.
func (a *app) warnUnknownSettings(p registry.Provider, settings map[string]any) {
	pp, ok := p.(*plugin.Provider)
	if !ok {
		return
	}
	m := pp.Plugin().Manifest
	unknown := m.UnknownSettings(settings)
	if len(unknown) == 0 {
		return
	}
	known := make([]string, 0, len(m.Settings()))
	for _, s := range m.Settings() {
		known = append(known, s.Name)
	}
	fmt.Fprintf(a.stderr, "%s %s does not describe the setting %s; it knows %s\n", a.colors().Yellow("warning:"),
		m.Metadata.Name, strings.Join(unknown, ", "), dash(strings.Join(known, ", ")))
}
