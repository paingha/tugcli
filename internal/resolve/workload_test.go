package resolve

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func deployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
	}
}

func pod(name, app string, owner string, ready bool, age time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "web",
			Labels:            map[string]string{"app": app},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	if owner != "" {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: app, Controller: &yes}}
	}
	return p
}

func objects() []runtime.Object {
	return []runtime.Object{
		deployment("api-server"),
		deployment("api-gateway"),
		deployment("worker"),
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "web"},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
		},
		pod("api-server-7d9f-aaaaa", "api-server", "ReplicaSet", false, time.Minute),
		pod("api-server-7d9f-bbbbb", "api-server", "ReplicaSet", true, time.Hour),
		pod("api-server-7d9f-ccccc", "api-server", "ReplicaSet", true, time.Second),
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "web"},
			Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "migrate"}}},
		},
		pod("debug", "debug", "", true, time.Minute),
		pod("migrate-x1", "migrate", "Job", false, time.Minute),
	}
}

func TestWorkloadResolution(t *testing.T) {
	cs := fake.NewSimpleClientset(objects()...)
	ctx := context.Background()
	all, err := Workloads(ctx, cs, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	// 3 deployments, 1 statefulset, 1 job and 2 standalone pods (debug, the
	// Job's pod).
	if len(all) != 7 {
		t.Fatalf("got %d candidates: %v", len(all), all)
	}

	tests := []struct {
		query string
		want  []string
	}{
		{"api", []string{"deploy/api-gateway", "deploy/api-server"}}, // tie: ask
		{"api-s", []string{"deploy/api-server"}},
		{"server", []string{"deploy/api-server"}},
		{"db", []string{"sts/db"}},
		{"debug", []string{"pod/debug"}},
		{"migrate", []string{"job/migrate"}},
		{"migrate-x", []string{"pod/migrate-x1"}},
		{"job/m", []string{"job/migrate"}},
		{"wrk", []string{"deploy/worker"}},
		{"deploy/db", nil},
		{"sts/d", []string{"sts/db"}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, err := ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got := MatchTargets(q, all)
			var names []string
			for _, g := range got {
				names = append(names, g.String())
			}
			if len(names) != len(tt.want) {
				t.Fatalf("MatchTargets(%q) = %v, want %v", tt.query, names, tt.want)
			}
			for i := range names {
				if names[i] != tt.want[i] {
					t.Fatalf("MatchTargets(%q) = %v, want %v", tt.query, names, tt.want)
				}
			}
		})
	}
}

func TestWorkloadsBeatPodsAtTheSameRank(t *testing.T) {
	cands := []Target{
		{Kind: KindPod, Name: "cache-0"},
		{Kind: KindStatefulSet, Name: "cache"},
	}
	got := MatchTargets(Query{Name: "cach"}, cands)
	if len(got) != 1 || got[0].Kind != KindStatefulSet {
		t.Fatalf("got %v", got)
	}
}

func TestPodsReadyFirstThenNewest(t *testing.T) {
	cs := fake.NewSimpleClientset(objects()...)
	all, _ := Workloads(context.Background(), cs, "web", KindDeployment)
	got := MatchTargets(Query{Name: "api-server"}, all)
	pods, err := Pods(context.Background(), cs, got[0])
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pods {
		names = append(names, p.Name)
	}
	want := []string{"api-server-7d9f-ccccc", "api-server-7d9f-bbbbb", "api-server-7d9f-aaaaa"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

func TestRunningPod(t *testing.T) {
	ctx := context.Background()
	pending := pod("worker-5c6d-aaaaa", "worker", "ReplicaSet", false, time.Minute)
	pending.Status.Phase = corev1.PodPending
	cs := fake.NewSimpleClientset(append(objects(), pending)...)
	all, _ := Workloads(ctx, cs, "web", "")
	byName := map[string]Target{}
	for _, t := range all {
		byName[t.String()] = t
	}

	p, err := RunningPod(ctx, cs, byName["deploy/api-server"])
	if err != nil || p.Name != "api-server-7d9f-ccccc" {
		t.Errorf("RunningPod(deploy/api-server) = %v, %v, want the newest ready pod", p, err)
	}
	if _, err := RunningPod(ctx, cs, byName["deploy/worker"]); err == nil || err.Error() != "deploy/worker has no running pod (first is worker-5c6d-aaaaa, Pending)" {
		t.Errorf("RunningPod(deploy/worker) error = %v, want no running pod", err)
	}
	if _, err := RunningPod(ctx, cs, byName["sts/db"]); err == nil || err.Error() != "sts/db has no pods" {
		t.Errorf("RunningPod(sts/db) error = %v, want no pods", err)
	}
}

func TestParseQuery(t *testing.T) {
	for _, bad := range []string{"", "deploy/", "/api"} {
		if _, err := ParseQuery(bad); err == nil {
			t.Errorf("ParseQuery(%q) succeeded", bad)
		}
	}
	q, err := ParseQuery("svc/api")
	if err != nil || q.Kind != "" || q.Resource != "svc" || q.Name != "api" {
		t.Fatalf("ParseQuery(svc/api) = %+v, %v, want resource svc", q, err)
	}
	if err := q.RequireWorkload(); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Errorf("RequireWorkload() for svc/api = %v, want unsupported kind", err)
	}
	q, err = ParseQuery("statefulsets/db")
	if err != nil || q.Kind != KindStatefulSet || q.Name != "db" {
		t.Fatalf("got %+v %v", q, err)
	}
	q, err = ParseQuery("jobs/migrate")
	if err != nil || q.Kind != KindJob || q.Name != "migrate" {
		t.Fatalf("ParseQuery(jobs/migrate) = %+v, %v, want job migrate", q, err)
	}
	if err := q.RequireWorkload(); err != nil {
		t.Errorf("RequireWorkload() for jobs/migrate = %v", err)
	}
}

func TestDefaultContainer(t *testing.T) {
	p := pod("x", "x", "", true, 0)
	if DefaultContainer(p) != "main" {
		t.Fatal("first container should be the default")
	}
	p.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "sidecar"}
	if DefaultContainer(p) != "sidecar" {
		t.Fatal("annotation ignored")
	}
}
