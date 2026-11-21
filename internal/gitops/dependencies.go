package gitops

import (
	"github.com/paingha/tugcli/internal/ops"
)

// DependencyNode is one object in a dependency graph, with its readiness
// and the objects beneath it.
type DependencyNode struct {
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Namespace string     `json:"namespace,omitempty"`
	Ready     ops.Health `json:"ready"`
	Status    string     `json:"status"`
	// Missing is true for a dependency that is not in the cluster.
	Missing bool `json:"missing,omitempty"`
	// Cycle is true for an object that is already above it in the graph:
	// the objects depend on each other, which Flux never resolves. What
	// is beneath it is not shown again.
	Cycle    bool             `json:"cycle,omitempty"`
	Children []DependencyNode `json:"children,omitempty"`
}

// Dependencies is an object's dependency graph both ways.
type Dependencies struct {
	// DependsOn is the object with what it depends on beneath it through
	// spec.dependsOn, and what those depend on in turn. An Argo CD
	// Application has no dependsOn; beneath one are the Applications it
	// manages, as an app of apps does.
	DependsOn DependencyNode `json:"dependsOn"`
	// RequiredBy is the object with what depends on it beneath, and in
	// turn.
	RequiredBy DependencyNode `json:"requiredBy"`
	// Note says how to read the graph for a tool, when it needs saying.
	Note string `json:"note,omitempty"`
}

// maxDependencyDepth bounds how deep a dependency graph goes.
const maxDependencyDepth = 32

// Dependencies returns a Kustomization's, HelmRelease's or Application's
// dependency graph, with each object's readiness.
func (s *Snapshot) Dependencies(o *Object) *Dependencies {
	requiredBy := map[key][]*Object{}
	for _, other := range s.objects {
		for _, r := range s.edges(other) {
			requiredBy[keyOf(r)] = append(requiredBy[keyOf(r)], other)
		}
	}
	d := &Dependencies{
		DependsOn: s.dependencyTree(o.Ref(), map[key]bool{}, 0, s.edges),
		RequiredBy: s.dependencyTree(o.Ref(), map[key]bool{}, 0, func(n *Object) []Ref {
			var refs []Ref
			for _, by := range requiredBy[keyOf(n.Ref())] {
				refs = append(refs, by.Ref())
			}
			return refs
		}),
	}
	if o.Tool == ArgoCD {
		d.Note = "Argo CD Applications have no dependsOn: shown are the Applications it manages, and those that manage it, as an app of apps does; sync waves order objects within one Application"
	}
	return d
}

// edges returns what an object depends on: its spec.dependsOn, or for an
// Application, the Applications it manages.
func (s *Snapshot) edges(o *Object) []Ref {
	if o.Tool != ArgoCD {
		return o.DependsOn
	}
	var out []Ref
	for _, r := range argoResources(o.u) {
		if r.Kind == KindApplication && r.Group == argoGroup {
			out = append(out, Ref{Kind: KindApplication, Name: r.Name, Namespace: r.Namespace})
		}
	}
	return out
}

// dependencyTree builds the graph beneath r, following next, and marks an
// object that is already on the path to it as a cycle.
func (s *Snapshot) dependencyTree(r Ref, path map[key]bool, depth int, next func(*Object) []Ref) DependencyNode {
	n := DependencyNode{Kind: r.Kind, Name: r.Name, Namespace: r.Namespace}
	o := s.get(r)
	if o == nil {
		n.Ready, n.Status, n.Missing = ops.HealthNotReady, "not in the cluster", true
		return n
	}
	n.Ready, n.Status = o.Ready, statusText(o)
	k := keyOf(r)
	if path[k] {
		n.Cycle = true
		return n
	}
	if depth >= maxDependencyDepth {
		return n
	}
	path[k] = true
	defer delete(path, k)
	for _, child := range next(o) {
		n.Children = append(n.Children, s.dependencyTree(child, path, depth+1, next))
	}
	return n
}
