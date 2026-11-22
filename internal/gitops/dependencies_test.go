package gitops

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// treeLines draws a dependency tree as indented lines with each node's
// readiness.
func treeLines(n DependencyNode, indent string) []string {
	line := fmt.Sprintf("%s%s/%s %s", indent, strings.ToLower(n.Kind), n.Name, n.Ready)
	if n.Missing {
		line += " missing"
	}
	if n.Cycle {
		line += " cycle"
	}
	out := []string{line}
	for _, c := range n.Children {
		out = append(out, treeLines(c, indent+"  ")...)
	}
	return out
}

func TestDependencies(t *testing.T) {
	s := load(t, shopCluster(t))
	for _, tt := range []struct {
		kind, ns, name        string
		dependsOn, requiredBy string
	}{
		{KindKustomization, "flux-system", "tenants",
			"kustomization/tenants not-ready\n" +
				"  kustomization/apps not-ready\n" +
				"    kustomization/infra-configs not-ready\n" +
				"      kustomization/infra-controllers ready\n" +
				"  kustomization/crds not-ready missing",
			"kustomization/tenants not-ready"},
		{KindKustomization, "flux-system", "infra-controllers",
			"kustomization/infra-controllers ready",
			"kustomization/infra-controllers ready\n" +
				"  kustomization/infra-configs not-ready\n" +
				"    kustomization/apps not-ready\n" +
				"      kustomization/tenants not-ready"},
		{KindApplication, "argocd", "platform",
			"application/platform ready\n" +
				"  application/guestbook not-ready\n" +
				"  application/payments not-ready",
			"application/platform ready"},
		{KindApplication, "argocd", "guestbook",
			"application/guestbook not-ready",
			"application/guestbook not-ready\n" +
				"  application/platform ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := s.Dependencies(find(t, s, tt.kind, tt.ns, tt.name))
			if got := strings.Join(treeLines(d.DependsOn, ""), "\n"); got != tt.dependsOn {
				t.Errorf("DependsOn =\n%s\nwant\n%s", got, tt.dependsOn)
			}
			if got := strings.Join(treeLines(d.RequiredBy, ""), "\n"); got != tt.requiredBy {
				t.Errorf("RequiredBy =\n%s\nwant\n%s", got, tt.requiredBy)
			}
			if (tt.kind == KindApplication) != (d.Note != "") {
				t.Errorf("Note = %q; only Applications, which have no dependsOn, need one", d.Note)
			}
		})
	}
}

func TestDependencyCycle(t *testing.T) {
	ks := func(name, dep string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"name": name, "namespace": "flux-system", "uid": "ks-" + name},
			"spec":     map[string]any{"dependsOn": []any{map[string]any{"name": dep}}},
			"status": map[string]any{"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "DependencyNotReady", "message": "dependency 'flux-system/" + dep + "' is not ready",
			}}},
		}}
	}
	s := load(t, fakeCluster(t, allResources(), nil, []*unstructured.Unstructured{ks("a", "b"), ks("b", "a")}))
	d := s.Dependencies(find(t, s, KindKustomization, "flux-system", "a"))
	want := "kustomization/a not-ready\n  kustomization/b not-ready\n    kustomization/a not-ready cycle"
	if got := strings.Join(treeLines(d.DependsOn, ""), "\n"); got != want {
		t.Errorf("DependsOn of a cycle =\n%s\nwant\n%s", got, want)
	}
	if roots := s.rootCauses(find(t, s, KindKustomization, "flux-system", "b")); len(roots) != 0 {
		t.Errorf("rootCauses in a cycle = %v, want none, as nothing outside it is to blame", roots)
	}
}
