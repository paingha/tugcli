package ops

import (
	"context"
	"fmt"
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

func workloadsDeployment(name string, desired, ready, updated, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &desired,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: updated, ReadyReplicas: ready, AvailableReplicas: ready,
		},
	}
}

// workloadsPod returns a pod of app, ready unless a waiting reason is given,
// controlled by owner (Kind/name) when it is not "".
func workloadsPod(name, app, owner, waiting string, restartCount int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web", Labels: map[string]string{"app": app}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}, {Name: "proxy"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, RestartCount: restartCount, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "proxy", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	if kind, n, ok := strings.Cut(owner, "/"); ok {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: n, Controller: &yes}}
	}
	if waiting != "" {
		p.Status.Conditions[0].Status = corev1.ConditionFalse
		p.Status.ContainerStatuses[0].Ready = false
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}
	}
	return p
}

func TestWorkloads(t *testing.T) {
	one, three := int32(1), int32(3)
	objects := []runtime.Object{
		workloadsDeployment("checkout", 2, 2, 2, 2),
		workloadsPod("checkout-a", "checkout", "ReplicaSet/checkout-6b8c", "", 0),
		workloadsPod("checkout-b", "checkout", "ReplicaSet/checkout-6b8c", "", 1),

		workloadsDeployment("billing", 1, 0, 1, 1),
		workloadsPod("billing-x", "billing", "ReplicaSet/billing-5d4f", "CrashLoopBackOff", 7),

		workloadsDeployment("rolling", 3, 3, 1, 4),
		workloadsDeployment("idle", 0, 0, 0, 0),

		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger", Namespace: "web", Generation: 1},
			Spec:       appsv1.StatefulSetSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ledger"}}},
			Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
		},
		workloadsPod("ledger-0", "ledger", "StatefulSet/ledger", "", 0),

		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "web", Generation: 1},
			Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}}},
			Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberReady: 2, NumberAvailable: 2},
		},

		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "web"},
			Spec:       batchv1.JobSpec{Completions: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "migrate"}}},
			Status: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}},
		},
		workloadsPod("migrate-q2x", "migrate", "Job/migrate", "", 0),
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "reindex", Namespace: "web"},
			Spec:       batchv1.JobSpec{Completions: &three, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "reindex"}}},
			Status: batchv1.JobStatus{Failed: 4, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
			}},
		},

		workloadsPod("debug", "debug", "", "", 0),
		workloadsPod("operator-run", "custom", "Widget/w1", "ImagePullBackOff", 0),
	}
	cs := fake.NewSimpleClientset(objects...)

	got, err := Workloads(context.Background(), cs, "web")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, w := range got {
		lines = append(lines, fmt.Sprintf("%s %d/%d %s %q restarts=%d", w.Target, w.Ready, w.Desired, w.Health, w.Status, w.Restarts))
	}
	want := []string{
		`deploy/billing 0/1 not-ready "CrashLoopBackOff" restarts=7`,
		`deploy/checkout 2/2 ready "Ready" restarts=1`,
		`deploy/idle 0/0 ready "Scaled to 0" restarts=0`,
		`deploy/rolling 3/3 not-ready "1/3 updated, 3 old" restarts=0`,
		`sts/ledger 1/1 ready "Ready" restarts=0`,
		`ds/agent 2/3 not-ready "Not ready" restarts=0`,
		`job/migrate 1/1 ready "complete, 1/1 succeeded" restarts=0`,
		`job/reindex 0/3 not-ready "failed: BackoffLimitExceeded" restarts=0`,
		`pod/debug 2/2 ready "Running" restarts=0`,
		`pod/operator-run 1/2 not-ready "ImagePullBackOff" restarts=0`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Workloads(web) =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got[0].Target.Selector == nil || got[0].Target.Namespace != "web" {
		t.Errorf("Workloads(web)[0].Target = %+v, want a selector in namespace web", got[0].Target)
	}
}

func TestWorkloadsEmptyNamespace(t *testing.T) {
	got, err := Workloads(context.Background(), fake.NewSimpleClientset(), "empty")
	if err != nil || len(got) != 0 {
		t.Fatalf("Workloads(empty) = %v, %v, want nothing", got, err)
	}
}

func TestWorkloadsKeepsCreationTime(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	p := workloadsPod("solo", "solo", "", "", 0)
	p.CreationTimestamp = created
	got, err := Workloads(context.Background(), fake.NewSimpleClientset(p), "web")
	if err != nil || len(got) != 1 || !got[0].Created.Equal(created.Time) {
		t.Fatalf("Workloads(web) = %+v, %v, want pod/solo created %s", got, err, created)
	}
}

func TestCheckRestartAndScale(t *testing.T) {
	pod := apiTarget
	pod.Kind = "pod"
	if err := CheckRestart(apiTarget); err != nil {
		t.Errorf("CheckRestart(%s) = %v, want nil", apiTarget, err)
	}
	if err := CheckRestart(pod); err == nil || !strings.Contains(err.Error(), "cannot restart pod/api") {
		t.Errorf("CheckRestart(%s) = %v, want cannot restart", pod, err)
	}
	if err := CheckScale(pod); err == nil || !strings.Contains(err.Error(), "only deployments and statefulsets") {
		t.Errorf("CheckScale(%s) = %v, want only deployments and statefulsets", pod, err)
	}
}
