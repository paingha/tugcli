package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/gitops"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

// gitopsOptions are the flags of the gitops commands.
type gitopsOptions struct {
	namespace     string
	allNamespaces bool
	output        string
}

func newGitOpsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gitops",
		Short: "See what Flux and Argo CD deliver, and where delivery fails",
		Long: `Read what Flux and Argo CD report about what they deliver, tied to the live
objects those manage: Flux's sources, Kustomizations and HelmReleases, and
Argo CD's Applications. tug finds each tool through the cluster's API
discovery and leaves out one the cluster does not serve.

gitops only reads. It never syncs, reconciles, suspends, resumes or rolls
anything back; where one of those is the next step, it says which command to
run.`,
	}
	cmd.AddCommand(newGitOpsStatusCmd(a), newGitOpsExplainCmd(a), newGitOpsDriftCmd(a), newGitOpsDependenciesCmd(a))
	return cmd
}

func newGitOpsStatusCmd(a *app) *cobra.Command {
	var o gitopsOptions
	cmd := &cobra.Command{
		Use:         "status",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "List Flux and Argo CD objects with their readiness, sync and revisions",
		Long: `List Flux's sources (GitRepositories, OCIRepositories, HelmRepositories and
Buckets), Kustomizations and HelmReleases, and Argo CD's Applications: whether
each is ready (for an Application, its sync and health status), whether it is
suspended, the revision its source offers against the one it last applied,
and how long ago its controller last reported.

Without -A, status shows what concerns the namespace: what lives in it,
deploys into it or manages objects in it, wherever that is defined (often
flux-system or argocd), and the sources those apply.`,
		Example: `  tug gitops status
  tug gitops status -A
  tug gitops status -n payments -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			env, _, s, err := a.gitopsSnapshot(ctx, o.namespace)
			if errors.Is(err, gitops.ErrNotInstalled) {
				fmt.Fprintln(a.stderr, "Neither Flux nor Argo CD is installed in this cluster.")
				if format == render.FormatJSON || format == render.FormatYAML {
					return render.Structured(a.stdout, format, &gitops.Report{Tools: []gitops.Tool{}, Objects: []*gitops.Object{}})
				}
				return nil
			}
			if err != nil {
				return err
			}
			ns := env.namespace
			if o.allNamespaces {
				ns = ""
			}
			r := s.Status(ns)
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, r)
			case render.FormatName:
				for _, obj := range r.Objects {
					fmt.Fprintln(a.stdout, obj.FullName())
				}
				return nil
			}
			return a.printGitOpsStatus(r)
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "show what concerns this namespace (default: the session's)")
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "show every namespace")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json, yaml or name")
	cmd.MarkFlagsMutuallyExclusive("namespace", "all-namespaces")
	return cmd
}

// gitopsSnapshot reads what Flux and Argo CD report in the session's
// cluster, warning on stderr about what it could not read.
func (a *app) gitopsSnapshot(ctx context.Context, namespace string) (*opsEnv, gitops.Clients, *gitops.Snapshot, error) {
	env, err := a.opsEnv(ctx, namespace)
	if err != nil {
		return nil, gitops.Clients{}, nil, err
	}
	oc, mapper, err := a.objectClients(env)
	if err != nil {
		return nil, gitops.Clients{}, nil, err
	}
	c := gitops.Clients{Clients: oc, Mapper: mapper}
	s, err := gitops.Load(ctx, c, env.namespace)
	if err != nil {
		return nil, gitops.Clients{}, nil, err
	}
	warn := a.colors().Yellow("warning:")
	if s.Namespace != "" {
		fmt.Fprintf(a.stderr, "%s not allowed to list Flux and Argo CD objects in every namespace, so only those in %s are read\n", warn, s.Namespace)
	}
	if len(s.Unlisted) > 0 {
		fmt.Fprintf(a.stderr, "%s could not list %s, so objects of those kinds are missing\n", warn, strings.Join(s.Unlisted, ", "))
	}
	return env, c, s, nil
}

func (a *app) printGitOpsStatus(r *gitops.Report) error {
	if len(r.Objects) == 0 {
		where := "in this cluster"
		if r.Namespace != "" {
			where = "concern namespace " + r.Namespace + "; see every namespace with -A"
		}
		fmt.Fprintf(a.stderr, "No %s objects %s.\n", gitopsToolNames(r.Tools), where)
		return nil
	}
	rows := make([][]string, 0, len(r.Objects))
	for _, o := range r.Objects {
		state, suspended := dash(o.Reason), "False"
		if o.Suspended {
			suspended = "True"
		}
		if o.Tool == gitops.ArgoCD {
			state, suspended = dash(strings.Join(nonEmptyStrings(o.Sync, o.Health), ", ")), "-"
		}
		rows = append(rows, []string{
			o.String(), o.Namespace, gitopsReady(o.Ready), state, suspended,
			dash(gitopsRevision(o.Revision)), dash(gitopsRevision(o.Applied)), a.gitopsAge(o.LastReconcile), dash(clipMessage(gitopsRevision(o.Message), 72)),
		})
	}
	return render.Table(a.stdout, []string{"NAME", "NAMESPACE", "READY", "STATUS", "SUSPENDED", "REVISION", "APPLIED", "RECONCILED", "MESSAGE"}, rows)
}

func gitopsToolNames(tools []gitops.Tool) string {
	var names []string
	for _, t := range tools {
		switch t {
		case gitops.Flux:
			names = append(names, "Flux")
		case gitops.ArgoCD:
			names = append(names, "Argo CD")
		}
	}
	return strings.Join(names, " or ")
}

// gitopsReady says a readiness as Flux's READY column does.
func gitopsReady(h ops.Health) string {
	switch h {
	case ops.HealthReady:
		return "True"
	case ops.HealthNotReady:
		return "False"
	case ops.HealthUnknown:
		return "Unknown"
	default:
		return "-"
	}
}

// gitopsLongHex is a commit SHA or a digest, which tables shorten.
var gitopsLongHex = regexp.MustCompile(`[0-9a-f]{20,}`)

// gitopsRevision shortens the commit SHAs and digests in a revision, as git
// does: "main@sha1:1a2b3c4".
func gitopsRevision(rev string) string {
	return gitopsLongHex.ReplaceAllStringFunc(rev, func(h string) string { return h[:7] })
}

// gitopsAge says how long ago t was, or "-" for no time.
func (a *app) gitopsAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return duration.HumanDuration(a.now().Sub(t))
}

// clipMessage cuts s to n characters, marking the cut.
func clipMessage(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func nonEmptyStrings(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolveGitOps finds the Kustomization, HelmRelease or Application a
// target argument names, asking when several match equally well. With a
// namespace, only objects in it match.
func (a *app) resolveGitOps(s *gitops.Snapshot, arg, namespace string) (*gitops.Object, error) {
	matches, err := s.Match(arg, namespace)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		where := ""
		if namespace != "" {
			where = " in namespace " + namespace
		}
		return nil, fmt.Errorf("no Kustomization, HelmRelease or Application%s matches %q; list them with `tug gitops status -A`", where, arg)
	case 1:
		return matches[0], nil
	}
	labels := make([]string, len(matches))
	for i, m := range matches {
		labels[i] = m.String() + " in " + m.Namespace
	}
	i, err := a.pick("Matches for "+arg+":", labels)
	if err != nil {
		return nil, err
	}
	return matches[i], nil
}

// gitopsTarget reads what Flux and Argo CD report and finds the object a
// command is about.
func (a *app) gitopsTarget(ctx context.Context, o gitopsOptions, arg string) (gitops.Clients, *gitops.Snapshot, *gitops.Object, error) {
	_, c, s, err := a.gitopsSnapshot(ctx, o.namespace)
	if err != nil {
		return gitops.Clients{}, nil, nil, err
	}
	obj, err := a.resolveGitOps(s, arg, o.namespace)
	if err != nil {
		return gitops.Clients{}, nil, nil, err
	}
	return c, s, obj, nil
}

// oneObjectFormat parses -o for a command whose output is one object, which
// has no name format.
func oneObjectFormat(command, output string) (render.Format, error) {
	format, err := render.ParseFormat(output)
	if err == nil && format == render.FormatName {
		err = fmt.Errorf("%s supports -o table, json or yaml", command)
	}
	return format, err
}

func newGitOpsExplainCmd(a *app) *cobra.Command {
	var o gitopsOptions
	cmd := &cobra.Command{
		Use:         "explain <name>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explain where delivering a Kustomization, HelmRelease or Application fails",
		Long: `Explain where delivering a Flux Kustomization or HelmRelease, or an Argo CD
Application, fails, with the evidence and what to do next: it is suspended;
something it depends on through spec.dependsOn is not ready (followed to
where the chain stops); its source cannot be fetched; its latest change or
revision is not reconciled yet; admission control rejects what it applies;
building, rendering or applying it fails, or its Helm chart does not install
or upgrade; or what it manages is not healthy.

What it manages comes from Flux's inventory, the labels helm-controller puts
on what it installs, or Argo CD's status, and each unhealthy object gets the
diagnosis tug why gives. Name the object as ks/<name>, hr/<name> or
app/<name>, or by name alone; without -n, every namespace is searched.

explain only reads. It exits 1 when the object is not healthy, as why does.`,
		Example: `  tug gitops explain apps
  tug gitops explain hr/podinfo -n shop
  tug gitops explain app/guestbook -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := oneObjectFormat("explain", o.output)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			c, s, obj, err := a.gitopsTarget(ctx, o, args[0])
			if err != nil {
				return err
			}
			x, err := gitops.Explain(ctx, c, s, obj, gitops.ExplainOptions{Now: a.now})
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				err = render.Structured(a.stdout, format, x)
			} else {
				err = a.printGitOpsExplain(x)
			}
			if err == nil && !x.Healthy {
				return &exitError{code: 1}
			}
			return err
		},
	}
	gitopsTargetFlags(cmd, &o)
	return cmd
}

// gitopsTargetFlags are the flags of the commands about one object.
func gitopsTargetFlags(cmd *cobra.Command, o *gitopsOptions) {
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace of the object (default: search every namespace)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
}

// gitopsHeader writes the line that says which object a report is about and
// what state it is in.
func (a *app) gitopsHeader(o *gitops.Object, state string) {
	c := a.colors()
	fmt.Fprintf(a.stdout, "%s in %s %s\n", c.Bold(o.String()), o.Namespace, state)
}

// gitopsRevisions says where an object's source is and which revisions it
// offers and last applied, on one line.
func (a *app) gitopsRevisions(o *gitops.Object) string {
	var parts []string
	switch {
	case o.Source != nil:
		src := o.Source.String()
		if o.Source.Namespace != o.Namespace {
			src += " in " + o.Source.Namespace
		}
		parts = append(parts, "source "+src)
	case o.Repo != "":
		parts = append(parts, "source "+o.Repo)
	}
	if o.Revision != "" {
		parts[len(parts)-1] += " at " + gitopsRevision(o.Revision)
	}
	applied := "last applied "
	if o.Tool == gitops.ArgoCD {
		applied = "last synced "
	}
	if o.Applied != "" {
		parts = append(parts, applied+gitopsRevision(o.Applied))
	}
	if !o.LastReconcile.IsZero() {
		parts = append(parts, "reported "+a.gitopsAge(o.LastReconcile)+" ago")
	}
	return strings.Join(parts, ", ")
}

func (a *app) printGitOpsExplain(x *gitops.Explanation) error {
	c := a.colors()
	w := a.stdout
	o := x.Object
	if x.Healthy {
		a.gitopsHeader(o, "is "+c.Green("healthy")+": "+x.Summary)
	} else {
		a.gitopsHeader(o, "is "+c.Red("not ready")+": "+x.Summary)
	}
	if o.Tool == gitops.ArgoCD {
		fmt.Fprintln(w, dash(strings.Join(nonEmptyStrings(o.Sync, o.Health), ", ")))
	}
	if line := a.gitopsRevisions(o); line != "" {
		fmt.Fprintln(w, line)
	}

	if len(o.Conditions) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(o.Conditions))
		for _, cond := range o.Conditions {
			since := "-"
			if !cond.LastTransition.IsZero() {
				since = a.gitopsAge(cond.LastTransition)
			}
			rows = append(rows, []string{cond.Type, cond.Status, cmp.Or(cond.Reason, "-"), since})
		}
		if err := render.Table(w, []string{"CONDITION", "STATUS", "REASON", "SINCE"}, rows); err != nil {
			return err
		}
	}

	width := render.Width(w)
	for _, f := range x.Findings {
		fmt.Fprintln(w)
		printFinding(w, c, width, f.Finding)
	}
	if x.Healthy && len(x.Findings) == 0 {
		fmt.Fprintf(w, "\n%s\n", c.Green("Nothing wrong found."))
	}

	if len(x.Managed) == 0 {
		return nil
	}
	var unhealthy [][]string
	for _, m := range x.Managed {
		if m.Health == ops.HealthNotReady || m.Health == ops.HealthUnknown {
			unhealthy = append(unhealthy, []string{ops.ShortKind(m.Kind) + "/" + m.Name, dash(m.Namespace), string(m.Health), dash(m.Sync), dash(m.Status)})
		}
	}
	if len(unhealthy) == 0 {
		fmt.Fprintf(w, "\nManages %s, none of them unhealthy.\n", countNoun(len(x.Managed), "object"))
		return nil
	}
	fmt.Fprintf(w, "\nManages %s, %d of them not healthy:\n", countNoun(len(x.Managed), "object"), len(unhealthy))
	return render.Table(w, []string{"OBJECT", "NAMESPACE", "HEALTH", "SYNC", "STATUS"}, unhealthy)
}

// countNoun counts things: "1 object", "7 objects".
func countNoun(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

func newGitOpsDriftCmd(a *app) *cobra.Command {
	var o gitopsOptions
	cmd := &cobra.Command{
		Use:         "drift <name>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "List the objects Flux or Argo CD reports out of sync with their source",
		Long: `List the objects that Flux or Argo CD itself reports out of sync with their
source: for an Argo CD Application, each object it found out of sync, missing
or due to be pruned when it last compared them; for a Flux HelmRelease with
spec.driftDetection on, what helm-controller last reported drifted. tug
computes no diff of its own. kustomize-controller undoes drift each time it
reconciles a Kustomization and reports none between, so drift says so and
how to compare a checkout of the source with the cluster.

drift only reads. It exits 1 when something has drifted, as diff does.`,
		Example: `  tug gitops drift app/guestbook
  tug gitops drift hr/podinfo -n shop -o yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := oneObjectFormat("drift", o.output)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			c, s, obj, err := a.gitopsTarget(ctx, o, args[0])
			if err != nil {
				return err
			}
			r, err := gitops.Drift(ctx, c, s, obj)
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				err = render.Structured(a.stdout, format, r)
			} else {
				err = a.printGitOpsDrift(r)
			}
			if err == nil && len(r.Drifted) > 0 {
				return &exitError{code: 1}
			}
			return err
		},
	}
	gitopsTargetFlags(cmd, &o)
	return cmd
}

func (a *app) printGitOpsDrift(r *gitops.DriftReport) error {
	w := a.stdout
	o := r.Object
	switch {
	case r.Detection == gitops.DetectionOff || r.Detection == gitops.DetectionCorrected:
		a.gitopsHeader(o, "has no drift report")
	case len(r.Drifted) == 0:
		a.gitopsHeader(o, "has no drift reported")
	default:
		a.gitopsHeader(o, "has "+a.colors().Yellow(countNoun(len(r.Drifted), "object")+" out of sync"))
	}
	if r.Detection == gitops.DetectionArgoCD && !o.LastReconcile.IsZero() {
		fmt.Fprintf(w, "Argo CD last compared it with its source %s ago.\n", a.gitopsAge(o.LastReconcile))
	}
	if r.Note != "" {
		writeIndented(w, "", "", strings.ToUpper(r.Note[:1])+r.Note[1:]+".", render.Width(w))
	}
	if len(r.Drifted) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	rows := make([][]string, 0, len(r.Drifted))
	for _, d := range r.Drifted {
		note := ""
		switch {
		case d.RequiresPruning:
			note = "no longer in its source; a sync with pruning deletes it"
		case d.Health == "Missing":
			note = "not in the cluster; a sync creates it"
		case !d.Seen.IsZero():
			note = "reported " + a.gitopsAge(d.Seen) + " ago"
		}
		rows = append(rows, []string{ops.ShortKind(d.Kind) + "/" + d.Name, dash(d.Namespace), d.Status, dash(d.Health), dash(note)})
	}
	return render.Table(w, []string{"OBJECT", "NAMESPACE", "STATUS", "HEALTH", "NOTE"}, rows)
}

func newGitOpsDependenciesCmd(a *app) *cobra.Command {
	var o gitopsOptions
	cmd := &cobra.Command{
		Use:         "dependencies <name>",
		Aliases:     []string{"deps"},
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show what a Kustomization, HelmRelease or Application depends on, and what depends on it",
		Long: `Show the spec.dependsOn graph of a Flux Kustomization or HelmRelease both
ways: what it depends on, and what that depends on in turn, and what depends
on it. Each object is marked ready (✓), not ready (✗) or unknown (?), with a
short status; a dependency that is not in the cluster, and a cycle, which
Flux never resolves, say so.

Argo CD Applications have no dependsOn: for one, the graph shows the
Applications it manages, as an app of apps does, and those that manage it.`,
		Example: `  tug gitops dependencies apps
  tug gitops deps hr/redis -n shop -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := oneObjectFormat("dependencies", o.output)
			if err != nil {
				return err
			}
			_, s, obj, err := a.gitopsTarget(cmd.Context(), o, args[0])
			if err != nil {
				return err
			}
			d := s.Dependencies(obj)
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, d)
			}
			return a.printGitOpsDependencies(obj, d)
		},
	}
	gitopsTargetFlags(cmd, &o)
	return cmd
}

func (a *app) printGitOpsDependencies(o *gitops.Object, d *gitops.Dependencies) error {
	w := a.stdout
	c := a.colors()
	dependsOn, requiredBy := "Depends on:", "Required by:"
	if o.Tool == gitops.ArgoCD {
		dependsOn, requiredBy = "Manages:", "Managed by:"
	}
	for i, part := range []struct {
		title string
		root  gitops.DependencyNode
	}{{dependsOn, d.DependsOn}, {requiredBy, d.RequiredBy}} {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, c.Bold(part.title))
		if len(part.root.Children) == 0 {
			fmt.Fprintln(w, "nothing")
			continue
		}
		if err := render.Tree(w, a.dependencyLines(part.root, o.Namespace)); err != nil {
			return err
		}
	}
	if d.Note != "" {
		fmt.Fprintln(w)
		writeIndented(w, "", "", d.Note+".", render.Width(w))
	}
	return nil
}

// dependencyLines turns a dependency graph into lines with a readiness
// marker, naming the namespace of objects outside ns.
func (a *app) dependencyLines(n gitops.DependencyNode, ns string) render.TreeNode {
	c := a.colors()
	label := strings.ToLower(n.Kind) + "/" + n.Name
	if n.Namespace != "" && n.Namespace != ns {
		label += " in " + n.Namespace
	}
	marker := c.Yellow("?")
	switch n.Ready {
	case ops.HealthReady:
		marker = c.Green("✓")
	case ops.HealthNotReady:
		marker = c.Red("✗")
	case ops.HealthNone:
		marker = c.Dim("·")
	}
	detail := marker + " " + n.Status
	if n.Cycle {
		detail += " " + c.Yellow("(a cycle: it is already above)")
	}
	out := render.TreeNode{Label: label, Detail: detail}
	for _, child := range n.Children {
		out.Children = append(out.Children, a.dependencyLines(child, ns))
	}
	return out
}
