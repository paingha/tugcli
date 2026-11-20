package resolve

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	// 3 deployments, 1 statefulset, 2 standalone pods (debug, the Job's pod).
	if len(all) != 6 {
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
		{"migrate", []string{"pod/migrate-x1"}},
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

func TestParseQuery(t *testing.T) {
	for _, bad := range []string{"", "svc/api", "deploy/"} {
		if _, err := ParseQuery(bad); err == nil {
			t.Errorf("ParseQuery(%q) succeeded", bad)
		}
	}
	q, err := ParseQuery("statefulsets/db")
	if err != nil || q.Kind != KindStatefulSet || q.Name != "db" {
		t.Fatalf("got %+v %v", q, err)
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
