package cli

import (
	"encoding/json"
	"regexp"
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

// mounting adds a volume for a ConfigMap and one for a Secret to a pod.
func mounting(configMap, secret string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes,
			corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
			}}},
			corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}},
		)
	}
}

func shopObjects() []runtime.Object {
	checkout := shopDeployment("checkout", 2, 2)
	checkoutRS := shopReplicaSet(checkout, "6b8c")
	checkoutLabels := checkoutRS.Labels
	checkoutMounts := mounting("checkout-config", "checkout-tls")

	pricing := shopDeployment("pricing", 1, 0)
	pricingRS := shopReplicaSet(pricing, "9f2a")
	missingSecret := shopPod("pricing-9f2a-m4k7p", pricingRS.Labels, controller("ReplicaSet", pricingRS.Name),
		mounting("checkout-config", "pricing-api-key"), func(p *corev1.Pod) {
			notReadyPod(p)
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: `secret "pricing-api-key" not found`,
			}}
		})

	one := int32(1)
	ledger := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ledger", Namespace: "shop", UID: "ledger", Generation: 1},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ledger"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ledger"}}},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, UpdateRevision: "ledger-5f7d8", CurrentRevision: "ledger-5f7d8"},
	}
	ledgerRev := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "ledger-5f7d8", Namespace: "shop", Labels: map[string]string{"app": "ledger"},
			OwnerReferences: controller("StatefulSet", "ledger")},
		Revision: 1,
	}
	waitingForVolume := shopPod("ledger-0", map[string]string{"app": "ledger", "controller-revision-hash": "ledger-5f7d8"},
		controller("StatefulSet", "ledger"), func(p *corev1.Pod) {
			p.Spec.NodeName = ""
			p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-ledger-0"},
			}}}
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/3 nodes are available: 3 pod has unbound immediate PersistentVolumeClaims.",
			}}}
		})
	fast := "fast-ssd"

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
		shopPod("checkout-6b8c-aaaaa", checkoutLabels, controller("ReplicaSet", checkoutRS.Name), checkoutMounts),
		shopPod("checkout-6b8c-bbbbb", checkoutLabels, controller("ReplicaSet", checkoutRS.Name), checkoutMounts),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "checkout"}, ClusterIP: "10.96.14.2",
				Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}, {Port: 9090, Protocol: corev1.ProtocolTCP}}},
		},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "checkout-config", Namespace: "shop"},
			Data: map[string]string{"currency": "EUR", "tax.yaml": "rate: 0.2"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "checkout-tls", Namespace: "shop"}, Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{"tls.crt": []byte("not a real certificate"), "tls.key": []byte("not a real key")}},

		pricing, pricingRS, missingSecret,

		ledger, ledgerRev, waitingForVolume,
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-ledger-0", Namespace: "shop"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &fast},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		},
		shopEvent("PersistentVolumeClaim", "data-ledger-0", "ProvisioningFailed", `storageclass.storage.k8s.io "fast-ssd" not found`, 40),

		// A bare pod whose image has no shell.
		shopPod("scratch", map[string]string{"run": "scratch"}, nil, func(p *corev1.Pod) {
			p.Spec.Containers[0] = corev1.Container{Name: "app", Image: "gcr.io/distroless/static-debian12"}
			p.Status.ContainerStatuses[0].Name = "app"
		}),

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
		"         next: read the crashed container's logs:\n               tug logs pod/billing-5d4f-x2x4z --previous -n shop\n",
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

	// A volume claim stuck in Pending, and the reason the provisioner gave.
	out = mustTug(t, "why", "sts/ledger", "-n", "shop")
	for _, want := range []string{
		"error    pvc/data-ledger-0: Pending",
		`is Pending: no volume is bound to it: storageclass.storage.k8s.io "fast-ssd" not found`,
		"next: check storage class fast-ssd and its provisioner:\n               tug describe pvc/data-ledger-0 -n shop\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug why sts/ledger lacks %q:\n%s", want, out)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("why wrote to the cluster %d times", writes.Load())
	}
}

func TestTree(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	want := `deploy/checkout                  ✓ 2/2 ready
├── svc/checkout                 ✓ ClusterIP 80/TCP,9090/TCP, 2/2 pods ready
└── rs/checkout-6b8c             ✓ 2/2 ready, revision 2
    ├── pod/checkout-6b8c-aaaaa  ✓ Running
    │   ├── container main       ✓ running
    │   ├── cm/checkout-config   ✓ 2 keys
    │   └── secret/checkout-tls  ✓ kubernetes.io/tls, 2 keys
    └── pod/checkout-6b8c-bbbbb  ✓ Running
        ├── container main       ✓ running
        ├── cm/checkout-config   ✓ 2 keys
        └── secret/checkout-tls  ✓ kubernetes.io/tls, 2 keys
`
	if got := mustTug(t, "tree", "checkout", "-n", "shop"); got != want {
		t.Errorf("tug tree checkout =\n%s\nwant\n%s", got, want)
	}

	out := mustTug(t, "tree", "pricing", "-n", "shop")
	for _, want := range []string{
		"└── pod/pricing-9f2a-m4k7p      ✗ CreateContainerConfigError",
		"├── cm/checkout-config      ✓ 2 keys",
		"└── secret/pricing-api-key  ✗ not found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug tree pricing lacks %q:\n%s", want, out)
		}
	}

	var root ops.TreeNode
	if err := json.Unmarshal([]byte(mustTug(t, "tree", "sts/ledger", "-n", "shop", "-o", "json")), &root); err != nil {
		t.Fatal(err)
	}
	if root.Kind != "StatefulSet" || root.Health != ops.HealthNotReady || len(root.Children) != 1 {
		t.Fatalf("tree root = %+v", root)
	}
	rev := root.Children[0]
	if rev.Kind != "ControllerRevision" || rev.Name != "ledger-5f7d8" || len(rev.Children) != 1 {
		t.Fatalf("revision = %+v", rev)
	}
	pod := rev.Children[0]
	if pod.Name != "ledger-0" || len(pod.Children) != 2 {
		t.Fatalf("pod = %+v", pod)
	}
	if pvc := pod.Children[1]; pvc.Kind != "PersistentVolumeClaim" || pvc.Health != ops.HealthNotReady || pvc.Status != "Pending, class fast-ssd" {
		t.Errorf("claim = %+v, want data-ledger-0 Pending", pvc)
	}

	if out := mustTug(t, "tree", "checkout", "-n", "shop", "-o", "yaml"); !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "health: ready") {
		t.Errorf("tug tree -o yaml:\n%s", out)
	}
	if out := mustTug(t, "tree", "checkout", "-n", "shop", "-o", "json"); strings.Contains(out, "not a real") {
		t.Errorf("tree printed a Secret's value:\n%s", out)
	}
	if writes.Load() != 0 {
		t.Fatalf("tree wrote to the cluster %d times", writes.Load())
	}
}

func TestShDebugContainers(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod") // env=prod: read-only

	// Adding a debug container is a write, refused without a terminal to ask on.
	r := tug(t, "sh", "scratch", "-n", "shop", "--debug-container")
	if r.code == 0 || !strings.Contains(r.stderr, "read-only") || !strings.Contains(r.stderr, "tug unlock") {
		t.Fatalf("sh --debug-container on a read-only session: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatalf("%d writes reached the API server", writes.Load())
	}
	mustTug(t, "unlock", "5m")

	// A container with a shell gets one, and runs a command given after --.
	if out := mustTug(t, "sh", "checkout", "-n", "shop"); out != "shell in checkout-6b8c-aaaaa/main\n" {
		t.Errorf("tug sh checkout = %q", out)
	}
	if out := mustTug(t, "sh", "checkout", "-n", "shop", "--", "ls", "/"); out != "ran in checkout-6b8c-aaaaa/main: ls /\n" {
		t.Errorf("tug sh checkout -- ls / = %q", out)
	}

	// Without a shell, and without a terminal to ask on, tug says how to get one.
	r = tug(t, "sh", "scratch", "-n", "shop")
	if r.code != 1 || !strings.Contains(r.stderr, "container app of pod/scratch has no shell") ||
		!strings.Contains(r.stderr, "run `tug sh scratch --debug-container -n shop`") {
		t.Fatalf("sh into a distroless container: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatal("a debug container was added without asking")
	}

	// --debug-container adds one that shares the app's processes, and attaches to it.
	r = tug(t, "sh", "scratch", "-n", "shop", "--debug-container")
	attached := regexp.MustCompile(`^attached to debugger-[a-z0-9]{5} \(image busybox:1\.36, target app, stdin true, tty false\)\n$`)
	if r.code != 0 || !attached.MatchString(r.stdout) || !strings.Contains(r.stderr, "sharing the processes of app") {
		t.Fatalf("sh --debug-container: %+v", r)
	}
	if writes.Load() != 1 {
		t.Fatalf("want 1 write (the ephemeral container), got %d", writes.Load())
	}

	// A command may finish before tug can attach; its logs are shown instead.
	r = tug(t, "sh", "scratch", "-n", "shop", "--debug-container", "--image", "alpine:3.20", "--", "ps")
	if r.code != 0 || !strings.Contains(r.stdout, "(image alpine:3.20, target app, stdin true, tty false, command ps)") {
		t.Fatalf("sh --debug-container --image alpine:3.20 -- ps: %+v", r)
	}

	// The debug containers stay in the pod, and its tree shows them.
	out := mustTug(t, "tree", "pod/scratch", "-n", "shop")
	if n := strings.Count(out, "ephemeral container debugger-"); n != 2 {
		t.Errorf("tree shows %d debug containers, want 2:\n%s", n, out)
	}
}
