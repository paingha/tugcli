package diff

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
)

// Object states in a Drift.
const (
	StateDiffers   = "differs"
	StateOnlyHere  = "only-here"
	StateOnlyThere = "only-there"
)

// DefaultDriftResources are the resources drift compares when it is not
// told which: what people deploy, not what controllers make from it.
// Secrets are left out unless asked for, and then only their metadata is
// compared.
var DefaultDriftResources = []string{
	"deployments.apps", "statefulsets.apps", "daemonsets.apps", "cronjobs.batch",
	"services", "configmaps", "ingresses.networking.k8s.io",
}

// crdResource is where the API server keeps CustomResourceDefinitions.
var crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// Discovery is what drift reads of a cluster's API discovery.
type Discovery interface {
	ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error)
	ServerVersionWithContext(ctx context.Context) (*version.Info, error)
}

// Cluster is one side of a drift comparison.
type Cluster struct {
	Name      string
	Clients   Clients
	Discovery Discovery
}

// DriftOptions say what Drift compares.
type DriftOptions struct {
	// Resources are the types of object to compare, as the first
	// cluster serves them.
	Resources []*meta.RESTMapping
	// Namespace is where namespaced objects are compared; "" compares
	// every namespace.
	Namespace string
	// AllFields keeps the fields that only the clusters set themselves.
	AllFields bool
}

// Drift is how two clusters differ: their API versions, their
// CustomResourceDefinitions and the objects of the resources compared.
type Drift struct {
	Here      ClusterVersion `json:"here"`
	There     ClusterVersion `json:"there"`
	Namespace string         `json:"namespace,omitempty"`
	Same      bool           `json:"same"`
	// APIVersions are the group versions only one cluster serves.
	APIVersions APIVersionDrift `json:"apiVersions"`
	// CRDs are the CustomResourceDefinitions that are on one cluster
	// only, or serve different versions.
	CRDs []CRDDrift `json:"crds"`
	// Resources are the resources compared, as group/resource.
	Resources []string `json:"resources"`
	// Unserved are the resources compared that the second cluster does
	// not serve in the version the first serves them in.
	Unserved []string `json:"unserved,omitempty"`
	// Compared counts the objects found on either cluster.
	Compared int `json:"compared"`
	// Objects are the objects that differ or are on one cluster only.
	Objects []ObjectDrift `json:"objects"`
	// Omitted counts the differing fields left out because only the
	// clusters set them.
	Omitted int `json:"omitted,omitempty"`
	// Problems are parts that could not be compared, such as
	// CustomResourceDefinitions the credential may not list.
	Problems []string `json:"problems,omitempty"`
}

// ClusterVersion names a cluster and its Kubernetes version.
type ClusterVersion struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// APIVersionDrift lists the group versions only one cluster serves.
type APIVersionDrift struct {
	OnlyHere  []string `json:"onlyHere"`
	OnlyThere []string `json:"onlyThere"`
}

// CRDDrift is a CustomResourceDefinition that differs.
type CRDDrift struct {
	Name string `json:"name"`
	// HereVersions and ThereVersions are the versions each cluster
	// serves; none when it does not have the definition.
	HereVersions  []string `json:"hereVersions"`
	ThereVersions []string `json:"thereVersions"`
}

// ObjectDrift is an object that differs, or is on one cluster only.
type ObjectDrift struct {
	// Resource is group/resource, such as deployments.apps.
	Resource  string   `json:"resource"`
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Changes   []Change `json:"changes,omitempty"`
}

// CompareClusters compares two clusters: the API versions and
// CustomResourceDefinitions they serve, and the objects of opts.Resources
// in opts.Namespace. A Secret is compared by its metadata alone; its data
// is never read.
func CompareClusters(ctx context.Context, here, there Cluster, opts DriftOptions) (*Drift, error) {
	d := &Drift{
		Here:      ClusterVersion{Name: here.Name},
		There:     ClusterVersion{Name: there.Name},
		Namespace: opts.Namespace,
		CRDs:      []CRDDrift{},
		Resources: []string{},
		Objects:   []ObjectDrift{},
	}
	var err error
	if d.Here.Version, err = serverVersion(ctx, here); err != nil {
		return nil, err
	}
	if d.There.Version, err = serverVersion(ctx, there); err != nil {
		return nil, err
	}
	if d.APIVersions, err = compareAPIVersions(ctx, here, there); err != nil {
		return nil, err
	}
	if err := compareCRDs(ctx, here, there, d); err != nil {
		return nil, err
	}
	for _, m := range opts.Resources {
		if err := compareResource(ctx, here, there, m, opts, d); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(d.Objects, func(a, b ObjectDrift) int {
		return cmp.Or(cmp.Compare(a.Resource, b.Resource), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	d.Same = len(d.APIVersions.OnlyHere) == 0 && len(d.APIVersions.OnlyThere) == 0 &&
		len(d.CRDs) == 0 && len(d.Unserved) == 0 && len(d.Objects) == 0
	return d, nil
}

func serverVersion(ctx context.Context, c Cluster) (string, error) {
	v, err := c.Discovery.ServerVersionWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the Kubernetes version of %s: %w", c.Name, err)
	}
	return v.GitVersion, nil
}

func compareAPIVersions(ctx context.Context, here, there Cluster) (APIVersionDrift, error) {
	a, err := groupVersions(ctx, here)
	if err != nil {
		return APIVersionDrift{}, err
	}
	b, err := groupVersions(ctx, there)
	if err != nil {
		return APIVersionDrift{}, err
	}
	return APIVersionDrift{OnlyHere: missingFrom(a, b), OnlyThere: missingFrom(b, a)}, nil
}

// groupVersions lists the group versions a cluster serves, sorted.
func groupVersions(ctx context.Context, c Cluster) ([]string, error) {
	groups, err := c.Discovery.ServerGroupsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the API groups of %s: %w", c.Name, err)
	}
	var out []string
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			out = append(out, v.GroupVersion)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// missingFrom lists the items of a that b lacks; both are sorted.
func missingFrom(a, b []string) []string {
	out := []string{}
	for _, s := range a {
		if _, found := slices.BinarySearch(b, s); !found {
			out = append(out, s)
		}
	}
	return out
}

func compareCRDs(ctx context.Context, here, there Cluster, d *Drift) error {
	a, err := crdVersions(ctx, here)
	if err != nil {
		if !apierrors.IsForbidden(err) {
			return err
		}
		d.Problems = append(d.Problems, err.Error())
		return nil
	}
	b, err := crdVersions(ctx, there)
	if err != nil {
		if !apierrors.IsForbidden(err) {
			return err
		}
		d.Problems = append(d.Problems, err.Error())
		return nil
	}
	names := make([]string, 0, len(a)+len(b))
	for name := range a {
		names = append(names, name)
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		va, vb := a[name], b[name]
		if !slices.Equal(va, vb) {
			d.CRDs = append(d.CRDs, CRDDrift{Name: name, HereVersions: orEmpty(va), ThereVersions: orEmpty(vb)})
		}
	}
	return nil
}

// orEmpty returns s, or an empty list for nil, so JSON shows [] rather
// than null.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// crdVersions maps each CustomResourceDefinition's name to the versions it
// serves, sorted.
func crdVersions(ctx context.Context, c Cluster) (map[string][]string, error) {
	out := map[string][]string{}
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := c.Clients.Dynamic.Resource(crdResource).List(ctx, opts)
		if apierrors.IsNotFound(err) {
			return out, nil // a cluster without the API extensions group has none
		}
		if err != nil {
			return nil, fmt.Errorf("listing the CustomResourceDefinitions of %s: %w", c.Name, err)
		}
		for _, it := range list.Items {
			spec, _ := it.Object["spec"].(map[string]any)
			versions, _ := spec["versions"].([]any)
			served := []string{}
			for _, v := range versions {
				m, _ := v.(map[string]any)
				if ok, _ := m["served"].(bool); ok {
					served = append(served, str(m["name"]))
				}
			}
			slices.Sort(served)
			out[it.GetName()] = served
		}
		if list.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = list.GetContinue()
	}
}

// compareResource compares the objects of one resource on both clusters.
func compareResource(ctx context.Context, here, there Cluster, m *meta.RESTMapping, opts DriftOptions, d *Drift) error {
	gr := m.Resource.GroupResource().String()
	d.Resources = append(d.Resources, gr)
	ns := opts.Namespace
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}
	a, err := listObjects(ctx, here, m.Resource, ns)
	if err != nil {
		return err
	}
	b, err := listObjects(ctx, there, m.Resource, ns)
	if apierrors.IsNotFound(err) {
		d.Unserved = append(d.Unserved, gr)
		return nil
	}
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	d.Compared += len(keys)
	for _, k := range keys {
		x, y := a[k], b[k]
		namespace, name, _ := strings.Cut(k, "/")
		od := ObjectDrift{Resource: gr, Kind: strings.ToLower(m.GroupVersionKind.Kind), Namespace: namespace, Name: name}
		switch {
		case y == nil:
			od.State = StateOnlyHere
		case x == nil:
			od.State = StateOnlyThere
		default:
			r, err := compareSides(sides{from: x, to: y, acrossClusters: true}, Options{AllFields: opts.AllFields})
			if err != nil {
				return fmt.Errorf("comparing %s %s: %w", gr, k, err)
			}
			d.Omitted += r.Omitted
			if r.Same {
				continue
			}
			od.State, od.Changes = StateDiffers, r.Changes
		}
		d.Objects = append(d.Objects, od)
	}
	return nil
}

// listObjects lists a resource's objects in ns, or everywhere for "",
// keyed by namespace/name. Secrets are listed through the metadata API, so
// their data never leaves the API server.
func listObjects(ctx context.Context, c Cluster, r schema.GroupVersionResource, ns string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	opts := metav1.ListOptions{Limit: 500}
	for {
		var (
			items []map[string]any
			next  string
		)
		if IsSecret(r) {
			list, err := c.Clients.Metadata.Resource(r).Namespace(ns).List(ctx, opts)
			if err != nil {
				return nil, listError(c, r, err)
			}
			for i := range list.Items {
				obj, err := secretMetadata(&list.Items[i])
				if err != nil {
					return nil, err
				}
				items = append(items, obj)
			}
			next = list.Continue
		} else {
			list, err := c.Clients.Dynamic.Resource(r).Namespace(ns).List(ctx, opts)
			if err != nil {
				return nil, listError(c, r, err)
			}
			for _, it := range list.Items {
				items = append(items, it.Object)
			}
			next = list.GetContinue()
		}
		for _, obj := range items {
			md, _ := obj["metadata"].(map[string]any)
			out[str(md["namespace"])+"/"+str(md["name"])] = obj
		}
		if next == "" {
			return out, nil
		}
		opts.Continue = next
	}
}

func listError(c Cluster, r schema.GroupVersionResource, err error) error {
	return fmt.Errorf("listing %s on %s: %w", r.GroupResource(), c.Name, err)
}
