package apiinfo

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

// lastApplied is the annotation kubectl apply keeps the applied object in.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Deprecation is an API version that a cluster serves or holds objects at
// and that is going away: upstream Kubernetes stops serving it in some
// release, or its custom resource definition marks it deprecated or has
// not yet migrated every object away from it.
type Deprecation struct {
	GroupVersion string `json:"groupVersion"`
	Kind         string `json:"kind"`
	// Resource is the resource type, such as
	// horizontalpodautoscalers.autoscaling.
	Resource string `json:"resource"`
	// CRD names the definition of a custom resource's version.
	CRD    string `json:"crd,omitempty"`
	Served bool   `json:"served"`
	// RemovedIn is the upstream release that stops serving a built-in
	// version.
	RemovedIn        string `json:"removedIn,omitempty"`
	Replacement      string `json:"replacement,omitempty"`
	ReplacementSince string `json:"replacementSince,omitempty"`
	// Note says what to do when there is no replacement.
	Note string `json:"note,omitempty"`
	// Deprecated is true for a version its definition marks deprecated,
	// and Warning is what the API server tells clients that use it.
	Deprecated bool   `json:"deprecated,omitempty"`
	Warning    string `json:"warning,omitempty"`
	// Stored is true when the definition says objects may still be stored
	// at this version in etcd.
	Stored bool `json:"stored,omitempty"`
	// Objects counts the objects last written at this version, by their
	// managedFields or kubectl's last-applied annotation, which shows the
	// clients and manifests still to update; nil when they were not
	// counted.
	Objects *int `json:"objects,omitempty"`

	group, name string
}

// DeprecationReport is what tug api deprecations finds on a cluster.
type DeprecationReport struct {
	Server Server `json:"server"`
	// TableChecked is the day tug's table of upstream removals was last
	// checked.
	TableChecked string        `json:"tableChecked"`
	Items        []Deprecation `json:"items"`
	// Unlisted are resource types whose objects could not be listed, so
	// their deprecations have no Objects.
	Unlisted []string `json:"unlisted,omitempty"`
}

// Deprecations finds what apis and crds serve or store that is going away
// and counts the objects last written at each such version.
func Deprecations(ctx context.Context, md metadata.Interface, apis *APIs, crds []CRD, t *Table) (*DeprecationReport, error) {
	r := &DeprecationReport{Server: apis.Server, TableChecked: t.Checked, Items: FindDeprecations(apis, crds, t)}
	unlisted, err := CountWritten(ctx, md, apis, r.Items)
	if err != nil {
		return nil, err
	}
	r.Unlisted = unlisted
	return r, nil
}

// FindDeprecations lists the built-in versions apis serves that upstream
// Kubernetes removes in some release, and the versions of crds that their
// definitions mark deprecated or that objects may still be stored at. It
// counts no objects.
func FindDeprecations(apis *APIs, crds []CRD, t *Table) []Deprecation {
	var out []Deprecation
	for _, r := range apis.Resources {
		for _, v := range r.Versions {
			gv := GroupVersion(r.Group, v)
			rm, ok := t.Removal(gv, r.Kind)
			if !ok {
				continue
			}
			out = append(out, Deprecation{
				GroupVersion:     gv,
				Kind:             r.Kind,
				Resource:         r.GroupResource(),
				Served:           true,
				RemovedIn:        rm.RemovedIn,
				Replacement:      rm.Replacement,
				ReplacementSince: rm.ReplacementSince,
				Note:             rm.Note,
				group:            r.Group,
				name:             r.Name,
			})
		}
	}
	for i := range crds {
		c := &crds[i]
		unmigrated := c.Unmigrated()
		for _, v := range c.Versions {
			stale := slices.Contains(unmigrated, v.Name)
			if !stale && (!v.Served || !v.Deprecated) {
				continue
			}
			d := Deprecation{
				GroupVersion: GroupVersion(c.Group, v.Name),
				Kind:         c.Kind,
				Resource:     qualify(c.Plural, c.Group),
				CRD:          c.Name,
				Served:       v.Served,
				Deprecated:   v.Deprecated,
				Warning:      v.DeprecationWarning,
				Stored:       slices.Contains(c.StoredVersions, v.Name),
				group:        c.Group,
				name:         c.Plural,
			}
			if to := c.replacement(v.Name); to != "" {
				d.Replacement = GroupVersion(c.Group, to)
			}
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Deprecation) int {
		return cmp.Or(compareGroups(a.group, b.group), cmp.Compare(a.name, b.name), cmp.Compare(a.GroupVersion, b.GroupVersion))
	})
	return out
}

// replacement is the version to move to from version from: the storage
// version, if it is served and not deprecated, else the most preferred
// such version.
func (c *CRD) replacement(from string) string {
	ok := func(name string) bool {
		v, found := c.Version(name)
		return found && name != from && v.Served && !v.Deprecated
	}
	if s := c.Storage(); ok(s) {
		return s
	}
	for _, name := range c.Served() {
		if ok(name) {
			return name
		}
	}
	return ""
}

// CountWritten sets the Objects of each deprecation by listing the
// metadata of its resource's objects, across all namespaces, once per
// resource. It returns the resource types it could not list.
func CountWritten(ctx context.Context, md metadata.Interface, apis *APIs, items []Deprecation) ([]string, error) {
	type key struct{ group, name string }
	byResource := map[key][]int{}
	var order []key
	for i, d := range items {
		k := key{d.group, d.name}
		if _, seen := byResource[k]; !seen {
			order = append(order, k)
		}
		byResource[k] = append(byResource[k], i)
	}
	var unlisted []string
	for _, k := range order {
		r, ok := apis.Resource(k.group, k.name)
		if !ok || !r.Can("list") {
			continue
		}
		written, err := writtenVersions(ctx, md, r)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			unlisted = append(unlisted, r.GroupResource())
			continue
		}
		for _, i := range byResource[k] {
			n := written[items[i].GroupVersion]
			items[i].Objects = &n
		}
	}
	return unlisted, nil
}

// writtenVersions counts a resource's objects by each API version they were
// last written at.
func writtenVersions(ctx context.Context, md metadata.Interface, r Resource) (map[string]int, error) {
	gvr := schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Name}
	counts := map[string]int{}
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := md.Resource(gvr).Namespace("").List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			for _, v := range writtenAt(&list.Items[i].ObjectMeta) {
				counts[v]++
			}
		}
		if list.Continue == "" {
			return counts, nil
		}
		opts.Continue = list.Continue
	}
}

// writtenAt returns the API versions an object was written at, once each.
func writtenAt(m *metav1.ObjectMeta) []string {
	var out []string
	add := func(v string) {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for _, f := range m.ManagedFields {
		add(f.APIVersion)
	}
	if s, ok := m.Annotations[lastApplied]; ok {
		var applied struct {
			APIVersion string `json:"apiVersion"`
		}
		if json.Unmarshal([]byte(s), &applied) == nil {
			add(applied.APIVersion)
		}
	}
	return out
}
