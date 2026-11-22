package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/resolve"
)

// operator is a database operator in db-system that reconciles the
// Databases and Backups of ordersCluster: its deployment, its pod, its
// leader Lease and the ClusterRole that lets it update their status.
type operator struct {
	deploy  *appsv1.Deployment
	pod     *corev1.Pod
	lease   *coordinationv1.Lease
	role    *rbacv1.ClusterRole
	binding *rbacv1.ClusterRoleBinding
}

func dbOperator() *operator {
	labels := map[string]string{"app": "db-operator"}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "db-operator", Namespace: "db-system", UID: "deploy-db-operator", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{ServiceAccountName: "db-operator",
					Containers: []corev1.Container{{Name: "manager", Image: "db-operator:v1"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	pod := whyPod("db-operator-5d4f-x2x4z", func(p *corev1.Pod) {
		p.Namespace, p.Labels = "db-system", labels
		p.OwnerReferences = controlledBy("ReplicaSet", "db-operator-5d4f", "rs-db-operator")
		p.Spec.ServiceAccountName = "db-operator"
		p.Spec.Containers[0].Name = "manager"
		st := &p.Status.ContainerStatuses[0]
		st.Name, st.RestartCount = "manager", 2
		st.State.Running.StartedAt = metav1.NewTime(whyNow.Add(-2 * time.Hour))
		st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(whyNow.Add(-2 * time.Hour)),
		}}
	})
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "db-operator-lock", Namespace: "db-system"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       new("db-operator-5d4f-x2x4z_8f2c1e"),
			LeaseDurationSeconds: new(int32(15)),
			AcquireTime:          &metav1.MicroTime{Time: whyNow.Add(-20 * time.Minute)},
			RenewTime:            &metav1.MicroTime{Time: whyNow.Add(-5 * time.Second)},
			LeaseTransitions:     new(int32(3)),
		},
	}
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "db-operator"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"db.example.com"}, Resources: []string{"databases", "backups"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"db.example.com"}, Resources: []string{"databases/status", "backups/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create"}},
		},
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "db-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "db-operator"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "db-operator", Namespace: "db-system"}},
	}
	return &operator{deploy: deploy, pod: pod, lease: lease, role: role, binding: binding}
}

func (op *operator) objects() []runtime.Object {
	out := []runtime.Object{op.deploy, op.pod, op.role, op.binding}
	if op.lease != nil {
		out = append(out, op.lease)
	}
	return out
}

// adminRBAC binds cluster-admin, which may write everything, to another
// service account, as clusters do.
func adminRBAC() []runtime.Object {
	return []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "admin-bot"},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "admin-bot", Namespace: "kube-system"}}},
	}
}

var operatorTarget = resolve.Target{Kind: resolve.KindDeployment, Name: "db-operator", Namespace: "db-system"}

func reasons(cs []Claim) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c.Severity) + " " + c.Reason + " (" + string(c.Basis) + ")"
	}
	return out
}

func TestControllerStatusOf(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(*operator) []runtime.Object
		forbid  string // a resource the caller may not list
		healthy bool
		leader  string
		want    []string // claims, as reasons gives them
		leases  int
	}{
		{
			name:    "healthy, with a recent change of leader",
			healthy: true, leader: "db-operator-5d4f-x2x4z", leases: 1,
			want: []string{"info LeaderChanged (observed)"},
		},
		{
			name: "expired lease",
			change: func(op *operator) []runtime.Object {
				op.lease.Spec.RenewTime = &metav1.MicroTime{Time: whyNow.Add(-2 * time.Minute)}
				return nil
			},
			leases: 1,
			want:   []string{"error LeaseExpired (observed)"},
		},
		{
			name: "lease of a pod that is gone",
			change: func(op *operator) []runtime.Object {
				op.lease.Spec.HolderIdentity = new("db-operator-7c9d-old00_1a2b")
				return nil
			},
			leases: 1,
			want:   []string{"warning LeaderElsewhere (inferred)"},
		},
		{
			name: "no lease",
			change: func(op *operator) []runtime.Object {
				op.lease = nil
				return nil
			},
			healthy: true,
			want:    []string{"info NoLease (observed)"},
		},
		{
			name: "a lease named alike but held by another workload's pod",
			change: func(op *operator) []runtime.Object {
				op.lease.Name = "db-operator-webhook-lock"
				op.lease.Spec.HolderIdentity = new("db-operator-webhook-6c5b-q8w2e")
				webhook := whyPod("db-operator-webhook-6c5b-q8w2e", func(p *corev1.Pod) { p.Namespace = "db-system" })
				return []runtime.Object{webhook}
			},
			healthy: true,
			want:    []string{"info NoLease (observed)"},
		},
		{
			name: "cluster-admin",
			change: func(op *operator) []runtime.Object {
				op.binding.RoleRef.Name = "cluster-admin"
				return nil
			},
			healthy: true, leader: "db-operator-5d4f-x2x4z", leases: 1,
			want: []string{"info LeaderChanged (observed)", "info BroadPermissions (inferred)"},
		},
		{
			name:    "RBAC that cannot be read",
			forbid:  "clusterrolebindings",
			healthy: true, leader: "db-operator-5d4f-x2x4z", leases: 1,
			want: []string{"info LeaderChanged (observed)", "info RBACUnreadable (observed)"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			op := dbOperator()
			var extra []runtime.Object
			if tt.change != nil {
				extra = tt.change(op)
			}
			c, _ := fakeClients(t, slices.Concat(op.objects(), adminRBAC(), extra))
			if tt.forbid != "" {
				c.Typed.(*fake.Clientset).PrependReactor("list", tt.forbid, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: tt.forbid}, "", nil)
				})
			}
			st, err := ControllerStatusOf(context.Background(), c, operatorTarget, WhyOptions{Now: func() time.Time { return whyNow }})
			if err != nil {
				t.Fatal(err)
			}
			if got := reasons(st.Findings); !slices.Equal(got, tt.want) {
				t.Errorf("findings = %q, want %q", got, tt.want)
			}
			if st.Healthy != tt.healthy || st.Leader != tt.leader || len(st.Leases) != tt.leases {
				t.Errorf("healthy %v, leader %q, %d leases; want %v, %q, %d", st.Healthy, st.Leader, len(st.Leases), tt.healthy, tt.leader, tt.leases)
			}
			if st.ServiceAccount != "db-system/db-operator" {
				t.Errorf("service account = %q", st.ServiceAccount)
			}
		})
	}
}

func TestControllerStatusOfAHealthyOperator(t *testing.T) {
	op := dbOperator()
	c, _ := fakeClients(t, slices.Concat(op.objects(), adminRBAC()))
	st, err := ControllerStatusOf(context.Background(), c, operatorTarget, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if st.Summary != "1/1 ready, leader db-operator-5d4f-x2x4z" {
		t.Errorf("summary = %q", st.Summary)
	}
	l := st.Leases[0]
	if l.Name != "db-operator-lock" || l.HolderPod != "db-operator-5d4f-x2x4z" || l.Basis != BasisObserved || l.Expired ||
		l.DurationSeconds != 15 || l.Transitions != 3 || !l.Renewed.Equal(whyNow.Add(-5*time.Second)) {
		t.Errorf("lease = %+v", l)
	}
	if len(st.Restarts) != 1 || st.Restarts[0].Count != 2 || st.Restarts[0].LastReason != "Error" || st.Restarts[0].Recent {
		t.Errorf("restarts = %+v, want manager's 2, the last two hours ago", st.Restarts)
	}
	var kinds []string
	for _, k := range st.Reconciles {
		kinds = append(kinds, k.Kind+" "+k.Resource+" "+string(k.Basis))
	}
	want := []string{"Backup backups.db.example.com inferred", "Database databases.db.example.com inferred"}
	if !slices.Equal(kinds, want) {
		t.Errorf("reconciles = %q, want %q", kinds, want)
	}
	if e := st.Reconciles[1].Evidence; e != "its service account may update databases/status through ClusterRole db-operator" {
		t.Errorf("evidence = %q", e)
	}
}

func TestControllerStatusOfAStaticPod(t *testing.T) {
	// kube-controller-manager runs as a static pod on the host's network,
	// so its Lease is held in its node's name.
	onNode := func(name string, static bool) *corev1.Pod {
		return whyPod(name, func(p *corev1.Pod) {
			p.Namespace, p.Spec.NodeName, p.Spec.HostNetwork = "kube-system", "cp-1", true
			if static {
				p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "4f9c"}
			}
		})
	}
	pod := onNode("kube-controller-manager-cp-1", true)
	proxy := onNode("kube-proxy-x2x4z", false) // a DaemonSet's pod, on the host's network too
	heldByNode := func(name string) *coordinationv1.Lease {
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity: new("cp-1_0f3e"), LeaseDurationSeconds: new(int32(15)),
				RenewTime: &metav1.MicroTime{Time: whyNow.Add(-time.Second)},
			},
		}
	}
	c, _ := fakeClients(t, []runtime.Object{pod, proxy, heldByNode("kube-controller-manager"), heldByNode("kube-scheduler")})
	st, err := ControllerStatusOf(context.Background(), c,
		resolve.Target{Kind: resolve.KindPod, Name: pod.Name, Namespace: "kube-system"}, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if st.Leader != pod.Name || len(st.Leases) != 1 || st.Leases[0].Name != "kube-controller-manager" || st.ServiceAccount != "kube-system/default" {
		t.Errorf("leader %q, leases %+v, service account %q; want %s with its own lease, kube-system/default", st.Leader, st.Leases, st.ServiceAccount, pod.Name)
	}

	// The node's name says nothing about a pod that is not static.
	st, err = ControllerStatusOf(context.Background(), c,
		resolve.Target{Kind: resolve.KindPod, Name: proxy.Name, Namespace: "kube-system"}, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if st.Leader != "" || len(st.Leases) != 0 {
		t.Errorf("kube-proxy: leader %q, leases %+v; want none", st.Leader, st.Leases)
	}
}

// managed returns a managedFields entry for fields written by a manager.
func managed(manager, subresource string, ago time.Duration, fields ...string) metav1.ManagedFieldsEntry {
	var raw []string
	for _, f := range fields {
		raw = append(raw, `"f:`+f+`":{}`)
	}
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "db.example.com/v1",
		Time: &metav1.Time{Time: whyNow.Add(-ago)}, Subresource: subresource, FieldsType: "FieldsV1",
		FieldsV1: metav1.NewFieldsV1("{" + strings.Join(raw, ",") + "}"),
	}
}

// reconcileCluster is ordersCluster with the operator and with the
// Database changed by change.
func reconcileCluster(t *testing.T, op *operator, change func(db *unstructured.Unstructured) []runtime.Object) (Clients, *unstructured.Unstructured) {
	t.Helper()
	typed, customs, db := ordersObjects()
	if change != nil {
		typed = append(typed, change(db)...)
	}
	if op != nil {
		typed = append(typed, op.objects()...)
	}
	c, _ := fakeClients(t, slices.Concat(typed, adminRBAC()), customs...)
	return c, db
}

func statusWrittenBy(manager string) func(*unstructured.Unstructured) []runtime.Object {
	return func(db *unstructured.Unstructured) []runtime.Object {
		db.SetManagedFields([]metav1.ManagedFieldsEntry{
			managed("kubectl-client-side-apply", "", 2*time.Hour, "metadata", "spec"),
			managed(manager, "status", 3*time.Minute, "status"),
		})
		return nil
	}
}

func evidence(c ControllerCandidate) []string {
	var out []string
	for _, e := range c.Evidence {
		out = append(out, string(e.Basis)+": "+e.Text)
	}
	return out
}

func TestExplainFindsTheOperator(t *testing.T) {
	c, db := reconcileCluster(t, dbOperator(), func(db *unstructured.Unstructured) []runtime.Object {
		statusWrittenBy("db-operator")(db)
		reported := warningEvent("Database", "orders", "", "ReconcileError", "backup failed", 1, time.Minute)
		reported.Name, reported.InvolvedObject.UID, reported.ReportingController = "orders.reported", db.GetUID(), "db-operator"
		return []runtime.Object{reported}
	})
	ex, err := Explain(context.Background(), c, objectOf(db, databases), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Healthy || ex.Generation != 4 || ex.ObservedGeneration != 3 {
		t.Errorf("healthy %v, generation %d/%d; want not healthy at 4, observed 3", ex.Healthy, ex.Generation, ex.ObservedGeneration)
	}
	if len(ex.Controllers) != 1 {
		t.Fatalf("controllers = %+v, want deploy/db-operator alone", ex.Controllers)
	}
	cand := ex.Controllers[0]
	if cand.Name != "db-operator" || cand.Workload != "deploy/db-operator" || cand.Namespace != "db-system" || cand.Basis != BasisInferred ||
		cand.Healthy == nil || !*cand.Healthy || cand.Leader != "db-operator-5d4f-x2x4z" {
		t.Errorf("controller = %+v", cand)
	}
	wantEvidence := []string{
		"observed: field manager db-operator has written its status, last at 2026-11-20T11:57:00Z",
		"inferred: deploy/db-operator in db-system has the same name",
		"observed: db-operator reported events about it",
		"inferred: it runs as db-system/db-operator, which may update databases/status through ClusterRole db-operator",
	}
	if got := evidence(cand); !slices.Equal(got, wantEvidence) {
		t.Errorf("evidence = %q\nwant %q", got, wantEvidence)
	}
	got := reasons(ex.Findings)
	for _, want := range []string{"error BackupFailing (observed)", "warning NotReconciled (observed)", "warning NotPickedUp (inferred)"} {
		if !slices.Contains(got, want) {
			t.Errorf("findings %q lack %q", got, want)
		}
	}
	if len(ex.Managers) != 2 || ex.Managers[0].Manager != "db-operator" || !ex.Managers[0].WritesStatus() || ex.Managers[1].WritesStatus() {
		t.Errorf("managers = %+v, want db-operator's status write first", ex.Managers)
	}
	for _, f := range ex.Findings {
		if f.Basis == "" {
			t.Errorf("finding %s has no basis", f.Reason)
		}
		if f.Reason == "NotPickedUp" && !strings.Contains(f.Next, "`tug reconcile trace databases.db.example.com/orders --logs`") {
			t.Errorf("NotPickedUp suggests %q", f.Next)
		}
	}
}

func TestExplain(t *testing.T) {
	for _, tt := range []struct {
		name string
		op   func(*operator)
		db   func(*unstructured.Unstructured) []runtime.Object
		// controllers lists the candidates as name, workload and basis.
		controllers []string
		findings    []string // claims that must be among the findings
		evidence    string   // evidence the first candidate must have
	}{
		{
			name:        "by its permissions alone",
			controllers: []string{"db-operator deploy/db-operator inferred"},
			evidence:    "inferred: it runs as db-system/db-operator, which may update databases/status through ClusterRole db-operator",
		},
		{
			name:        "a field manager named after its container",
			db:          statusWrittenBy("manager"),
			controllers: []string{"db-operator deploy/db-operator inferred"},
			evidence:    "inferred: deploy/db-operator runs a container named manager",
		},
		{
			name:        "a field manager that matches nothing",
			op:          func(op *operator) { op.binding.Subjects[0].Name = "someone-else" },
			db:          statusWrittenBy("orders-reconciler"),
			controllers: []string{"orders-reconciler - observed"},
		},
		{
			name: "an operator that is down",
			op: func(op *operator) {
				notReady(op.pod)
				op.deploy.Status.ReadyReplicas, op.deploy.Status.AvailableReplicas = 0, 0
			},
			db:          statusWrittenBy("db-operator"),
			controllers: []string{"db-operator deploy/db-operator inferred"},
			findings:    []string{"error ControllerNotReady (inferred)"},
		},
		{
			name:        "an operator without a leader",
			op:          func(op *operator) { op.lease.Spec.HolderIdentity = new("") },
			db:          statusWrittenBy("db-operator"),
			controllers: []string{"db-operator deploy/db-operator inferred"},
			findings:    []string{"error NoLeader (inferred)"},
		},
		{
			name:     "no evidence at all",
			op:       func(op *operator) { op.binding.Subjects[0].Name = "someone-else" },
			findings: []string{"info ControllerUnknown (observed)"},
		},
		{
			name: "deleted, but held by a finalizer",
			db: func(db *unstructured.Unstructured) []runtime.Object {
				db.SetDeletionTimestamp(&metav1.Time{Time: whyNow.Add(-time.Hour)})
				db.SetFinalizers([]string{"db.example.com/backups"})
				return nil
			},
			controllers: []string{"db-operator deploy/db-operator inferred"},
			findings:    []string{"warning Terminating (observed)"},
		},
		{
			name: "suspended",
			db: func(db *unstructured.Unstructured) []runtime.Object {
				_ = unstructured.SetNestedField(db.Object, true, "spec", "suspend")
				return nil
			},
			controllers: []string{"db-operator deploy/db-operator inferred"},
			findings:    []string{"warning Suspended (observed)"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			op := dbOperator()
			if tt.op != nil {
				tt.op(op)
			}
			c, db := reconcileCluster(t, op, tt.db)
			ex, err := Explain(context.Background(), c, objectOf(db, databases), WhyOptions{Now: func() time.Time { return whyNow }})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, cand := range ex.Controllers {
				got = append(got, cand.Name+" "+cmp.Or(cand.Workload, "-")+" "+string(cand.Basis))
			}
			if !slices.Equal(got, tt.controllers) {
				t.Errorf("controllers = %q, want %q", got, tt.controllers)
			}
			for _, want := range tt.findings {
				if !slices.Contains(reasons(ex.Findings), want) {
					t.Errorf("findings %q lack %q", reasons(ex.Findings), want)
				}
			}
			if tt.evidence != "" && (len(ex.Controllers) == 0 || !slices.Contains(evidence(ex.Controllers[0]), tt.evidence)) {
				t.Errorf("evidence lacks %q: %+v", tt.evidence, ex.Controllers)
			}
		})
	}
}

func TestExplainABuiltInKind(t *testing.T) {
	d := apiDeployment("1", "api:v1")
	d.UID = "deploy-api"
	d.Status = appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	d.ManagedFields = []metav1.ManagedFieldsEntry{managed("kube-controller-manager", "status", time.Minute, "status")}
	reported := warningEvent("Deployment", d.Name, "", "ScalingReplicaSet", "Scaled up replica set api-aaa to 1", 1, time.Minute)
	reported.Type, reported.InvolvedObject.UID, reported.Source.Component = corev1.EventTypeNormal, d.UID, "deployment-controller"
	pod := whyPod("api-aaa-x2x4z")
	c, _ := fakeClients(t, []runtime.Object{d, pod, reported})
	o := resolve.Object{Resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Name: d.Name, Namespace: d.Namespace}
	ex, err := Explain(context.Background(), c, o, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Controllers) != 1 {
		t.Fatalf("controllers = %+v, want kube-controller-manager alone", ex.Controllers)
	}
	cand := ex.Controllers[0]
	want := []string{
		"observed: field manager kube-controller-manager has written its status, last at 2026-11-20T11:59:00Z",
		"observed: kube-controller-manager is part of Kubernetes' control plane",
		"observed: deployment-controller reported events about it",
		"observed: deployment-controller is one of the controllers in kube-controller-manager, part of Kubernetes' control plane",
	}
	if cand.Name != "kube-controller-manager" || cand.Workload != "" || cand.Basis != BasisObserved || !slices.Equal(evidence(cand), want) {
		t.Errorf("controller = %+v\nevidence %q\nwant %q", cand, evidence(cand), want)
	}
}

func TestExplainAWellKnownOperator(t *testing.T) {
	certificates := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	cert := custom("cert-manager.io/v1", "Certificate", "web", "web-tls", "Ready=True Ready: certificate is up to date")
	labels := map[string]string{"app": "cert-manager"}
	cm := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-manager", UID: "deploy-cert-manager"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cert-manager-controller"}}}}},
	}
	c, _ := fakeClients(t, []runtime.Object{cm}, cert)
	ex, err := Explain(context.Background(), c, objectOf(cert, certificates), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Controllers) != 1 || ex.Controllers[0].Workload != "deploy/cert-manager" ||
		!slices.Contains(evidence(ex.Controllers[0]), "inferred: certificates.cert-manager.io is usually reconciled by cert-manager") {
		t.Errorf("controllers = %+v", ex.Controllers)
	}
}

func TestRBACIndex(t *testing.T) {
	sa := func(ns, name string) []rbacv1.Subject {
		return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: ns}}
	}
	objs := []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "backup-writer"}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"db.example.com"}, Resources: []string{"backups/status"}, Verbs: []string{"patch"}},
		}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "db-writer", Namespace: "web"}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"db.example.com"}, Resources: []string{"databases/status"}, Verbs: []string{"update"}},
			{APIGroups: []string{"db.example.com"}, Resources: []string{"databases"}, Verbs: []string{"update"}}, // not its status
			{APIGroups: []string{"db.example.com"}, Resources: []string{"databases/status"}, Verbs: []string{"get"}},
		}},
		// A RoleBinding's service account without a namespace is in the
		// binding's.
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "backups", Namespace: "web"},
			RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "backup-writer"}, Subjects: sa("", "op")},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "dbs", Namespace: "web"},
			RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "db-writer"}, Subjects: sa("ops", "op")},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "users"},
			RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: "backup-writer"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "alice"}}},
	}
	c, _ := fakeClients(t, objs)
	x, err := loadRBAC(context.Background(), c.Typed)
	if err != nil {
		t.Fatal(err)
	}
	describe := func(gs []statusGrant) []string {
		var out []string
		for _, g := range gs {
			out = append(out, fmt.Sprintf("%s %s/%s by %s in %q", g.account, g.group, g.resource, g.role, g.namespace))
		}
		slices.Sort(out)
		return out
	}
	for _, tt := range []struct {
		resource, ns string
		want         []string
	}{
		{"backups", "web", []string{`web/op db.example.com/backups by ClusterRole backup-writer in "web"`}},
		{"backups", "other", nil},
		{"databases", "web", []string{`ops/op db.example.com/databases by Role web/db-writer in "web"`}},
	} {
		got := describe(x.writers(schema.GroupResource{Group: "db.example.com", Resource: tt.resource}, tt.ns))
		if !slices.Equal(got, tt.want) {
			t.Errorf("writers(%s, %s) = %q, want %q", tt.resource, tt.ns, got, tt.want)
		}
	}
	if got := describe(x.of(serviceAccount{"ops", "op"})); len(got) != 1 {
		t.Errorf("grants of ops/op = %q, want the Role's one", got)
	}
}
