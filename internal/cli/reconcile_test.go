package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/paingha/tugcli/internal/ops"
)

// reconcileShopObjects are shopCustomObjects with database/orders a
// generation ahead of its status, which db-operator last wrote.
func reconcileShopObjects() []runtime.Object {
	objs := shopCustomObjects()
	now := time.Now()
	fields := func(f string) *metav1.FieldsV1 { return metav1.NewFieldsV1(`{"f:` + f + `":{}}`) }
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetName() == "orders" {
			u.SetGeneration(2)
			u.SetCreationTimestamp(metav1.NewTime(now.Add(-48 * time.Hour)))
			u.SetManagedFields([]metav1.ManagedFieldsEntry{
				{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate,
					Time: &metav1.Time{Time: now.Add(-time.Hour)}, FieldsType: "FieldsV1", FieldsV1: fields("spec")},
				{Manager: "db-operator", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status",
					Time: &metav1.Time{Time: now.Add(-3 * time.Minute)}, FieldsType: "FieldsV1", FieldsV1: fields("status")},
			})
		}
	}
	return objs
}

const operatorPod = "db-operator-5d4f-x2x4z"

// dbSystemObjects are a database operator: a deployment with one ready pod.
func dbSystemObjects() []runtime.Object {
	labels := map[string]string{"app": "db-operator"}
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "db-operator", Namespace: "db-system", UID: "deploy-db-operator", Generation: 1},
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
					ServiceAccountName: "db-operator", Containers: []corev1.Container{{Name: "manager", Image: "db-operator:v1"}}}},
			},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: operatorPod, Namespace: "db-system", UID: "pod-db-operator", Labels: labels, CreationTimestamp: started},
			Spec:       corev1.PodSpec{NodeName: "node-1", ServiceAccountName: "db-operator", Containers: []corev1.Container{{Name: "manager"}}},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}}},
			},
		},
	}
}

// operatorLogs is what the operator's pod logged, with kubelet timestamps.
func operatorLogs() string {
	at := func(ago time.Duration) string { return time.Now().Add(-ago).UTC().Format(time.RFC3339Nano) }
	return at(4*time.Minute) + ` {"level":"error","msg":"Reconciler error","Database":{"name":"orders","namespace":"shop"},"error":"bucket not found"}` + "\n" +
		at(3*time.Minute) + " reconciled shop/inventory\n" +
		at(2*time.Minute) + " patched status of database-orders\n"
}

// reconcileAPIServer serves a cluster with the shop namespace of
// reconcileShopObjects and a database operator in db-system that holds its
// leader Lease, may update databases' status, and has logged about orders.
// A watch of the events in shop sends one more BackupFailed about orders,
// then stays open until the client goes.
func reconcileAPIServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	shop := newFakeNamespace(t, reconcileShopObjects()...)
	dbSystem := newFakeNamespace(t, dbSystemObjects()...)
	empty := newFakeNamespace(t)
	lease := coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "db-operator-lock", Namespace: "db-system"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: new(operatorPod + "_8f2c1e"), LeaseDurationSeconds: new(int32(15)),
			AcquireTime: &metav1.MicroTime{Time: time.Now().Add(-time.Hour)}, RenewTime: &metav1.MicroTime{Time: time.Now().Add(-2 * time.Second)},
			LeaseTransitions: new(int32(1)),
		},
	}
	rbac := map[string]any{
		"clusterroles": rbacv1.ClusterRoleList{Items: []rbacv1.ClusterRole{{
			ObjectMeta: metav1.ObjectMeta{Name: "db-operator"},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"db.example.com"},
				Resources: []string{"databases/status", "backups/status"}, Verbs: []string{"update", "patch"}}},
		}}},
		"clusterrolebindings": rbacv1.ClusterRoleBindingList{Items: []rbacv1.ClusterRoleBinding{{
			ObjectMeta: metav1.ObjectMeta{Name: "db-operator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "db-operator"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "db-operator", Namespace: "db-system"}},
		}}},
		"roles":        rbacv1.RoleList{},
		"rolebindings": rbacv1.RoleBindingList{},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && !strings.Contains(r.URL.Path, "/selfsubject") {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		if body, ok := fakeDiscovery[p]; ok {
			fmt.Fprint(w, body)
			return
		}
		const rbacPrefix = "/apis/rbac.authorization.k8s.io/v1/"
		switch {
		case p == "/version":
			fmt.Fprint(w, `{"major":"1","minor":"35","gitVersion":"v1.35.1"}`)
			return
		case p == "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			fmt.Fprint(w, `{"kind":"SelfSubjectReview","apiVersion":"authentication.k8s.io/v1","status":{"userInfo":{"username":"alice"}}}`)
			return
		case p == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			fmt.Fprint(w, `{"kind":"SelfSubjectAccessReview","apiVersion":"authorization.k8s.io/v1","status":{"allowed":true}}`)
			return
		case p == "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","apiVersion":"v1","items":[{"metadata":{"name":"default"}},{"metadata":{"name":"shop"}},{"metadata":{"name":"db-system"}}]}`)
			return
		case strings.HasPrefix(p, rbacPrefix) && rbac[strings.TrimPrefix(p, rbacPrefix)] != nil:
			writeJSON(t, w, rbac[strings.TrimPrefix(p, rbacPrefix)])
			return
		case p == "/apis/apps/v1/deployments" || p == "/apis/apps/v1/statefulsets" || p == "/apis/apps/v1/daemonsets":
			listEverywhere(t, w, strings.TrimPrefix(p, "/apis/apps/v1/"), shop, dbSystem)
			return
		case p == "/apis/coordination.k8s.io/v1/namespaces/db-system/leases":
			writeJSON(t, w, coordinationv1.LeaseList{Items: []coordinationv1.Lease{lease}})
			return
		case p == "/api/v1/namespaces/db-system/pods/"+operatorPod+"/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, operatorLogs())
			return
		case p == "/api/v1/namespaces/shop/events" && r.URL.Query().Get("watch") == "true":
			watchShopEvents(t, w, r)
			return
		}
		if ns, parts, ok := namespacedParts(p); ok {
			switch ns {
			case "shop":
				shop.serve(w, r, parts)
			case "db-system":
				dbSystem.serve(w, r, parts)
			default:
				empty.serve(w, r, parts)
			}
			return
		}
		writeStatus(w, http.StatusNotFound, "NotFound", p+" not served")
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

// listEverywhere lists a resource in every namespace the fake serves.
func listEverywhere(t *testing.T, w http.ResponseWriter, resource string, namespaces ...*fakeNamespace) {
	items := []runtime.Object{}
	for _, ns := range namespaces {
		ns.mu.Lock()
		items = append(items, ns.objects[resource]...)
		ns.mu.Unlock()
	}
	res := fakeResources[resource]
	writeJSON(t, w, map[string]any{"kind": res.listKind, "apiVersion": res.groupVersion, "metadata": map[string]any{}, "items": items})
}

// watchShopEvents answers a watch of shop's events with one new event about
// orders, then holds the watch open until the client goes.
func watchShopEvents(t *testing.T, w http.ResponseWriter, r *http.Request) {
	e := shopEvent("Database", "orders", "BackupFailed", "backup orders-nightly failed again", 4)
	e.TypeMeta = metav1.TypeMeta{Kind: "Event", APIVersion: "v1"}
	e.Name, e.UID, e.InvolvedObject.UID = "orders.watched", "event-watched", "database-orders"
	e.LastTimestamp = metav1.NewTime(time.Now())
	b, err := json.Marshal(map[string]any{"type": "ADDED", "object": e})
	if err != nil {
		t.Errorf("fake API server: encoding a watch event: %v", err)
		return
	}
	fmt.Fprintf(w, "%s\n", b)
	_ = http.NewResponseController(w).Flush() // a server that cannot flush fails the test anyway
	<-r.Context().Done()
}

var (
	minutesAndSeconds = regexp.MustCompile(`\b(\d+m)\d+s\b`)
	seconds           = regexp.MustCompile(`\b\d+s\b`)
	padding           = regexp.MustCompile(` {2,}`)
)

// steady drops from tug's output what changes while a test runs: the
// seconds of ages, and the padding that an age of another width changes.
func steady(s string) string {
	s = minutesAndSeconds.ReplaceAllString(s, "$1")
	s = seconds.ReplaceAllString(s, "Ns")
	return padding.ReplaceAllString(s, "  ")
}

// hasSteady reports whether out has want, once both are steady.
func hasSteady(out, want string) bool { return strings.Contains(steady(out), steady(want)) }

// connectToReconcileCluster imports and connects to reconcileAPIServer.
func connectToReconcileCluster(t *testing.T) *atomic.Int32 {
	t.Helper()
	isolate(t)
	srv, writes := reconcileAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	return writes
}

func TestReconcileStatus(t *testing.T) {
	writes := connectToReconcileCluster(t)

	// The session's namespace is default; the operator runs in db-system.
	out := mustTug(t, "reconcile", "status", "db-operator")
	for _, want := range []string{
		"deploy/db-operator in db-system is healthy: 1/1 ready, leader db-operator-5d4f-x2x4z\n",
		"service account db-system/db-operator\n",
		"db-operator-5d4f-x2x4z (leader)   Running",
		"lease/db-operator-lock   db-operator-5d4f-x2x4z_8f2c1e   2s ago    15s        1         observed",
		"Reconciles, as its permissions tell (inferred):\n",
		"Database   databases.db.example.com   its service account may update databases/status through ClusterRole db-operator",
		"Backup     backups.db.example.com",
	} {
		if !hasSteady(out, want) {
			t.Errorf("tug reconcile status db-operator lacks %q:\n%s", want, out)
		}
	}

	var st ops.ControllerStatus
	if err := json.Unmarshal([]byte(mustTug(t, "reconcile", "status", "deploy/db-operator", "-n", "db-system", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Healthy || st.Leader != operatorPod || len(st.Leases) != 1 || st.Leases[0].Basis != ops.BasisObserved || len(st.Reconciles) != 2 {
		t.Errorf("tug reconcile status -o json = %+v", st)
	}

	r := tug(t, "reconcile", "status", "db-operator", "-n", "shop")
	if want := `nothing in namespace shop matches "db-operator"; leave out -n to look in every namespace`; r.code == 0 || !strings.Contains(r.stderr, want) {
		t.Errorf("tug reconcile status -n shop = %d, stderr %q, want %q", r.code, r.stderr, want)
	}
	if r := tug(t, "reconcile", "status", "db-operator", "-o", "name"); r.code == 0 || !strings.Contains(r.stderr, "supports -o table, json or yaml") {
		t.Errorf("tug reconcile status -o name = %d, stderr %q", r.code, r.stderr)
	}
	if writes.Load() != 0 {
		t.Fatalf("reconcile status wrote to the cluster %d times", writes.Load())
	}
}

func TestReconcileExplain(t *testing.T) {
	writes := connectToReconcileCluster(t)

	r := tug(t, "reconcile", "explain", "db/orders", "-n", "shop")
	if r.code != 1 {
		t.Fatalf("tug reconcile explain db/orders: exit %d, want 1 for an object that is not ready\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"database/orders in shop is not ready: BackupFailing: the latest backup failed (owns 2, 1 not ready)\n",
		"generation 2, last reconciled at 1: its controller has not caught up\n",
		"Controllers that may reconcile it, likeliest first:\n" +
			"  deploy/db-operator in db-system is healthy: 1/1 ready, leader db-operator-5d4f-x2x4z (inferred)\n" +
			"    observed field manager db-operator has written its status, last at ",
		"    inferred deploy/db-operator in db-system has the same name\n",
		"    inferred it runs as db-system/db-operator, which may update databases/status through ClusterRole db-operator\n",
		"FIELD MANAGER               OPERATION   WROTE    LAST WRITE\ndb-operator                 Update      status   3m ago\n",
		"error    database/orders: BackupFailing (observed)\n",
		"warning  database/orders: NotPickedUp (inferred)\n",
		"               tug reconcile trace databases.db.example.com/orders --logs -n shop\n",
	} {
		if !hasSteady(r.stdout, want) {
			t.Errorf("tug reconcile explain db/orders lacks %q:\n%s", want, r.stdout)
		}
	}

	var ex ops.Explanation
	r = tug(t, "reconcile", "explain", "db/orders", "-n", "shop", "-o", "json")
	if err := json.Unmarshal([]byte(r.stdout), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.Healthy || len(ex.Controllers) != 1 || ex.Controllers[0].Workload != "deploy/db-operator" || ex.Generation != 2 || ex.ObservedGeneration != 1 {
		t.Errorf("tug reconcile explain -o json = %+v", ex)
	}
	for _, f := range ex.Findings {
		if f.Basis != ops.BasisObserved && f.Basis != ops.BasisInferred {
			t.Errorf("finding %s has basis %q", f.Reason, f.Basis)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("reconcile explain wrote to the cluster %d times", writes.Load())
	}
}

func TestReconcileTrace(t *testing.T) {
	writes := connectToReconcileCluster(t)

	r := tug(t, "reconcile", "trace", "db/orders", "-n", "shop", "--logs")
	if r.code != 0 {
		t.Fatalf("tug reconcile trace: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"database/orders in shop: generation 2, last reconciled at 1: its controller has not caught up\n",
		"logs from deploy/db-operator in db-system, the likeliest controller (inferred)\n",
		"AGE   SOURCE          OBJECT                       WHAT\n",
		"2d    metadata        database/orders              created\n",
		"60m   managedFields   database/orders              kubectl-client-side-apply last wrote spec (Update)\n",
		"10m   condition       database/orders              Ready became False: BackupFailing: the latest backup failed\n",
		`4m    log             pod/db-operator-5d4f-x2x4z   (inferred) {"level":"error","msg":"Reconciler error","Database":{"name":"orders","namespace":"shop"},"error":"bucket not found"}`,
		"3m    managedFields   database/orders              db-operator last wrote status (Update)\n",
		"2m    event           database/orders              Warning BackupFailed: backup orders-nightly failed (x3)\n",
		"2m    log             pod/db-operator-5d4f-x2x4z   patched status of database-orders\n",
	} {
		if !hasSteady(r.stdout, want) {
			t.Errorf("tug reconcile trace lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "inventory") {
		t.Errorf("tug reconcile trace shows a log line about another object:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "note: Kubernetes keeps no history of reconciles.") {
		t.Errorf("tug reconcile trace does not say that Kubernetes keeps no history:\n%s", r.stderr)
	}

	var tl ops.Timeline
	if err := json.Unmarshal([]byte(mustTug(t, "reconcile", "trace", "db/orders", "-n", "shop", "--controller", "db-operator", "-o", "json")), &tl); err != nil {
		t.Fatal(err)
	}
	if tl.Controller != "deploy/db-operator" || tl.ControllerBasis != ops.BasisObserved || len(tl.Notes) == 0 || tl.Notes[0] != ops.NoReconcileHistory {
		t.Errorf("tug reconcile trace --controller -o json = controller %s (%s), notes %q", tl.Controller, tl.ControllerBasis, tl.Notes)
	}
	logs := 0
	for _, e := range tl.Entries {
		if e.Source == ops.SourceLog {
			logs++
		}
	}
	if logs != 2 {
		t.Errorf("trace has %d log entries, want 2:\n%+v", logs, tl.Entries)
	}
	if r := tug(t, "reconcile", "trace", "db/orders", "-n", "shop", "--since", "0s"); r.code == 0 || !strings.Contains(r.stderr, "--since wants a positive duration") {
		t.Errorf("tug reconcile trace --since 0s = %d, stderr %q", r.code, r.stderr)
	}
	if writes.Load() != 0 {
		t.Fatalf("reconcile trace wrote to the cluster %d times", writes.Load())
	}
}

func TestReconcileEvents(t *testing.T) {
	writes := connectToReconcileCluster(t)

	out := mustTug(t, "reconcile", "events", "db/orders", "-n", "shop")
	if want := "LAST SEEN   TYPE      OBJECT            REASON         FROM   MESSAGE\n2m (x3)     Warning   database/orders   BackupFailed   -      backup orders-nightly failed\n"; steady(out) != steady(want) {
		t.Errorf("tug reconcile events db/orders =\n%s\nwant\n%s", out, want)
	}
	var records []ops.EventRecord
	if err := json.Unmarshal([]byte(mustTug(t, "reconcile", "events", "db/orders", "-n", "shop", "-o", "json")), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Reason != "BackupFailed" || records[0].Count != 3 {
		t.Errorf("tug reconcile events -o json = %+v", records)
	}

	// -f prints what there is, then what the watch brings, until Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncWriter
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"reconcile", "events", "db/orders", "-n", "shop", "-f"}, strings.NewReader(""), &stdout, &stderr)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(stdout.String(), "backup orders-nightly failed again (x4)") {
		if time.Now().After(deadline) {
			t.Fatalf("the watched event did not arrive\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d after Ctrl-C, want 0\nstderr: %s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tug did not stop following after its context was cancelled")
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "  Warning  database/orders  BackupFailed: backup orders-nightly failed (x3)") ||
		!strings.HasSuffix(lines[1], "  Warning  database/orders  BackupFailed: backup orders-nightly failed again (x4)") {
		t.Errorf("tug reconcile events -f =\n%s", stdout.String())
	}
	if writes.Load() != 0 {
		t.Fatalf("reconcile events wrote to the cluster %d times", writes.Load())
	}
}
