package gitops

import (
	"fmt"
	"slices"
	"strings"

	"github.com/paingha/tugcli/internal/resolve"
)

// Report is what Status found.
type Report struct {
	// Tools are the GitOps tools the cluster serves.
	Tools []Tool `json:"tools"`
	// Namespace is the namespace the report is about; "" for all of them.
	Namespace string    `json:"namespace,omitempty"`
	Objects   []*Object `json:"objects"`
	// Unlisted names the resources that could not be listed, whose objects
	// are missing.
	Unlisted []string `json:"unlisted,omitempty"`
}

// Status returns the Flux sources, Kustomizations and HelmReleases and the
// Argo CD Applications that concern a namespace, or every one for "". One
// concerns a namespace when it is in it, deploys into it, or manages
// objects there, and a source does when one of those applies it.
func (s *Snapshot) Status(namespace string) *Report {
	r := &Report{Tools: s.Tools, Namespace: namespace, Objects: []*Object{}, Unlisted: s.Unlisted}
	wanted := map[*Object]bool{}
	for _, o := range s.objects {
		if delivers(o.Kind) && (namespace == "" || o.Namespace == namespace || touches(o, namespace)) {
			wanted[o] = true
			// The sources it applies, and theirs: a HelmRelease's
			// HelmChart and the HelmChart's repository.
			for src, seen := o.Source, 0; src != nil && seen < 3; seen++ {
				so := s.get(*src)
				if so == nil {
					break
				}
				wanted[so] = true
				src = so.Source
			}
		}
	}
	for _, o := range s.objects {
		if !o.k.listed {
			continue
		}
		if wanted[o] || namespace == "" || (o.k.source && o.Namespace == namespace) {
			r.Objects = append(r.Objects, o)
		}
	}
	return r
}

// touches reports whether an object deploys into a namespace or manages
// objects there.
func touches(o *Object, ns string) bool {
	switch o.Kind {
	case KindKustomization, KindHelmRelease:
		if o.remote() {
			return false
		}
		if text(o.u.Object, "spec", "targetNamespace") == ns {
			return true
		}
		return slices.ContainsFunc(inventory(o.u), func(r objectRef) bool { return r.Namespace == ns })
	case KindApplication:
		if !o.inCluster() {
			return false
		}
		if o.Destination != nil && o.Destination.Namespace == ns {
			return true
		}
		return slices.ContainsFunc(argoResources(o.u), func(r argoResource) bool { return r.Namespace == ns })
	}
	return false
}

// Match returns the Kustomizations, HelmReleases and Applications a query
// may mean, best matches first: a name as tug matches names, optionally
// after a kind as kubectl names it ("ks/apps", "helmrelease/podinfo",
// "applications.argoproj.io/guestbook"). With a namespace, only objects in
// it match.
func (s *Snapshot) Match(query, namespace string) ([]*Object, error) {
	kindName, name, err := parseQuery(query)
	if err != nil {
		return nil, err
	}
	var candidates []*Object
	var names []string
	for _, o := range s.objects {
		if !delivers(o.Kind) || (kindName != "" && o.Kind != kindName) || (namespace != "" && o.Namespace != namespace) {
			continue
		}
		candidates = append(candidates, o)
		if !slices.Contains(names, o.Name) {
			names = append(names, o.Name)
		}
	}
	var out []*Object
	for _, n := range resolve.Best(name, names) {
		for _, o := range candidates {
			if o.Name == n {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

// parseQuery splits "kind/name" into the kind it names and the name; a bare
// name has no kind.
func parseQuery(q string) (string, string, error) {
	resource, name, ok := strings.Cut(q, "/")
	if !ok {
		return "", q, nil
	}
	if name == "" {
		return "", "", fmt.Errorf("%q names no object", q)
	}
	resource = strings.ToLower(resource)
	// A group, and a version before it, may follow the resource:
	// kustomizations.v1.kustomize.toolkit.fluxcd.io.
	base, group, _ := strings.Cut(resource, ".")
	for _, k := range kinds {
		if !delivers(k.name) {
			continue
		}
		if group != "" && group != k.group && !strings.HasSuffix(group, "."+k.group) {
			continue
		}
		if base == strings.ToLower(k.name) || base == k.resource || slices.Contains(k.aliases, base) {
			return k.name, name, nil
		}
	}
	return "", "", fmt.Errorf("%q is not a Kustomization, HelmRelease or Application; name one as ks/<name>, hr/<name> or app/<name>", resource)
}
