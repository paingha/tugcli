package resolve

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Kind is a kind of workload target.
type Kind string

// Workload kinds tug resolves.
const (
	KindDeployment  Kind = "deployment"
	KindStatefulSet Kind = "statefulset"
	KindDaemonSet   Kind = "daemonset"
	KindJob         Kind = "job"
	KindPod         Kind = "pod"
)

// Short returns the kubectl short name, for example "deploy".
func (k Kind) Short() string {
	switch k {
	case KindDeployment:
		return "deploy"
	case KindStatefulSet:
		return "sts"
	case KindDaemonSet:
		return "ds"
	default:
		return string(k)
	}
}

// ParseKind accepts kubectl's names for a kind: deploy, deployment,
// deployments, sts, ds, job, po, pod and so on.
func ParseKind(s string) (Kind, error) {
	switch strings.ToLower(s) {
	case "deploy", "deployment", "deployments":
		return KindDeployment, nil
	case "sts", "statefulset", "statefulsets":
		return KindStatefulSet, nil
	case "ds", "daemonset", "daemonsets":
		return KindDaemonSet, nil
	case "job", "jobs":
		return KindJob, nil
	case "po", "pod", "pods":
		return KindPod, nil
	default:
		return "", fmt.Errorf("unsupported kind %q (want deploy, sts, ds, job or pod)", s)
	}
}

// Target is a workload the user named.
type Target struct {
	Kind      Kind
	Name      string
	Namespace string
	// Selector picks the workload's pods; nil for a single pod.
	Selector labels.Selector
}

// String returns "deploy/api-server".
func (t Target) String() string { return t.Kind.Short() + "/" + t.Name }

// IsWorkload reports whether the target is a controller rather than a pod.
func (t Target) IsWorkload() bool { return t.Kind != KindPod }

// Query is a parsed target argument: "api", or "deploy/api" with a kind.
type Query struct {
	Kind Kind // "" when not given, or when the kind is not a workload's
	// Resource is the kind as typed when it is not a workload's, such as
	// "certificate" or "kustomizations.kustomize.toolkit.fluxcd.io". Only
	// the commands that work on any object accept one.
	Resource string
	Name     string
}

// ParseQuery splits an optional kind prefix from a target argument.
func ParseQuery(s string) (Query, error) {
	if kind, name, ok := strings.Cut(s, "/"); ok {
		if kind == "" || name == "" {
			return Query{}, fmt.Errorf("target %q needs a kind and a name, such as deploy/api", s)
		}
		k, err := ParseKind(kind)
		if err != nil {
			return Query{Resource: kind, Name: name}, nil
		}
		return Query{Kind: k, Name: name}, nil
	}
	if s == "" {
		return Query{}, fmt.Errorf("empty target")
	}
	return Query{Name: s}, nil
}

// RequireWorkload returns an error when the query names a kind that is not a
// workload's, for the commands that need pods.
func (q Query) RequireWorkload() error {
	if q.Resource == "" {
		return nil
	}
	_, err := ParseKind(q.Resource)
	return err
}

// Workloads lists the targets in a namespace. When kind is "", it returns
// deployments, statefulsets, daemonsets, jobs and the pods that no controller
// owns (pods of a listed workload are reached through the workload, except a
// Job's, which stay reachable by name). When kind is KindPod, every pod is
// returned.
func Workloads(ctx context.Context, cs kubernetes.Interface, ns string, kind Kind) ([]Target, error) {
	var out []Target
	want := func(k Kind) bool { return kind == "" || kind == k }

	if want(KindDeployment) {
		list, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, d := range list.Items {
			sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
			if err != nil {
				continue
			}
			out = append(out, Target{Kind: KindDeployment, Name: d.Name, Namespace: ns, Selector: sel})
		}
	}
	if want(KindStatefulSet) {
		list, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, s := range list.Items {
			sel, err := metav1.LabelSelectorAsSelector(s.Spec.Selector)
			if err != nil {
				continue
			}
			out = append(out, Target{Kind: KindStatefulSet, Name: s.Name, Namespace: ns, Selector: sel})
		}
	}
	if want(KindDaemonSet) {
		list, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, d := range list.Items {
			sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
			if err != nil {
				continue
			}
			out = append(out, Target{Kind: KindDaemonSet, Name: d.Name, Namespace: ns, Selector: sel})
		}
	}
	if want(KindJob) {
		list, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, j := range list.Items {
			if j.Spec.Selector == nil {
				continue
			}
			sel, err := metav1.LabelSelectorAsSelector(j.Spec.Selector)
			if err != nil {
				continue
			}
			out = append(out, Target{Kind: KindJob, Name: j.Name, Namespace: ns, Selector: sel})
		}
	}
	if want(KindPod) {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, p := range list.Items {
			if kind == "" && ownedByWorkload(&p) {
				continue
			}
			out = append(out, Target{Kind: KindPod, Name: p.Name, Namespace: ns})
		}
	}
	return out, nil
}

// ownedByWorkload reports whether a controller tug lists (directly, or through
// a ReplicaSet) manages the pod.
func ownedByWorkload(p *corev1.Pod) bool {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return false
	}
	switch ref.Kind {
	case "ReplicaSet", "StatefulSet", "DaemonSet":
		return true
	default:
		return false // a Job's pods and others stay reachable by name
	}
}

// MatchTargets returns the best targets for a query. Exact beats prefix beats
// fuzzy; within the best rank, workloads beat standalone pods. More than one
// result means the caller should ask.
func MatchTargets(q Query, candidates []Target) []Target {
	var (
		best     []Target
		bestRank = rankNone
		bestFuzz = 0
	)
	for _, c := range candidates {
		if q.Kind != "" && c.Kind != q.Kind {
			continue
		}
		r, score := rank(q.Name, c.Name)
		if r == rankNone {
			continue
		}
		switch {
		case r < bestRank:
			best, bestRank, bestFuzz = []Target{c}, r, score
		case r > bestRank:
			// a worse rank than what we have
		case r != RankFuzzy || score == bestFuzz:
			best = append(best, c)
		case score < bestFuzz:
			best, bestFuzz = []Target{c}, score // a tighter fuzzy match
		}
	}
	if slices.ContainsFunc(best, Target.IsWorkload) {
		best = slices.DeleteFunc(best, func(t Target) bool { return !t.IsWorkload() })
	}
	sort.SliceStable(best, func(i, j int) bool { return best[i].String() < best[j].String() })
	return best
}

// Pods returns a target's pods, ready ones first, then newest first.
func Pods(ctx context.Context, cs kubernetes.Interface, t Target) ([]corev1.Pod, error) {
	if t.Kind == KindPod {
		p, err := cs.CoreV1().Pods(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return []corev1.Pod{*p}, nil
	}
	list, err := cs.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: t.Selector.String()})
	if err != nil {
		return nil, err
	}
	pods := list.Items
	sort.SliceStable(pods, func(i, j int) bool {
		ri, rj := PodReady(&pods[i]), PodReady(&pods[j])
		if ri != rj {
			return ri
		}
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	return pods, nil
}

// PodReady reports whether a pod is running with its Ready condition true.
func PodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// DefaultContainer returns the container named by the
// kubectl.kubernetes.io/default-container annotation, or the first one.
func DefaultContainer(p *corev1.Pod) string {
	if name := p.Annotations["kubectl.kubernetes.io/default-container"]; name != "" {
		for _, c := range p.Spec.Containers {
			if c.Name == name {
				return name
			}
		}
	}
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return ""
}
