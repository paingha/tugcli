package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/policy"
	"github.com/paingha/tugcli/internal/render"
)

type checkOptions struct {
	namespace     string
	allNamespaces bool
	packs         []string
	output        string
	list          bool
}

func newCheckCmd(a *app) *cobra.Command {
	var o checkOptions
	cmd := &cobra.Command{
		Use:         "check [target]",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Check workloads and other objects against policy packs",
		Long: `Check the workloads and other objects in a namespace, in every namespace, or
one target, against policy packs: versioned files of checks such as resource
requests and limits, probes, a non-root user, an allowed image registry, a
PodDisruptionBudget, an allowed storage class or a condition that a custom
resource must have. Each failure is reported as tug why reports a finding,
with the check's remediation as its next step.

tug has one pack built in, baseline, whose checks are all advisory. Others
come from policy.packs in config.yaml: files, or https URLs pinned by their
sha256. Without --pack, check runs the packs that policy.default names, or
else all of them; --list shows them and their checks.

Pods, ReplicaSets and Jobs that a workload controls are checked through the
workload. Kinds that are not namespaced are checked with --all-namespaces or
when named. check only reads, and never reads a Secret's data. It exits 1
when a blocking check fails; advisory checks never fail it.`,
		Example: `  tug check                          # this session's namespace
  tug check -A --pack acme           # every namespace, with one pack
  tug check deploy/api -o json
  tug check certificate/web-tls      # any kind a pack names
  tug check --list                   # the packs and their checks
  tug check --cluster-selector env=prod --pack acme`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runCheck(cmd.Context(), args, o)
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "check every namespace, and the kinds that are not namespaced")
	cmd.Flags().StringArrayVar(&o.packs, "pack", nil, "pack to run (repeatable; default: those policy.default names, or all)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().BoolVar(&o.list, "list", false, "list the packs check would run and their checks, without reading the cluster")
	return cmd
}

func (a *app) runCheck(ctx context.Context, args []string, o checkOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	if format == render.FormatName {
		return errors.New("check supports -o table, json or yaml")
	}
	if len(args) > 0 && o.allNamespaces {
		return errors.New("name a target or use --all-namespaces, not both")
	}
	loader := policy.Loader{Dir: a.paths.Config, Cache: filepath.Join(a.paths.Cache, "policy")}
	all, err := loader.Load(ctx, a.cfg.Policy.Packs)
	if err != nil {
		return err
	}
	packs, err := policy.Select(all, o.packs, a.cfg.Policy.Default)
	if err != nil {
		return err
	}
	if o.list {
		return a.printPacks(format, packs)
	}

	env, err := a.opsEnv(ctx, o.namespace)
	if err != nil {
		return err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return err
	}
	scope := policy.Scope{Namespace: env.namespace}
	target := ""
	switch {
	case len(args) == 1:
		obj, err := a.resolveObject(ctx, env, clients, mapper, args[0])
		if err != nil {
			return err
		}
		scope.Object = &obj
		target = ops.ShortKind(obj.Kind) + "/" + obj.Name
		scope.Namespace = obj.Namespace
	case o.allNamespaces:
		scope.Namespace = ""
	}
	inv, err := policy.Gather(ctx, clients, mapper, packs, scope)
	if err != nil {
		return err
	}
	if len(inv.Unlisted) > 0 {
		fmt.Fprintf(a.stderr, "%s could not list %s, so findings about them may be missing\n",
			render.ColorsFor(a.stderr).Yellow("warning:"), joinAnd(inv.Unlisted))
	}
	r := policy.Evaluate(packs, inv)
	r.Target, r.Namespace = target, scope.Namespace
	if format != render.FormatTable {
		err = render.Structured(a.stdout, format, r)
	} else {
		a.printCheck(r, o.allNamespaces)
	}
	if err == nil && !r.Passed {
		return &exitError{code: 1}
	}
	return err
}

// printCheck writes a summary line, then each finding as why writes them,
// named by its pack and check. Across namespaces, findings come under a
// heading for each namespace.
func (a *app) printCheck(r *policy.Report, allNamespaces bool) {
	c := a.colors()
	w := a.stdout
	var subject string
	switch {
	case r.Target != "":
		subject = r.Target
		if r.Namespace != "" {
			subject += " in " + r.Namespace
		}
	case allNamespaces:
		subject = plural(r.Objects, "object", "objects") + " in every namespace"
	default:
		subject = plural(r.Objects, "object", "objects") + " in " + r.Namespace
	}
	names := make([]string, len(r.Packs))
	for i, p := range r.Packs {
		names[i] = p.Name + " " + p.Version
	}
	if r.Target != "" && r.Objects == 0 {
		fmt.Fprintf(w, "No check of %s applies to %s.\n", joinAnd(names), subject)
		return
	}
	verdict := c.Green("nothing wrong found")
	if len(r.Findings) > 0 {
		blocking := r.Blocking()
		var parts []string
		if blocking > 0 {
			parts = append(parts, c.Red(plural(blocking, "blocking failure", "blocking failures")))
		}
		if advisory := len(r.Findings) - blocking; advisory > 0 {
			parts = append(parts, c.Yellow(plural(advisory, "advisory finding", "advisory findings")))
		}
		verdict = joinAnd(parts)
	}
	fmt.Fprintf(w, "Checked %s against %s: %s.\n", subject, joinAnd(names), verdict)

	width := render.Width(w)
	namespace := "\x00" // matches no namespace, so the first finding gets a heading
	for _, f := range r.Findings {
		if allNamespaces && f.Namespace != namespace {
			namespace = f.Namespace
			heading := "In " + namespace + ":"
			if namespace == "" {
				heading = "Not namespaced:"
			}
			fmt.Fprintf(w, "\n%s\n", c.Bold(heading))
		}
		shown := f.Finding
		shown.Reason = f.Pack + "/" + f.Reason
		if f.Blocking {
			shown.Reason += " [blocking]"
		}
		fmt.Fprintln(w)
		printFinding(w, c, width, shown)
	}
}

// packView is a pack as check --list shows it.
type packView struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Description string      `json:"description,omitempty"`
	Source      string      `json:"source"`
	Checks      []checkView `json:"checks"`
}

// checkView is a check as the pack has it, with the kinds it applies to,
// which may be its type's defaults.
type checkView struct {
	policy.Check
	Kinds []string `json:"kinds"`
}

// printPacks lists packs and their checks.
func (a *app) printPacks(format render.Format, packs []*policy.Pack) error {
	if format != render.FormatTable {
		views := make([]packView, len(packs))
		for i, p := range packs {
			views[i] = packView{Name: p.Name, Version: p.Version, Description: p.Description, Source: p.Source}
			for _, ch := range p.Checks {
				views[i].Checks = append(views[i].Checks, checkView{Check: ch, Kinds: ch.Kinds()})
			}
		}
		return render.Structured(a.stdout, format, views)
	}
	var rows [][]string
	for _, p := range packs {
		for _, ch := range p.Checks {
			rows = append(rows, []string{p.Name + " " + p.Version, ch.ID, string(ch.Severity), string(ch.Enforcement), ch.Description})
		}
	}
	return render.Table(a.stdout, []string{"PACK", "CHECK", "SEVERITY", "ENFORCEMENT", "DESCRIPTION"}, rows)
}
