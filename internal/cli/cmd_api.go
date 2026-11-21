package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/paingha/tugcli/internal/apiinfo"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/version"
)

func newAPICmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use: "api",
		// The commands beneath it that only read one cluster run across
		// clusters; diff, which reads two of its own, does not.
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explore the APIs a cluster serves, their deprecations and its Kubernetes version",
		Long: `Explore what the session's cluster serves: its resource types and API versions
from discovery, the versions that upstream Kubernetes removes in a later
release or that a custom resource definition deprecates, and where its
Kubernetes release is in its upstream support window. diff compares two
catalog clusters.

kubectl's own api-resources and api-versions still run through tug as usual.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newAPIResourcesCmd(a), newAPIVersionsCmd(a), newAPIDeprecationsCmd(a), newAPIServerVersionCmd(a), newAPIDiffCmd(a))
	return cmd
}

type apiResourcesOptions struct {
	group  string
	verbs  []string
	scope  string
	output string
}

func newAPIResourcesCmd(a *app) *cobra.Command {
	var o apiResourcesOptions
	cmd := &cobra.Command{
		Use:         "resources [query]",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Search the resource types the cluster serves",
		Long: `List the resource types the cluster serves, with their group, every version
that serves them (the preferred one first), scope, kind, short names and
categories; -o json or yaml adds their verbs. A query matches names, short
names, kinds, groups and categories, ignoring case: exact matches come first,
then those that start with it, then those that contain it.

Discovery is read afresh, so a CRD installed a moment ago shows up.`,
		Example: `  tug api resources
  tug api resources cert          # anything named, grouped or categorised like cert
  tug api resources --group core --verb list
  tug api resources --scope cluster -o name`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			scope, err := apiinfo.ParseScope(o.scope)
			if err != nil {
				return err
			}
			q := apiinfo.Query{Group: o.group, Verbs: o.verbs, Scope: scope}
			if len(args) == 1 {
				q.Text = args[0]
			}
			apis, err := a.sessionAPIs(cmd.Context())
			if err != nil {
				return err
			}
			found := apis.Find(q)
			if len(found) == 0 {
				return noResourceMatches(q)
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, found)
			case render.FormatName:
				for _, r := range found {
					fmt.Fprintln(a.stdout, r.GroupResource())
				}
				return nil
			}
			rows := make([][]string, len(found))
			for i, r := range found {
				group := r.Group
				if group == "" {
					group = apiinfo.CoreGroup
				}
				rows[i] = []string{r.Name, dash(strings.Join(r.ShortNames, ",")), group, strings.Join(r.Versions, ","),
					scopeName(r.Namespaced), r.Kind, dash(strings.Join(r.Categories, ","))}
			}
			return render.Table(a.stdout, []string{"NAME", "SHORTNAMES", "GROUP", "VERSIONS", "SCOPE", "KIND", "CATEGORIES"}, rows)
		},
	}
	cmd.Flags().StringVar(&o.group, "group", "", "only this API group; core for the core group")
	cmd.Flags().StringArrayVar(&o.verbs, "verb", nil, "only resource types that support this verb, such as list (repeatable)")
	cmd.Flags().StringVar(&o.scope, "scope", "", "only namespaced or cluster-scoped resource types")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func noResourceMatches(q apiinfo.Query) error {
	var what []string
	if q.Text != "" {
		what = append(what, strconv.Quote(q.Text))
	}
	if q.Group != "" {
		what = append(what, "group "+q.Group)
	}
	for _, v := range q.Verbs {
		what = append(what, "verb "+v)
	}
	if q.Scope != apiinfo.AnyScope {
		what = append(what, string(q.Scope)+" scope")
	}
	if len(what) == 0 {
		return errors.New("the cluster serves no resource types")
	}
	return fmt.Errorf("no resource type matches %s; `tug api resources` lists them all", joinAnd(what))
}

func scopeName(namespaced bool) string {
	if namespaced {
		return "Namespaced"
	}
	return "Cluster"
}

func newAPIVersionsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:         "versions",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "List the API group-versions the cluster serves",
		Long: `List every API group-version the cluster serves, as kubectl api-versions does,
with which version of each group clients use by default, how many resource
types it serves, and the upstream release that stops serving it, from tug's
table of Kubernetes API removals.`,
		Example: `  tug api versions
  tug api versions -o name`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			apis, err := a.sessionAPIs(cmd.Context())
			if err != nil {
				return err
			}
			gvs := apis.GroupVersions(apiinfo.Upstream())
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, gvs)
			case render.FormatName:
				for _, gv := range gvs {
					fmt.Fprintln(a.stdout, gv.GroupVersion)
				}
				return nil
			}
			c := a.colors()
			rows := make([][]string, len(gvs))
			for i, gv := range gvs {
				preferred := ""
				if gv.Preferred {
					preferred = "yes"
				}
				removed := ""
				if gv.RemovedIn != "" {
					removed = c.Yellow(gv.RemovedIn)
				}
				rows[i] = []string{gv.GroupVersion, preferred, strconv.Itoa(gv.Resources), removed}
			}
			return render.Table(a.stdout, []string{"API VERSION", "PREFERRED", "RESOURCES", "REMOVED IN"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newAPIDeprecationsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:         "deprecations",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "List deprecated API versions the cluster still serves, and the objects written at them",
		Long: `List the API versions the cluster still serves that upstream Kubernetes stops
serving in some release, from tug's table of the Deprecated API Migration
Guide, with what to use instead; and the custom resource versions their
definitions mark deprecated, or that objects may still be stored at in etcd
because they were never migrated to the storage version.

OBJECTS counts the objects last written at each version, by their
managedFields or kubectl's last-applied annotation, which shows the manifests
and clients still to update. A count of 0 means no object was last written at
the old version; a client that only reads at it would still break.

deprecations only reads, and reads objects' metadata only. Run it across a
fleet with --cluster-selector before an upgrade.`,
		Example: `  tug api deprecations
  tug api deprecations -o json
  tug api deprecations --cluster-selector env=prod`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("deprecations supports -o table, json or yaml")
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, "")
			if err != nil {
				return err
			}
			clients, _, err := a.objectClients(env)
			if err != nil {
				return err
			}
			apis, err := a.discoverAPIs(ctx, clients.Discovery)
			if err != nil {
				return err
			}
			crds, err := a.listCRDs(ctx, clients.Dynamic)
			if err != nil {
				return err
			}
			report, err := apiinfo.Deprecations(ctx, clients.Metadata, apis, crds, apiinfo.Upstream())
			if err != nil {
				return err
			}
			if len(report.Unlisted) > 0 {
				a.apiWarning(fmt.Sprintf("could not list %s, so their objects are not counted", joinAnd(report.Unlisted)))
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, report)
			}
			return a.writeDeprecations(report)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

func (a *app) writeDeprecations(r *apiinfo.DeprecationReport) error {
	if len(r.Items) == 0 {
		_, err := fmt.Fprintf(a.stdout, "Nothing deprecated: %s serves no API version that upstream Kubernetes removes, "+
			"and no custom resource version is deprecated or holds objects not yet migrated.\n", dash(r.Server.GitVersion))
		return err
	}
	c := a.colors()
	unlisted := map[string]bool{}
	for _, u := range r.Unlisted {
		unlisted[u] = true
	}
	rows := make([][]string, len(r.Items))
	for i, d := range r.Items {
		removed := "-"
		if d.RemovedIn != "" {
			removed = d.RemovedIn
			if apiinfo.AtLeast(r.Server.Minor, d.RemovedIn) {
				removed = c.Red(removed)
			} else {
				removed = c.Yellow(removed)
			}
		}
		instead := dash(d.Replacement)
		if d.ReplacementSince != "" {
			instead += " (since " + d.ReplacementSince + ")"
		}
		objects := "-"
		switch {
		case d.Objects != nil:
			objects = strconv.Itoa(*d.Objects)
		case unlisted[d.Resource]:
			objects = "?"
		}
		rows[i] = []string{d.GroupVersion, d.Kind, removed, instead, objects, deprecationNote(r.Server, d)}
	}
	return render.Table(a.stdout, []string{"API VERSION", "KIND", "REMOVED IN", "USE INSTEAD", "OBJECTS", "NOTE"}, rows)
}

// deprecationNote says what else matters about a deprecated version.
func deprecationNote(server apiinfo.Server, d apiinfo.Deprecation) string {
	var notes []string
	if d.RemovedIn != "" && apiinfo.AtLeast(server.Minor, d.RemovedIn) {
		notes = append(notes, "removed upstream in "+d.RemovedIn+", yet still served")
	}
	if d.Note != "" {
		notes = append(notes, d.Note)
	}
	if d.CRD != "" {
		if d.Deprecated {
			notes = append(notes, "deprecated by its definition")
		}
		if !d.Served {
			notes = append(notes, "no longer served")
		}
		if d.Stored {
			notes = append(notes, "objects may still be stored at it")
		}
	}
	return strings.Join(notes, "; ")
}

func newAPIServerVersionCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:         "server-version",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show the cluster's Kubernetes version and where it is in its support window",
		Long: `Show the cluster's Kubernetes version and where its minor release is in the
upstream support window, from tug's table of Kubernetes releases: supported,
in maintenance (its last two months, critical fixes only) or past its end of
life, how many releases it is behind the newest, and what to upgrade to.
Managed services such as EKS, GKE and AKS may support a release for longer.

With --cluster-selector it shows every matching cluster's version, one line
each.`,
		Example: `  tug api server-version
  tug api server-version --cluster-selector env=prod`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("server-version supports -o table, json or yaml")
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, "")
			if err != nil {
				return err
			}
			dc, err := env.factory.DiscoveryFor(env.cfg)
			if err != nil {
				return err
			}
			info, err := discovery.ToCachedDiscoveryInterfaceWithContext(dc).ServerVersionWithContext(ctx)
			if err != nil {
				return fmt.Errorf("reading the server's version: %w", err)
			}
			v := serverVersionView{Cluster: env.session.State.ClusterName, Server: apiinfo.ServerOf(info)}
			v.Support = apiinfo.Upstream().Support(v.Minor, a.now())
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, v)
			}
			_, err = fmt.Fprintf(a.stdout, "%s: %s\n", a.colors().Bold(dash(v.GitVersion)), a.supportLine(v.Minor, v.Support))
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// serverVersionView is what `tug api server-version -o json` prints.
type serverVersionView struct {
	Cluster string `json:"cluster"`
	apiinfo.Server
	Support apiinfo.Support `json:"support"`
}

// supportLine says where a release stands, in a few words.
func (a *app) supportLine(minor string, s apiinfo.Support) string {
	c := a.colors()
	var line string
	switch s.Status {
	case apiinfo.Supported:
		line = c.Green("supported") + " upstream until " + s.EndOfLife
	case apiinfo.Maintenance:
		line = c.Yellow("in maintenance") + " (critical fixes only) until " + s.EndOfLife
	case apiinfo.EndOfLife:
		line = c.Red("end of life")
		if s.EndOfLife != "" {
			line += " since " + s.EndOfLife
		}
		if s.OldestSupported != "" {
			line += "; upgrade to " + s.OldestSupported + " or later"
		}
	default:
		if minor == "" {
			return "not a release tug can place"
		}
		return "newer than tug's table of releases, last checked " + s.TableChecked
	}
	switch {
	case s.Behind == 1:
		line += "; 1 release behind " + s.Newest
	case s.Behind > 1:
		line += fmt.Sprintf("; %d releases behind %s", s.Behind, s.Newest)
	}
	return line
}

func newAPIDiffCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "diff <clusterA> <clusterB>",
		Short: "Compare the APIs and CRD versions two catalog clusters serve",
		Long: `Compare what two catalog clusters serve: each resource type that one serves
and the other does not, that they serve at different versions, or whose custom
resource definition stores objects at a different version. Clusters are named
as for tug connect, by name, prefix or fuzzy match, and need no session.

diff only reads. Use it before promoting manifests from one cluster to another,
or to see what an upgrade changed.`,
		Example: `  tug api diff payments-staging payments-prod
  tug api diff eu-prod us-prod -o json`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeClusters,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("diff supports -o table, json or yaml")
			}
			ctx := cmd.Context()
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			var clusters [2]*catalog.Cluster
			for i, arg := range args {
				if clusters[i], err = a.chooseCluster(cat, []string{arg}); err != nil {
					return err
				}
			}
			if clusters[0].ID == clusters[1].ID {
				return fmt.Errorf("%s and %s are both %s; name two different clusters", args[0], args[1], clusters[0].Name)
			}
			var snaps [2]apiinfo.Snapshot
			for i, cl := range clusters {
				if snaps[i], err = a.snapshot(ctx, cat, cl); err != nil {
					return err
				}
			}
			d := apiinfo.Compare(snaps[0], snaps[1])
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, d)
			}
			return a.writeAPIDiff(d)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// snapshot reads what a catalog cluster serves, through a read-only guard
// whatever any session's mode.
func (a *app) snapshot(ctx context.Context, cat *catalog.Catalog, cl *catalog.Cluster) (apiinfo.Snapshot, error) {
	_, broker, err := a.services(cat)
	if err != nil {
		return apiinfo.Snapshot{}, err
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
		return apiinfo.Snapshot{}, err
	}
	dc, err := f.DiscoveryFor(cfg)
	if err != nil {
		return apiinfo.Snapshot{}, err
	}
	apis, err := a.discoverAPIs(ctx, dc)
	if err != nil {
		return apiinfo.Snapshot{}, fmt.Errorf("%s: %w", cl.Name, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return apiinfo.Snapshot{}, err
	}
	crds, err := a.listCRDs(ctx, dyn)
	if err != nil {
		return apiinfo.Snapshot{}, fmt.Errorf("%s: %w", cl.Name, err)
	}
	return apiinfo.Snapshot{Cluster: cl.Name, APIs: apis, CRDs: crds}, nil
}

func (a *app) writeAPIDiff(d *apiinfo.Diff) error {
	c := a.colors()
	fmt.Fprintf(a.stdout, "%s runs %s, %s runs %s\n", c.Bold(d.A.Cluster), dash(d.A.Server.GitVersion), c.Bold(d.B.Cluster), dash(d.B.Server.GitVersion))
	if len(d.Resources) == 0 {
		_, err := fmt.Fprintln(a.stdout, "They serve the same resource types at the same versions.")
		return err
	}
	fmt.Fprintln(a.stdout)
	rows := make([][]string, len(d.Resources))
	for i, r := range d.Resources {
		rows[i] = []string{r.Resource, r.Kind, servedCell(r.A), servedCell(r.B)}
	}
	return render.Table(a.stdout, []string{"RESOURCE", "KIND", strings.ToUpper(d.A.Cluster), strings.ToUpper(d.B.Cluster)}, rows)
}

// servedCell writes how one cluster serves a resource type.
func servedCell(s apiinfo.Served) string {
	if len(s.Versions) == 0 {
		return "-"
	}
	out := strings.Join(s.Versions, ",")
	if s.Storage != "" {
		out += " (stores " + s.Storage + ")"
	}
	return out
}

// sessionAPIs reads afresh what the session's cluster serves.
func (a *app) sessionAPIs(ctx context.Context) (*apiinfo.APIs, error) {
	env, err := a.opsEnv(ctx, "")
	if err != nil {
		return nil, err
	}
	dc, err := env.factory.DiscoveryFor(env.cfg)
	if err != nil {
		return nil, err
	}
	return a.discoverAPIs(ctx, dc)
}

// discoverAPIs reads a cluster's discovery afresh, as kubectl api-resources
// does, so a CRD installed since the cache was filled shows up, and warns
// about group-versions it could not read.
func (a *app) discoverAPIs(ctx context.Context, dc discovery.DiscoveryInterface) (*apiinfo.APIs, error) {
	if cached, ok := dc.(discovery.CachedDiscoveryInterface); ok {
		cached.Invalidate()
	}
	apis, err := apiinfo.Discover(ctx, discovery.ToDiscoveryInterfaceWithContext(dc))
	if err != nil {
		return nil, err
	}
	if len(apis.Unavailable) > 0 {
		a.apiWarning(fmt.Sprintf("discovery could not read %s, so what it serves is missing", joinAnd(apis.Unavailable)))
	}
	return apis, nil
}

// listCRDs reads every custom resource definition, or returns none, with
// a warning, when listing them is forbidden.
func (a *app) listCRDs(ctx context.Context, dyn dynamic.Interface) ([]apiinfo.CRD, error) {
	crds, err := apiinfo.ListCRDs(ctx, dyn)
	if apierrors.IsForbidden(err) {
		a.apiWarning("not allowed to list custom resource definitions, so their versions are left out")
		return nil, nil
	}
	return crds, err
}

// apiWarning writes a warning to stderr.
func (a *app) apiWarning(msg string) {
	fmt.Fprintf(a.stderr, "%s %s\n", render.ColorsFor(a.stderr).Yellow("warning:"), msg)
}

func newCRDCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "crd",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explain a custom resource definition and its schema",
		Long: `Explain a custom resource definition: its versions and which are served,
stored and deprecated, how objects are converted between them, its printer
columns, and its schema field by field. Name it as kubectl names its resource,
by kind, plural, singular or short name, with a group to be precise, or by the
definition's own name; a fuzzy name works too, and a picker opens on ties.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newCRDExplainCmd(a), newCRDSchemaCmd(a))
	return cmd
}

func newCRDExplainCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:         "explain <crd>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show a CRD's versions, conversion, stored versions and printer columns",
		Example: `  tug crd explain certificate
  tug crd explain kustomizations.kustomize.toolkit.fluxcd.io -o yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("crd explain supports -o table, json or yaml")
			}
			crd, err := a.sessionCRD(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, crd)
			}
			return a.writeCRD(crd)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

func (a *app) writeCRD(crd *apiinfo.CRD) error {
	c := a.colors()
	names := []string{crd.Plural}
	if crd.Singular != "" {
		names = append(names, crd.Singular)
	}
	names = append(names, crd.ShortNames...)
	kind := crd.Kind
	if crd.ListKind != "" {
		kind += " (list " + crd.ListKind + ")"
	}
	conversion := crd.Conversion.Strategy
	if crd.Conversion.Webhook != "" {
		conversion += " " + crd.Conversion.Webhook
	}
	if len(crd.Conversion.ReviewVersions) > 0 {
		conversion += ", ConversionReview " + strings.Join(crd.Conversion.ReviewVersions, ",")
	}
	stored := dash(strings.Join(crd.StoredVersions, ", "))
	if old := crd.Unmigrated(); len(old) > 0 {
		stored += c.Yellow(fmt.Sprintf(" (objects may still be stored at %s; rewrite them before dropping it)", joinAnd(old)))
	}
	var conditions []string
	for _, cond := range crd.Conditions {
		s := cond.Type + "=" + cond.Status
		if cond.Status != "True" && cond.Message != "" {
			s += " (" + cond.Message + ")"
		}
		conditions = append(conditions, s)
	}
	lines := [][]string{
		{"Name:", c.Bold(crd.Name)},
		{"Kind:", kind},
		{"Group:", crd.Group},
		{"Scope:", crd.Scope},
		{"Names:", strings.Join(names, ", ")},
		{"Categories:", dash(strings.Join(crd.Categories, ", "))},
		{"Conversion:", conversion},
		{"Stored versions:", stored},
		{"Conditions:", dash(strings.Join(conditions, ", "))},
	}
	if err := writeAligned(a.stdout, lines); err != nil {
		return err
	}

	fmt.Fprintln(a.stdout)
	rows := make([][]string, len(crd.Versions))
	var warnings []string
	for i, v := range crd.Versions {
		rows[i] = []string{v.Name, yes(v.Served), yes(v.Storage), yes(v.Deprecated), dash(strings.Join(v.Subresources, ", "))}
		if v.Deprecated && v.DeprecationWarning != "" {
			warnings = append(warnings, v.Name+": "+v.DeprecationWarning)
		}
	}
	if err := render.Table(a.stdout, []string{"VERSION", "SERVED", "STORAGE", "DEPRECATED", "SUBRESOURCES"}, rows); err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(a.stdout, "%s %s\n", c.Yellow("deprecated"), w)
	}

	preferred := crd.Preferred()
	v, ok := crd.Version(preferred)
	if !ok || len(v.PrinterColumns) == 0 {
		return nil
	}
	fmt.Fprintf(a.stdout, "\nPrinter columns of %s:\n", preferred)
	rows = make([][]string, len(v.PrinterColumns))
	for i, col := range v.PrinterColumns {
		typ := col.Type
		if col.Format != "" {
			typ += " (" + col.Format + ")"
		}
		if col.Priority > 0 {
			typ += ", -o wide"
		}
		rows[i] = []string{col.Name, typ, col.JSONPath, dash(col.Description)}
	}
	return render.Table(a.stdout, []string{"NAME", "TYPE", "JSONPATH", "DESCRIPTION"}, rows)
}

// writeAligned writes label and value pairs with the values lined up.
func writeAligned(w io.Writer, lines [][]string) error {
	width := 0
	for _, l := range lines {
		width = max(width, utf8.RuneCountInString(l[0]))
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l[0] + strings.Repeat(" ", width-utf8.RuneCountInString(l[0])+2) + l[1] + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

type crdSchemaOptions struct {
	field   string
	version string
	depth   int
	output  string
}

func newCRDSchemaCmd(a *app) *cobra.Command {
	var o crdSchemaOptions
	cmd := &cobra.Command{
		Use:         "schema <crd>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show a CRD's schema field by field, with types, required fields and descriptions",
		Long: `Show a custom resource's schema from its definition's openAPIV3Schema, one
field a line, indented under its parent, with its type, whether it is required
and the first sentence of its description. Fields of a list's items and of a
map's values are shown under the list or map. --field shows one field in full,
with its whole description, allowed values and default, and what is below it.

It shows the version clients use by default unless --version says otherwise.`,
		Example: `  tug crd schema certificate
  tug crd schema certificate --field spec.issuerRef
  tug crd schema widgets.example.com --version v1beta1 --depth 2
  tug crd schema certificate -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("crd schema supports -o table, json or yaml")
			}
			if o.depth < 0 {
				return fmt.Errorf("--depth wants a number from 0 (every level), got %d", o.depth)
			}
			crd, err := a.sessionCRD(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			name := o.version
			if name == "" {
				name = crd.Preferred()
			}
			v, ok := crd.Version(name)
			if !ok {
				var have []string
				for _, v := range crd.Versions {
					have = append(have, v.Name)
				}
				return fmt.Errorf("%s has no version %q; it has %s", crd.Name, name, joinAnd(have))
			}
			fields, err := apiinfo.Fields(v.Schema, o.field, o.depth)
			if err != nil {
				return fmt.Errorf("%s %s: %w", crd.Name, v.Name, err)
			}
			view := schemaView{CRD: crd.Name, Kind: crd.Kind, APIVersion: apiinfo.GroupVersion(crd.Group, v.Name), Fields: fields}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, view)
			}
			return a.writeSchema(view, v, o.field != "")
		},
	}
	cmd.Flags().StringVar(&o.field, "field", "", "show only this field and what is below it, as a dotted path such as spec.issuerRef")
	cmd.Flags().StringVar(&o.version, "version", "", "the version's schema to show (default: the version clients use)")
	cmd.Flags().IntVar(&o.depth, "depth", 0, "how many levels of fields to show (default: all)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// schemaView is what `tug crd schema -o json` prints.
type schemaView struct {
	CRD        string          `json:"crd"`
	Kind       string          `json:"kind"`
	APIVersion string          `json:"apiVersion"`
	Fields     []apiinfo.Field `json:"fields"`
}

func (a *app) writeSchema(view schemaView, v *apiinfo.CRDVersion, oneField bool) error {
	c := a.colors()
	fields := view.Fields
	if oneField {
		f := fields[0]
		head := f.Type
		if f.Required {
			head += ", required"
		}
		fmt.Fprintf(a.stdout, "%s (%s) in %s %s\n", c.Bold(f.Path), head, view.Kind, view.APIVersion)
		for _, line := range fieldDetail(f, render.Width(a.stdout)) {
			fmt.Fprintln(a.stdout, "  "+line)
		}
		fields = fields[1:]
		if len(fields) == 0 {
			return nil
		}
		fmt.Fprintln(a.stdout)
	} else {
		head := c.Bold(view.Kind) + " " + view.APIVersion
		if v.Schema != nil && v.Schema.Description != "" {
			head += ": " + summary(v.Schema.Description)
		}
		fmt.Fprintln(a.stdout, head)
		if len(fields) == 0 {
			_, err := fmt.Fprintln(a.stdout, "The schema declares no fields; the version takes any.")
			return err
		}
		fmt.Fprintln(a.stdout)
	}
	base := fields[0].Depth
	rows := make([][]string, len(fields))
	for i, f := range fields {
		required := ""
		if f.Required {
			required = "yes"
		}
		rows[i] = []string{strings.Repeat("  ", f.Depth-base) + f.Name, f.Type, required, fieldSummary(f)}
	}
	return render.Table(a.stdout, []string{"FIELD", "TYPE", "REQUIRED", "DESCRIPTION"}, rows)
}

// fieldDetail is a field's whole description, allowed values and default,
// wrapped to width.
func fieldDetail(f apiinfo.Field, width int) []string {
	var lines []string
	if width > 0 {
		width -= 2
	}
	for _, para := range strings.Split(f.Description, "\n") {
		if para = strings.TrimSpace(para); para != "" {
			lines = append(lines, render.Wrap(para, width)...)
		}
	}
	if len(f.Enum) > 0 {
		lines = append(lines, "One of: "+strings.Join(f.Enum, ", "))
	}
	if f.Default != "" {
		lines = append(lines, "Default: "+f.Default)
	}
	if len(lines) == 0 {
		lines = append(lines, "No description.")
	}
	return lines
}

// fieldSummary is the first sentence of a field's description, with its
// allowed values and default.
func fieldSummary(f apiinfo.Field) string {
	s := summary(f.Description)
	if len(f.Enum) > 0 {
		s = strings.TrimSpace(s + " One of: " + strings.Join(f.Enum, ", ") + ".")
	}
	if f.Default != "" {
		s = strings.TrimSpace(s + " Default: " + f.Default + ".")
	}
	return s
}

// maxSummary bounds a description's summary, in characters.
const maxSummary = 100

// summary is the first sentence of a description, cut short when long.
func summary(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = desc[:i]
	}
	if i := strings.Index(desc, ". "); i >= 0 {
		desc = desc[:i+1]
	}
	if utf8.RuneCountInString(desc) > maxSummary {
		r := []rune(desc)
		desc = strings.TrimSpace(string(r[:maxSummary-1])) + "…"
	}
	return desc
}

// sessionCRD finds the custom resource definition an argument names on the
// session's cluster.
func (a *app) sessionCRD(ctx context.Context, arg string) (*apiinfo.CRD, error) {
	env, err := a.opsEnv(ctx, "")
	if err != nil {
		return nil, err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return nil, err
	}
	return a.resolveCRD(ctx, clients, mapper, arg)
}

// resolveCRD finds the custom resource definition an argument names: the
// definition's own name; any name kubectl takes for its resource, such as
// its kind or a short name; or else a fuzzy match on definitions' names,
// asking when several match equally well.
func (a *app) resolveCRD(ctx context.Context, c ops.Clients, mapper meta.RESTMapper, arg string) (*apiinfo.CRD, error) {
	if strings.Contains(arg, ".") {
		crd, err := apiinfo.ReadCRD(ctx, c.Dynamic, strings.ToLower(arg))
		if !errors.Is(err, apiinfo.ErrNoCRD) {
			return crd, err
		}
	}
	if m, err := resolve.FindResource(mapper, arg); err == nil {
		gr := m.Resource.GroupResource()
		crd, err := apiinfo.ReadCRD(ctx, c.Dynamic, gr.String())
		if errors.Is(err, apiinfo.ErrNoCRD) {
			return nil, fmt.Errorf("%s is not defined by a custom resource definition; `tug api resources %s` shows it", gr, m.Resource.Resource)
		}
		return crd, err
	}
	names, err := apiinfo.CRDNames(ctx, c.Metadata)
	if err != nil {
		return nil, err
	}
	matches := resolve.Best(arg, names)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no custom resource definition matches %q; `tug api resources` lists what the cluster serves", arg)
	}
	i, err := a.pick("Definitions matching "+arg+":", matches)
	if err != nil {
		return nil, err
	}
	return apiinfo.ReadCRD(ctx, c.Dynamic, matches[i])
}
