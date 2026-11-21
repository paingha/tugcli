package bundle

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// resourceInfo is one resource the cluster serves, at its preferred
// version.
type resourceInfo struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	list       bool
	// names are what kubectl accepts for it, in lower case: plural,
	// singular, short names and kind.
	names []string
}

func (r resourceInfo) groupKind() schema.GroupKind {
	return schema.GroupKind{Group: r.gvr.Group, Kind: r.kind}
}

// index is what the cluster serves, from API discovery.
type index struct {
	resources []resourceInfo
}

// loadIndex reads the cluster's resources. A group whose discovery fails is
// returned in failed rather than failing the whole.
func loadIndex(ctx context.Context, d discovery.DiscoveryInterface) (*index, []string, error) {
	lists, err := discovery.ServerPreferredResourcesWithContext(ctx, discovery.ToDiscoveryInterfaceWithContext(d))
	var failed []string
	if err != nil {
		var partial *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &partial) {
			return nil, nil, fmt.Errorf("discovering the cluster's resources: %w", err)
		}
		for gv := range partial.Groups {
			failed = append(failed, gv.String())
		}
		slices.Sort(failed)
	}
	ix := &index{}
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue // a subresource, such as pods/log
			}
			names := []string{r.Name, strings.ToLower(r.Kind)}
			if r.SingularName != "" {
				names = append(names, r.SingularName)
			}
			for _, s := range r.ShortNames {
				names = append(names, strings.ToLower(s))
			}
			ix.resources = append(ix.resources, resourceInfo{
				gvr: gv.WithResource(r.Name), kind: r.Kind, namespaced: r.Namespaced,
				list: slices.Contains(r.Verbs, "list"), names: names,
			})
		}
	}
	slices.SortFunc(ix.resources, func(a, b resourceInfo) int {
		return cmp.Or(strings.Compare(a.gvr.Group, b.gvr.Group), strings.Compare(a.gvr.Resource, b.gvr.Resource))
	})
	return ix, failed, nil
}

// find returns the resource serving a group and resource.
func (ix *index) find(gr schema.GroupResource) (resourceInfo, bool) {
	for _, r := range ix.resources {
		if r.gvr.GroupResource() == gr {
			return r, true
		}
	}
	return resourceInfo{}, false
}

// ofKind returns the resources serving a kind: the core group's first, then
// prefer's, then the rest by group.
func (ix *index) ofKind(kind, prefer string) []resourceInfo {
	var out []resourceInfo
	for _, r := range ix.resources {
		if r.kind == kind {
			out = append(out, r)
		}
	}
	rank := func(r resourceInfo) int {
		switch r.gvr.Group {
		case "":
			return 0
		case prefer:
			return 1
		default:
			return 2
		}
	}
	slices.SortStableFunc(out, func(a, b resourceInfo) int { return cmp.Compare(rank(a), rank(b)) })
	return out
}

// match returns the kinds that a name, as kubectl accepts it, stands for:
// "deploy", "deployments.apps", "Certificate".
func (ix *index) match(name string) []schema.GroupKind {
	name = strings.ToLower(name)
	var out []schema.GroupKind
	for _, r := range ix.resources {
		for _, n := range r.names {
			if name == n || (r.gvr.Group != "" && name == n+"."+r.gvr.Group) {
				if gk := r.groupKind(); !slices.Contains(out, gk) {
					out = append(out, gk)
				}
				break
			}
		}
	}
	return out
}

// kindFilter is what --include and --exclude leave in a bundle.
type kindFilter struct {
	include, exclude map[schema.GroupKind]bool
}

func newKindFilter(ix *index, include, exclude []string) (kindFilter, error) {
	f := kindFilter{include: map[schema.GroupKind]bool{}, exclude: map[schema.GroupKind]bool{}}
	add := func(flag string, names []string, set map[schema.GroupKind]bool) error {
		for _, name := range names {
			gks := ix.match(name)
			if len(gks) == 0 {
				return fmt.Errorf("%s %q: the cluster has no such kind; list them with `tug api-resources`", flag, name)
			}
			for _, gk := range gks {
				set[gk] = true
			}
		}
		return nil
	}
	if err := add("--include", include, f.include); err != nil {
		return kindFilter{}, err
	}
	if err := add("--exclude", exclude, f.exclude); err != nil {
		return kindFilter{}, err
	}
	return f, nil
}

// keeps reports whether objects of a kind go in the bundle.
func (f kindFilter) keeps(gk schema.GroupKind) bool {
	if len(f.include) > 0 && !f.include[gk] {
		return false
	}
	return !f.exclude[gk]
}
