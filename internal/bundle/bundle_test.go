package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/doctor"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// now is when the tests collect their bundles.
var now = time.Date(2026, 11, 21, 15, 30, 0, 0, time.UTC)

// testResources is what the fake cluster's discovery serves.
var testResources = []*metav1.APIResourceList{
	{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "pods", SingularName: "pod", ShortNames: []string{"po"}, Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
		{Name: "services", SingularName: "service", ShortNames: []string{"svc"}, Kind: "Service", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "configmaps", SingularName: "configmap", ShortNames: []string{"cm"}, Kind: "ConfigMap", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "secrets", SingularName: "secret", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "events", SingularName: "event", ShortNames: []string{"ev"}, Kind: "Event", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "namespaces", SingularName: "namespace", ShortNames: []string{"ns"}, Kind: "Namespace", Verbs: []string{"get", "list"}},
		{Name: "nodes", SingularName: "node", ShortNames: []string{"no"}, Kind: "Node", Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
		{Name: "deployments", SingularName: "deployment", ShortNames: []string{"deploy"}, Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "replicasets", SingularName: "replicaset", ShortNames: []string{"rs"}, Kind: "ReplicaSet", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "statefulsets", SingularName: "statefulset", Kind: "StatefulSet", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "daemonsets", SingularName: "daemonset", Kind: "DaemonSet", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
		{Name: "jobs", SingularName: "job", Kind: "Job", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "db.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "databases", SingularName: "database", ShortNames: []string{"db"}, Kind: "Database", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
}

// Secrets planted in every place a bundle must redact; none may be in it.
const (
	envSecret         = "env-hunter2"
	appliedEnvSecret  = "applied-hunter2"
	argSecret         = "arg-token-123"
	configMapSecret   = "cm-secret-value"
	configMapURLPass  = "cm-url-pass"
	secretData        = "secret-data-hunter2"
	logBearer         = "logbearerabcdef123"
	logURLPass        = "log-url-pass"
	logPassword       = "log-kv-pass"
	logJWT            = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJib2IifQ.c2lnbmF0dXJl"
	logAWSKey         = "AKIAIOSFODNN7EXAMPLE"
	customerID        = "customer-acme-4242"
	eventToken        = "event-token-xyz"
	customFieldSecret = "cr-field-pass"
	customURLPass     = "cr-url-pass"
)

var plantedSecrets = []string{
	envSecret, appliedEnvSecret, argSecret, configMapSecret, configMapURLPass,
	secretData, base64.StdEncoding.EncodeToString([]byte(secretData)),
	logBearer, logURLPass, logPassword, logJWT, logAWSKey, customerID, eventToken,
	customFieldSecret, customURLPass,
}

// shop returns the objects of the shop namespace: deploy/api, whose pod
// crashes and mounts a ConfigMap and a Secret, a Service, events, the node
// the pod runs on, and a Database custom resource that owns a Secret.
func shop() ([]k8sruntime.Object, []*unstructured.Unstructured) {
	yes := true
	labels := map[string]string{"app": "api"}
	one := int32(1)
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "shop/api:v2",
			Args: []string{"--listen=:8080", "--api-token=" + argSecret},
			Env: []corev1.EnvVar{
				{Name: "DATABASE_PASSWORD", Value: envSecret},
				{Name: "DATABASE_USER", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "api-creds"}, Key: "user",
				}}},
			},
		}}},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", UID: "deploy-api", Generation: 1,
			Annotations: map[string]string{
				lastApplied: `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DATABASE_PASSWORD","value":"` + appliedEnvSecret + `"}]}]}}}}`,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1},
	}
	rsLabels := map[string]string{"app": "api", "pod-template-hash": "7d9f"}
	rsTemplate := template
	rsTemplate.Labels = rsLabels
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f", Namespace: "shop", UID: "rs-api", Labels: rsLabels,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "deploy-api", Controller: &yes}}},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: rsLabels}, Template: rsTemplate},
		Status: appsv1.ReplicaSetStatus{Replicas: 1},
	}
	podSpec := *rsTemplate.Spec.DeepCopy()
	podSpec.NodeName = "node-1"
	podSpec.Volumes = []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"}}}},
		{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "api-creds"}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f-x2x4z", Namespace: "shop", UID: "pod-api", Labels: rsLabels,
			CreationTimestamp: metav1.NewTime(now.Add(-3 * time.Hour)),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-7d9f", UID: "rs-api", Controller: &yes}}},
		Spec: podSpec,
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 4,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", UID: "svc-api"},
		Spec:       corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 80}}},
	}
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "api-config", Namespace: "shop", UID: "cm-api"},
		Data: map[string]string{
			"api.key":      configMapSecret,
			"DATABASE_URL": "postgres://app:" + configMapURLPass + "@db:5432/orders",
		},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-creds", Namespace: "shop", UID: "secret-api",
			Annotations: map[string]string{
				lastApplied: `{"apiVersion":"v1","kind":"Secret","stringData":{"password":"` + secretData + `"}}`,
			}},
		Data: map[string][]byte{"password": []byte(secretData), "user": []byte("app")},
	}
	event := func(name, kind, object string, uid types.UID, reason, msg string, ago time.Duration) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID("event-" + name)},
			InvolvedObject: corev1.ObjectReference{Kind: kind, Name: object, Namespace: "shop", UID: uid},
			Type:           corev1.EventTypeWarning, Reason: reason, Message: msg, Count: 3,
			LastTimestamp: metav1.NewTime(now.Add(-ago)),
		}
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"topology.kubernetes.io/zone": "eu-west-1a"},
			Annotations: map[string]string{"node.alpha.kubernetes.io/ttl": "0"}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, Reason: "KubeletHasInsufficientMemory"},
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.35.1"},
			Images:   []corev1.ContainerImage{{Names: []string{"shop/api:v2"}}},
		},
	}

	orders := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "db.example.com/v1", "kind": "Database",
		"metadata": map[string]any{"name": "orders", "namespace": "shop", "uid": "db-orders", "generation": int64(1)},
		"spec": map[string]any{
			"adminPassword":         customFieldSecret,
			"dsn":                   "mysql://root:" + customURLPass + "@db:3306/orders",
			"credentialsSecretName": "orders-creds",
		},
		"status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "BackupFailing", "message": "the latest backup failed",
		}}},
	}}
	ordersCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-creds", Namespace: "shop", UID: "secret-orders",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "db.example.com/v1", Kind: "Database", Name: "orders", UID: "db-orders", Controller: &yes}}},
		Data: map[string][]byte{"password": []byte(secretData)},
	}

	typed := []k8sruntime.Object{
		deploy, rs, pod, service, config, creds, node, ordersCreds,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "ns-shop"}},
		event("api.backoff", "Pod", pod.Name, pod.UID, "BackOff", "Back-off restarting failed container app", 2*time.Minute),
		event("api.pull", "Pod", pod.Name, "", "Failed", "pull failed with token="+eventToken, 5*time.Minute),
		event("api.old", "Pod", pod.Name, pod.UID, "Ancient", "outside the window", 3*time.Hour),
		event("other.warn", "Pod", "other-pod", "pod-other", "Unrelated", "about another pod", time.Minute),
		event("orders.backup", "Database", "orders", "db-orders", "BackupFailed", "backup failed", time.Minute),
	}
	return typed, []*unstructured.Unstructured{orders}
}

// fakeClients serves the objects through the typed, dynamic and metadata
// clients, with testResources' discovery, and answers pods' logs with
// lines that hold credentials.
func fakeClients(t *testing.T, typed []k8sruntime.Object, custom ...*unstructured.Unstructured) ops.Clients {
	t.Helper()
	cs := fake.NewSimpleClientset(typed...)
	cs.Resources = testResources
	cs.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		opts, _ := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		logs := "2026-11-21T15:29:00Z listening on :8080\n" +
			"2026-11-21T15:29:01Z GET /orders Authorization: Bearer " + logBearer + "\n" +
			"2026-11-21T15:29:02Z connecting to postgres://app:" + logURLPass + "@db:5432/orders\n" +
			"2026-11-21T15:29:03Z config password=" + logPassword + " session " + logJWT + "\n"
		if opts != nil && opts.Previous {
			logs = "2026-11-21T14:00:00Z using " + logAWSKey + " for " + customerID + "\n" +
				"2026-11-21T14:00:01Z panic: cannot reach the database\n"
		}
		return true, &k8sruntime.Unknown{Raw: []byte(logs)}, nil
	})

	var dyn, md []k8sruntime.Object
	for _, o := range typed {
		gvks, _, err := scheme.Scheme.ObjectKinds(o)
		if err != nil {
			t.Fatal(err)
		}
		m, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: m}
		u.SetGroupVersionKind(gvks[0])
		dyn = append(dyn, u)
		md = append(md, partial(u))
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
	listKinds := map[schema.GroupVersionResource]string{}
	for _, l := range testResources {
		gv, _ := schema.ParseGroupVersion(l.GroupVersion)
		for _, r := range l.APIResources {
			listKinds[gv.WithResource(r.Name)] = r.Kind + "List"
		}
	}
	return ops.Clients{
		Typed:     cs,
		Dynamic:   dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds, dyn...),
		Metadata:  metadatafake.NewSimpleMetadataClient(sch, md...),
		Discovery: cs.Discovery(),
	}
}

func partial(u *unstructured.Unstructured) *metav1.PartialObjectMetadata {
	p := &metav1.PartialObjectMetadata{}
	p.SetGroupVersionKind(u.GroupVersionKind())
	p.ObjectMeta = metav1.ObjectMeta{
		Name: u.GetName(), Namespace: u.GetNamespace(), UID: u.GetUID(), Labels: u.GetLabels(),
		Annotations: u.GetAnnotations(), OwnerReferences: u.GetOwnerReferences(), CreationTimestamp: u.GetCreationTimestamp(),
	}
	return p
}

var (
	deploymentAPI = resolve.Object{Resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Name: "api", Namespace: "shop"}
	databaseOrder = resolve.Object{Resource: schema.GroupVersionResource{Group: "db.example.com", Version: "v1", Resource: "databases"}, Kind: "Database", Name: "orders", Namespace: "shop"}
	namespaceShop = resolve.Object{Resource: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, Kind: "Namespace", Name: "shop"}
)

func collect(t *testing.T, target resolve.Object, opts Options) *Bundle {
	t.Helper()
	typed, custom := shop()
	if opts.Redactor == nil {
		r, err := NewRedactor([]string{`customer-[a-z]+-[0-9]+`})
		if err != nil {
			t.Fatal(err)
		}
		opts.Redactor = r
	}
	opts.Now = func() time.Time { return now }
	opts.Cluster = ClusterInfo{Name: "prod-eu", Provider: "eks", Region: "eu-west-1"}
	if opts.Since == 0 {
		opts.Since = time.Hour
	}
	if opts.LogsTail == 0 {
		opts.LogsTail = 1000
	}
	b, err := Collect(context.Background(), fakeClients(t, typed, custom...), target, opts)
	if err != nil {
		t.Fatalf("Collect(%s) = %v", target, err)
	}
	return b
}

// file returns a bundle's file by path.
func file(t *testing.T, b *Bundle, p string) string {
	t.Helper()
	for _, f := range b.Files {
		if f.Path == p {
			return string(f.Data)
		}
	}
	t.Fatalf("bundle has no %s; it has %v", p, paths(b))
	return ""
}

func paths(b *Bundle) []string {
	out := make([]string, len(b.Files))
	for i, f := range b.Files {
		out[i] = f.Path
	}
	return out
}

// assertRedacted fails if any planted secret is anywhere in the bundle.
func assertRedacted(t *testing.T, b *Bundle) {
	t.Helper()
	for _, f := range b.Files {
		for _, s := range plantedSecrets {
			if strings.Contains(string(f.Data), s) {
				t.Errorf("%s holds the secret %q:\n%s", f.Path, s, f.Data)
			}
		}
	}
	for _, p := range b.Problems {
		for _, s := range plantedSecrets {
			if strings.Contains(p, s) {
				t.Errorf("problem %q holds the secret %q", p, s)
			}
		}
	}
}

func TestCollectObject(t *testing.T) {
	var steps []string
	b := collect(t, deploymentAPI, Options{Progress: func(s string) { steps = append(steps, s) },
		Doctor: func(context.Context) (doctor.Report, error) {
			return doctor.Report{Cluster: "prod-eu", Results: []doctor.Result{{Group: doctor.GroupCluster, Name: "API server", Status: doctor.OK, Detail: "reached with Bearer " + logBearer}}}, nil
		}})
	assertRedacted(t, b)

	want := []string{
		"README.md", "manifest.json",
		"cluster/api-resources.json", "cluster/version.json", "doctor.json", "events.json",
		"logs/shop/api-7d9f-x2x4z/app.log", "logs/shop/api-7d9f-x2x4z/app.previous.log",
		"nodes/node-1.yaml",
		"objects/shop/configmaps/api-config.yaml",
		"objects/shop/deployments.apps/api.yaml",
		"objects/shop/pods/api-7d9f-x2x4z.yaml",
		"objects/shop/replicasets.apps/api-7d9f.yaml",
		"objects/shop/secrets/api-creds.yaml",
		"objects/shop/services/api.yaml",
		"tree/deployment-api.json", "why/deployment-api.json",
	}
	if got := paths(b); !slices.Equal(got, want) {
		t.Errorf("Collect(deploy/api) files =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(steps) == 0 {
		t.Error("Collect reported no progress")
	}

	deploy := file(t, b, "objects/shop/deployments.apps/api.yaml")
	for _, s := range []string{"DATABASE_PASSWORD", "secretKeyRef", "--api-token=<redacted>", "value: <redacted>", lastApplied + ": <redacted>"} {
		if !strings.Contains(deploy, s) {
			t.Errorf("deployment YAML lacks %q:\n%s", s, deploy)
		}
	}
	if strings.Contains(deploy, "managedFields") {
		t.Errorf("deployment YAML kept managedFields:\n%s", deploy)
	}
	cm := file(t, b, "objects/shop/configmaps/api-config.yaml")
	if !strings.Contains(cm, "api.key: <redacted>") || !strings.Contains(cm, "DATABASE_URL: <redacted>") {
		t.Errorf("ConfigMap YAML:\n%s", cm)
	}
	secret := file(t, b, "objects/shop/secrets/api-creds.yaml")
	if !strings.HasPrefix(secret, secretHeader) || strings.Contains(secret, "\ndata:") || !strings.Contains(secret, "name: api-creds") {
		t.Errorf("Secret YAML should be metadata only:\n%s", secret)
	}
	current := file(t, b, "logs/shop/api-7d9f-x2x4z/app.log")
	for _, s := range []string{"listening on :8080", "Authorization: Bearer <redacted>", "postgres://app:<redacted>@db", "password=<redacted>", "session <redacted>"} {
		if !strings.Contains(current, s) {
			t.Errorf("log lacks %q:\n%s", s, current)
		}
	}
	if previous := file(t, b, "logs/shop/api-7d9f-x2x4z/app.previous.log"); !strings.Contains(previous, "panic: cannot reach the database") {
		t.Errorf("previous log:\n%s", previous)
	}

	var events []eventRecord
	if err := json.Unmarshal([]byte(file(t, b, "events.json")), &events); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, e := range events {
		reasons = append(reasons, e.Reason)
	}
	if !slices.Equal(reasons, []string{"Failed", "BackOff"}) {
		t.Errorf("events.json reasons = %v, want [Failed BackOff]: the pod's, in the window, oldest first", reasons)
	}

	node := file(t, b, "nodes/node-1.yaml")
	if !strings.Contains(node, "KubeletHasInsufficientMemory") || strings.Contains(node, "shop/api:v2") || strings.Contains(node, "ttl") {
		t.Errorf("node YAML should have conditions but no images or annotations:\n%s", node)
	}

	var d ops.Diagnosis
	if err := json.Unmarshal([]byte(file(t, b, "why/deployment-api.json")), &d); err != nil {
		t.Fatal(err)
	}
	if d.Target != "deploy/api" || d.Healthy {
		t.Errorf("why/deployment-api.json = %+v", d)
	}

	var m manifest
	if err := json.Unmarshal([]byte(file(t, b, "manifest.json")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Target != "deployment/api" || m.Cluster.Name != "prod-eu" || m.Since != "1h" || m.RedactPatterns != 1 || m.Format != manifestFormat {
		t.Errorf("manifest = %+v", m)
	}
	if len(m.Files) != len(b.Files)-1 {
		t.Errorf("manifest lists %d files, want every file but itself, %d", len(m.Files), len(b.Files)-1)
	}
	for _, mf := range m.Files {
		if data := file(t, b, mf.Path); mf.Size != len(data) || mf.Source == "" {
			t.Errorf("manifest has %s at %d bytes from %q; it is %d bytes", mf.Path, mf.Size, mf.Source, len(data))
		}
		if mf.Path == "objects/shop/secrets/api-creds.yaml" && !strings.Contains(mf.Source, "(metadata only)") {
			t.Errorf("manifest source of the Secret = %q, want it marked metadata only", mf.Source)
		}
		if strings.Contains(mf.Source, customerID) || strings.Contains(string(file(t, b, "manifest.json")), "customer-[a-z]") {
			t.Errorf("manifest records the redact pattern")
		}
	}
	if readme := file(t, b, "README.md"); !strings.Contains(readme, "1 pattern given with `--redact-pattern`") || !strings.Contains(readme, "tar -xzf "+b.Name+".tar.gz") {
		t.Errorf("README:\n%s", readme)
	}
}

func TestCollectCustomResource(t *testing.T) {
	b := collect(t, databaseOrder, Options{})
	assertRedacted(t, b)
	for _, p := range []string{
		"objects/shop/databases.db.example.com/orders.yaml", "objects/shop/secrets/orders-creds.yaml",
		"why/database-orders.json", "tree/database-orders.json",
	} {
		file(t, b, p)
	}
	db := file(t, b, "objects/shop/databases.db.example.com/orders.yaml")
	if !strings.Contains(db, "adminPassword: <redacted>") || !strings.Contains(db, "credentialsSecretName: orders-creds") || !strings.Contains(db, "mysql://root:<redacted>@db") {
		t.Errorf("Database YAML:\n%s", db)
	}
	if events := file(t, b, "events.json"); !strings.Contains(events, "BackupFailed") || strings.Contains(events, "BackOff") {
		t.Errorf("events.json should have the Database's event only:\n%s", events)
	}
}

func TestCollectNamespace(t *testing.T) {
	b := collect(t, namespaceShop, Options{Exclude: []string{"cm"}})
	assertRedacted(t, b)
	for _, p := range []string{
		"objects/_cluster/namespaces/shop.yaml",
		"objects/shop/deployments.apps/api.yaml", "objects/shop/databases.db.example.com/orders.yaml",
		"objects/shop/secrets/api-creds.yaml", "objects/shop/secrets/orders-creds.yaml",
		"why/deployment-api.json", "tree/deployment-api.json",
		"logs/shop/api-7d9f-x2x4z/app.log", "nodes/node-1.yaml",
	} {
		file(t, b, p)
	}
	for _, f := range b.Files {
		if strings.Contains(f.Path, "configmaps") || strings.Contains(f.Path, "events/") {
			t.Errorf("namespace bundle with --exclude cm has %s", f.Path)
		}
	}
	if events := file(t, b, "events.json"); !strings.Contains(events, "Unrelated") {
		t.Errorf("a namespace's events.json should have all of its events:\n%s", events)
	}
	secret := file(t, b, "objects/shop/secrets/api-creds.yaml")
	if !strings.HasPrefix(secret, secretHeader) || !strings.Contains(secret, lastApplied+": <redacted>") {
		t.Errorf("listed Secret YAML:\n%s", secret)
	}
}

func TestCollectKindFilter(t *testing.T) {
	b := collect(t, deploymentAPI, Options{Include: []string{"deployments.apps", "Pod"}, LogsTail: 0})
	for _, f := range b.Files {
		if strings.HasPrefix(f.Path, "objects/") && f.Path != "objects/shop/deployments.apps/api.yaml" && f.Path != "objects/shop/pods/api-7d9f-x2x4z.yaml" {
			t.Errorf("--include deployments.apps --include Pod kept %s", f.Path)
		}
		if f.Path == "events.json" || strings.HasPrefix(f.Path, "nodes/") {
			t.Errorf("--include without events or nodes kept %s", f.Path)
		}
	}

	typed, custom := shop()
	_, err := Collect(context.Background(), fakeClients(t, typed, custom...), deploymentAPI, Options{Exclude: []string{"widgets"}})
	if err == nil || !strings.Contains(err.Error(), `--exclude "widgets": the cluster has no such kind`) {
		t.Errorf("Collect with --exclude widgets = %v", err)
	}
}

func TestCollectWithoutLogs(t *testing.T) {
	typed, custom := shop()
	b, err := Collect(context.Background(), fakeClients(t, typed, custom...), deploymentAPI, Options{Since: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Files {
		if strings.HasPrefix(f.Path, "logs/") {
			t.Errorf("LogsTail 0 kept %s", f.Path)
		}
	}
	if b.Name != "tug-bundle-deployment-api-20261121T153000Z" {
		t.Errorf("default name = %q", b.Name)
	}
}

func TestWriteFile(t *testing.T) {
	b := collect(t, deploymentAPI, Options{Name: "bundle"})
	name := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := b.WriteFile(name); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("bundle mode = %o, want 600", fi.Mode().Perm())
		}
	}
	if err := b.WriteFile(name); !errors.Is(err, ErrExists) {
		t.Errorf("WriteFile over an existing file = %v, want ErrExists", err)
	}
	entries, err := os.ReadDir(filepath.Dir(name))
	if err != nil || len(entries) != 1 {
		t.Errorf("WriteFile left %v beside the bundle (%v)", entries, err)
	}

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, data)
	if len(got) != len(b.Files) {
		t.Errorf("archive has %d files, want %d", len(got), len(b.Files))
	}
	for _, f := range b.Files {
		if got["bundle/"+f.Path] != string(f.Data) {
			t.Errorf("archive's bundle/%s differs from the bundle's", f.Path)
		}
	}
}

// untar reads a gzipped tar archive into its files' contents by name.
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Mode != 0o600 {
			t.Errorf("%s has mode %o in the archive, want 600", hdr.Name, hdr.Mode)
		}
		out[hdr.Name] = string(b)
	}
}

func TestNames(t *testing.T) {
	for _, tt := range []struct{ cluster, target, want string }{
		{"prod-eu", "deployment/api", "tug-bundle-prod-eu-deployment-api-20261121T153000Z"},
		{"arn:aws:eks:eu-west-1:123456789012:cluster/prod", "certificate/web-tls", "tug-bundle-arn-aws-eks-eu-west-1-123456789012-cluster-prod-certificate-web-tls-20261121T153000Z"},
		{"", "namespace/shop", "tug-bundle-namespace-shop-20261121T153000Z"},
	} {
		if got := DefaultName(tt.cluster, tt.target, now); got != tt.want {
			t.Errorf("DefaultName(%q, %q) = %q, want %q", tt.cluster, tt.target, got, tt.want)
		}
	}
	for _, tt := range []struct{ file, want string }{
		{"/tmp/x/tug-bundle-a.tar.gz", "tug-bundle-a"},
		{"case-1234.tgz", "case-1234"},
		{"evidence", "evidence"},
	} {
		if got := NameOf(tt.file); got != tt.want {
			t.Errorf("NameOf(%q) = %q, want %q", tt.file, got, tt.want)
		}
	}
	if got := objectPath(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "", "system:node"); got != "objects/_cluster/clusterroles.rbac.authorization.k8s.io/system_node.yaml" {
		t.Errorf("objectPath of a ClusterRole = %q", got)
	}
}
