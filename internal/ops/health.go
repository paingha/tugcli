package ops

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// Rollout is how far a workload's latest change has rolled out.
type Rollout struct {
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Updated   int32 `json:"updated"`
	Available int32 `json:"available"`
	// Active, Succeeded and Failed count a job's pods.
	Active    int32 `json:"active,omitempty"`
	Succeeded int32 `json:"succeeded,omitempty"`
	Failed    int32 `json:"failed,omitempty"`
	// Complete is true when every desired pod runs the latest template and
	// is available, or when a job has finished successfully.
	Complete bool `json:"complete"`
	// Summary says the same in a few words, such as "1/3 ready, 2/3 updated".
	Summary string `json:"summary"`
}

// workload is the object behind a target, fetched once. Exactly one of the
// pointers is set.
type workload struct {
	target resolve.Target
	deploy *appsv1.Deployment
	sts    *appsv1.StatefulSet
	ds     *appsv1.DaemonSet
	job    *batchv1.Job
	pod    *corev1.Pod
}

func fetchWorkload(ctx context.Context, cs kubernetes.Interface, t resolve.Target) (*workload, error) {
	w := &workload{target: t}
	opts := metav1.GetOptions{}
	var err error
	switch t.Kind {
	case resolve.KindDeployment:
		w.deploy, err = cs.AppsV1().Deployments(t.Namespace).Get(ctx, t.Name, opts)
	case resolve.KindStatefulSet:
		w.sts, err = cs.AppsV1().StatefulSets(t.Namespace).Get(ctx, t.Name, opts)
	case resolve.KindDaemonSet:
		w.ds, err = cs.AppsV1().DaemonSets(t.Namespace).Get(ctx, t.Name, opts)
	case resolve.KindJob:
		w.job, err = cs.BatchV1().Jobs(t.Namespace).Get(ctx, t.Name, opts)
	case resolve.KindPod:
		w.pod, err = cs.CoreV1().Pods(t.Namespace).Get(ctx, t.Name, opts)
	default:
		return nil, fmt.Errorf("unsupported target %s", t)
	}
	if err != nil {
		return nil, fmt.Errorf("getting %s: %w", t, err)
	}
	return w, nil
}

// object returns the workload's metadata.
func (w *workload) object() metav1.Object {
	switch {
	case w.deploy != nil:
		return w.deploy
	case w.sts != nil:
		return w.sts
	case w.ds != nil:
		return w.ds
	case w.job != nil:
		return w.job
	default:
		return w.pod
	}
}

// template returns the workload's pod template; nil for a bare pod.
func (w *workload) template() *corev1.PodTemplateSpec {
	switch {
	case w.deploy != nil:
		return &w.deploy.Spec.Template
	case w.sts != nil:
		return &w.sts.Spec.Template
	case w.ds != nil:
		return &w.ds.Spec.Template
	case w.job != nil:
		return &w.job.Spec.Template
	default:
		return nil
	}
}

// pods returns the workload's pods, sorted by name.
func (w *workload) pods(ctx context.Context, cs kubernetes.Interface) ([]corev1.Pod, error) {
	if w.pod != nil {
		return []corev1.Pod{*w.pod}, nil
	}
	pods, err := resolve.Pods(ctx, cs, w.target)
	if err != nil {
		return nil, fmt.Errorf("listing the pods of %s: %w", w.target, err)
	}
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return pods, nil
}

// rollout describes the workload's progress; nil for a bare pod.
func (w *workload) rollout() *Rollout {
	switch {
	case w.deploy != nil:
		return deploymentRollout(w.deploy)
	case w.sts != nil:
		return statefulSetRollout(w.sts)
	case w.ds != nil:
		return daemonSetRollout(w.ds)
	case w.job != nil:
		return jobRollout(w.job)
	default:
		return nil
	}
}

func deploymentRollout(d *appsv1.Deployment) *Rollout {
	s := d.Status
	desired := replicas(d.Spec.Replicas)
	r := &Rollout{Desired: desired, Ready: s.ReadyReplicas, Updated: s.UpdatedReplicas, Available: s.AvailableReplicas}
	r.Complete = s.ObservedGeneration >= d.Generation &&
		s.UpdatedReplicas >= desired && s.Replicas <= s.UpdatedReplicas && s.AvailableReplicas >= desired
	parts := []string{fmt.Sprintf("%d/%d ready", s.ReadyReplicas, desired)}
	if s.UpdatedReplicas < desired {
		parts = append(parts, fmt.Sprintf("%d/%d updated", s.UpdatedReplicas, desired))
	}
	if old := s.Replicas - s.UpdatedReplicas; old > 0 {
		parts = append(parts, fmt.Sprintf("%d old", old))
	}
	if d.Spec.Paused {
		parts = append(parts, "paused")
	}
	r.Summary = strings.Join(parts, ", ")
	return r
}

func statefulSetRollout(sts *appsv1.StatefulSet) *Rollout {
	s := sts.Status
	desired := replicas(sts.Spec.Replicas)
	r := &Rollout{Desired: desired, Ready: s.ReadyReplicas, Updated: s.UpdatedReplicas, Available: s.AvailableReplicas}
	toUpdate := desired
	onDelete := sts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType
	if ru := sts.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		toUpdate = max(desired-*ru.Partition, 0)
	}
	r.Complete = s.ObservedGeneration >= sts.Generation && s.ReadyReplicas >= desired &&
		(onDelete || s.UpdatedReplicas >= toUpdate)
	parts := []string{fmt.Sprintf("%d/%d ready", s.ReadyReplicas, desired)}
	if s.UpdatedReplicas < desired {
		parts = append(parts, fmt.Sprintf("%d/%d updated", s.UpdatedReplicas, desired))
	}
	r.Summary = strings.Join(parts, ", ")
	return r
}

func daemonSetRollout(ds *appsv1.DaemonSet) *Rollout {
	s := ds.Status
	desired := s.DesiredNumberScheduled
	r := &Rollout{Desired: desired, Ready: s.NumberReady, Updated: s.UpdatedNumberScheduled, Available: s.NumberAvailable}
	r.Complete = s.ObservedGeneration >= ds.Generation &&
		s.UpdatedNumberScheduled >= desired && s.NumberAvailable >= desired
	parts := []string{fmt.Sprintf("%d/%d ready", s.NumberReady, desired)}
	if s.UpdatedNumberScheduled < desired {
		parts = append(parts, fmt.Sprintf("%d/%d updated", s.UpdatedNumberScheduled, desired))
	}
	if s.NumberMisscheduled > 0 {
		parts = append(parts, fmt.Sprintf("%d misscheduled", s.NumberMisscheduled))
	}
	r.Summary = strings.Join(parts, ", ")
	return r
}

func jobRollout(j *batchv1.Job) *Rollout {
	s := j.Status
	completions := int32(1)
	if j.Spec.Completions != nil {
		completions = *j.Spec.Completions
	}
	r := &Rollout{Desired: completions, Active: s.Active, Succeeded: s.Succeeded, Failed: s.Failed}
	if s.Ready != nil {
		r.Ready = *s.Ready
	}
	switch {
	case jobCondition(j, batchv1.JobComplete) != nil:
		r.Complete = true
		r.Summary = fmt.Sprintf("complete, %d/%d succeeded", s.Succeeded, completions)
	case jobCondition(j, batchv1.JobFailed) != nil:
		r.Summary = "failed: " + jobCondition(j, batchv1.JobFailed).Reason
	case jobCondition(j, batchv1.JobSuspended) != nil:
		r.Summary = "suspended"
	default:
		r.Summary = fmt.Sprintf("running, %d active, %d/%d succeeded", s.Active, s.Succeeded, completions)
	}
	if s.Failed > 0 && !strings.HasPrefix(r.Summary, "failed") {
		r.Summary += fmt.Sprintf(", %d failed", s.Failed)
	}
	return r
}

// jobCondition returns a job's condition of the type when it is true.
func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range j.Status.Conditions {
		if c := &j.Status.Conditions[i]; c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// replicas returns a replica count, which Kubernetes defaults to 1.
func replicas(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}

// podCondition returns a pod's condition of the type, or nil.
func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// podHealthy reports whether a pod is doing its job: ready, or finished
// successfully.
func podHealthy(p *corev1.Pod) bool {
	return resolve.PodReady(p) || p.Status.Phase == corev1.PodSucceeded
}

// podStatus summarises a pod as kubectl's STATUS column does: why a container
// is waiting or has stopped, Init:1/2 while init containers run, Terminating,
// or the phase.
func podStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	if c := podCondition(p, corev1.PodScheduled); c != nil && c.Reason == corev1.PodReasonSchedulingGated {
		reason = corev1.PodReasonSchedulingGated
	}

	initializing := false
	for i, st := range p.Status.InitContainerStatuses {
		if isSidecar(p, st.Name) && st.Started != nil && *st.Started {
			continue
		}
		switch t, w := st.State.Terminated, st.State.Waiting; {
		case t != nil && t.ExitCode == 0:
			continue
		case t != nil:
			reason = "Init:" + terminatedReason(t)
		case w != nil && w.Reason != "" && w.Reason != "PodInitializing":
			reason = "Init:" + w.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}

	if !initializing {
		running := false
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			st := p.Status.ContainerStatuses[i]
			switch {
			case st.State.Waiting != nil && st.State.Waiting.Reason != "":
				reason = st.State.Waiting.Reason
			case st.State.Terminated != nil:
				reason = terminatedReason(st.State.Terminated)
			case st.Ready && st.State.Running != nil:
				running = true
			}
		}
		if reason == "Completed" && running {
			reason = "NotReady"
			if resolve.PodReady(p) {
				reason = string(corev1.PodRunning)
			}
		}
	}
	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return reason
}

// terminatedReason names a stopped container's state, as kubectl does.
func terminatedReason(t *corev1.ContainerStateTerminated) string {
	switch {
	case t.Reason != "":
		return t.Reason
	case t.Signal != 0:
		return fmt.Sprintf("Signal:%d", t.Signal)
	default:
		return fmt.Sprintf("ExitCode:%d", t.ExitCode)
	}
}

// isSidecar reports whether an init container keeps running beside the
// app's containers (restartPolicy: Always).
func isSidecar(p *corev1.Pod, name string) bool {
	for _, c := range p.Spec.InitContainers {
		if c.Name == name {
			return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		}
	}
	return false
}

// readyContainers counts a pod's ready app containers.
func readyContainers(p *corev1.Pod) int {
	n := 0
	for _, st := range p.Status.ContainerStatuses {
		if st.Ready {
			n++
		}
	}
	return n
}

// restarts sums the restarts of a pod's containers.
func restarts(p *corev1.Pod) int32 {
	var n int32
	for _, st := range p.Status.InitContainerStatuses {
		n += st.RestartCount
	}
	for _, st := range p.Status.ContainerStatuses {
		n += st.RestartCount
	}
	return n
}

// ShortKind returns kubectl's short name for a kind, as in "rs/api", and the
// kind in lower case when it has none.
func ShortKind(kind string) string {
	switch kind {
	case "Deployment":
		return "deploy"
	case "StatefulSet":
		return "sts"
	case "DaemonSet":
		return "ds"
	case "ReplicaSet":
		return "rs"
	case "Service":
		return "svc"
	case "ConfigMap":
		return "cm"
	case "PersistentVolumeClaim":
		return "pvc"
	default:
		return strings.ToLower(kind)
	}
}
