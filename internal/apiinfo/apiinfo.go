// Package apiinfo explains what a cluster's API serves: the resource types
// and group-versions its discovery reports, the custom resource definitions
// behind some of them, and how both line up with the API removals and
// support windows of upstream Kubernetes releases, which it keeps in an
// embedded table. It only reads, and never prints; the CLI renders what it
// returns.
package apiinfo

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
)

// Discovery is the part of a discovery client that apiinfo reads. The
// cached client from kube.Factory.DiscoveryFor is one.
type Discovery interface {
	ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
	ServerVersionWithContext(ctx context.Context) (*version.Info, error)
}

var _ Discovery = discovery.CachedDiscoveryInterfaceWithContext(nil)

// APIs is what a cluster serves, as its API discovery reports it.
type APIs struct {
	Server Server `json:"server"`
	// Groups are the API groups, the core group ("") first.
	Groups []Group `json:"groups"`
	// Resources are the resource types, without subresources, the core
	// group's first.
	Resources []Resource `json:"resources"`
	// Unavailable are the group-versions discovery could not read, such
	// as an aggregated API whose server is down.
	Unavailable []string `json:"unavailable,omitempty"`
}

// Group is an API group and the versions it serves.
type Group struct {
	// Name is "" for the core group.
	Name string `json:"name"`
	// Versions are in the server's order of preference.
	Versions  []string `json:"versions"`
	Preferred string   `json:"preferredVersion"`
}

// Resource is a resource type the cluster serves.
type Resource struct {
	// Name is the plural name, such as deployments.
	Name     string `json:"name"`
	Singular string `json:"singularName,omitempty"`
	// Group is "" for the core group.
	Group string `json:"group"`
	// Version is the version clients use by default: the group's
	// preferred version when it serves the resource, else the most
	// preferred one that does.
	Version string `json:"version"`
	// Versions are every version of the group that serves the resource,
	// most preferred first.
	Versions   []string `json:"versions"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	ShortNames []string `json:"shortNames,omitempty"`
	Verbs      []string `json:"verbs,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

// GroupResource names the resource as kubectl accepts it whatever else the
// cluster serves: deployments.apps, or pods for the core group.
func (r Resource) GroupResource() string { return qualify(r.Name, r.Group) }

// GroupVersion returns the resource's default group-version, such as
// apps/v1.
func (r Resource) GroupVersion() string { return GroupVersion(r.Group, r.Version) }

// Can reports whether the resource supports a verb, such as list.
func (r Resource) Can(verb string) bool { return slices.Contains(r.Verbs, verb) }

// GroupVersion joins a group and a version as the API writes them: apps/v1,
// or v1 for the core group.
func GroupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// qualify writes name.group, or name alone for the core group.
func qualify(name, group string) string {
	if group == "" {
		return name
	}
	return name + "." + group
}

// Discover reads what the cluster serves. A group-version that discovery
// cannot read is listed in Unavailable rather than failing the whole read.
func Discover(ctx context.Context, dc Discovery) (*APIs, error) {
	info, err := dc.ServerVersionWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the server's version: %w", err)
	}
	groups, lists, err := dc.ServerGroupsAndResourcesWithContext(ctx)
	var unavailable []string
	if failed, ok := discovery.GroupDiscoveryFailedErrorGroups(err); ok {
		for gv := range failed {
			unavailable = append(unavailable, gv.String())
		}
		slices.Sort(unavailable)
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading API discovery: %w", err)
	}
	if len(groups) == 0 {
		return nil, errors.New("API discovery listed no API groups")
	}
	apis := &APIs{Server: ServerOf(info), Unavailable: unavailable}
	byGV := make(map[string]*metav1.APIResourceList, len(lists))
	for _, l := range lists {
		if l != nil {
			byGV[l.GroupVersion] = l
		}
	}
	for _, g := range groups {
		if g == nil {
			continue
		}
		group := Group{Name: g.Name, Preferred: g.PreferredVersion.Version}
		for _, v := range g.Versions {
			group.Versions = append(group.Versions, v.Version)
		}
		apis.Groups = append(apis.Groups, group)
		apis.Resources = append(apis.Resources, groupResources(group, byGV)...)
	}
	slices.SortFunc(apis.Groups, func(a, b Group) int { return compareGroups(a.Name, b.Name) })
	slices.SortFunc(apis.Resources, func(a, b Resource) int {
		return cmp.Or(compareGroups(a.Group, b.Group), cmp.Compare(a.Name, b.Name))
	})
	return apis, nil
}

// groupResources merges the resources a group serves at each of its
// versions into one per resource type.
func groupResources(g Group, byGV map[string]*metav1.APIResourceList) []Resource {
	var out []Resource
	index := map[string]int{}
	for _, v := range g.Versions {
		list := byGV[GroupVersion(g.Name, v)]
		if list == nil {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") {
				continue // a subresource, such as pods/log
			}
			if i, ok := index[r.Name]; ok {
				out[i].Versions = append(out[i].Versions, v)
				continue
			}
			index[r.Name] = len(out)
			out = append(out, Resource{
				Name:       r.Name,
				Singular:   r.SingularName,
				Group:      g.Name,
				Versions:   []string{v},
				Kind:       r.Kind,
				Namespaced: r.Namespaced,
				ShortNames: r.ShortNames,
				Verbs:      r.Verbs,
				Categories: r.Categories,
			})
		}
	}
	for i := range out {
		out[i].Version = out[i].Versions[0]
		if slices.Contains(out[i].Versions, g.Preferred) {
			out[i].Version = g.Preferred
		}
	}
	return out
}

// compareGroups sorts the core group first, then groups by name.
func compareGroups(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return -1
	case b == "":
		return 1
	}
	return cmp.Compare(a, b)
}

// Resource returns the resource type of a group with a plural name.
func (a *APIs) Resource(group, name string) (Resource, bool) {
	for _, r := range a.Resources {
		if r.Group == group && r.Name == name {
			return r, true
		}
	}
	return Resource{}, false
}

// Scope narrows resources to namespaced or cluster-scoped ones.
type Scope string

// Scopes.
const (
	AnyScope   Scope = ""
	Namespaced Scope = "namespaced"
	Cluster    Scope = "cluster"
)

// ParseScope reads a scope as the --scope flag takes it.
func ParseScope(s string) (Scope, error) {
	if sc := Scope(strings.ToLower(s)); sc == AnyScope || sc == Namespaced || sc == Cluster {
		return sc, nil
	}
	return "", fmt.Errorf("unknown scope %q (want namespaced or cluster)", s)
}

// CoreGroup is how a Query names the core group.
const CoreGroup = "core"

// Query narrows a list of resource types. The zero Query matches them all.
type Query struct {
	// Text matches names, singular and short names, kinds, groups and
	// categories, ignoring case: exact matches first, then those starting
	// with it, then those containing it.
	Text string
	// Group is an API group, or CoreGroup.
	Group string
	// Verbs are verbs a resource must all support, such as list.
	Verbs []string
	Scope Scope
}

// Find returns the resource types that match q, best matches first and
// otherwise in the order of a.Resources.
func (a *APIs) Find(q Query) []Resource {
	type scored struct {
		r     Resource
		score int
	}
	var hits []scored
	text := strings.ToLower(strings.TrimSpace(q.Text))
	for _, r := range a.Resources {
		if !q.admits(r) {
			continue
		}
		score := 0
		if text != "" {
			if score = matchScore(text, r); score == 0 {
				continue
			}
		}
		hits = append(hits, scored{r, score})
	}
	slices.SortStableFunc(hits, func(x, y scored) int { return cmp.Compare(y.score, x.score) })
	out := make([]Resource, len(hits))
	for i, h := range hits {
		out[i] = h.r
	}
	return out
}

func (q Query) admits(r Resource) bool {
	switch {
	case q.Group == CoreGroup && r.Group != "",
		q.Group != "" && q.Group != CoreGroup && !strings.EqualFold(q.Group, r.Group),
		q.Scope == Namespaced && !r.Namespaced,
		q.Scope == Cluster && r.Namespaced:
		return false
	}
	for _, v := range q.Verbs {
		if !r.Can(strings.ToLower(v)) {
			return false
		}
	}
	return true
}

// Match scores: how well a query matches a resource's best name.
const (
	matchContains = 1 + iota
	matchPrefix
	matchExact
)

func matchScore(text string, r Resource) int {
	names := append([]string{r.Name, r.Singular, r.Kind, r.Group, r.GroupResource(), qualify(r.Kind, r.Group)}, r.ShortNames...)
	names = append(names, r.Categories...)
	best := 0
	for _, n := range names {
		n = strings.ToLower(n)
		switch {
		case n == "":
		case n == text:
			return matchExact
		case strings.HasPrefix(n, text):
			best = max(best, matchPrefix)
		case strings.Contains(n, text):
			best = max(best, matchContains)
		}
	}
	return best
}

// GroupVersionInfo is a group-version the cluster serves.
type GroupVersionInfo struct {
	GroupVersion string `json:"groupVersion"`
	Group        string `json:"group"`
	Version      string `json:"version"`
	// Preferred is true for the version clients use by default.
	Preferred bool `json:"preferred"`
	// Resources counts the resource types it serves.
	Resources int `json:"resources"`
	// RemovedIn is the first upstream release that stops serving it for
	// any of the kinds it serves here, when one does.
	RemovedIn string `json:"removedIn,omitempty"`
}

// GroupVersions lists every group-version the cluster serves, the core
// group first and each group's versions in order of preference, with the
// upstream release that removes it, from t.
func (a *APIs) GroupVersions(t *Table) []GroupVersionInfo {
	var out []GroupVersionInfo
	for _, g := range a.Groups {
		for _, v := range g.Versions {
			gv := GroupVersionInfo{GroupVersion: GroupVersion(g.Name, v), Group: g.Name, Version: v, Preferred: v == g.Preferred}
			for _, r := range a.Resources {
				if r.Group != g.Name || !slices.Contains(r.Versions, v) {
					continue
				}
				gv.Resources++
				if rm, ok := t.Removal(gv.GroupVersion, r.Kind); ok && (gv.RemovedIn == "" || compareMinor(rm.RemovedIn, gv.RemovedIn) < 0) {
					gv.RemovedIn = rm.RemovedIn
				}
			}
			out = append(out, gv)
		}
	}
	return out
}
