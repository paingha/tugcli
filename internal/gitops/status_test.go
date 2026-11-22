package gitops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/ops"
)

func names(objs []*Object) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Namespace + "/" + o.String()
	}
	return out
}

func TestLoadFindsEachToolWhereItIsServed(t *testing.T) {
	c := shopCluster(t)
	s := load(t, c)
	if !slices.Equal(s.Tools, []Tool{Flux, ArgoCD}) {
		t.Errorf("Tools = %v, want [flux argocd]", s.Tools)
	}
	if len(s.Unlisted) != 0 || s.Namespace != "" {
		t.Errorf("Unlisted = %v, Namespace = %q, want everything listed", s.Unlisted, s.Namespace)
	}
	// OCIRepository is only at v1beta2, after the preferred v1.
	var listed []schema.GroupVersionResource
	for _, a := range c.dyn.Actions() {
		if a.GetVerb() == "list" {
			listed = append(listed, a.GetResource())
		}
	}
	for _, want := range []schema.GroupVersionResource{
		{Group: fluxSourceGroup, Version: "v1", Resource: "gitrepositories"},
		{Group: fluxSourceGroup, Version: "v1beta2", Resource: "ocirepositories"},
		{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"},
	} {
		if !slices.Contains(listed, want) {
			t.Errorf("Load listed %v, want %v among them", listed, want)
		}
	}
	if slices.Contains(listed, schema.GroupVersionResource{Group: fluxSourceGroup, Version: "v1beta2", Resource: "gitrepositories"}) {
		t.Errorf("Load listed GitRepositories at v1beta2 as well as at the preferred v1")
	}
}

func TestLoadSkipsAToolTheClusterDoesNotServe(t *testing.T) {
	resources := append(append([]*metav1.APIResourceList{}, builtinResources...), argoResourcesServed...)
	c := fakeCluster(t, resources, nil, fixtures(t, "argocd.yaml"))
	s := load(t, c)
	if !slices.Equal(s.Tools, []Tool{ArgoCD}) {
		t.Errorf("Tools = %v, want [argocd]", s.Tools)
	}
	if got := len(s.Status("").Objects); got != 5 {
		t.Errorf("Status lists %d objects, want the 5 Applications", got)
	}

	c = fakeCluster(t, builtinResources, nil, nil)
	if _, err := Load(context.Background(), c.c, ""); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Load without Flux or Argo CD = %v, want ErrNotInstalled", err)
	}
}

func TestLoadFallsBackToTheNamespace(t *testing.T) {
	c := shopCluster(t)
	c.dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "" {
			return true, nil, forbidden(a.GetResource())
		}
		return false, nil, nil
	})
	s, err := Load(context.Background(), c.c, "argocd")
	if err != nil {
		t.Fatal(err)
	}
	if s.Namespace != "argocd" {
		t.Errorf("Namespace = %q, want argocd, the only one it could list", s.Namespace)
	}
	if got := names(s.Status("").Objects); len(got) != 5 || !strings.HasPrefix(got[0], "argocd/") {
		t.Errorf("Status = %v, want the 5 Applications in argocd", got)
	}
}

func forbidden(r schema.GroupVersionResource) error {
	return apierrors.NewForbidden(r.GroupResource(), "", errors.New("not in every namespace"))
}

func TestParseFlux(t *testing.T) {
	s := load(t, shopCluster(t))
	for _, tt := range []struct {
		kind, ns, name string
		want           Object
	}{
		{KindGitRepository, "flux-system", "flux-system", Object{
			Ready: ops.HealthReady, Reason: "Succeeded", Revision: "main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Interval: "1m0s",
			LastReconcile: time.Date(2026, 11, 21, 11, 58, 0, 0, time.UTC),
		}},
		{KindGitRepository, "flux-system", "team", Object{Ready: ops.HealthNotReady, Reason: "GitOperationFailed", Interval: "5m0s",
			LastReconcile: time.Date(2026, 11, 21, 11, 40, 0, 0, time.UTC)}},
		{KindHelmRepository, "flux-system", "ghcr", Object{Ready: ops.HealthNone}},
		{KindKustomization, "flux-system", "apps", Object{
			Ready: ops.HealthNotReady, Reason: "DependencyNotReady", Interval: "10m0s",
			Source:   &Ref{Kind: KindGitRepository, Name: "flux-system", Namespace: "flux-system"},
			Revision: "main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Applied: "main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e",
			Attempted:     "main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e",
			DependsOn:     []Ref{{Kind: KindKustomization, Name: "infra-configs", Namespace: "flux-system"}},
			LastReconcile: time.Date(2026, 11, 21, 11, 59, 10, 0, time.UTC),
		}},
		{KindKustomization, "flux-system", "monitoring", Object{Ready: ops.HealthUnknown, Reason: "ReconciliationSucceeded", Interval: "30m0s",
			Revision: "main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Applied: "main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"}},
		{KindKustomization, "flux-system", "legacy", Object{Ready: ops.HealthReady, Suspended: true, Reason: "ReconciliationSucceeded", Interval: "10m0s",
			Revision: "main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", Applied: "main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"}},
		{KindHelmRelease, "shop", "podinfo", Object{
			Ready: ops.HealthNotReady, Reason: "UpgradeFailed", Interval: "10m0s",
			Source:   &Ref{Kind: KindHelmChart, Name: "shop-podinfo", Namespace: "flux-system"},
			Revision: "6.7.1", Applied: "6.7.0", Attempted: "6.7.1",
			LastReconcile: time.Date(2026, 11, 21, 11, 45, 0, 0, time.UTC),
		}},
		{KindHelmRelease, "shop", "redis", Object{
			Ready: ops.HealthNotReady, Reason: "ArtifactFailed", Interval: "10m0s",
			Source: &Ref{Kind: KindHelmChart, Name: "shop-redis", Namespace: "flux-system"},
		}},
	} {
		t.Run(tt.kind+"/"+tt.name, func(t *testing.T) {
			o := find(t, s, tt.kind, tt.ns, tt.name)
			w := tt.want
			if o.Tool != Flux || o.Ready != w.Ready || o.Reason != w.Reason || o.Suspended != w.Suspended || o.Interval != w.Interval {
				t.Errorf("tool %s, ready %s, reason %q, suspended %v, interval %q; want flux, %s, %q, %v, %q",
					o.Tool, o.Ready, o.Reason, o.Suspended, o.Interval, w.Ready, w.Reason, w.Suspended, w.Interval)
			}
			if o.Revision != w.Revision || (w.Applied != "" && o.Applied != w.Applied) || (w.Attempted != "" && o.Attempted != w.Attempted) {
				t.Errorf("revision %q, applied %q, attempted %q; want %q, %q, %q", o.Revision, o.Applied, o.Attempted, w.Revision, w.Applied, w.Attempted)
			}
			if w.Source != nil && (o.Source == nil || *o.Source != *w.Source) {
				t.Errorf("source = %v, want %v", o.Source, w.Source)
			}
			if w.DependsOn != nil && !slices.Equal(o.DependsOn, w.DependsOn) {
				t.Errorf("dependsOn = %v, want %v", o.DependsOn, w.DependsOn)
			}
			if !w.LastReconcile.IsZero() && !o.LastReconcile.Equal(w.LastReconcile) {
				t.Errorf("last reconcile = %v, want %v", o.LastReconcile, w.LastReconcile)
			}
		})
	}
}

func TestParseApplication(t *testing.T) {
	s := load(t, shopCluster(t))
	for _, tt := range []struct {
		name                string
		ready               ops.Health
		reason, sync, healt string
		revision, applied   string
	}{
		{"guestbook", ops.HealthNotReady, "Degraded", "OutOfSync", "Degraded", "3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a", "3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"},
		{"payments", ops.HealthNotReady, "ComparisonError", "Unknown", "Healthy", "", ""},
		{"ledger", ops.HealthNotReady, "SyncFailed", "OutOfSync", "Healthy", "7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b1a0f9e8d", ""},
		{"platform", ops.HealthReady, "", "Synced", "Healthy", "5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b", "5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b"},
	} {
		o := find(t, s, KindApplication, "argocd", tt.name)
		if o.Tool != ArgoCD || o.Ready != tt.ready || o.Reason != tt.reason || o.Sync != tt.sync || o.Health != tt.healt ||
			o.Revision != tt.revision || o.Applied != tt.applied {
			t.Errorf("application %s = tool %s, ready %s, reason %q, sync %q, health %q, revision %q, applied %q; want argocd, %s, %q, %q, %q, %q, %q",
				tt.name, o.Tool, o.Ready, o.Reason, o.Sync, o.Health, o.Revision, o.Applied, tt.ready, tt.reason, tt.sync, tt.healt, tt.revision, tt.applied)
		}
	}
	g := find(t, s, KindApplication, "argocd", "guestbook")
	if g.Repo != "https://github.com/example/apps.git" || !g.inCluster() || !g.LastReconcile.Equal(time.Date(2026, 11, 21, 11, 58, 0, 0, time.UTC)) {
		t.Errorf("guestbook: repo %q, in cluster %v, reconciled %v", g.Repo, g.inCluster(), g.LastReconcile)
	}
	if e := find(t, s, KindApplication, "argocd", "edge-shop"); e.inCluster() {
		t.Errorf("edge-shop deploys to %+v, which is not this cluster", e.Destination)
	}
}

func TestStatusScope(t *testing.T) {
	s := load(t, shopCluster(t))
	for _, tt := range []struct {
		ns   string
		want []string
	}{
		{"shop", []string{
			// The sources of what deploys into shop, and theirs.
			"flux-system/gitrepository/flux-system", "flux-system/helmrepository/bitnami", "flux-system/helmrepository/podinfo",
			// apps targets shop and storefront manages objects there;
			// edge targets shop on another cluster.
			"flux-system/kustomization/apps", "flux-system/kustomization/storefront",
			"shop/helmrelease/podinfo", "shop/helmrelease/redis",
			"argocd/application/guestbook", "argocd/application/ledger",
		}},
		{"payments", []string{"argocd/application/payments"}},
		{"nothing", nil},
	} {
		if got := names(s.Status(tt.ns).Objects); !slices.Equal(got, tt.want) {
			t.Errorf("Status(%q) =\n%v\nwant\n%v", tt.ns, got, tt.want)
		}
	}
	all := s.Status("")
	if len(all.Objects) != 5+12+2+5 {
		t.Errorf(`Status("") lists %d objects, want every source but HelmCharts, Kustomization, HelmRelease and Application: %v`, len(all.Objects), names(all.Objects))
	}
	if slices.ContainsFunc(all.Objects, func(o *Object) bool { return o.Kind == KindHelmChart }) {
		t.Errorf(`Status("") lists HelmCharts, which helm-controller makes`)
	}
}

func TestMatch(t *testing.T) {
	s := load(t, shopCluster(t))
	for _, tt := range []struct {
		query, ns string
		want      []string
	}{
		{"apps", "", []string{"flux-system/kustomization/apps"}},
		{"podinfo", "", []string{"shop/helmrelease/podinfo"}},
		{"hr/podinfo", "", []string{"shop/helmrelease/podinfo"}},
		{"ks/podinfo", "", nil},
		{"kustomization.kustomize.toolkit.fluxcd.io/infra-c", "", []string{"flux-system/kustomization/infra-configs", "flux-system/kustomization/infra-controllers"}},
		{"applications.argoproj.io/guest", "", []string{"argocd/application/guestbook"}},
		{"app/guestbook", "shop", nil},
		{"guestbook", "argocd", []string{"argocd/application/guestbook"}},
		{"gitrepository/flux-system", "", nil},
	} {
		got, err := s.Match(tt.query, tt.ns)
		if tt.query == "gitrepository/flux-system" {
			if err == nil || !strings.Contains(err.Error(), "is not a Kustomization, HelmRelease or Application") {
				t.Errorf("Match(%q) error = %v, want one naming the kinds it takes", tt.query, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Match(%q, %q): %v", tt.query, tt.ns, err)
			continue
		}
		if g := names(got); !slices.Equal(g, tt.want) {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.query, tt.ns, g, tt.want)
		}
	}
}

func TestParseInventoryID(t *testing.T) {
	for _, tt := range []struct {
		id   string
		want objectRef
		ok   bool
	}{
		{"shop_pricing_apps_Deployment", objectRef{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "shop", Name: "pricing"}, true},
		{"shop_pricing__Service", objectRef{Version: "v1", Kind: "Service", Namespace: "shop", Name: "pricing"}, true},
		{"_system__storefront-reader_rbac.authorization.k8s.io_ClusterRole", objectRef{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Name: "system:storefront-reader"}, true},
		{"nonsense", objectRef{}, false},
	} {
		got, ok := parseInventoryID(tt.id, "v1")
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseInventoryID(%q) = %+v, %v, want %+v, %v", tt.id, got, ok, tt.want, tt.ok)
		}
	}
}
