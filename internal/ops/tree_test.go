package ops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/paingha/tugcli/internal/resolve"
)

// outline writes a tree as indented "Kind/name health: status" lines.
func outline(n TreeNode) string {
	var b strings.Builder
	var walk func(TreeNode, int)
	walk = func(n TreeNode, depth int) {
		fmt.Fprintf(&b, "%s%s/%s %s: %s\n", strings.Repeat("  ", depth), n.Kind, n.Name, n.Health, n.Status)
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}

func controlledBy(kind, name, uid string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: &yes}}
}

func TestTreeOfADeployment(t *testing.T) {
	current := replicaSet("api-bbb", "2", "api:v2")
	current.UID = "rs-bbb"
	current.Spec.Replicas = new(int32(2))
	current.Status = appsv1.ReplicaSetStatus{Replicas: 2, ReadyReplicas: 1}
	old := replicaSet("api-aaa", "1", "api:v1")
	old.UID = "rs-aaa"
	old.Spec.Replicas = new(int32(0))

	withMounts := func(p *corev1.Pod) {
		p.OwnerReferences = controlledBy("ReplicaSet", "api-bbb", "rs-bbb")
		p.Spec.Volumes = []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"},
			}}},
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "api-data"}}},
			{Name: "kube-api-access-x7z2q", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}}},
			}}},
		}
		p.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "api-db"}, Key: "password",
			}}},
			{Name: "FLAGS", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "api-flags"}, Key: "flags", Optional: new(true),
			}}},
		}
	}
	cs := fake.NewSimpleClientset(
		deployStatus(2, 1, 2, 2), current, old,
		whyPod("api-bbb-1", withMounts),
		whyPod("api-bbb-2", withMounts, waiting("CrashLoopBackOff", ""), lastExit(1, "Error", 0)),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}, ClusterIP: "10.96.0.10",
				Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "web"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "other"}},
		},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "api-config", Namespace: "web"}, Data: map[string]string{"a": "1", "b": "2"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-db", Namespace: "web"}, Data: map[string][]byte{"user": []byte("u")}},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "api-data", Namespace: "web"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound,
				Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
	)
	root, err := Tree(context.Background(), cs, apiTarget)
	if err != nil {
		t.Fatal(err)
	}
	want := `Deployment/api not-ready: 1/2 ready
  Service/api ready: ClusterIP 80/TCP, 1/2 pods ready
  ReplicaSet/api-bbb not-ready: 1/2 ready, revision 2
    Pod/api-bbb-1 ready: Running
      Container/api ready: running
      ConfigMap/api-config ready: 2 keys
      ConfigMap/api-flags ready: not found (optional)
      Secret/api-db not-ready: missing key password
      PersistentVolumeClaim/api-data ready: Bound, 10Gi
    Pod/api-bbb-2 not-ready: CrashLoopBackOff, 5 restarts
      Container/api not-ready: CrashLoopBackOff, 5 restarts
      ConfigMap/api-config ready: 2 keys
      ConfigMap/api-flags ready: not found (optional)
      Secret/api-db not-ready: missing key password
      PersistentVolumeClaim/api-data ready: Bound, 10Gi
`
	if got := outline(*root); got != want {
		t.Fatalf("Tree(deploy/api) =\n%s\nwant\n%s", got, want)
	}
}

func TestTreeOfAStatefulSet(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "web", UID: "sts-uid"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}}},
		},
		Status: appsv1.StatefulSetStatus{Replicas: 2, ReadyReplicas: 1, UpdatedReplicas: 1, UpdateRevision: "db-new", CurrentRevision: "db-old"},
	}
	rev := func(name string, n int64) *appsv1.ControllerRevision {
		return &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web", Labels: map[string]string{"app": "db"},
				OwnerReferences: controlledBy("StatefulSet", "db", "sts-uid")},
			Revision: n,
		}
	}
	dbPod := func(name, revision string, opts ...func(*corev1.Pod)) *corev1.Pod {
		return whyPod(name, append([]func(*corev1.Pod){func(p *corev1.Pod) {
			p.Labels = map[string]string{"app": "db", appsv1.ControllerRevisionHashLabelKey: revision}
			p.OwnerReferences = controlledBy("StatefulSet", "db", "sts-uid")
			p.Spec.Containers[0].Name = "postgres"
			p.Status.ContainerStatuses[0].Name = "postgres"
			p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + name},
			}}}
		}}, opts...)...)
	}
	cs := fake.NewSimpleClientset(sts, rev("db-old", 1), rev("db-new", 2), rev("db-older", 0),
		dbPod("db-0", "db-old"),
		dbPod("db-1", "db-new", unscheduled("0/3 nodes are available")),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "web"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-1", Namespace: "web"},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: new("fast")},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
	)
	target := resolve.Target{Kind: resolve.KindStatefulSet, Name: "db", Namespace: "web",
		Selector: labels.SelectorFromSet(labels.Set{"app": "db"})}
	root, err := Tree(context.Background(), cs, target)
	if err != nil {
		t.Fatal(err)
	}
	want := `StatefulSet/db not-ready: 1/2 ready, 1/2 updated
  ControllerRevision/db-new not-ready: revision 2, 0/1 pods ready, latest
    Pod/db-1 not-ready: Pending
      Container/postgres not-ready: not started
      PersistentVolumeClaim/data-db-1 not-ready: Pending, class fast
  ControllerRevision/db-old ready: revision 1, 1/1 pods ready
    Pod/db-0 ready: Running
      Container/postgres ready: running
      PersistentVolumeClaim/data-db-0 ready: Bound
`
	if got := outline(*root); got != want {
		t.Fatalf("Tree(sts/db) =\n%s\nwant\n%s", got, want)
	}
}

func TestTreeOfAPodWithADebugContainer(t *testing.T) {
	p := whyPod("scratch", func(p *corev1.Pod) {
		p.Labels = map[string]string{"run": "scratch"}
		p.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
		}}}
		p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger-abcde"}}}
		p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debugger-abcde", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 130},
		}}}
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "scratch-env"},
		}}}
	})
	cs := fake.NewSimpleClientset(p,
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "scratch", Namespace: "web"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"run": "scratch"}, ClusterIP: corev1.ClusterIPNone},
		},
	)
	root, err := Tree(context.Background(), cs, resolve.Target{Kind: resolve.KindPod, Name: "scratch", Namespace: "web"})
	if err != nil {
		t.Fatal(err)
	}
	want := `Pod/scratch ready: Running
  Service/scratch ready: headless, 1/1 pods ready
  InitContainer/setup ready: completed
  Container/api ready: running
  EphemeralContainer/debugger-abcde ready: exited 130
  Secret/scratch-env not-ready: not found
`
	if got := outline(*root); got != want {
		t.Fatalf("Tree(pod/scratch) =\n%s\nwant\n%s", got, want)
	}
}

func TestTreeOfAFailedJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "web"},
		Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		}}},
	}
	cs := fake.NewSimpleClientset(job, failedJobPod("migrate-a"))
	root, err := Tree(context.Background(), cs, jobTarget)
	if err != nil {
		t.Fatal(err)
	}
	want := `Job/migrate not-ready: failed: BackoffLimitExceeded
  Pod/migrate-a not-ready: Error
    Container/migrate not-ready: exited 2 (Error)
`
	if got := outline(*root); got != want {
		t.Fatalf("Tree(job/migrate) =\n%s\nwant\n%s", got, want)
	}
}

func TestTreeOfAMissingWorkload(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if _, err := Tree(context.Background(), cs, jobTarget); err == nil || !strings.Contains(err.Error(), "getting job/migrate") {
		t.Fatalf("Tree of a missing job: %v, want a not found error", err)
	}
}
