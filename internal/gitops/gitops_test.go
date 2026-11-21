package gitops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/restmapper"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/ops"
)

// testNow is the clock of the tests, which the fixtures' times are near.
var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func now() time.Time { return testNow }

func resourceList(gv string, namespaced bool, rs ...string) *metav1.APIResourceList {
	l := &metav1.APIResourceList{GroupVersion: gv}
	for _, r := range rs {
		name, kind, _ := strings.Cut(r, ":")
		l.APIResources = append(l.APIResources, metav1.APIResource{Name: name, Kind: kind, Namespaced: namespaced, Verbs: []string{"get", "list", "watch"}})
	}
	return l
}

// builtinResources are the built-in kinds the fake clusters serve.
var builtinResources = []*metav1.APIResourceList{
	resourceList("v1", true, "pods:Pod", "services:Service", "configmaps:ConfigMap", "secrets:Secret", "events:Event"),
	resourceList("apps/v1", true, "deployments:Deployment", "replicasets:ReplicaSet", "statefulsets:StatefulSet", "daemonsets:DaemonSet"),
	resourceList("batch/v1", true, "jobs:Job"),
	resourceList("rbac.authorization.k8s.io/v1", false, "clusterroles:ClusterRole"),
	resourceList("db.example.com/v1", true, "databases:Database"),
}

// fluxResources are what Flux 2.5 serves: OCIRepository only at v1beta2,
// after the preferred v1 of its group.
var fluxResources = []*metav1.APIResourceList{
	resourceList("source.toolkit.fluxcd.io/v1", true, "gitrepositories:GitRepository", "helmrepositories:HelmRepository", "helmcharts:HelmChart", "buckets:Bucket"),
	resourceList("source.toolkit.fluxcd.io/v1beta2", true, "gitrepositories:GitRepository", "ocirepositories:OCIRepository"),
	resourceList("kustomize.toolkit.fluxcd.io/v1", true, "kustomizations:Kustomization"),
	resourceList("helm.toolkit.fluxcd.io/v2", true, "helmreleases:HelmRelease"),
}

var argoResourcesServed = []*metav1.APIResourceList{
	resourceList("argoproj.io/v1alpha1", true, "applications:Application", "appprojects:AppProject"),
}

func allResources() []*metav1.APIResourceList {
	return append(append(append([]*metav1.APIResourceList{}, builtinResources...), fluxResources...), argoResourcesServed...)
}

// cluster is a fake cluster: its clients, and the dynamic and metadata fakes
// to check what was read.
type cluster struct {
	c   Clients
	dyn *dynamicfake.FakeDynamicClient
	md  *metadatafake.FakeMetadataClient
}

// fakeCluster serves typed objects through the typed, dynamic and metadata
// clients, and custom resources through the dynamic and metadata ones, with
// discovery of resources.
func fakeCluster(t *testing.T, resources []*metav1.APIResourceList, typed []runtime.Object, custom []*unstructured.Unstructured) *cluster {
	t.Helper()
	cs := fake.NewSimpleClientset(typed...)
	cs.Resources = resources

	listKinds := map[schema.GroupVersionResource]string{}
	for _, l := range resources {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range l.APIResources {
			listKinds[gv.WithResource(r.Name)] = r.Kind + "List"
		}
	}
	var dyn, md []runtime.Object
	for _, o := range typed {
		gvks, _, err := scheme.Scheme.ObjectKinds(o)
		if err != nil {
			t.Fatal(err)
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: m}
		u.SetGroupVersionKind(gvks[0])
		if _, ok := o.(*corev1.Event); !ok {
			dyn = append(dyn, u)
			md = append(md, partial(u))
		}
	}
	for _, u := range custom {
		dyn = append(dyn, u)
		md = append(md, partial(u))
	}
	sch := metadatafake.NewTestScheme()
	for _, o := range md {
		if gvk := o.GetObjectKind().GroupVersionKind(); !sch.Recognizes(gvk) {
			sch.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
		}
	}
	mdc := metadatafake.NewSimpleMetadataClient(sch, md...)
	dync := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, dyn...)
	groups, err := restmapper.GetAPIGroupResources(cs.Discovery())
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{
		c: Clients{
			Clients: ops.Clients{Typed: cs, Dynamic: dync, Metadata: mdc, Discovery: cs.Discovery()},
			Mapper:  restmapper.NewDiscoveryRESTMapper(groups),
		},
		dyn: dync, md: mdc,
	}
}

func partial(u *unstructured.Unstructured) *metav1.PartialObjectMetadata {
	p := &metav1.PartialObjectMetadata{}
	p.SetGroupVersionKind(u.GroupVersionKind())
	p.Name, p.Namespace, p.UID = u.GetName(), u.GetNamespace(), u.GetUID()
	p.Labels, p.OwnerReferences = u.GetLabels(), u.GetOwnerReferences()
	p.CreationTimestamp = u.GetCreationTimestamp()
	return p
}

// fixtures reads the objects of a YAML file in testdata.
func fixtures(t *testing.T, file string) []*unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	for _, doc := range bytes.Split(b, []byte("\n---\n")) {
		// Through JSON, as the dynamic client decodes, so numbers are
		// int64s.
		j, err := yaml.YAMLToJSON(doc)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(j); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out = append(out, u)
	}
	return out
}

// deployment returns a deployment in shop with its ReplicaSet and pods,
// ready unless a pod option says otherwise.
func deployment(name string, replicas int32, labels map[string]string, podOpts ...func(*corev1.Pod)) []runtime.Object {
	sel := map[string]string{"app": name}
	podLabels := map[string]string{"app": name, "pod-template-hash": "7d9f"}
	yes := true
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID("deploy-" + name), Generation: 1, Labels: labels,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "registry.example.com/" + name + ":v2"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-7d9f", Namespace: "shop", UID: types.UID("rs-" + name), Labels: podLabels,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: d.UID, Controller: &yes}}},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: sel}, Template: d.Spec.Template},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas},
	}
	out := []runtime.Object{d, rs}
	for i := range replicas {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name + "-7d9f-" + string(rune('a'+int(i))), Namespace: "shop", UID: types.UID("pod-" + name + string(rune('a'+int(i)))), Labels: podLabels,
				CreationTimestamp: metav1.NewTime(testNow.Add(-3 * time.Hour)),
				OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}},
			},
			Spec: corev1.PodSpec{NodeName: "node-1", Containers: d.Spec.Template.Spec.Containers},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true, Image: d.Spec.Template.Spec.Containers[0].Image,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(testNow.Add(-3 * time.Hour))}}}},
			},
		}
		for _, o := range podOpts {
			o(p)
		}
		out = append(out, p)
	}
	return out
}

// missingSecret makes a pod fail to start for want of a Secret.
func missingSecret(p *corev1.Pod) {
	p.Status.Conditions[1] = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "CreateContainerConfigError", Message: `secret "pricing-api-key" not found`,
	}}
}

// shopObjects are the live objects in shop that Flux and Argo CD manage:
// a healthy checkout, a pricing deployment whose pod lacks a Secret, a
// podinfo deployment from a Helm release, a failing Database, a ConfigMap,
// a Secret and a ClusterRole.
func shopObjects() ([]runtime.Object, []*unstructured.Unstructured) {
	var typed []runtime.Object
	typed = append(typed, deployment("checkout", 2, nil)...)
	pricing := deployment("pricing", 1, nil, missingSecret)
	d := pricing[0].(*appsv1.Deployment)
	d.Status.ReadyReplicas, d.Status.AvailableReplicas = 0, 0
	rs := pricing[1].(*appsv1.ReplicaSet)
	rs.Status.ReadyReplicas, rs.Status.AvailableReplicas = 0, 0
	typed = append(typed, pricing...)
	typed = append(typed, deployment("podinfo", 1, map[string]string{
		"helm.toolkit.fluxcd.io/name": "podinfo", "helm.toolkit.fluxcd.io/namespace": "shop", "app.kubernetes.io/managed-by": "Helm",
	})...)
	typed = append(typed,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "checkout-config", Namespace: "shop", UID: "cm-checkout-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "checkout-tls", Namespace: "shop", UID: "secret-checkout-tls"},
			Data: map[string][]byte{"tls.key": []byte("hunter2")}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:storefront-reader", UID: "clusterrole-storefront-reader"}},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "podinfo.drift", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "podinfo", Namespace: "shop", UID: "hr-podinfo", APIVersion: "helm.toolkit.fluxcd.io/v2"},
			Type:           corev1.EventTypeWarning, Reason: "DriftDetected",
			Message: "Cluster state of release shop/podinfo.v5 (chart podinfo@6.7.0) has drifted from the desired state:\n" +
				"Deployment/shop/podinfo changed (0 additions, 1 changes, 0 removals)\n" +
				"Service/shop/podinfo removed",
			Count: 3, LastTimestamp: metav1.NewTime(testNow.Add(-4 * time.Minute)),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "podinfo.olddrift", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "podinfo", Namespace: "shop", UID: "hr-podinfo", APIVersion: "helm.toolkit.fluxcd.io/v2"},
			Type:           corev1.EventTypeWarning, Reason: "DriftDetected",
			Message: "Cluster state of release shop/podinfo.v5 (chart podinfo@6.7.0) has drifted from the desired state:\nConfigMap/shop/podinfo-old changed",
			Count:   1, LastTimestamp: metav1.NewTime(testNow.Add(-50 * time.Minute)),
		},
	)
	orders := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "db.example.com/v1", "kind": "Database",
		"metadata": map[string]any{"name": "orders", "namespace": "shop", "uid": "database-orders", "generation": int64(1)},
		"status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "BackupFailing", "message": "the latest backup failed",
			"lastTransitionTime": testNow.Add(-10 * time.Minute).Format(time.RFC3339),
		}}},
	}}
	return typed, []*unstructured.Unstructured{orders}
}

// shopCluster is a cluster with Flux and Argo CD, the fixtures in testdata
// and the live objects of shopObjects.
func shopCluster(t *testing.T) *cluster {
	t.Helper()
	typed, live := shopObjects()
	custom := append(append(fixtures(t, "flux.yaml"), fixtures(t, "argocd.yaml")...), live...)
	return fakeCluster(t, allResources(), typed, custom)
}

func load(t *testing.T, c *cluster) *Snapshot {
	t.Helper()
	s, err := Load(context.Background(), c.c, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// find returns the one object of a kind with a name.
func find(t *testing.T, s *Snapshot, kind, ns, name string) *Object {
	t.Helper()
	o := s.get(Ref{Kind: kind, Name: name, Namespace: ns})
	if o == nil {
		t.Fatalf("no %s %s/%s in the snapshot", kind, ns, name)
	}
	return o
}

// secretReads returns the reads of Secrets through the dynamic client, which
// would carry their data.
func secretReads(c *cluster) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range c.dyn.Actions() {
		if a.GetResource().Resource == "secrets" {
			out = append(out, a)
		}
	}
	return out
}

// writes returns any request that is not a read.
func writes(c *cluster) []k8stesting.Action {
	var out []k8stesting.Action
	all := append(append(c.dyn.Actions(), c.md.Actions()...), c.c.Typed.(*fake.Clientset).Actions()...)
	for _, a := range all {
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			out = append(out, a)
		}
	}
	return out
}
