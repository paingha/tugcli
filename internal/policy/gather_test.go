package policy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// served are the resources the fake cluster serves, with their kinds and
// whether they are namespaced.
var served = []struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
}{
	{schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "Pod", true},
	{schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, "Secret", true},
	{schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, "PersistentVolumeClaim", true},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "Deployment", true},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "StatefulSet", true},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, "DaemonSet", true},
	{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, "ReplicaSet", true},
	{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "Job", true},
	{schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, "CronJob", true},
	{schema.GroupVersionResource{Group: "db.example.com", Version: "v1", Resource: "databases"}, "Database", true},
	{schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "clusterwidgets"}, "ClusterWidget", false},
}

type fakeCluster struct {
	clients ops.Clients
	mapper  meta.RESTMapper
	typed   *fake.Clientset
	dynamic *dynamicfake.FakeDynamicClient
}

// newFakeCluster serves objects through the typed, dynamic and metadata
// clients, with a RESTMapper for the served resources. Budgets are served
// only through the typed client, as Gather reads them.
func newFakeCluster(t *testing.T, budgets []runtime.Object, objs ...runtime.Object) *fakeCluster {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{}
	var versions []schema.GroupVersion
	for _, s := range served {
		if !slices.Contains(versions, s.gvr.GroupVersion()) {
			versions = append(versions, s.gvr.GroupVersion())
		}
	}
	mapper := meta.NewDefaultRESTMapper(versions)
	for _, s := range served {
		listKinds[s.gvr] = s.kind + "List"
		scope := meta.RESTScopeRoot
		if s.namespaced {
			scope = meta.RESTScopeNamespace
		}
		mapper.Add(s.gvr.GroupVersion().WithKind(s.kind), scope)
	}
	us := toUnstructured(t, objs...)
	dyn := make([]runtime.Object, len(us))
	md := make([]runtime.Object, len(us))
	sch := metadatafake.NewTestScheme()
	for i, u := range us {
		dyn[i] = u
		p := &metav1.PartialObjectMetadata{}
		p.SetGroupVersionKind(u.GroupVersionKind())
		p.Name, p.Namespace, p.UID = u.GetName(), u.GetNamespace(), u.GetUID()
		p.Labels, p.Annotations, p.OwnerReferences = u.GetLabels(), u.GetAnnotations(), u.GetOwnerReferences()
		md[i] = p
		if gvk := u.GroupVersionKind(); !sch.Recognizes(gvk) {
			sch.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
		}
	}
	cs := fake.NewClientset(budgets...)
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, dyn...)
	return &fakeCluster{
		clients: ops.Clients{Typed: cs, Dynamic: d, Metadata: metadatafake.NewSimpleMetadataClient(sch, md...), Discovery: cs.Discovery()},
		mapper:  mapper,
		typed:   cs,
		dynamic: d,
	}
}

func refs(objs []*unstructured.Unstructured) []string {
	out := make([]string, len(objs))
	for i, u := range objs {
		out[i] = u.GetNamespace() + "/" + newObject(u).ref()
	}
	slices.Sort(out)
	return out
}

// gatherPack names every kind the fake cluster has objects of, and one it
// does not serve, and wants budgets.
const gatherPack = "{id: owner, severity: info, description: d, requiredAnnotations: [owner], match: {kinds: " +
	"[Deployment, ReplicaSet, Pod, Job, CronJob, Secret, Database.db.example.com, ClusterWidget.example.com, Certificate.cert-manager.io]}}"

func gatherObjects() (budgets, objs []runtime.Object) {
	api := deployment("api", 2)
	rs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"metadata": map[string]any{"name": "api-6b8c", "namespace": "shop"},
	}}
	rs.SetOwnerReferences(controller("apps/v1", "Deployment", "api"))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nightly-29918057", Namespace: "shop", OwnerReferences: controller("batch/v1", "CronJob", "nightly")}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "shop", Annotations: map[string]string{
			"owner":     "payments",
			lastApplied: `{"data":{"password":"aHVudGVyMg=="}}`,
		}},
		Data: map[string][]byte{"password": []byte("hunter2")},
	}
	web := deployment("web", 2)
	web.Namespace = "other"
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "ClusterWidget", "metadata": map[string]any{"name": "w"},
	}}
	// An operator's pod, which no built-in workload controls.
	operated := pod("orders-1", controller("db.example.com/v1", "Database", "orders"))
	objs = []runtime.Object{
		api, rs, pod("api-6b8c-x2x4z", controller("apps/v1", "ReplicaSet", "api-6b8c")), pod("debug", nil), operated,
		cronJob("nightly"), job, secret, database("orders"), web, widget,
	}
	budgets = []runtime.Object{
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "other"}},
	}
	return budgets, objs
}

func TestGather(t *testing.T) {
	ctx := context.Background()
	p := packOf(t, gatherPack, "{id: budget, severity: warning, description: d, podDisruptionBudget: {}}")
	budgets, objs := gatherObjects()

	t.Run("a namespace", func(t *testing.T) {
		c := newFakeCluster(t, budgets, objs...)
		inv, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Namespace: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"shop/cronjob/nightly", "shop/database/orders", "shop/deploy/api", "shop/pod/debug", "shop/pod/orders-1", "shop/secret/creds"}
		if got := refs(inv.Objects); !slices.Equal(got, want) {
			t.Errorf("Gather(shop) objects = %v, want %v", got, want)
		}
		if len(inv.PodDisruptionBudgets) != 1 || inv.PodDisruptionBudgets[0].Name != "api" || inv.BudgetsUnknown || len(inv.Unlisted) != 0 {
			t.Errorf("Gather(shop) = budgets %v, unknown %v, unlisted %v", inv.PodDisruptionBudgets, inv.BudgetsUnknown, inv.Unlisted)
		}
		for _, u := range inv.Objects {
			if u.GetKind() != "Secret" {
				continue
			}
			if _, ok := u.Object["data"]; ok {
				t.Errorf("Gather read a Secret's data: %v", u.Object)
			}
			if a := u.GetAnnotations(); a["owner"] != "payments" || a[lastApplied] != "-" {
				t.Errorf("Secret annotations = %v, want owner kept and the last applied configuration left out", a)
			}
		}
		for _, a := range c.dynamic.Actions() {
			if a.GetResource().Resource == "secrets" {
				t.Errorf("Gather read Secrets in full: %v", a)
			}
			if l, ok := a.(k8stesting.ListAction); ok && a.GetResource().Resource == "pods" && l.GetListRestrictions().Fields.String() == "" {
				t.Errorf("Gather listed every pod in full: %v", a)
			}
		}
	})

	t.Run("every namespace", func(t *testing.T) {
		c := newFakeCluster(t, budgets, objs...)
		inv, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"/clusterwidget/w", "other/deploy/web", "shop/cronjob/nightly", "shop/database/orders", "shop/deploy/api",
			"shop/pod/debug", "shop/pod/orders-1", "shop/secret/creds"}
		if got := refs(inv.Objects); !slices.Equal(got, want) {
			t.Errorf("Gather(all) objects = %v, want %v", got, want)
		}
		if len(inv.PodDisruptionBudgets) != 2 {
			t.Errorf("Gather(all) budgets = %v, want both", inv.PodDisruptionBudgets)
		}
	})

	t.Run("one object", func(t *testing.T) {
		c := newFakeCluster(t, budgets, objs...)
		o := resolve.Object{Resource: served[0].gvr, Kind: "Pod", Name: "api-6b8c-x2x4z", Namespace: "shop"}
		inv, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Object: &o})
		if err != nil {
			t.Fatal(err)
		}
		// Named, a pod is checked even though a ReplicaSet controls it; a
		// pod has no replicas, so no budget is needed.
		if got := refs(inv.Objects); !slices.Equal(got, []string{"shop/pod/api-6b8c-x2x4z"}) || inv.PodDisruptionBudgets != nil {
			t.Errorf("Gather(pod) = %v, budgets %v", got, inv.PodDisruptionBudgets)
		}
		if n := len(c.typed.Actions()); n != 0 {
			t.Errorf("Gather(pod) made %d typed requests, want none for budgets", n)
		}

		o = resolve.Object{Resource: served[3].gvr, Kind: "Deployment", Name: "api", Namespace: "shop"}
		if inv, err = Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Object: &o}); err != nil || len(inv.PodDisruptionBudgets) != 1 {
			t.Errorf("Gather(deploy/api) = %v, budgets %v", err, inv)
		}

		o.Name = "gone"
		if _, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Object: &o}); !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "reading deployment/gone") {
			t.Errorf("Gather(deploy/gone) = %v, want not found", err)
		}
	})

	t.Run("refused lists", func(t *testing.T) {
		c := newFakeCluster(t, budgets, objs...)
		forbid := func(resource string) k8stesting.ReactionFunc {
			return func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC"))
			}
		}
		c.dynamic.PrependReactor("list", "cronjobs", forbid("cronjobs"))
		c.typed.PrependReactor("list", "poddisruptionbudgets", forbid("poddisruptionbudgets"))
		inv, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Namespace: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(inv.Unlisted, []string{"cronjobs.batch", "poddisruptionbudgets.policy"}) || !inv.BudgetsUnknown {
			t.Errorf("Gather = unlisted %v, budgets unknown %v", inv.Unlisted, inv.BudgetsUnknown)
		}
		if got := refs(inv.Objects); slices.Contains(got, "shop/cronjob/nightly") || !slices.Contains(got, "shop/deploy/api") {
			t.Errorf("Gather objects = %v, want all but the cronjob", got)
		}
	})

	t.Run("an unreachable cluster", func(t *testing.T) {
		c := newFakeCluster(t, budgets, objs...)
		c.dynamic.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})
		if _, err := Gather(ctx, c.clients, c.mapper, []*Pack{p}, Scope{Namespace: "shop"}); err == nil || !strings.Contains(err.Error(), "listing deployments.apps: connection refused") {
			t.Errorf("Gather = %v, want the list's error", err)
		}
	})
}
