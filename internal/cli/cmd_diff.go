package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"

	"github.com/paingha/tugcli/internal/diff"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/version"
)

// diffErrorCode is what tug diff and tug drift exit with on an error, as
// kubectl diff does: 1 means that something differs.
const diffErrorCode = 2

// diffFailed makes an error from tug diff or tug drift exit with
// diffErrorCode once printed, leaving an exit code it already has alone.
func diffFailed(err error) error {
	var ee *exitError
	if err == nil || errors.As(err, &ee) {
		return err
	}
	return &codedError{code: diffErrorCode, err: err}
}

// diffCommand makes a command's every failure, from parsing its flags and
// arguments and setting tug up to running it, exit with diffErrorCode.
func diffCommand(cmd *cobra.Command) *cobra.Command {
	args, run := cmd.Args, cmd.RunE
	cmd.Args = func(c *cobra.Command, a []string) error { return diffFailed(args(c, a)) }
	cmd.RunE = func(c *cobra.Command, a []string) error { return diffFailed(run(c, a)) }
	// A command's own PersistentPreRunE takes the place of the root's, so
	// this one runs the root's.
	cmd.PersistentPreRunE = func(c *cobra.Command, a []string) error {
		if pre := c.Root().PersistentPreRunE; pre != nil {
			return diffFailed(pre(c, a))
		}
		return nil
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return diffFailed(err) })
	return cmd
}

type diffOptions struct {
	namespace     string
	files         []string
	against       string
	managedFields bool
	status        bool
	allFields     bool
	fieldManager  string
	output        string
	serverSide    bool
}

func newDiffCmd(a *app) *cobra.Command {
	var o diffOptions
	cmd := &cobra.Command{
		Use:   "diff [target] (-f <file> | --against <cluster> | --managed-fields)",
		Short: "Compare an object with a manifest or another cluster, and show who owns each field",
		Long: `Compare a live object with a manifest, or with the same object on another
catalog cluster, or list which field manager owns each of its fields.

With -f, tug asks the API server what applying the manifests would make of
each object, by a server-side dry run (dryRun=All, as field manager tug), and
compares that with the live object. The dry run runs admission, mutating
webhooks included, and defaulting, so each new value is marked as the
manifest's, admission's or a default. A dry run does not predict what
controllers do once the apply is made, such as an autoscaler changing the
replicas or a rollout replacing pods. It changes nothing, so it works on
read-only sessions. Without a target every object in the files is compared,
as kubectl diff does; with one, only that object.

With --against, the object is compared with the object of the same kind,
namespace and name on another cluster in the catalog. Fields that only the
clusters set themselves, such as allocated IPs, revision annotations and
what Kubernetes' own controllers own, are left out unless --all-fields.

Both leave out what the API server generates (uid, resourceVersion,
generation, creationTimestamp and managedFields) and, unless --status,
status. Each differing field is attributed to the managers that own it, from
managedFields, and a field an apply as tug would take from another manager is
marked as a conflict.

A Secret's data is never read: --against and --managed-fields compare and
show its metadata only, and -f skips Secrets.

tug diff takes the place of kubectl diff and exits as it does, so scripts
keep working: 0 when nothing differs, 1 when something does, and 2 on an
error.`,
		Example: `  tug diff -f deploy.yaml                        # every object in the file
  tug diff api -f k8s/                           # only deploy/api, from a directory of manifests
  kustomize build overlays/prod | tug diff -f -
  tug diff deploy/api --against payments-staging
  tug diff certificate/web-tls --against eu-prod -o json
  tug diff deploy/api --managed-fields           # who owns each field`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.diff(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's, or a manifest's own)")
	f.StringArrayVarP(&o.files, "filename", "f", nil, "manifest file, directory of manifests, or - for stdin, to compare with the live objects (repeatable)")
	f.StringVar(&o.against, "against", "", "catalog cluster to compare the object with")
	f.BoolVar(&o.managedFields, "managed-fields", false, "list the object's field managers and the fields each owns")
	f.BoolVar(&o.status, "status", false, "compare status too")
	f.BoolVar(&o.allFields, "all-fields", false, "with --against, also compare the fields that only the clusters set themselves")
	f.StringVar(&o.fieldManager, "field-manager", diff.DefaultFieldManager, "field manager the dry run applies as, and conflicts are judged for")
	f.StringVarP(&o.output, "output", "o", "", "output format: json or yaml (default: a unified diff)")
	// kubectl diff --server-side callers keep working: tug always diffs
	// server-side.
	f.BoolVar(&o.serverSide, "server-side", true, "diff with a server-side apply (always on)")
	_ = f.MarkHidden("server-side")
	_ = cmd.RegisterFlagCompletionFunc("against", a.completeClusters)
	return diffCommand(cmd)
}

func (a *app) diff(ctx context.Context, args []string, o diffOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	if format == render.FormatName {
		return errors.New("diff supports -o json or yaml")
	}
	modes := 0
	for _, on := range []bool{len(o.files) > 0, o.against != "", o.managedFields} {
		if on {
			modes++
		}
	}
	switch {
	case modes != 1:
		return errors.New("say what to compare with: one of -f <file>, --against <cluster> or --managed-fields")
	case len(args) == 0 && len(o.files) == 0:
		return errors.New("name the object to compare, such as deploy/api")
	case o.allFields && o.against == "":
		return errors.New("--all-fields works only with --against")
	case o.fieldManager == "":
		return errors.New("--field-manager needs a name")
	}
	env, err := a.opsEnv(ctx, o.namespace)
	if err != nil {
		return err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return err
	}
	here := diff.Clients{Dynamic: clients.Dynamic, Metadata: clients.Metadata}
	var target *resolve.Object
	if len(args) == 1 {
		t, err := a.resolveObject(ctx, env, clients, mapper, args[0])
		if err != nil {
			return err
		}
		target = &t
	}
	opts := diff.Options{Status: o.status, FieldManager: o.fieldManager, AllFields: o.allFields}

	switch {
	case o.managedFields:
		return a.printManagedFields(ctx, here, *target, format)
	case o.against != "":
		other, err := a.againstCluster(ctx, env, o.against)
		if err != nil {
			return err
		}
		r, err := diff.Against(ctx, here, other.Clients, *target, opts)
		if err != nil {
			return err
		}
		r.Label(env.session.State.ClusterName, other.Name)
		if format != render.FormatTable {
			err = render.Structured(a.stdout, format, r)
		} else {
			err = a.printDiff(r, "OWNERS ON "+strings.ToUpper(r.From), "OWNERS ON "+strings.ToUpper(r.To))
			a.againstNotes(r)
		}
		if err == nil && !r.Same {
			return &exitError{code: 1}
		}
		return err
	}

	manifests, err := diff.ReadFiles(o.files, a.stdin)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		return fmt.Errorf("no objects in %s", strings.Join(o.files, ", "))
	}
	results := []*diff.Result{}
	found := false
	for _, m := range manifests {
		ref, err := diff.Locate(mapper, m.Object, env.namespace)
		if err != nil {
			return fmt.Errorf("%s: %w", sourceName(m.Source), err)
		}
		if target != nil && (ref.Resource.GroupResource() != target.Resource.GroupResource() || ref.Namespace != target.Namespace || ref.Name != target.Name) {
			continue
		}
		found = true
		if diff.IsSecret(ref.Resource) {
			fmt.Fprintf(a.stderr, "%s skipped %s from %s: a dry run returns a Secret's data, which tug never reads\n",
				render.ColorsFor(a.stderr).Yellow("warning:"), ref, sourceName(m.Source))
			continue
		}
		r, err := diff.File(ctx, here, ref, m.Object, opts)
		if err != nil {
			return err
		}
		from := env.session.State.ClusterName + " (live)"
		if r.Missing == "from" {
			from = env.session.State.ClusterName + " (not there yet)"
		}
		r.Label(from, fmt.Sprintf("%s (server-side dry run as %s)", sourceName(m.Source), o.fieldManager))
		results = append(results, r)
	}
	if target != nil && !found {
		where := ""
		if target.Namespace != "" {
			where = " in namespace " + target.Namespace
		}
		return fmt.Errorf("%s has no %s%s", strings.Join(o.files, ", "), target, where)
	}
	if format != render.FormatTable {
		err = render.Structured(a.stdout, format, results)
	} else {
		for _, r := range results {
			if err = a.printDiff(r, "LIVE OWNERS", "OWNERS AFTER"); err != nil {
				break
			}
		}
		a.fileNotes(results, o.fieldManager)
	}
	if err != nil {
		return err
	}
	if slices.ContainsFunc(results, func(r *diff.Result) bool { return !r.Same }) {
		return &exitError{code: 1}
	}
	return nil
}

// sourceName names where a manifest came from.
func sourceName(s string) string {
	if s == "-" {
		return "stdin"
	}
	return s
}

// againstCluster returns read-only clients for a catalog cluster other than
// the session's, named as tug connect names one. Its guard refuses every
// write without asking.
func (a *app) againstCluster(ctx context.Context, env *opsEnv, name string) (diff.Cluster, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return diff.Cluster{}, err
	}
	cl, err := a.chooseCluster(cat, []string{name})
	if err != nil {
		return diff.Cluster{}, err
	}
	if cl.ID == env.session.State.ClusterID {
		return diff.Cluster{}, fmt.Errorf("--against names %s, which this terminal is connected to; name another cluster", cl.Name)
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return diff.Cluster{}, err
	}
	f := &kube.Factory{
		Cluster:   cl,
		Broker:    broker,
		Guard:     &guard.Policy{Cluster: cl.Name, ClusterID: cl.ID, ReadOnly: true, Now: a.now},
		CacheDir:  a.paths.Cache,
		UserAgent: "tug/" + version.String(),
		Tunnels:   a.tunnels(),
	}
	cfg, err := f.RESTConfig(ctx)
	if err != nil {
		return diff.Cluster{}, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return diff.Cluster{}, err
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return diff.Cluster{}, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return diff.Cluster{}, err
	}
	return diff.Cluster{Name: cl.Name, Clients: diff.Clients{Dynamic: dyn, Metadata: md}, Discovery: dc}, nil
}

// printDiff writes one object's unified diff and a table of the fields that
// differ with their owners on each side. An object with no differences
// gets a line on stderr, so that stdout stays empty, as kubectl diff's
// does.
func (a *app) printDiff(r *diff.Result, fromHead, toHead string) error {
	c := a.colors()
	name := r.Object
	if r.Namespace != "" {
		name += " in " + r.Namespace
	}
	if r.Same {
		fmt.Fprintf(a.stderr, "%s: no differences between %s and %s\n", name, r.From, r.To)
		return nil
	}
	w := a.stdout
	fmt.Fprintln(w, c.Bold(name))
	fmt.Fprintln(w, c.Red("--- "+r.From))
	fmt.Fprintln(w, c.Green("+++ "+r.To))
	for _, h := range r.Hunks {
		fmt.Fprintln(w, c.Cyan(h.Header()))
		for _, l := range h.Lines {
			text := string(l.Op) + l.Text
			switch l.Op {
			case diff.LineRemoved:
				text = c.Red(text)
			case diff.LineAdded:
				text = c.Green(text)
			}
			fmt.Fprintln(w, text)
		}
	}
	if len(r.Changes) == 0 {
		fmt.Fprintln(w)
		return nil
	}
	rows := make([][]string, 0, len(r.Changes))
	for _, ch := range r.Changes {
		field := ch.Path.String()
		if field == "" {
			field = "(the whole object)"
		}
		rows = append(rows, []string{field, ownerList(ch.FromOwners, ch.Op == diff.OpAdd), ownerList(ch.ToOwners, ch.Op == diff.OpRemove), changeNote(ch)})
	}
	fmt.Fprintln(w)
	if err := render.Table(w, []string{"FIELD", fromHead, toHead, "NOTE"}, rows); err != nil {
		return err
	}
	fmt.Fprintln(w)
	return nil
}

// ownerList names a field's owners, with the subresource each wrote
// through. A side the field is not on shows "(not set)"; a field nobody
// owns, "-".
func ownerList(owners []diff.Owner, absent bool) string {
	if absent {
		return "(not set)"
	}
	if len(owners) == 0 {
		return "-"
	}
	names := make([]string, 0, len(owners))
	for _, o := range owners {
		n := o.Manager
		if o.Subresource != "" {
			n += " (" + o.Subresource + ")"
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return strings.Join(names, ", ")
}

// changeNote says what set a field's new value and whom an apply of it
// would conflict with.
func changeNote(ch diff.Change) string {
	var notes []string
	if len(ch.Conflicts) > 0 {
		notes = append(notes, "conflicts with "+strings.Join(ch.Conflicts, ", "))
	}
	switch ch.Origin {
	case diff.OriginAdmission:
		notes = append(notes, "set by admission")
	case diff.OriginDefault:
		notes = append(notes, "defaulted by the API server")
	}
	if len(notes) == 0 {
		return ""
	}
	return strings.Join(notes, "; ")
}

// fileNotes says, on stderr, what a dry run cannot show, and what to do
// about conflicts.
func (a *app) fileNotes(results []*diff.Result, manager string) {
	if len(results) == 0 {
		return
	}
	c := render.ColorsFor(a.stderr)
	if slices.ContainsFunc(results, func(r *diff.Result) bool { return len(r.Conflicts) > 0 }) {
		fmt.Fprintf(a.stderr, "%s an apply as %s would conflict with the field managers named above; "+
			"the dry run was forced to show the result. Leave those fields out, or apply with --force-conflicts to take them over\n",
			c.Yellow("warning:"), manager)
	}
	fmt.Fprintf(a.stderr, "%s a dry run shows what admission and defaulting make of an apply; "+
		"it does not predict what controllers do once it is made, such as an autoscaler changing the replicas\n", c.Dim("note:"))
}

// againstNotes says, on stderr, what a comparison across clusters left out
// and what its conflicts mean.
func (a *app) againstNotes(r *diff.Result) {
	c := render.ColorsFor(a.stderr)
	switch r.Missing {
	case "from":
		fmt.Fprintf(a.stderr, "%s %s is not on %s\n", c.Yellow("warning:"), r.Object, r.From)
	case "to":
		fmt.Fprintf(a.stderr, "%s %s is not on %s\n", c.Yellow("warning:"), r.Object, r.To)
	}
	if slices.ContainsFunc(r.Changes, func(ch diff.Change) bool { return len(ch.Conflicts) > 0 }) {
		fmt.Fprintf(a.stderr, "%s an apply of %s's values here would take the fields marked conflicts from their managers on %s\n",
			c.Dim("note:"), r.To, r.From)
	}
	if r.Omitted > 0 {
		fmt.Fprintf(a.stderr, "%s left out %d %s that only the clusters set themselves; --all-fields shows them\n",
			c.Dim("note:"), r.Omitted, plural(r.Omitted, "field", "fields"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// managedFieldsView is what tug diff --managed-fields prints with -o json
// or yaml.
type managedFieldsView struct {
	Object    string         `json:"object"`
	Namespace string         `json:"namespace,omitempty"`
	Managers  []diff.Manager `json:"managers"`
}

func (a *app) printManagedFields(ctx context.Context, c diff.Clients, o resolve.Object, format render.Format) error {
	obj, err := diff.Get(ctx, c, o)
	if err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("%s is gone", o)
	}
	mgrs, err := diff.Managers(obj)
	if err != nil {
		return fmt.Errorf("reading the managedFields of %s: %w", o, err)
	}
	if format != render.FormatTable {
		return render.Structured(a.stdout, format, managedFieldsView{Object: o.String(), Namespace: o.Namespace, Managers: mgrs})
	}
	col := a.colors()
	w := a.stdout
	name := o.String()
	if o.Namespace != "" {
		name += " in " + o.Namespace
	}
	if len(mgrs) == 0 {
		fmt.Fprintf(w, "%s has no field managers on record\n", col.Bold(name))
		return nil
	}
	fmt.Fprintf(w, "%s has %d field %s\n\n", col.Bold(name), len(mgrs), plural(len(mgrs), "manager", "managers"))
	rows := make([][]string, 0, len(mgrs))
	for _, m := range mgrs {
		updated := "-"
		if !m.Time.IsZero() {
			updated = duration.HumanDuration(a.now().Sub(m.Time))
		}
		rows = append(rows, []string{m.Manager, m.Operation, cmp.Or(m.Subresource, "-"), updated, strconv.Itoa(len(m.Fields))})
	}
	if err := render.Table(w, []string{"MANAGER", "OPERATION", "SUBRESOURCE", "UPDATED", "FIELDS"}, rows); err != nil {
		return err
	}
	for _, m := range mgrs {
		head := m.Manager + " (" + m.Operation
		if m.Subresource != "" {
			head += ", " + m.Subresource
		}
		fmt.Fprintf(w, "\n%s owns:\n", col.Bold(head+")"))
		for _, f := range m.Fields {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	return nil
}

type driftOptions struct {
	namespace     string
	allNamespaces bool
	against       string
	allFields     bool
	output        string
}

func newDriftCmd(a *app) *cobra.Command {
	var o driftOptions
	cmd := &cobra.Command{
		Use:   "drift --against <cluster> [resource...]",
		Short: "Compare this cluster's objects, CRDs and API versions with another cluster's",
		Long: `Compare the session's cluster with another cluster in the catalog: the API
versions each serves, the CustomResourceDefinitions on each and the versions
they serve, and the objects of the resources named, in the session's namespace
(or -n, or every namespace with -A). Name resources as kubectl does; without
any, drift compares deployments, statefulsets, daemonsets, cronjobs, services,
configmaps and ingresses.

Objects are matched by namespace and name and compared as tug diff --against
compares one: what the API server generates, status, and fields that only the
clusters set themselves are left out unless --all-fields. Secrets are compared
only when named, and then by their metadata alone: their data is never read.

drift only reads. It exits 0 when the clusters match, 1 when they differ and 2
on an error, so CI can gate on it; -o json gives the details.`,
		Example: `  tug drift --against payments-staging
  tug drift --against payments-staging deploy cm -n checkout
  tug drift --against eu-prod -A certificates.cert-manager.io
  tug drift --against payments-staging -o json | jq '.objects[] | select(.state == "differs")'`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.drift(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.against, "against", "", "catalog cluster to compare with")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "compare every namespace")
	f.BoolVar(&o.allFields, "all-fields", false, "also compare the fields that only the clusters set themselves")
	f.StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	_ = cmd.RegisterFlagCompletionFunc("against", a.completeClusters)
	return diffCommand(cmd)
}

func (a *app) drift(ctx context.Context, args []string, o driftOptions) error {
	format, err := render.ParseFormat(o.output)
	if err != nil {
		return err
	}
	if format == render.FormatName {
		return errors.New("drift supports -o table, json or yaml")
	}
	switch {
	case o.against == "":
		return errors.New("name the cluster to compare with, such as --against payments-staging")
	case o.allNamespaces && o.namespace != "":
		return errors.New("-n and -A cannot be used together")
	}
	env, err := a.opsEnv(ctx, o.namespace)
	if err != nil {
		return err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return err
	}
	opts := diff.DriftOptions{Namespace: env.namespace, AllFields: o.allFields}
	if o.allNamespaces {
		opts.Namespace = ""
	}
	named, defaults := args, false
	if len(named) == 0 {
		named, defaults = diff.DefaultDriftResources, true
	}
	for _, r := range named {
		m, err := resolve.FindResource(mapper, r)
		if err != nil {
			if defaults {
				continue // a cluster without ingresses, say, has none to compare
			}
			return err
		}
		if !slices.ContainsFunc(opts.Resources, func(x *meta.RESTMapping) bool { return x.Resource == m.Resource }) {
			opts.Resources = append(opts.Resources, m)
		}
	}
	other, err := a.againstCluster(ctx, env, o.against)
	if err != nil {
		return err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(env.cfg)
	if err != nil {
		return err
	}
	here := diff.Cluster{
		Name:      env.session.State.ClusterName,
		Clients:   diff.Clients{Dynamic: clients.Dynamic, Metadata: clients.Metadata},
		Discovery: dc,
	}
	d, err := diff.CompareClusters(ctx, here, other, opts)
	if err != nil {
		return err
	}
	if format != render.FormatTable {
		err = render.Structured(a.stdout, format, d)
	} else {
		err = a.printDrift(d)
	}
	if err == nil && !d.Same {
		return &exitError{code: 1}
	}
	return err
}

func (a *app) printDrift(d *diff.Drift) error {
	c := a.colors()
	w := a.stdout
	scope := "namespace " + d.Namespace
	if d.Namespace == "" {
		scope = "every namespace"
	}
	fmt.Fprintf(w, "%s (%s) against %s (%s), %s\n", c.Bold(d.Here.Name), d.Here.Version, c.Bold(d.There.Name), d.There.Version, scope)
	if v := d.APIVersions.OnlyHere; len(v) > 0 {
		fmt.Fprintf(w, "\nAPI versions only on %s: %s\n", d.Here.Name, strings.Join(v, ", "))
	}
	if v := d.APIVersions.OnlyThere; len(v) > 0 {
		fmt.Fprintf(w, "\nAPI versions only on %s: %s\n", d.There.Name, strings.Join(v, ", "))
	}
	if len(d.CRDs) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(d.CRDs))
		for _, crd := range d.CRDs {
			rows = append(rows, []string{crd.Name, versionList(crd.HereVersions), versionList(crd.ThereVersions)})
		}
		if err := render.Table(w, []string{"CRD", strings.ToUpper(d.Here.Name), strings.ToUpper(d.There.Name)}, rows); err != nil {
			return err
		}
	}
	if len(d.Unserved) > 0 {
		fmt.Fprintf(w, "\nNot served by %s: %s\n", d.There.Name, strings.Join(d.Unserved, ", "))
	}
	if len(d.Objects) > 0 {
		fmt.Fprintln(w)
		header := []string{"KIND", "NAME", "STATE", "FIELDS"}
		if d.Namespace == "" {
			header = []string{"KIND", "NAMESPACE", "NAME", "STATE", "FIELDS"}
		}
		rows := make([][]string, 0, len(d.Objects))
		for _, o := range d.Objects {
			state := o.State
			switch o.State {
			case diff.StateOnlyHere:
				state = "only on " + d.Here.Name
			case diff.StateOnlyThere:
				state = "only on " + d.There.Name
			}
			row := []string{o.Kind, o.Name, state, changedFields(o.Changes)}
			if d.Namespace == "" {
				row = []string{o.Kind, cmp.Or(o.Namespace, "-"), o.Name, state, changedFields(o.Changes)}
			}
			rows = append(rows, row)
		}
		if err := render.Table(w, header, rows); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	resources := strings.Join(d.Resources, ", ")
	if resources == "" {
		resources = "no resources"
	}
	if d.Same {
		fmt.Fprintf(w, "%s %d %s of %s, the CRDs and the API versions match\n",
			c.Green("No drift:"), d.Compared, plural(d.Compared, "object", "objects"), resources)
	} else {
		fmt.Fprintf(w, "%s %d of %d %s of %s differ or are on one cluster only\n",
			c.Red("Drift:"), len(d.Objects), d.Compared, plural(d.Compared, "object", "objects"), resources)
	}
	ec := render.ColorsFor(a.stderr)
	for _, p := range d.Problems {
		fmt.Fprintf(a.stderr, "%s %s\n", ec.Yellow("warning:"), p)
	}
	if d.Omitted > 0 {
		fmt.Fprintf(a.stderr, "%s left out %d %s that only the clusters set themselves; --all-fields shows them\n",
			ec.Dim("note:"), d.Omitted, plural(d.Omitted, "field", "fields"))
	}
	if slices.ContainsFunc(d.Objects, func(o diff.ObjectDrift) bool { return o.State == diff.StateDiffers }) {
		fmt.Fprintf(a.stderr, "%s `tug diff <kind>/<name> --against %s` shows an object's differences in full\n", ec.Dim("hint:"), d.There.Name)
	}
	return nil
}

// versionList writes a CRD's served versions, or "-" when the cluster does
// not have it.
func versionList(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return strings.Join(v, ", ")
}

// changedFields names up to three of an object's differing fields and
// counts the rest.
func changedFields(changes []diff.Change) string {
	if len(changes) == 0 {
		return "-"
	}
	const show = 3
	names := make([]string, 0, show)
	for _, ch := range changes[:min(show, len(changes))] {
		names = append(names, cmp.Or(ch.Path.String(), "(the whole object)"))
	}
	s := strings.Join(names, ", ")
	if len(changes) > show {
		s += fmt.Sprintf(" and %d more", len(changes)-show)
	}
	return s
}
