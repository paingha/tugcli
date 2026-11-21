// Package gitops reads what Flux and Argo CD report about the objects they
// deliver and ties it to the live objects those manage: whether each is
// ready, where a failure sits, what the tool itself reports out of sync, and
// how they depend on each other. It reads through the dynamic and metadata
// clients, so it needs neither tool's Go modules, and it skips quietly a tool
// the cluster does not serve. It only reads: it never asks either tool to
// sync, reconcile or roll back, and never reads a Secret's data.
package gitops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/paingha/tugcli/internal/ops"
)

// Clients are what gitops reads with: the clients of ops, whose diagnoses it
// gives for what Flux and Argo CD manage, and a RESTMapper to find the
// resource of each managed object's kind.
type Clients struct {
	ops.Clients
	Mapper meta.RESTMapper
}

// Tool is a GitOps tool tug understands.
type Tool string

// The tools.
const (
	Flux   Tool = "flux"
	ArgoCD Tool = "argocd"
)

// The kinds tug reads.
const (
	KindGitRepository  = "GitRepository"
	KindOCIRepository  = "OCIRepository"
	KindHelmRepository = "HelmRepository"
	KindBucket         = "Bucket"
	KindHelmChart      = "HelmChart"
	KindKustomization  = "Kustomization"
	KindHelmRelease    = "HelmRelease"
	KindApplication    = "Application"
)

const (
	fluxSourceGroup = "source.toolkit.fluxcd.io"
	argoGroup       = "argoproj.io"
)

// kind is one kind of object tug reads.
type kind struct {
	tool     Tool
	name     string
	group    string
	resource string
	// aliases are kubectl's short names for it.
	aliases []string
	// source kinds fetch artifacts that others apply.
	source bool
	// listed kinds are shown by Status; a HelmChart is not, since
	// helm-controller makes one for each HelmRelease.
	listed bool
}

// kinds are in the order Status lists them: sources before what applies
// them.
var kinds = []kind{
	{tool: Flux, name: KindGitRepository, group: fluxSourceGroup, resource: "gitrepositories", aliases: []string{"gitrepo"}, source: true, listed: true},
	{tool: Flux, name: KindOCIRepository, group: fluxSourceGroup, resource: "ocirepositories", aliases: []string{"ocirepo"}, source: true, listed: true},
	{tool: Flux, name: KindHelmRepository, group: fluxSourceGroup, resource: "helmrepositories", aliases: []string{"helmrepo"}, source: true, listed: true},
	{tool: Flux, name: KindBucket, group: fluxSourceGroup, resource: "buckets", source: true, listed: true},
	{tool: Flux, name: KindHelmChart, group: fluxSourceGroup, resource: "helmcharts", aliases: []string{"hc"}, source: true},
	{tool: Flux, name: KindKustomization, group: "kustomize.toolkit.fluxcd.io", resource: "kustomizations", aliases: []string{"ks"}, listed: true},
	{tool: Flux, name: KindHelmRelease, group: "helm.toolkit.fluxcd.io", resource: "helmreleases", aliases: []string{"hr"}, listed: true},
	{tool: ArgoCD, name: KindApplication, group: argoGroup, resource: "applications", aliases: []string{"app", "apps"}, listed: true},
}

func kindNamed(name string) (kind, bool) {
	i := slices.IndexFunc(kinds, func(k kind) bool { return k.name == name })
	if i < 0 {
		return kind{}, false
	}
	return kinds[i], true
}

func kindRank(name string) int {
	return slices.IndexFunc(kinds, func(k kind) bool { return k.name == name })
}

// delivers reports whether a kind applies objects to a cluster, rather than
// fetching them.
func delivers(kindName string) bool {
	return kindName == KindKustomization || kindName == KindHelmRelease || kindName == KindApplication
}

// Ref names a Flux or Argo CD object.
type Ref struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// String returns the kind in lower case and the name, as tug prints objects:
// "kustomization/apps".
func (r Ref) String() string { return strings.ToLower(r.Kind) + "/" + r.Name }

// Destination is where an Argo CD Application deploys to.
type Destination struct {
	Server    string `json:"server,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// Object is what Flux or Argo CD reports about one of its objects.
type Object struct {
	Tool      Tool   `json:"tool"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Ready is tug's reading of its state: ready, not-ready, unknown while
	// it is being reconciled, or none when it reports nothing.
	Ready ops.Health `json:"ready"`
	// Reason and Message are what it says about its state, such as Flux's
	// Ready condition or the error Argo CD reports.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Sync is Argo CD's sync status: Synced, OutOfSync or Unknown.
	Sync string `json:"sync,omitempty"`
	// Health is Argo CD's health status, such as Healthy or Degraded.
	Health string `json:"health,omitempty"`
	// Suspended is true when Flux has been told not to reconcile it.
	Suspended bool `json:"suspended"`
	// Source is the Flux source it applies: a GitRepository, OCIRepository
	// or Bucket for a Kustomization; for a HelmRelease, the HelmChart or
	// OCIRepository of its chart; for a HelmChart, its repository.
	Source *Ref `json:"source,omitempty"`
	// Repo is the repository of an Argo CD Application's first source.
	Repo string `json:"repo,omitempty"`
	// Destination is where an Argo CD Application deploys to.
	Destination *Destination `json:"destination,omitempty"`
	// Revision is what its source offers now: a source's artifact, a
	// HelmRelease's chart version, or the revision Argo CD compares an
	// Application with.
	Revision string `json:"revision,omitempty"`
	// Applied is the revision it last applied successfully.
	Applied string `json:"applied,omitempty"`
	// Attempted is the revision it last tried to apply.
	Attempted string `json:"attempted,omitempty"`
	// LastReconcile is when its controller last reported a result: Argo
	// CD's status.reconciledAt or, since Flux does not record each
	// reconcile, the latest change of its conditions or artifact.
	LastReconcile time.Time `json:"lastReconcile,omitzero"`
	// Interval is how often Flux reconciles it, as spec.interval says.
	Interval           string          `json:"interval,omitempty"`
	Generation         int64           `json:"generation"`
	ObservedGeneration int64           `json:"observedGeneration"`
	DependsOn          []Ref           `json:"dependsOn,omitempty"`
	Conditions         []ops.Condition `json:"conditions,omitempty"`

	k        kind
	resource schema.GroupVersionResource
	u        *unstructured.Unstructured
}

// Ref returns the object's name.
func (o *Object) Ref() Ref { return Ref{Kind: o.Kind, Name: o.Name, Namespace: o.Namespace} }

// String returns the kind in lower case and the name: "helmrelease/podinfo".
func (o *Object) String() string { return o.Ref().String() }

// FullName returns the name kubectl resolves to the object's resource
// whatever else the cluster serves: "kustomizations.kustomize.toolkit.fluxcd.io/apps".
func (o *Object) FullName() string { return o.k.resource + "." + o.k.group + "/" + o.Name }

// condition returns the condition of a type, or nil.
func (o *Object) condition(typ string) *ops.Condition {
	i := slices.IndexFunc(o.Conditions, func(c ops.Condition) bool { return c.Type == typ })
	if i < 0 {
		return nil
	}
	return &o.Conditions[i]
}

func (o *Object) conditionTrue(typ string) bool {
	c := o.condition(typ)
	return c != nil && c.Status == string(metav1.ConditionTrue)
}

// key finds an object in a Snapshot.
type key struct{ kind, namespace, name string }

func keyOf(r Ref) key { return key{r.Kind, r.Namespace, r.Name} }

// Snapshot is what Flux and Argo CD report, read once.
type Snapshot struct {
	// Tools are the tools the cluster serves.
	Tools []Tool
	// Unlisted names the resources that could not be listed, whose objects
	// are missing.
	Unlisted []string
	// Namespace is "" when every namespace was read, and otherwise the one
	// namespace that could be.
	Namespace string

	objects []*Object
	byKey   map[key]*Object
}

// ErrNotInstalled is returned when the cluster serves neither Flux nor Argo
// CD.
var ErrNotInstalled = errors.New("the cluster serves neither Flux nor Argo CD")

// Load reads every Flux and Argo CD object in the cluster. When a resource
// may not be listed in every namespace, it is listed in namespace instead,
// if one is given. It returns ErrNotInstalled when the cluster serves
// neither tool.
func Load(ctx context.Context, c Clients, namespace string) (*Snapshot, error) {
	served, unlisted, err := discover(ctx, c.Discovery)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Unlisted: unlisted, byKey: map[key]*Object{}}
	for _, k := range kinds {
		r, ok := served[k.name]
		if !ok {
			continue
		}
		if !slices.Contains(s.Tools, k.tool) {
			s.Tools = append(s.Tools, k.tool)
		}
		items, err := list(ctx, c.Dynamic, r, metav1.NamespaceAll)
		if apierrors.IsForbidden(err) && namespace != "" {
			s.Namespace = namespace
			items, err = list(ctx, c.Dynamic, r, namespace)
		}
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			s.Unlisted = append(s.Unlisted, r.GroupResource().String())
			continue
		}
		for i := range items {
			o := parse(k, r, &items[i])
			s.objects = append(s.objects, o)
			s.byKey[keyOf(o.Ref())] = o
		}
	}
	if len(s.Tools) == 0 {
		return nil, ErrNotInstalled
	}
	s.resolveRevisions()
	slices.SortFunc(s.objects, func(a, b *Object) int {
		return cmp.Or(cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)), strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	slices.Sort(s.Unlisted)
	return s, nil
}

// get returns the object a reference names, or nil.
func (s *Snapshot) get(r Ref) *Object { return s.byKey[keyOf(r)] }

// served reports whether the cluster serves a kind: one it does not serve
// has no objects to find.
func (s *Snapshot) served(kindName string) bool {
	k, ok := kindNamed(kindName)
	return ok && slices.Contains(s.Tools, k.tool)
}

// resolveRevisions sets what the source of each Kustomization and
// HelmRelease offers now.
func (s *Snapshot) resolveRevisions() {
	for _, o := range s.objects {
		if o.Tool != Flux || o.k.source || o.Source == nil {
			continue
		}
		if src := s.get(*o.Source); src != nil {
			o.Revision = src.Revision
		}
	}
}

// discover finds which of the kinds the cluster serves, and where: for each,
// the first version of its group, the preferred one first, that serves it.
// It lists groups once and reads only the groups of Flux and Argo CD.
func discover(ctx context.Context, d discovery.DiscoveryInterface) (map[string]schema.GroupVersionResource, []string, error) {
	dc := discovery.ToDiscoveryInterfaceWithContext(d)
	groups, err := dc.ServerGroupsWithContext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discovering the cluster's API groups: %w", err)
	}
	served := map[string]schema.GroupVersionResource{}
	var unlisted []string
	for _, g := range groups.Groups {
		var wanted []kind
		for _, k := range kinds {
			if k.group == g.Name {
				wanted = append(wanted, k)
			}
		}
		if len(wanted) == 0 {
			continue
		}
		versions := []metav1.GroupVersionForDiscovery{g.PreferredVersion}
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				versions = append(versions, v)
			}
		}
		for _, v := range versions {
			if v.Version == "" {
				continue
			}
			l, err := dc.ServerResourcesForGroupVersionWithContext(ctx, v.GroupVersion)
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				unlisted = append(unlisted, v.GroupVersion)
				continue
			}
			for _, r := range l.APIResources {
				for _, k := range wanted {
					if _, done := served[k.name]; !done && r.Name == k.resource && slices.Contains(r.Verbs, "list") {
						served[k.name] = schema.GroupVersionResource{Group: g.Name, Version: v.Version, Resource: r.Name}
					}
				}
			}
		}
	}
	return served, unlisted, nil
}

// list returns every object of a resource in a namespace, or in every
// namespace for "", a page at a time.
func list(ctx context.Context, dyn dynamic.Interface, r schema.GroupVersionResource, ns string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	opts := metav1.ListOptions{Limit: 500}
	for {
		l, err := dyn.Resource(r).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, l.Items...)
		if l.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = l.GetContinue()
	}
}
