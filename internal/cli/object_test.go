package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/paingha/tugcli/internal/ops"
)

// fakeDiscovery is the API discovery the fake API server answers, by path:
// what the shop namespace serves, and a custom resource group with a short
// name.
var fakeDiscovery = func() map[string]string {
	resources := func(gv string, rs ...metav1.APIResource) string {
		for i := range rs {
			rs[i].Namespaced = true
			rs[i].Verbs = metav1.Verbs{"get", "list"}
			rs[i].SingularName = strings.ToLower(rs[i].Kind)
		}
		b, _ := json.Marshal(metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: gv, APIResources: rs})
		return string(b)
	}
	group := func(name string) metav1.APIGroup {
		v := metav1.GroupVersionForDiscovery{GroupVersion: name + "/v1", Version: "v1"}
		return metav1.APIGroup{Name: name, Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v}
	}
	groups, _ := json.Marshal(metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
		Groups:   []metav1.APIGroup{group("apps"), group("batch"), group("db.example.com")},
	})
	return map[string]string{
		"/api":  `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`,
		"/apis": string(groups),
		"/api/v1": resources("v1",
			metav1.APIResource{Name: "pods", Kind: "Pod", ShortNames: []string{"po"}},
			metav1.APIResource{Name: "services", Kind: "Service", ShortNames: []string{"svc"}},
			metav1.APIResource{Name: "configmaps", Kind: "ConfigMap", ShortNames: []string{"cm"}},
			metav1.APIResource{Name: "secrets", Kind: "Secret"},
			metav1.APIResource{Name: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", ShortNames: []string{"pvc"}},
			metav1.APIResource{Name: "events", Kind: "Event", ShortNames: []string{"ev"}}),
		"/apis/apps/v1": resources("apps/v1",
			metav1.APIResource{Name: "deployments", Kind: "Deployment", ShortNames: []string{"deploy"}},
			metav1.APIResource{Name: "replicasets", Kind: "ReplicaSet", ShortNames: []string{"rs"}},
			metav1.APIResource{Name: "statefulsets", Kind: "StatefulSet", ShortNames: []string{"sts"}},
			metav1.APIResource{Name: "daemonsets", Kind: "DaemonSet", ShortNames: []string{"ds"}},
			metav1.APIResource{Name: "controllerrevisions", Kind: "ControllerRevision"}),
		"/apis/batch/v1": resources("batch/v1",
			metav1.APIResource{Name: "jobs", Kind: "Job"}),
		"/apis/db.example.com/v1": resources("db.example.com/v1",
			metav1.APIResource{Name: "databases", Kind: "Database", ShortNames: []string{"db"}},
			metav1.APIResource{Name: "backups", Kind: "Backup"}),
	}
}()

// shopDatabase returns a db.example.com custom resource in shop with one
// Ready condition.
func shopDatabase(kind, name, status, reason, msg string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "db.example.com/v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": "shop", "uid": strings.ToLower(kind) + "-" + name, "generation": int64(1)},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"conditions": []any{map[string]any{
				"type": "Ready", "status": status, "reason": reason, "message": msg,
				"lastTransitionTime": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
			}},
		},
	}}
	return u
}

func ownerRefs(owner *unstructured.Unstructured) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes}}
}

// shopCustomObjects are a failing Database that owns a failing Backup and a
// Secret, and a healthy Database.
func shopCustomObjects() []runtime.Object {
	orders := shopDatabase("Database", "orders", "False", "BackupFailing", "the latest backup failed")
	backup := shopDatabase("Backup", "orders-nightly", "False", "UploadFailed", `bucket "orders-backups" not found`)
	backup.SetOwnerReferences(ownerRefs(orders))
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-credentials", Namespace: "shop", UID: "secret-orders-credentials", OwnerReferences: ownerRefs(orders)},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	event := shopEvent("Database", "orders", "BackupFailed", "backup orders-nightly failed", 3)
	event.InvolvedObject.UID = orders.GetUID()
	inventory := shopDatabase("Database", "inventory", "True", "Serving", "accepting connections")
	return []runtime.Object{orders, backup, creds, event, inventory}
}

func TestWhyOfACustomResource(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	unhealthy := func(args ...string) string {
		t.Helper()
		r := tug(t, append([]string{"why"}, args...)...)
		if r.code != 1 {
			t.Fatalf("tug why %v: exit %d, want 1 for an object that is not ready\n%s%s", args, r.code, r.stdout, r.stderr)
		}
		return r.stdout
	}
	out := unhealthy("db/orders", "-n", "shop") // db is the CRD's short name
	for _, want := range []string{
		"database/orders in shop is not ready: BackupFailing: the latest backup failed (owns 2, 1 not ready)\n",
		"CONDITION   STATUS   REASON          SINCE\nReady       False    BackupFailing   10m\n",
		"error    database/orders: BackupFailing\n         Ready is False: the latest backup failed\n",
		"         next: see its status and events:\n               tug describe databases.db.example.com/orders -n shop\n",
		"error    backup/orders-nightly: UploadFailed\n",
		"         next: see why:\n               tug why backups.db.example.com/orders-nightly -n shop\n",
		"database/orders   BackupFailed   backup orders-nightly failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug why db/orders lacks %q:\n%s", want, out)
		}
	}

	out = mustTug(t, "why", "databases.db.example.com/inv", "-n", "shop")
	if !strings.Contains(out, "database/inventory in shop is healthy: Serving: accepting connections\n") || !strings.Contains(out, "Nothing wrong found.") {
		t.Errorf("tug why databases.db.example.com/inv:\n%s", out)
	}

	var d ops.Diagnosis
	if err := json.Unmarshal([]byte(unhealthy("database/orders", "-n", "shop", "-o", "json")), &d); err != nil {
		t.Fatal(err)
	}
	if d.Healthy || d.Target != "database/orders" || len(d.Conditions) != 1 || d.Conditions[0].Reason != "BackupFailing" {
		t.Errorf("tug why database/orders -o json = %+v", d)
	}

	for _, tt := range []struct{ args, want string }{
		{"why widget/x -n shop", `the cluster has no resource type "widget"; list them with ` + "`tug api-resources`"},
		{"why db/nothing -n shop", `no databases.db.example.com in namespace shop matches "nothing"; use -n for another namespace`},
		{"logs db/orders -n shop", `unsupported kind "db"`},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("why wrote to the cluster %d times", writes.Load())
	}
}

func TestTreeOfACustomResource(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	got := mustTug(t, "tree", "db/orders", "-n", "shop")
	for _, want := range []string{
		"database/orders                ✗ BackupFailing: the latest backup failed\n",
		"├── backup/orders-nightly      ✗ UploadFailed: bucket \"orders-backups\" not found\n",
		"└── secret/orders-credentials  · ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tug tree db/orders lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("tug tree showed a Secret's data:\n%s", got)
	}

	var root ops.TreeNode
	if err := json.Unmarshal([]byte(mustTug(t, "tree", "db/orders", "-n", "shop", "-o", "json")), &root); err != nil {
		t.Fatal(err)
	}
	if root.Kind != "Database" || len(root.Children) != 2 || root.Children[1].Health != ops.HealthNone {
		t.Errorf("tug tree -o json = %+v, want the Database with a Backup and a Secret of no health", root)
	}
	if writes.Load() != 0 {
		t.Fatalf("tree wrote to the cluster %d times", writes.Load())
	}
}
