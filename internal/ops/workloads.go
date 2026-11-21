package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// WorkloadState is a workload or bare pod as a list shows it: how much of
// it is ready, whether it is healthy and, when it is not, why in a word or
// two.
type WorkloadState struct {
	Target resolve.Target
	// Ready and Desired are a controller's ready pods and the replicas it
	// wants, a job's succeeded pods and the completions it needs, or a bare
	// pod's ready containers and all of its containers.
	Ready   int32
	Desired int32
	Health  Health
	// Status is what kubectl's STATUS column would show for the first
	// unhealthy pod, such as CrashLoopBackOff, or else how the rollout
	// stands, such as "Ready" or "1/3 updated, 2 old".
	Status string
	// Restarts adds up the restarts of the workload's containers.
	Restarts int32
	Created  time.Time
}

// workloadKinds is the order Workloads lists kinds in.
var workloadKinds = []resolve.Kind{
	resolve.KindDeployment, resolve.KindStatefulSet, resolve.KindDaemonSet, resolve.KindJob, resolve.KindPod,
}

// Workloads lists the deployments, statefulsets, daemonsets and jobs of a
// namespace, and the pods none of them controls, with their state: by kind,
// then by name. It reads each kind once and the namespace's pods once, so a
// view can call it on a timer. It only reads.
func Workloads(ctx context.Context, cs kubernetes.Interface, ns string) ([]WorkloadState, error) {
	opts := metav1.ListOptions{}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", ns, err)
	}
	var out []WorkloadState
	add := func(t resolve.Target, meta metav1.ObjectMeta, r *Rollout, healthy bool) {
		w := WorkloadState{Target: t, Ready: r.Ready, Desired: r.Desired, Health: healthOf(healthy), Created: meta.CreationTimestamp.Time}
		var sick *corev1.Pod
		for i := range pods.Items {
			p := &pods.Items[i]
			if !t.Selector.Matches(labels.Set(p.Labels)) {
				continue
			}
			w.Restarts += restarts(p)
			if !podHealthy(p) && (sick == nil || p.Name < sick.Name) {
				sick = p
			}
		}
		w.Status = controllerStatus(t.Kind, r, healthy, sick)
		out = append(out, w)
	}

	deploys, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing deployments in %s: %w", ns, err)
	}
	for i := range deploys.Items {
		d := &deploys.Items[i]
		if t, ok := target(resolve.KindDeployment, d.Name, ns, d.Spec.Selector); ok {
			r := deploymentRollout(d)
			add(t, d.ObjectMeta, r, r.Complete)
		}
	}
	sets, err := cs.AppsV1().StatefulSets(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets in %s: %w", ns, err)
	}
	for i := range sets.Items {
		s := &sets.Items[i]
		if t, ok := target(resolve.KindStatefulSet, s.Name, ns, s.Spec.Selector); ok {
			r := statefulSetRollout(s)
			add(t, s.ObjectMeta, r, r.Complete)
		}
	}
	daemons, err := cs.AppsV1().DaemonSets(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets in %s: %w", ns, err)
	}
	for i := range daemons.Items {
		d := &daemons.Items[i]
		if t, ok := target(resolve.KindDaemonSet, d.Name, ns, d.Spec.Selector); ok {
			r := daemonSetRollout(d)
			add(t, d.ObjectMeta, r, r.Complete)
		}
	}
	jobs, err := cs.BatchV1().Jobs(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing jobs in %s: %w", ns, err)
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if t, ok := target(resolve.KindJob, j.Name, ns, j.Spec.Selector); ok {
			r := jobRollout(j)
			// A job shows how many pods succeeded, as kubectl's
			// COMPLETIONS column does; it is healthy while it has not
			// failed, as in Tree.
			r.Ready = r.Succeeded
			add(t, j.ObjectMeta, r, jobCondition(j, batchv1.JobFailed) == nil)
		}
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		if controlledByWorkload(p) {
			continue
		}
		out = append(out, WorkloadState{
			Target:   resolve.Target{Kind: resolve.KindPod, Name: p.Name, Namespace: ns},
			Ready:    int32(readyContainers(p)),
			Desired:  int32(len(p.Spec.Containers)),
			Health:   healthOf(podHealthy(p)),
			Status:   podStatus(p),
			Restarts: restarts(p),
			Created:  p.CreationTimestamp.Time,
		})
	}

	slices.SortStableFunc(out, func(a, b WorkloadState) int {
		return cmp.Or(
			cmp.Compare(slices.Index(workloadKinds, a.Target.Kind), slices.Index(workloadKinds, b.Target.Kind)),
			strings.Compare(a.Target.Name, b.Target.Name),
		)
	})
	return out, nil
}

// target builds a workload's target from its selector, as resolve.Workloads
// does; a workload without a usable selector has no pods to reach.
func target(kind resolve.Kind, name, ns string, sel *metav1.LabelSelector) (resolve.Target, bool) {
	if sel == nil {
		return resolve.Target{}, false
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return resolve.Target{}, false
	}
	return resolve.Target{Kind: kind, Name: name, Namespace: ns, Selector: s}, true
}

// controllerStatus says how a controller stands: why its first unhealthy pod
// is unhealthy, else how far its rollout is.
func controllerStatus(kind resolve.Kind, r *Rollout, healthy bool, sick *corev1.Pod) string {
	switch {
	case kind == resolve.KindJob && (healthy || sick == nil):
		return r.Summary
	case !healthy && sick != nil:
		return podStatus(sick)
	case !healthy:
		// The summary starts with the ready count the list already shows.
		_, rest, ok := strings.Cut(r.Summary, ", ")
		if !ok {
			return "Not ready"
		}
		return rest
	case r.Desired == 0:
		return "Scaled to 0"
	default:
		return "Ready"
	}
}

// controlledByWorkload reports whether a workload Workloads lists controls
// the pod, directly or through a ReplicaSet, so the pod is shown through it.
func controlledByWorkload(p *corev1.Pod) bool {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return false
	}
	switch ref.Kind {
	case "ReplicaSet", "StatefulSet", "DaemonSet", "Job":
		return true
	default:
		return false
	}
}
