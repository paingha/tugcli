package apiinfo

import (
	"cmp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/version"
)

// Snapshot is what one cluster serves, for Compare.
type Snapshot struct {
	// Cluster is the cluster's name in the catalog.
	Cluster string
	APIs    *APIs
	// CRDs are its custom resource definitions; nil when they could not
	// be read, in which case storage versions are not compared.
	CRDs []CRD
}

// Diff is how the APIs two clusters serve differ.
type Diff struct {
	A Side `json:"a"`
	B Side `json:"b"`
	// Resources are the resource types whose served versions, or whose
	// definition's storage version, differ.
	Resources []ResourceDiff `json:"resources"`
}

// Side is one of the clusters a Diff compares.
type Side struct {
	Cluster string `json:"cluster"`
	Server  Server `json:"server"`
}

// ResourceDiff is one resource type that two clusters serve differently.
type ResourceDiff struct {
	// Resource is the resource type, such as certificates.cert-manager.io.
	Resource string `json:"resource"`
	Kind     string `json:"kind"`
	A        Served `json:"a"`
	B        Served `json:"b"`
}

// Served is how one cluster serves a resource type.
type Served struct {
	// Versions are the versions it serves, most preferred first; none
	// when it does not serve the type.
	Versions []string `json:"versions"`
	// Storage is the version a custom resource's definition stores new
	// objects at.
	Storage string `json:"storage,omitempty"`
	// CRD is true when a custom resource definition defines the type.
	CRD bool `json:"crd,omitempty"`
}

// Compare lists the resource types a and b serve at different versions,
// or whose definitions store objects at different versions, in the order
// of their groups and names.
func Compare(a, b Snapshot) *Diff {
	d := &Diff{
		A:         Side{Cluster: a.Cluster, Server: a.APIs.Server},
		B:         Side{Cluster: b.Cluster, Server: b.APIs.Server},
		Resources: []ResourceDiff{},
	}
	type entry struct {
		group, name, kind string
		a, b              Served
	}
	entries := map[string]*entry{}
	add := func(s Snapshot, pick func(*entry) *Served) {
		storage := map[string]string{}
		for _, c := range s.CRDs {
			storage[c.Name] = c.Storage()
		}
		for _, r := range s.APIs.Resources {
			k := r.GroupResource()
			e := entries[k]
			if e == nil {
				e = &entry{group: r.Group, name: r.Name, kind: r.Kind}
				entries[k] = e
			}
			sv := pick(e)
			sv.Versions = sortVersions(r.Versions)
			sv.Storage, sv.CRD = storage[k]
		}
	}
	add(a, func(e *entry) *Served { return &e.a })
	add(b, func(e *entry) *Served { return &e.b })
	compareStorage := a.CRDs != nil && b.CRDs != nil
	for k, e := range entries {
		same := slices.Equal(e.a.Versions, e.b.Versions)
		if compareStorage && len(e.a.Versions) > 0 && len(e.b.Versions) > 0 {
			same = same && e.a.Storage == e.b.Storage
		}
		if same {
			continue
		}
		d.Resources = append(d.Resources, ResourceDiff{Resource: k, Kind: e.kind, A: e.a, B: e.b})
	}
	slices.SortFunc(d.Resources, func(x, y ResourceDiff) int {
		gx, nx := splitGroupResource(x.Resource)
		gy, ny := splitGroupResource(y.Resource)
		return cmp.Or(compareGroups(gx, gy), cmp.Compare(nx, ny))
	})
	return d
}

// sortVersions orders versions as Kubernetes prefers them: v2, v1,
// v1beta2, v1beta1, v1alpha1.
func sortVersions(vs []string) []string {
	out := slices.Clone(vs)
	slices.SortStableFunc(out, func(a, b string) int { return -version.CompareKubeAwareVersionStrings(a, b) })
	return out
}

// splitGroupResource splits name.group into its group and name.
func splitGroupResource(s string) (group, name string) {
	name, group, _ = strings.Cut(s, ".")
	return group, name
}
