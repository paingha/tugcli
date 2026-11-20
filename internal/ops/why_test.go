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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/paingha/tugcli/internal/resolve"
)

var whyNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// deployStatus returns deploy/api with desired replicas and the counts its
// controller reports.
func deployStatus(desired, ready, updated, total int32) *appsv1.Deployment {
	d := apiDeployment("1", "api:v1")
	d.Spec.Replicas = &desired
	d.Status = appsv1.DeploymentStatus{
		Replicas: total, ReadyReplicas: ready, AvailableReplicas: ready, UpdatedReplicas: updated,
	}
	return d
}

// whyPod returns a ready pod of deploy/api, changed by the options.
func whyPod(name string, opts ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "web", UID: types.UID(name),
			Labels:            map[string]string{"app": "api"},
			CreationTimestamp: metav1.NewTime(whyNow.Add(-time.Hour)),
		},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "api", Image: "api:v1"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", Ready: true, Image: "api:v1",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func notReady(p *corev1.Pod) {
	p.Status.Conditions[1] = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse,
		Reason: "ContainersNotReady", Message: "containers with unready status: [api]"}
	p.Status.ContainerStatuses[0].Ready = false
}

func waiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		notReady(p)
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	}
}

func lastExit(code int32, reason string, ago time.Duration) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		st := &p.Status.ContainerStatuses[0]
		st.RestartCount = 5
		st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, FinishedAt: metav1.NewTime(whyNow.Add(-ago)),
		}}
	}
}

func unscheduled(msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.NodeName = ""
		p.Status = corev1.PodStatus{Phase: corev1.PodPending}
		if msg != "" {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: msg}}
		}
	}
}

func warningEvent(kind, name, fieldPath, reason, msg string, count int32, ago time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%s.%d", name, strings.ToLower(reason), ago), Namespace: "web"},
		InvolvedObject: corev1.ObjectReference{
			Kind: kind, Name: name, Namespace: "web", FieldPath: fieldPath,
		},
		Type: corev1.EventTypeWarning, Reason: reason, Message: msg, Count: count,
		LastTimestamp: metav1.NewTime(whyNow.Add(-ago)),
	}
}

var jobTarget = resolve.Target{
	Kind: resolve.KindJob, Name: "migrate", Namespace: "web",
	Selector: labels.SelectorFromSet(labels.Set{"job-name": "migrate"}),
}

func failedJobPod(name string) *corev1.Pod {
	return whyPod(name, func(p *corev1.Pod) {
		p.Labels = map[string]string{"job-name": "migrate"}
		p.Spec.RestartPolicy = corev1.RestartPolicyNever
		p.Spec.Containers[0].Name = "migrate"
		p.Status.Phase = corev1.PodFailed
		p.Status.Conditions = nil
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "migrate", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"},
		}}}
	})
}

func TestWhy(t *testing.T) {
	podTarget := resolve.Target{Kind: resolve.KindPod, Name: "api-1", Namespace: "web"}
	tests := []struct {
		name    string
		target  resolve.Target
		objects []runtime.Object
		healthy bool
		// want lists "severity reason" for each finding, worst first.
		want []string
		// contains are expected in a message or next step; missing are not.
		contains, missing []string
	}{
		{
			name:    "healthy deployment",
			objects: []runtime.Object{deployStatus(2, 2, 2, 2), whyPod("api-1"), whyPod("api-2")},
			healthy: true,
		},
		{
			name: "crash loop",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", waiting("CrashLoopBackOff", "back-off 5m0s restarting failed container"), lastExit(1, "Error", time.Minute)),
			},
			want:     []string{"error CrashLoopBackOff"},
			contains: []string{"keeps crashing", "last exit code 1 (Error)", "`tug logs pod/api-1 --previous`"},
		},
		{
			name: "pods with the same problem share a finding",
			objects: []runtime.Object{
				deployStatus(2, 0, 2, 2),
				whyPod("api-1", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", time.Minute)),
				whyPod("api-2", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", 2*time.Minute)),
			},
			want: []string{"error CrashLoopBackOff"},
		},
		{
			name: "image pull",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", waiting("ImagePullBackOff", `Back-off pulling image "registry.example.com/api:v2"`), func(p *corev1.Pod) {
					p.Spec.Containers[0].Image = "registry.example.com/api:v2"
				}),
			},
			want:     []string{"error ImagePullBackOff"},
			contains: []string{"cannot pull image registry.example.com/api:v2", "imagePullSecrets"},
		},
		{
			name: "out of memory",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", waiting("CrashLoopBackOff", ""), lastExit(137, "OOMKilled", time.Minute), func(p *corev1.Pod) {
					p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
				}),
			},
			want:     []string{"error OOMKilled"},
			contains: []string{"limit of 128Mi", "`tug top deploy/api`"},
		},
		{
			name: "missing secret",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", waiting("CreateContainerConfigError", `secret "api-db" not found`)),
			},
			want:     []string{"error CreateContainerConfigError"},
			contains: []string{`secret "api-db" not found`, "`tug tree deploy/api`"},
		},
		{
			name: "failing readiness probe",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", notReady),
				warningEvent("Pod", "api-1", "spec.containers{api}", "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503", 12, time.Minute),
			},
			want:     []string{"warning ReadinessProbeFailed"},
			contains: []string{"failed 12 times", "statuscode: 503", "gets no traffic"},
		},
		{
			name: "failing liveness probe",
			objects: []runtime.Object{
				deployStatus(1, 1, 1, 1),
				whyPod("api-1", lastExit(137, "Error", 5*time.Minute)),
				warningEvent("Pod", "api-1", "spec.containers{api}", "Unhealthy", `Liveness probe failed: Get "http://10.0.0.5:8080/healthz": context deadline exceeded`, 3, 5*time.Minute),
			},
			want:     []string{"error LivenessProbeFailed", "warning Restarted"},
			contains: []string{"kubelet restarts the container", "context deadline exceeded", "--previous"},
		},
		{
			name: "no node has a free GPU",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", unscheduled("0/3 nodes are available: 1 Insufficient nvidia.com/gpu, 2 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.")),
			},
			want:     []string{"error FailedScheduling"},
			contains: []string{"1 Insufficient nvidia.com/gpu", "free GPUs", "toleration"},
			missing:  []string{"preemption"},
		},
		{
			name: "node selector, from an event",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", unscheduled("")),
				warningEvent("Pod", "api-1", "", "FailedScheduling", "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.", 4, time.Minute),
			},
			want:     []string{"error FailedScheduling"},
			contains: []string{"nodeSelector", "`tug get nodes --show-labels`"},
		},
		{
			name: "pending volume claim",
			objects: []runtime.Object{
				deployStatus(1, 0, 1, 1),
				whyPod("api-1", unscheduled("0/1 nodes are available: 1 pod has unbound immediate PersistentVolumeClaims."), func(p *corev1.Pod) {
					p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
					}}}
				}),
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "web"},
					Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: new("fast")},
					Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
				},
				warningEvent("PersistentVolumeClaim", "data", "", "ProvisioningFailed", `storageclass.storage.k8s.io "fast" not found`, 9, time.Minute),
			},
			want:     []string{"error FailedScheduling", "error Pending"},
			contains: []string{"storage class fast", `"fast" not found`, "`tug get pvc`", "`tug describe pvc/data`"},
		},
		{
			name: "pods that were never created",
			objects: []runtime.Object{
				deployStatus(1, 0, 0, 0),
				replicaSet("api-7d9f", "1", "api:v1"),
				warningEvent("ReplicaSet", "api-7d9f", "", "FailedCreate", `pods "api-7d9f-x2x4z" is forbidden: exceeded quota: compute, requested: cpu=2, used: cpu=8, limited: cpu=8`, 20, time.Minute),
			},
			want:     []string{"error FailedCreate"},
			contains: []string{"exceeded quota", "`tug describe rs/api-7d9f`"},
		},
		{
			name:   "failed job",
			target: jobTarget,
			objects: []runtime.Object{
				&batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "web"},
					Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": "migrate"}}},
					Status: batchv1.JobStatus{Failed: 2, Conditions: []batchv1.JobCondition{{
						Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
						Message: "Job has reached the specified backoff limit",
					}}},
				},
				failedJobPod("migrate-a"), failedJobPod("migrate-b"),
			},
			want:     []string{"error BackoffLimitExceeded", "error Failed"},
			contains: []string{"job has reached the specified backoff limit", "exited with code 2 (Error)", "`tug logs job/migrate`"},
		},
		{
			name:    "healthy bare pod",
			target:  podTarget,
			objects: []runtime.Object{whyPod("api-1")},
			healthy: true,
		},
		{
			name:     "rollout in progress",
			objects:  []runtime.Object{deployStatus(3, 3, 1, 4), whyPod("api-1"), whyPod("api-2"), whyPod("api-3")},
			want:     []string{"info RolloutInProgress"},
			contains: []string{"1/3 updated, 3 old", "`tug rollout status deploy/api`"},
		},
		{
			name:     "scaled to zero",
			objects:  []runtime.Object{deployStatus(0, 0, 0, 0)},
			want:     []string{"warning ScaledToZero"},
			contains: []string{"`tug scale deploy/api 1`"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.target
			if target.Name == "" {
				target = apiTarget
			}
			cs := fake.NewSimpleClientset(tt.objects...)
			d, err := Why(context.Background(), cs, target, WhyOptions{Now: func() time.Time { return whyNow }})
			if err != nil {
				t.Fatal(err)
			}
			var got, text []string
			for _, f := range d.Findings {
				got = append(got, string(f.Severity)+" "+f.Reason)
				text = append(text, f.Message, f.Next)
			}
			if strings.Join(got, ", ") != strings.Join(tt.want, ", ") {
				t.Fatalf("Why(%s) findings = %v, want %v\n%+v", target, got, tt.want, d.Findings)
			}
			if d.Healthy != tt.healthy {
				t.Errorf("Why(%s).Healthy = %v, want %v (%s)", target, d.Healthy, tt.healthy, d.Summary)
			}
			all := strings.Join(text, "\n")
			for _, s := range tt.contains {
				if !strings.Contains(all, s) {
					t.Errorf("findings lack %q:\n%s", s, all)
				}
			}
			for _, s := range tt.missing {
				if strings.Contains(all, s) {
					t.Errorf("findings contain %q:\n%s", s, all)
				}
			}
		})
	}
}

func TestWhyGroupsPodsAndSummarises(t *testing.T) {
	cs := fake.NewSimpleClientset(
		deployStatus(2, 0, 2, 2),
		whyPod("api-1", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", time.Minute)),
		whyPod("api-2", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", time.Minute)),
	)
	d, err := Why(context.Background(), cs, apiTarget, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Findings[0].Objects; strings.Join(got, ",") != "pod/api-1,pod/api-2" {
		t.Errorf("objects = %v, want both pods", got)
	}
	if d.Summary != "0/2 ready" || d.Rollout.Desired != 2 {
		t.Errorf("summary = %q, rollout = %+v", d.Summary, d.Rollout)
	}
	if len(d.Pods) != 2 || d.Pods[0].Status != "CrashLoopBackOff" || d.Pods[0].Restarts != 5 || d.Pods[0].ReadyContainers != 0 {
		t.Errorf("pods = %+v", d.Pods)
	}
	if c := d.Pods[0].Conditions; len(c) != 1 || c[0].Type != "Ready" || c[0].Reason != "ContainersNotReady" {
		t.Errorf("conditions = %+v, want Ready=False", c)
	}
}

func TestWhyListsRecentWarningsNewestFirst(t *testing.T) {
	cs := fake.NewSimpleClientset(
		deployStatus(1, 0, 1, 1),
		whyPod("api-1", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", time.Minute)),
		warningEvent("Pod", "api-1", "spec.containers{api}", "BackOff", "Back-off restarting failed container api", 30, time.Minute),
		warningEvent("Pod", "api-1", "", "FailedScheduling", "0/1 nodes are available", 1, 10*time.Minute),
		warningEvent("Pod", "api-1", "", "Evicted", "old news", 1, 2*time.Hour),
		warningEvent("Pod", "other-pod", "", "BackOff", "someone else's problem", 1, time.Minute),
	)
	d, err := Why(context.Background(), cs, apiTarget, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range d.Events {
		got = append(got, e.Reason)
	}
	if strings.Join(got, ",") != "BackOff,FailedScheduling" {
		t.Fatalf("events = %v, want BackOff then FailedScheduling", got)
	}
	if d.Events[0].Object != "pod/api-1" || d.Events[0].Count != 30 || !d.Events[0].LastSeen.Equal(whyNow.Add(-time.Minute)) {
		t.Errorf("event = %+v", d.Events[0])
	}
}

func TestWhySuggestsTheNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(
		deployStatus(1, 0, 1, 1),
		whyPod("api-1", waiting("CrashLoopBackOff", ""), lastExit(1, "Error", time.Minute)),
	)
	d, err := Why(context.Background(), cs, apiTarget, WhyOptions{Now: func() time.Time { return whyNow }, ExplicitNamespace: true})
	if err != nil {
		t.Fatal(err)
	}
	if next := d.Findings[0].Next; !strings.Contains(next, "`tug logs pod/api-1 --previous -n web`") {
		t.Fatalf("next = %q, want the namespace in the command", next)
	}
}

func TestPodStatus(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"running", whyPod("a"), "Running"},
		{"waiting", whyPod("a", waiting("ErrImagePull", "")), "ErrImagePull"},
		{"unscheduled", whyPod("a", unscheduled("x")), "Pending"},
		{"evicted", whyPod("a", func(p *corev1.Pod) { p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted" }), "Evicted"},
		{"terminating", whyPod("a", func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: whyNow} }), "Terminating"},
		{"init failing", whyPod("a", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "migrate", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			}}}
		}), "Init:CrashLoopBackOff"},
		{"init running", whyPod("a", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "a"}, {Name: "b"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{Name: "a", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}
		}), "Init:1/2"},
		{"killed", whyPod("a", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Signal: 9}}
		}), "Signal:9"},
	}
	for _, tt := range tests {
		if got := podStatus(tt.pod); got != tt.want {
			t.Errorf("podStatus(%s) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLowerFirst(t *testing.T) {
	tests := map[string]string{
		"Job has reached the specified backoff limit": "job has reached the specified backoff limit",
		"PodSecurity forbids it":                      "PodSecurity forbids it",
		"":                                            "",
	}
	for in, want := range tests {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}
