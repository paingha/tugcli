package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/ops"
)

// The "shop" namespace of the fake API server holds a healthy deployment
// (checkout) and broken workloads, one for each kind of problem.

func shopDeployment(name string, desired, ready int32) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", UID: types.UID(name), Generation: 1,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &desired,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: name + ":v2"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: desired, UpdatedReplicas: desired, ReadyReplicas: ready, AvailableReplicas: ready,
		},
	}
}

func controller(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(name), Controller: &yes}}
}

func shopReplicaSet(d *appsv1.Deployment, hash string) *appsv1.ReplicaSet {
	labels := map[string]string{"app": d.Name, "pod-template-hash": hash}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: d.Name + "-" + hash, Namespace: "shop", UID: types.UID(d.Name + "-" + hash), Labels: labels,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
			OwnerReferences: controller("Deployment", d.Name),
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: d.Spec.Replicas, Template: d.Spec.Template},
		Status: appsv1.ReplicaSetStatus{
			Replicas: d.Status.Replicas, ReadyReplicas: d.Status.ReadyReplicas, AvailableReplicas: d.Status.AvailableReplicas,
		},
	}
}

// shopPod returns a running, ready pod with one container, main, changed by
// the options.
func shopPod(name string, labels map[string]string, owner []metav1.OwnerReference, opts ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", UID: types.UID(name), Labels: labels, OwnerReferences: owner,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour)),
		},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "main", Image: "registry.example.com/" + labels["app"] + ":v2"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func shopEvent(kind, name, reason, msg string, count int32) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + "." + strings.ToLower(reason), Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: "shop"},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: msg, Count: count,
		LastTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
	}
}

func notReadyPod(p *corev1.Pod) {
	p.Status.Conditions[1] = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}
	p.Status.ContainerStatuses[0].Ready = false
}

func shopObjects() []runtime.Object {
	checkout := shopDeployment("checkout", 2, 2)
	checkoutRS := shopReplicaSet(checkout, "6b8c")
	checkoutLabels := checkoutRS.Labels

	billing := shopDeployment("billing", 1, 0)
	billingRS := shopReplicaSet(billing, "5d4f")
	crashing := shopPod("billing-5d4f-x2x4z", billingRS.Labels, controller("ReplicaSet", billingRS.Name), func(p *corev1.Pod) {
		notReadyPod(p)
		st := &p.Status.ContainerStatuses[0]
		st.RestartCount = 7
		st.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}
	})

	trainer := shopDeployment("ml-trainer", 1, 0)
	trainerRS := shopReplicaSet(trainer, "7c9d")
	unschedulable := shopPod("ml-trainer-7c9d-q8w2e", trainerRS.Labels, controller("ReplicaSet", trainerRS.Name), func(p *corev1.Pod) {
		p.Spec.NodeName = ""
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/3 nodes are available: 1 Insufficient nvidia.com/gpu, 2 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
		}}}
	})

	return []runtime.Object{
		checkout, checkoutRS,
		shopPod("checkout-6b8c-aaaaa", checkoutLabels, controller("ReplicaSet", checkoutRS.Name)),
		shopPod("checkout-6b8c-bbbbb", checkoutLabels, controller("ReplicaSet", checkoutRS.Name)),

		billing, billingRS, crashing,
		shopEvent("Pod", crashing.Name, "BackOff", "Back-off restarting failed container main in pod "+crashing.Name, 31),

		trainer, trainerRS, unschedulable,
		shopEvent("Pod", unschedulable.Name, "FailedScheduling", unschedulable.Status.Conditions[0].Message, 12),
	}
}

func TestWhy(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod") // read-only, which why does not mind

	out := mustTug(t, "why", "billing", "-n", "shop")
	for _, want := range []string{
		"deploy/billing in shop is not ready: 0/1 ready",
		"billing-5d4f-x2x4z   CrashLoopBackOff   0/1     7",
		"error    pod/billing-5d4f-x2x4z, container main: CrashLoopBackOff",
		"         keeps crashing and waits to restart; last exit code 1 (Error)",
		"         next: read the crashed container's logs: `tug logs pod/billing-5d4f-x2x4z --previous -n shop`",
		"Recent warning events:",
		"pod/billing-5d4f-x2x4z   BackOff   Back-off restarting failed container main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug why billing lacks %q:\n%s", want, out)
		}
	}

	out = mustTug(t, "why", "checkout", "-n", "shop")
	if !strings.Contains(out, "deploy/checkout in shop is healthy: 2/2 ready") || !strings.Contains(out, "Nothing wrong found.") {
		t.Errorf("tug why checkout:\n%s", out)
	}

	var d ops.Diagnosis
	if err := json.Unmarshal([]byte(mustTug(t, "why", "ml", "-n", "shop", "-o", "json")), &d); err != nil {
		t.Fatal(err)
	}
	if d.Healthy || d.Target != "deploy/ml-trainer" || len(d.Findings) != 1 {
		t.Fatalf("tug why ml -o json = %+v", d)
	}
	if f := d.Findings[0]; f.Severity != ops.SeverityError || f.Reason != "FailedScheduling" ||
		!strings.Contains(f.Message, "Insufficient nvidia.com/gpu") || !strings.Contains(f.Next, "free GPUs") {
		t.Errorf("finding = %+v, want a scheduling failure for want of a GPU", f)
	}

	var y map[string]any
	if err := yaml.Unmarshal([]byte(mustTug(t, "why", "billing", "-n", "shop", "-o", "yaml")), &y); err != nil || y["healthy"] != false {
		t.Errorf("tug why -o yaml = %v, %v", y, err)
	}
	if r := tug(t, "why", "billing", "-n", "shop", "-o", "name"); r.code == 0 || !strings.Contains(r.stderr, "table, json or yaml") {
		t.Errorf("tug why -o name: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatalf("why wrote to the cluster %d times", writes.Load())
	}
}
