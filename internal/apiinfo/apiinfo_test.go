package apiinfo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

// apiResources builds one group-version's discovery, every resource
// namespaced with the given verbs unless it says otherwise.
func apiResources(gv string, rs ...metav1.APIResource) *metav1.APIResourceList {
	for i := range rs {
		if rs[i].SingularName == "" {
			rs[i].SingularName = strings.ToLower(rs[i].Kind)
		}
		if rs[i].Verbs == nil {
			rs[i].Verbs = metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete"}
		}
	}
	return &metav1.APIResourceList{GroupVersion: gv, APIResources: rs}
}

// fakeCluster is the discovery of a 1.24 cluster that still serves
// deprecated versions, with one custom resource. The fake client takes a
// group's first group-version as its preferred one.
func fakeCluster() *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{
		Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
			apiResources("v1",
				metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Categories: []string{"all"}},
				metav1.APIResource{Name: "pods/log", Kind: "Pod", Namespaced: true},
				metav1.APIResource{Name: "namespaces", Kind: "Namespace", ShortNames: []string{"ns"}},
				metav1.APIResource{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: metav1.Verbs{"create"}}),
			apiResources("apps/v1",
				metav1.APIResource{Name: "deployments", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Categories: []string{"all"}}),
			apiResources("autoscaling/v2",
				metav1.APIResource{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, ShortNames: []string{"hpa"}}),
			apiResources("autoscaling/v1",
				metav1.APIResource{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, ShortNames: []string{"hpa"}}),
			apiResources("autoscaling/v2beta2",
				metav1.APIResource{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, ShortNames: []string{"hpa"}}),
			apiResources("policy/v1",
				metav1.APIResource{Name: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Namespaced: true, ShortNames: []string{"pdb"}}),
			apiResources("policy/v1beta1",
				metav1.APIResource{Name: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Namespaced: true, ShortNames: []string{"pdb"}},
				metav1.APIResource{Name: "podsecuritypolicies", Kind: "PodSecurityPolicy", ShortNames: []string{"psp"}}),
			apiResources("example.com/v1",
				metav1.APIResource{Name: "widgets", Kind: "Widget", Namespaced: true, ShortNames: []string{"wd"}, Categories: []string{"all", "toys"}}),
			apiResources("example.com/v1beta1",
				metav1.APIResource{Name: "widgets", Kind: "Widget", Namespaced: true, ShortNames: []string{"wd"}, Categories: []string{"all", "toys"}}),
		}},
		FakedServerVersion: &version.Info{Major: "1", Minor: "24", GitVersion: "v1.24.17"},
	}
}

func discover(t *testing.T, dc Discovery) *APIs {
	t.Helper()
	apis, err := Discover(context.Background(), dc)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return apis
}

func names(rs []Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.GroupResource()
	}
	return out
}

func TestDiscover(t *testing.T) {
	apis := discover(t, fakeCluster())
	if apis.Server.Minor != "1.24" || apis.Server.GitVersion != "v1.24.17" {
		t.Errorf("Server = %+v, want 1.24", apis.Server)
	}
	want := []string{"bindings", "namespaces", "pods", "deployments.apps", "horizontalpodautoscalers.autoscaling",
		"widgets.example.com", "poddisruptionbudgets.policy", "podsecuritypolicies.policy"}
	if got := names(apis.Resources); !slices.Equal(got, want) {
		t.Errorf("Resources = %v, want %v (the core group first, no subresources)", got, want)
	}
	hpa, ok := apis.Resource("autoscaling", "horizontalpodautoscalers")
	if !ok || hpa.Version != "v2" || !slices.Equal(hpa.Versions, []string{"v2", "v1", "v2beta2"}) || hpa.GroupVersion() != "autoscaling/v2" {
		t.Errorf("Resource(autoscaling, horizontalpodautoscalers) = %+v, want v2 preferred of v2, v1, v2beta2", hpa)
	}
	if ns, _ := apis.Resource("", "namespaces"); ns.Namespaced || ns.GroupVersion() != "v1" || !ns.Can("list") {
		t.Errorf("namespaces = %+v, want cluster-scoped in v1 and listable", ns)
	}
	if apis.Groups[0].Name != "" || apis.Groups[0].Preferred != "v1" {
		t.Errorf("Groups[0] = %+v, want the core group", apis.Groups[0])
	}
}

// partialDiscovery fails to read some group-versions, as discovery does
// when an aggregated API's server is down.
type partialDiscovery struct{ *discoveryfake.FakeDiscovery }

func (p partialDiscovery) ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	groups, lists, _ := p.FakeDiscovery.ServerGroupsAndResourcesWithContext(ctx)
	return groups, lists, &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("the server is currently unable to handle the request"),
	}}
}

func TestDiscoverPartial(t *testing.T) {
	apis := discover(t, partialDiscovery{fakeCluster()})
	if !slices.Equal(apis.Unavailable, []string{"metrics.k8s.io/v1beta1"}) || len(apis.Resources) == 0 {
		t.Errorf("Discover with a failing group = unavailable %v and %d resources, want metrics.k8s.io/v1beta1 and the rest", apis.Unavailable, len(apis.Resources))
	}
}

func TestDiscoverFails(t *testing.T) {
	dc := fakeCluster()
	dc.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if _, err := Discover(context.Background(), dc); err == nil || !strings.Contains(err.Error(), "reading the server's version: connection refused") {
		t.Errorf("Discover of an unreachable server = %v", err)
	}
}

func TestFind(t *testing.T) {
	apis := discover(t, fakeCluster())
	tests := []struct {
		name string
		q    Query
		want []string
	}{
		{"everything", Query{}, names(apis.Resources)},
		{"short name first", Query{Text: "hpa"}, []string{"horizontalpodautoscalers.autoscaling"}},
		{"exact before prefix", Query{Text: "pod"}, []string{"pods", "poddisruptionbudgets.policy", "podsecuritypolicies.policy", "horizontalpodautoscalers.autoscaling"}},
		{"kind ignores case", Query{Text: "deployment"}, []string{"deployments.apps"}},
		{"group", Query{Text: "example"}, []string{"widgets.example.com"}},
		{"category", Query{Text: "toys"}, []string{"widgets.example.com"}},
		{"qualified name", Query{Text: "Widgets.example.com"}, []string{"widgets.example.com"}},
		{"core group", Query{Group: CoreGroup}, []string{"bindings", "namespaces", "pods"}},
		{"named group", Query{Group: "policy"}, []string{"poddisruptionbudgets.policy", "podsecuritypolicies.policy"}},
		{"cluster scope", Query{Scope: Cluster}, []string{"namespaces", "podsecuritypolicies.policy"}},
		{"verbs", Query{Group: CoreGroup, Verbs: []string{"List"}}, []string{"namespaces", "pods"}},
		{"nothing", Query{Text: "gadget"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(apis.Find(tt.q)); !slices.Equal(got, tt.want) {
				t.Errorf("Find(%+v) = %v, want %v", tt.q, got, tt.want)
			}
		})
	}
}

func TestParseScope(t *testing.T) {
	for in, want := range map[string]Scope{"": AnyScope, "Namespaced": Namespaced, "cluster": Cluster} {
		if got, err := ParseScope(in); err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := ParseScope("global"); err == nil {
		t.Error("ParseScope(global) = nil error, want one")
	}
}

func TestGroupVersions(t *testing.T) {
	apis := discover(t, fakeCluster())
	got := map[string]GroupVersionInfo{}
	var order []string
	for _, gv := range apis.GroupVersions(Upstream()) {
		got[gv.GroupVersion] = gv
		order = append(order, gv.GroupVersion)
	}
	if want := []string{"v1", "apps/v1", "autoscaling/v2", "autoscaling/v1", "autoscaling/v2beta2", "example.com/v1", "example.com/v1beta1", "policy/v1", "policy/v1beta1"}; !slices.Equal(order, want) {
		t.Errorf("GroupVersions order = %v, want %v", order, want)
	}
	for gv, want := range map[string]GroupVersionInfo{
		"v1":                  {GroupVersion: "v1", Version: "v1", Preferred: true, Resources: 3},
		"autoscaling/v2beta2": {GroupVersion: "autoscaling/v2beta2", Group: "autoscaling", Version: "v2beta2", Resources: 1, RemovedIn: "1.26"},
		"policy/v1beta1":      {GroupVersion: "policy/v1beta1", Group: "policy", Version: "v1beta1", Resources: 2, RemovedIn: "1.25"},
		"example.com/v1":      {GroupVersion: "example.com/v1", Group: "example.com", Version: "v1", Preferred: true, Resources: 1},
	} {
		if got[gv] != want {
			t.Errorf("GroupVersions()[%s] = %+v, want %+v", gv, got[gv], want)
		}
	}
}
