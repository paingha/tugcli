package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/team"
)

// TeamKeyEnv supplies a team catalog signing key to tug team sign, for CI,
// instead of the credential store.
const TeamKeyEnv = "TUG_TEAM_KEY"

// errNoTeam is returned by team commands when no team catalog is set up.
var errNoTeam = errors.New("no team catalog is set up; add one with `tug team add <name> <url> --public-key <key>`, or under teams in config.yaml")

func (a *app) teamStore() *team.Store { return team.NewStore(a.paths.Data, a.now) }

func (a *app) teamFetcher() *team.Fetcher {
	return &team.Fetcher{WorkDir: filepath.Join(a.paths.Cache, "team-git")}
}

// teamSources returns the team catalogs tug follows: those in config.yaml,
// then those added with tug team add. A team that is set up wrongly is
// left out and reported in the error, which commands that only need the
// teams' policies treat as a warning.
func (a *app) teamSources() ([]team.Source, error) {
	added, err := a.teamStore().Added()
	if err != nil {
		return nil, err
	}
	var out []team.Source
	var errs []error
	seen := map[string]bool{}
	add := func(tc config.TeamCatalog, inConfig bool) {
		src, err := team.NewSource(tc, inConfig)
		switch {
		case err != nil && inConfig:
			errs = append(errs, fmt.Errorf("teams in config.yaml: %w", err))
		case err != nil:
			errs = append(errs, fmt.Errorf("teams added with tug team add: %w", err))
		case seen[src.Name]:
			errs = append(errs, fmt.Errorf("team %s is in config.yaml and was added with tug team add too; `tug team remove %s` drops the second", src.Name, src.Name))
		default:
			seen[src.Name] = true
			out = append(out, src)
		}
	}
	for _, tc := range a.cfg.Teams {
		add(tc, true)
	}
	for _, tc := range added {
		add(tc, false)
	}
	return out, errors.Join(errs...)
}

// acceptedTeam is a team tug follows and the last good version of its
// catalog.
type acceptedTeam struct {
	src team.Source
	acc *team.Accepted
}

// acceptedTeams returns the last good catalog of every team tug follows
// that has one. Teams that are set up wrongly, or whose saved catalog no
// longer checks out, are left out with a warning.
func (a *app) acceptedTeams() []acceptedTeam {
	srcs, err := a.teamSources()
	if err != nil {
		a.log.Warn("leaving out team catalogs", "err", err)
	}
	store := a.teamStore()
	var out []acceptedTeam
	for _, src := range srcs {
		acc, err := store.Accepted(src)
		if err != nil {
			a.log.Warn("leaving out a team catalog until `tug team sync` fetches it again", "team", src.Name, "err", err)
			continue
		}
		if acc != nil {
			out = append(out, acceptedTeam{src: src, acc: acc})
		}
	}
	return out
}

// limits returns whether a session on cl opens read-only and how long an
// unlock may last, from guard.readOnlyLabels and the teams' policies.
func (a *app) limits(cl *catalog.Cluster) guard.Limits {
	var rules []team.Rule
	for _, t := range a.acceptedTeams() {
		rules = append(rules, team.Policies(t.src.Name, t.acc.Document)...)
	}
	return team.Limits(cl, a.cfg.Guard.ReadOnlyLabels, rules)
}

// suggestedUnlock is how long a hint to run tug unlock on session st
// suggests, as tug connect works it out, so that a policy that limits
// unlocks on its cluster does not refuse the unlock the hint suggests.
func (a *app) suggestedUnlock(st session.State) time.Duration {
	cat, err := a.loadCatalog()
	if err != nil {
		return guard.DefaultUnlock
	}
	cl, err := cat.Get(st.ClusterID)
	if err != nil {
		return guard.DefaultUnlock
	}
	return a.limits(cl).SuggestedUnlock()
}

// teamIndexes returns the plugin indexes the teams approve.
func (a *app) teamIndexes() []config.PluginIndex {
	var out []config.PluginIndex
	for _, t := range a.acceptedTeams() {
		out = append(out, team.Indexes(t.src.Name, t.acc.Document)...)
	}
	return out
}

// applyTeams applies the last good catalog of every team to cat, so that
// the clusters a sync or an import just listed get the names and labels
// their team gives them. Each version was reviewed when it was accepted,
// so this asks nothing; it says what it changed.
func (a *app) applyTeams(cat *catalog.Catalog) {
	c := a.colors()
	for _, t := range a.acceptedTeams() {
		for _, ch := range team.ApplyClusters(cat, t.src.Name, t.acc.Document).Changes {
			fmt.Fprintln(a.stderr, c.Dim("team "+t.src.Name+": "+ch.String()))
		}
	}
}

func newTeamCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Follow team catalogs: shared cluster names, labels, accounts and safety policies",
		Long: `A team catalog is a signed document in which a team publishes names, aliases
and labels for its clusters, provider account settings, approved plugin
indexes and safety policies. It never carries credentials, and following one
grants no Kubernetes permission: you still list and reach every cluster as
yourself, through your own provider and OIDC sign-ins.

  tug team add platform https://teams.example.com/platform/team.yaml --public-key <key>
  tug team sync          # fetch every team's catalog and review what changed
  tug team ls | show platform | remove platform

Teams can also be listed under teams in config.yaml. Publishers use
tug team keygen, sign and verify.`,
	}
	cmd.AddCommand(newTeamAddCmd(a), newTeamSyncCmd(a), newTeamLsCmd(a), newTeamShowCmd(a), newTeamRemoveCmd(a),
		newTeamKeygenCmd(a), newTeamSignCmd(a), newTeamVerifyCmd(a))
	return cmd
}

type teamAddOptions struct {
	publicKey string
	git       string
	path      string
	ref       string
	yes       bool
}

func newTeamAddCmd(a *app) *cobra.Command {
	var o teamAddOptions
	cmd := &cobra.Command{
		Use:   "add <name> [url]",
		Short: "Follow a team's catalog, after showing what it would change",
		Long: `Follow a team's catalog. tug fetches it, checks its signature against the key
given, shows what it can do and what it would change in your catalog, and
asks before trusting it. The catalog is at an https URL, with its signature
at the same URL plus .sig, or in a git repository, which tug clones with
your own git credentials.`,
		Example: `  tug team add platform https://teams.example.com/platform/team.yaml --public-key <key>
  tug team add platform --git git@github.com:acme/tug-team.git --path platform.yaml --ref main --public-key <key>`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			tc := config.TeamCatalog{Name: args[0], PublicKey: o.publicKey}
			switch {
			case len(args) == 2 && o.git != "":
				return errors.New("give the catalog's url or --git, not both")
			case o.git == "" && (o.path != "" || o.ref != ""):
				return errors.New("--path and --ref go with --git")
			case len(args) == 2:
				tc.URL = args[1]
			case o.git != "":
				tc.Git = &config.TeamGit{Repo: o.git, Path: o.path, Ref: o.ref}
			default:
				return errors.New("say where the catalog is: an https url, or --git <repository> [--path <file>] [--ref <branch>]")
			}
			src, err := team.NewSource(tc, false)
			if err != nil {
				return err
			}
			existing, _ := a.teamSources() // a team set up wrongly is no reason not to add another
			if slices.ContainsFunc(existing, func(s team.Source) bool { return s.Name == src.Name }) {
				return fmt.Errorf("team %s is already set up; `tug team sync %s` fetches its latest catalog", src.Name, src.Name)
			}
			store := a.teamStore()
			u, err := store.Check(cmd.Context(), a.teamFetcher(), src)
			if err != nil {
				return fmt.Errorf("team %s: %w; nothing was added", src.Name, err)
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			plan := team.Review(cat, src.Name, nil, u.Document)
			a.describeTeam(src, u.Document)
			a.printPlan(plan)
			if !o.yes {
				if !a.interactive() {
					return errors.New("adding a team trusts its catalog; review it above, then pass --yes to add it without a terminal")
				}
				if !a.confirm(fmt.Sprintf("Follow team %s's catalog and apply it?", src.Name), false) {
					return errors.New("not added")
				}
			}
			if err := store.Accept(src.Name, u.Data, u.Sig, u.Document); err != nil {
				return err
			}
			if err := store.Add(src.Config(o.publicKey)); err != nil {
				return err
			}
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s team %s, version %d: %s\n", a.colors().Green("Added"), src.Name, u.Document.Version, planCounts(plan))
			a.hintSync(plan)
			return nil
		},
	}
	cmd.Flags().StringVar(&o.publicKey, "public-key", "", "the base64 ed25519 key the catalog must be signed with, from the team")
	cmd.Flags().StringVar(&o.git, "git", "", "git repository the catalog is in, instead of a url")
	cmd.Flags().StringVar(&o.path, "path", "", "the catalog's path in the git repository (default: "+team.DefaultGitPath+")")
	cmd.Flags().StringVar(&o.ref, "ref", "", "branch or tag to read (default: the repository's default branch)")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "follow the team without asking")
	_ = cmd.MarkFlagRequired("public-key")
	return cmd
}

// describeTeam shows on stderr what following a team's catalog means.
func (a *app) describeTeam(src team.Source, doc *team.Document) {
	c := a.colors()
	title := fmt.Sprintf("%s version %d", c.Bold("team "+src.Name), doc.Version)
	if doc.Description != "" {
		title += ": " + doc.Description
	}
	fmt.Fprintln(a.stderr, title)
	fmt.Fprintf(a.stderr, "  from:           %s\n", src.Location())
	fmt.Fprintf(a.stderr, "  signed by:      %s\n", team.Fingerprint(src.PublicKey))
	accounts := make([]string, 0, len(doc.Accounts))
	for _, acct := range doc.Accounts {
		accounts = append(accounts, acct.Key())
	}
	fmt.Fprintf(a.stderr, "  accounts:       %s\n", dash(strings.Join(accounts, ", ")))
	fmt.Fprintf(a.stderr, "  clusters:       %d named or labelled\n", len(doc.Clusters))
	for i, ix := range team.Indexes(src.Name, doc) {
		label := ""
		if i == 0 {
			label = "plugin indexes:"
		}
		fmt.Fprintf(a.stderr, "  %-16s%s %s\n", label, ix.Name, ix.URL)
	}
	for i, p := range doc.Policies {
		label := ""
		if i == 0 {
			label = "policies:"
		}
		fmt.Fprintf(a.stderr, "  %-16s%s (%s)\n", label, p.Name, p.Summary())
	}
	fmt.Fprintln(a.stderr, c.Dim("A team catalog names and labels clusters, adds provider accounts and plugin indexes, and sets"))
	fmt.Fprintln(a.stderr, c.Dim("safety policies, which only ever make tug stricter. It holds no credentials and grants no"))
	fmt.Fprintln(a.stderr, c.Dim("Kubernetes permission: you list and reach every cluster as yourself."))
}

// printPlan shows on stderr what applying a version of a team's catalog
// changes.
func (a *app) printPlan(p team.Plan) {
	c := a.colors()
	if len(p.Changes) == 0 && len(p.Conflicts) == 0 && len(p.Waiting) == 0 {
		fmt.Fprintln(a.stderr, "No changes.")
		return
	}
	if len(p.Changes) > 0 {
		fmt.Fprintln(a.stderr, "Changes:")
		for _, ch := range p.Changes {
			line := ch.String()
			switch ch.Op {
			case team.OpAdd:
				line = c.Green(line)
			case team.OpRemove:
				line = c.Red(line)
			default:
				line = c.Yellow(line)
			}
			fmt.Fprintln(a.stderr, "  "+line)
		}
	}
	if len(p.Conflicts) > 0 {
		fmt.Fprintln(a.stderr, "Conflicts, where your own entries or another team's win:")
		for _, cf := range p.Conflicts {
			fmt.Fprintln(a.stderr, "  "+c.Yellow(cf.String()))
		}
	}
	if len(p.Waiting) > 0 {
		fmt.Fprintf(a.stderr, "Not in your catalog yet, named and labelled once tug sync lists them: %s\n", strings.Join(p.Waiting, ", "))
	}
}

func planCounts(p team.Plan) string {
	s := plural(len(p.Changes), "change", "changes")
	if n := len(p.Conflicts); n > 0 {
		s += ", " + plural(n, "conflict", "conflicts")
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// hintSync says how to list the clusters of accounts a team added.
func (a *app) hintSync(p team.Plan) {
	if slices.ContainsFunc(p.Changes, func(ch team.Change) bool { return ch.Kind == team.KindAccount && ch.Op == team.OpAdd }) {
		fmt.Fprintln(a.stderr, a.colors().Dim("Next: tug sync lists the clusters of the team's accounts, signing in to each provider as you"))
	}
}

func newTeamSyncCmd(a *app) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "sync [team...]",
		Short: "Fetch the teams' catalogs and apply new versions after review",
		Long: `Fetch every team's catalog, or the ones named, and check its signature and
version. A new version is shown as a summary of what it changes before it
applies; your own entries win over the team's, and each conflict is listed.
When a catalog cannot be fetched or does not check out, tug keeps using the
last good version, so names, labels and policies keep working offline.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srcs, err := a.teamSources()
			if err != nil {
				return err
			}
			if len(srcs) == 0 {
				return errNoTeam
			}
			if len(args) > 0 {
				var picked []team.Source
				for _, name := range args {
					i := slices.IndexFunc(srcs, func(s team.Source) bool { return s.Name == name })
					if i < 0 {
						return fmt.Errorf("no team %s; `tug team ls` lists them", name)
					}
					picked = append(picked, srcs[i])
				}
				srcs = picked
			}
			var failed, unreviewed int
			for _, src := range srcs {
				ok, err := a.syncTeam(cmd, src, yes)
				switch {
				case errors.Is(err, errNotInteractive):
					unreviewed++
				case err != nil:
					return err
				case !ok:
					failed++
				}
			}
			switch {
			case unreviewed > 0:
				return fmt.Errorf("%s waiting for review; run tug team sync in a terminal, or pass --yes to apply without asking",
					plural(unreviewed, "new version", "new versions"))
			case failed > 0:
				return fmt.Errorf("%d of %d teams could not be checked; tug keeps using their last good versions", failed, len(srcs))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply new versions without asking")
	return cmd
}

// syncTeam fetches and checks one team's catalog and applies a new version
// once it is approved. It reports false when the catalog could not be
// fetched or checked, and returns errNotInteractive when a new version
// needs a review that nobody can give.
func (a *app) syncTeam(cmd *cobra.Command, src team.Source, yes bool) (bool, error) {
	c := a.colors()
	store := a.teamStore()
	u, err := store.Check(cmd.Context(), a.teamFetcher(), src)
	if err != nil {
		if noteErr := store.NoteCheck(src.Name, err); noteErr != nil {
			return false, noteErr
		}
		kept := "no version of it is saved yet"
		if st, _ := store.State(src.Name); st.Version > 0 {
			kept = fmt.Sprintf("keeping version %d from %s", st.Version, st.AcceptedAt.Local().Format(time.DateTime))
		}
		fmt.Fprintf(a.stderr, "%s team %s: %v; %s\n", c.Red("✗"), src.Name, err, kept)
		return false, nil
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return false, err
	}
	if !u.New {
		// The version was reviewed when it was accepted; what applying it
		// changes now follows from clusters listed since.
		plan := team.ApplyClusters(cat, src.Name, u.Document)
		if len(plan.Changes) > 0 {
			if err := cat.Save(a.catalogPath()); err != nil {
				return false, err
			}
		}
		if err := store.NoteCheck(src.Name, nil); err != nil {
			return false, err
		}
		msg := fmt.Sprintf("version %d, up to date", u.Document.Version)
		if len(plan.Changes) > 0 {
			msg += ", " + plural(len(plan.Changes), "change", "changes") + " for clusters listed since"
		}
		fmt.Fprintf(a.stdout, "%s team %s: %s\n", c.Green("✓"), src.Name, msg)
		return true, nil
	}
	var prev *team.Document
	if u.Previous != nil {
		prev = u.Previous.Document
	}
	plan := team.Review(cat, src.Name, prev, u.Document)
	from := "first version"
	if prev != nil {
		from = fmt.Sprintf("version %d", prev.Version)
	}
	fmt.Fprintf(a.stderr, "%s: %s → version %d\n", c.Bold("team "+src.Name), from, u.Document.Version)
	a.printPlan(plan)
	if !yes {
		if !a.interactive() {
			return false, errNotInteractive
		}
		if !a.confirm(fmt.Sprintf("Apply version %d of team %s's catalog?", u.Document.Version, src.Name), false) {
			fmt.Fprintf(a.stderr, "Not applied; team %s stays at %s\n", src.Name, from)
			return true, nil
		}
	}
	if err := store.Accept(src.Name, u.Data, u.Sig, u.Document); err != nil {
		return false, err
	}
	if err := cat.Save(a.catalogPath()); err != nil {
		return false, err
	}
	fmt.Fprintf(a.stdout, "%s team %s: version %d applied, %s\n", c.Green("✓"), src.Name, u.Document.Version, planCounts(plan))
	a.hintSync(plan)
	return true, nil
}

// teamView is what tug team ls prints with -o json|yaml.
type teamView struct {
	Name        string          `json:"name"`
	URL         string          `json:"url,omitempty"`
	Git         *config.TeamGit `json:"git,omitempty"`
	Fingerprint string          `json:"fingerprint"`
	Version     int64           `json:"version,omitempty"`
	AcceptedAt  *time.Time      `json:"acceptedAt,omitempty"`
	CheckedAt   *time.Time      `json:"checkedAt,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	// SetIn is config.yaml, or tug team add.
	SetIn string `json:"setIn"`
}

func (a *app) teamView(src team.Source) (teamView, error) {
	st, err := a.teamStore().State(src.Name)
	if err != nil {
		return teamView{}, err
	}
	v := teamView{Name: src.Name, URL: src.URL, Git: src.Git, Fingerprint: team.Fingerprint(src.PublicKey),
		Version: st.Version, LastError: st.LastError, SetIn: "tug team add"}
	if src.InConfig {
		v.SetIn = "config.yaml"
	}
	if !st.AcceptedAt.IsZero() {
		v.AcceptedAt = &st.AcceptedAt
	}
	if !st.CheckedAt.IsZero() {
		v.CheckedAt = &st.CheckedAt
	}
	return v, nil
}

func newTeamLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the team catalogs tug follows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			srcs, srcErr := a.teamSources()
			views := make([]teamView, 0, len(srcs))
			for _, src := range srcs {
				v, err := a.teamView(src)
				if err != nil {
					return err
				}
				views = append(views, v)
			}
			if srcErr != nil {
				fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), srcErr)
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
				_, err := fmt.Fprintln(a.stdout, "No team catalogs. `tug team add <name> <url> --public-key <key>` follows one.")
				return err
			}
			c := a.colors()
			rows := make([][]string, 0, len(views))
			for i, v := range views {
				version, checked := "-", "-"
				if v.Version > 0 {
					version = fmt.Sprint(v.Version)
				}
				if v.CheckedAt != nil {
					checked = v.CheckedAt.Local().Format(time.DateTime)
				}
				if v.LastError != "" {
					checked = c.Yellow("failed: " + v.LastError)
				}
				rows = append(rows, []string{v.Name, srcs[i].Location(), version, checked, v.SetIn})
			}
			return render.Table(a.stdout, []string{"NAME", "FROM", "VERSION", "CHECKED", "SET IN"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

// teamDetail is what tug team show prints with -o json|yaml.
type teamDetail struct {
	teamView
	Catalog   *team.Document  `json:"catalog,omitempty"`
	Clusters  []teamCluster   `json:"clusters,omitempty"`
	Conflicts []team.Conflict `json:"conflicts,omitempty"`
}

// teamCluster is a cluster in the local catalog that a team describes.
type teamCluster struct {
	Name    string            `json:"name"`
	ID      string            `json:"id"`
	Aliases []string          `json:"aliases,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Version int64             `json:"version"`
}

func newTeamShowCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "show <team>",
		Short: "Show a team's catalog as last accepted, and what it does here",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			srcs, _ := a.teamSources() // only this team matters here
			i := slices.IndexFunc(srcs, func(s team.Source) bool { return s.Name == args[0] })
			if i < 0 {
				return fmt.Errorf("no team %s; `tug team ls` lists them", args[0])
			}
			src := srcs[i]
			v, err := a.teamView(src)
			if err != nil {
				return err
			}
			d := teamDetail{teamView: v}
			acc, err := a.teamStore().Accepted(src)
			if err != nil {
				return fmt.Errorf("%w; `tug team sync %s` fetches it again", err, src.Name)
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			if acc != nil {
				d.Catalog = acc.Document
				// Applying the accepted version again, without saving, says
				// what does not apply here and why.
				d.Conflicts = team.Apply(cat, src.Name, acc.Document).Conflicts
			}
			for _, cl := range cat.Clusters {
				if cl.Team != nil && cl.Team.Team == src.Name {
					d.Clusters = append(d.Clusters, teamCluster{Name: cl.Name, ID: cl.ID, Aliases: cl.Team.Aliases, Labels: cl.Team.Labels, Version: cl.Team.Version})
				}
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, d)
			case render.FormatName:
				_, err := fmt.Fprintln(a.stdout, src.Name)
				return err
			}
			return a.printTeamDetail(src, d)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func (a *app) printTeamDetail(src team.Source, d teamDetail) error {
	c := a.colors()
	w := a.stdout
	fmt.Fprintln(w, c.Bold("team "+src.Name))
	fmt.Fprintf(w, "  from:       %s (set in %s)\n", src.Location(), d.SetIn)
	fmt.Fprintf(w, "  signed by:  %s\n", d.Fingerprint)
	if d.Catalog == nil {
		_, err := fmt.Fprintf(w, "  version:    none accepted yet; `tug team sync %s` fetches it\n", src.Name)
		return err
	}
	accepted := ""
	if d.AcceptedAt != nil {
		accepted = ", accepted " + d.AcceptedAt.Local().Format(time.DateTime)
	}
	fmt.Fprintf(w, "  version:    %d%s\n", d.Catalog.Version, accepted)
	if d.LastError != "" {
		fmt.Fprintf(w, "  last check: %s\n", c.Yellow("failed: "+d.LastError))
	} else if d.CheckedAt != nil {
		fmt.Fprintf(w, "  last check: %s\n", d.CheckedAt.Local().Format(time.DateTime))
	}
	if d.Catalog.Description != "" {
		fmt.Fprintf(w, "  about:      %s\n", d.Catalog.Description)
	}
	if len(d.Catalog.Accounts) > 0 {
		fmt.Fprintln(w, "Accounts:")
		for _, acct := range d.Catalog.Accounts {
			fmt.Fprintf(w, "  %s\n", acct.Key())
		}
	}
	if len(d.Clusters) > 0 {
		fmt.Fprintln(w, "Clusters it names or labels here:")
		rows := make([][]string, 0, len(d.Clusters))
		for _, cl := range d.Clusters {
			labels := make([]string, 0, len(cl.Labels))
			for k, v := range cl.Labels {
				labels = append(labels, k+"="+v)
			}
			slices.Sort(labels)
			rows = append(rows, []string{"  " + cl.Name, dash(strings.Join(cl.Aliases, ",")), dash(strings.Join(labels, ","))})
		}
		if err := render.Table(w, []string{"  NAME", "ALIASES", "LABELS"}, rows); err != nil {
			return err
		}
	}
	if ixs := team.Indexes(src.Name, d.Catalog); len(ixs) > 0 {
		fmt.Fprintln(w, "Plugin indexes:")
		for _, ix := range ixs {
			fmt.Fprintf(w, "  %s  %s\n", ix.Name, ix.URL)
		}
	}
	if len(d.Catalog.Policies) > 0 {
		fmt.Fprintln(w, "Policies:")
		for _, p := range d.Catalog.Policies {
			fmt.Fprintf(w, "  %s: %s\n", p.Name, p.Summary())
		}
	}
	if len(d.Conflicts) > 0 {
		fmt.Fprintln(w, "Conflicts, where your own entries or another team's win:")
		for _, cf := range d.Conflicts {
			fmt.Fprintln(w, "  "+c.Yellow(cf.String()))
		}
	}
	return nil
}

func newTeamRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <team>",
		Short: "Stop following a team's catalog and take back what it added",
		Long: `Stop following a team's catalog. Its policies and plugin indexes stop
applying, and its names, aliases and labels come off your clusters, which
keep the names they have. Accounts the team added stay as your own, since
you listed their clusters with your own credentials; tug forget removes them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !team.ValidName(name) {
				return fmt.Errorf("team name %q is not valid", name)
			}
			srcs, _ := a.teamSources() // removing one team must work while another is set up wrongly
			if i := slices.IndexFunc(srcs, func(s team.Source) bool { return s.Name == name }); i >= 0 && srcs[i].InConfig {
				return fmt.Errorf("team %s is set in config.yaml; delete it there, then run `tug team remove %s` to take back what it added", name, name)
			}
			store := a.teamStore()
			removed, err := store.RemoveAdded(name)
			if err != nil {
				return err
			}
			known, err := store.Known()
			if err != nil {
				return err
			}
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			plan := team.Detach(cat, name)
			if !removed && !slices.Contains(known, name) && len(plan.Changes) == 0 {
				return fmt.Errorf("no team %s; `tug team ls` lists them", name)
			}
			if err := cat.Save(a.catalogPath()); err != nil {
				return err
			}
			if err := store.Forget(name); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Removed team %s: its policies and plugin indexes no longer apply\n", name)
			for _, ch := range plan.Changes {
				fmt.Fprintln(a.stdout, "  "+ch.String())
			}
			return nil
		},
	}
}

func teamKeyName(name string) string { return "team-key/" + name }

func newTeamKeygenCmd(a *app) *cobra.Command {
	var name string
	var force bool
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Make a team catalog signing key, kept in the credential store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.credentialStore()
			if err != nil {
				return err
			}
			if _, err := store.Get(teamKeyName(name)); err == nil && !force {
				return fmt.Errorf("a team key named %s exists; pass --force to replace it, which makes catalogs signed with it unverifiable", name)
			}
			priv, pub, err := team.NewKey()
			if err != nil {
				return err
			}
			if err := store.Set(teamKeyName(name), priv); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "The private key is in %s as %s. For CI, export it once with your store's tools and set %s.\n",
				store.Name(), teamKeyName(name), TeamKeyEnv)
			fmt.Fprintln(a.stderr, "Give your team the public key below, for tug team add --public-key or publicKey in config.yaml.")
			_, err = fmt.Fprintln(a.stdout, pub)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "default", "name of the key")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing key")
	return cmd
}

func newTeamSignCmd(a *app) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "sign <team.yaml>",
		Short: "Check a team catalog and sign it, writing <team.yaml>.sig",
		Long: `Check a team catalog and sign it. The catalog's version must go up with every
change you publish: tug refuses a version lower than the one it has, and a
changed catalog that kept its version.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := os.Getenv(TeamKeyEnv)
			if key == "" {
				store, err := a.credentialStore()
				if err != nil {
					return err
				}
				if key, err = store.Get(teamKeyName(name)); errors.Is(err, credentials.ErrNotFound) {
					return fmt.Errorf("no team key named %s; run tug team keygen, or set %s", name, TeamKeyEnv)
				} else if err != nil {
					return err
				}
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := team.Sign(data, key)
			if err != nil {
				return err
			}
			out := args[0] + team.SignatureSuffix
			if err := os.WriteFile(out, sig, 0o644); err != nil { //nolint:gosec // a signature is published
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Wrote %s\n", out)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "default", "name of the key")
	return cmd
}

func newTeamVerifyCmd(a *app) *cobra.Command {
	var publicKey string
	cmd := &cobra.Command{
		Use:   "verify <team.yaml> --public-key <key>",
		Short: "Check a team catalog against its signature and public key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := team.ParsePublicKey(publicKey)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := os.ReadFile(args[0] + team.SignatureSuffix)
			if err != nil {
				return err
			}
			doc, err := team.Parse(data, sig, key)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "%s is version %d, signed by that key: %s, %s, %s, %s\n", args[0], doc.Version,
				plural(len(doc.Accounts), "account", "accounts"), plural(len(doc.Clusters), "cluster", "clusters"),
				plural(len(doc.PluginIndexes), "plugin index", "plugin indexes"), plural(len(doc.Policies), "policy", "policies"))
			return err
		},
	}
	cmd.Flags().StringVar(&publicKey, "public-key", "", "the base64 public key from tug team keygen")
	_ = cmd.MarkFlagRequired("public-key")
	return cmd
}
