package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// Health says whether an object in a tree is working.
type Health string

// Health values.
const (
	HealthReady    Health = "ready"
	HealthNotReady Health = "not-ready"
	// HealthUnknown is for objects tug may not read, and objects whose
	// controller has not yet caught up with them.
	HealthUnknown Health = "unknown"
	// HealthNone is for objects that report no health of their own, such
	// as a ConfigMap or a custom resource without status conditions.
	HealthNone Health = "none"
)

func healthOf(ok bool) Health {
	if ok {
		return HealthReady
	}
	return HealthNotReady
}

// Kinds of tree nodes that are parts of a pod rather than Kubernetes kinds.
const (
	KindContainer          = "Container"
	KindInitContainer      = "InitContainer"
	KindEphemeralContainer = "EphemeralContainer"
)

// TreeNode is one object in a workload's tree, with its health, a short
// status and the objects beneath it.
type TreeNode struct {
	// Kind is a Kubernetes kind, such as ReplicaSet, or one of the
	// container kinds.
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Health   Health     `json:"health"`
	Status   string     `json:"status"`
	Children []TreeNode `json:"children,omitempty"`
}

// Tree returns a workload's ownership tree, as k9s's xray does. Beneath the
// workload are the Services that select its pods and its ReplicaSets (for a
// deployment) or ControllerRevisions (for a statefulset or daemonset);
// beneath those are the pods, and beneath each pod its containers and the
// ConfigMaps, Secrets and PersistentVolumeClaims it uses. A bare pod is the
// root of its own tree. It only reads.
func Tree(ctx context.Context, cs kubernetes.Interface, t resolve.Target) (*TreeNode, error) {
	w, err := fetchWorkload(ctx, cs, t)
	if err != nil {
		return nil, err
	}
	pods, err := w.pods(ctx, cs)
	if err != nil {
		return nil, err
	}
	b := &treeBuilder{cs: cs, ns: t.Namespace, lookups: map[string]lookup{}}
	services, err := b.services(ctx, w, pods)
	if err != nil {
		return nil, err
	}
	if w.pod != nil {
		root := b.pod(ctx, w.pod)
		root.Children = slices.Concat(services, root.Children)
		return &root, nil
	}
	root := workloadNode(w)
	owned, err := b.owned(ctx, w, pods)
	if err != nil {
		return nil, err
	}
	root.Children = slices.Concat(services, owned)
	return &root, nil
}

// treeBuilder builds one tree, reading each mounted object once.
type treeBuilder struct {
	cs      kubernetes.Interface
	ns      string
	lookups map[string]lookup
}

// lookup is what a read of a ConfigMap, Secret or PVC found.
type lookup struct {
	keys   map[string]bool
	detail string
	ready  bool
	err    error
}

func workloadNode(w *workload) TreeNode {
	r := w.rollout()
	ok := r.Complete
	if w.job != nil {
		ok = jobCondition(w.job, batchv1.JobFailed) == nil
	}
	return TreeNode{Kind: kindOf(w.target.Kind), Name: w.target.Name, Health: healthOf(ok), Status: r.Summary}
}

// owned returns the nodes beneath a controller: ReplicaSets or
// ControllerRevisions with their pods, then any pods outside them.
func (b *treeBuilder) owned(ctx context.Context, w *workload, pods []corev1.Pod) ([]TreeNode, error) {
	var nodes []TreeNode
	placed := map[string]bool{}
	sel := metav1.ListOptions{LabelSelector: w.target.Selector.String()}
	switch {
	case w.deploy != nil:
		list, err := b.cs.AppsV1().ReplicaSets(b.ns).List(ctx, sel)
		if err != nil {
			return nil, fmt.Errorf("listing the ReplicaSets of %s: %w", w.target, err)
		}
		sets := slices.DeleteFunc(list.Items, func(rs appsv1.ReplicaSet) bool { return !metav1.IsControlledBy(&rs, w.deploy) })
		slices.SortFunc(sets, func(a, b appsv1.ReplicaSet) int {
			return cmp.Or(cmp.Compare(revision(&b.ObjectMeta), revision(&a.ObjectMeta)), strings.Compare(a.Name, b.Name))
		})
		for i := range sets {
			rs := &sets[i]
			mine := podsOwnedBy(pods, rs.UID)
			desired := replicas(rs.Spec.Replicas)
			if desired == 0 && rs.Status.Replicas == 0 && len(mine) == 0 {
				continue // an old revision kept for rollbacks
			}
			n := TreeNode{
				Kind: "ReplicaSet", Name: rs.Name, Health: healthOf(rs.Status.ReadyReplicas >= desired),
				Status: fmt.Sprintf("%d/%d ready, revision %d", rs.Status.ReadyReplicas, desired, revision(&rs.ObjectMeta)),
			}
			for j := range mine {
				n.Children = append(n.Children, b.pod(ctx, mine[j]))
				placed[mine[j].Name] = true
			}
			nodes = append(nodes, n)
		}
	case w.sts != nil || w.ds != nil:
		list, err := b.cs.AppsV1().ControllerRevisions(b.ns).List(ctx, sel)
		if err != nil {
			return nil, fmt.Errorf("listing the ControllerRevisions of %s: %w", w.target, err)
		}
		revs := slices.DeleteFunc(list.Items, func(cr appsv1.ControllerRevision) bool { return !metav1.IsControlledBy(&cr, w.object()) })
		slices.SortFunc(revs, func(a, b appsv1.ControllerRevision) int {
			return cmp.Or(cmp.Compare(b.Revision, a.Revision), strings.Compare(a.Name, b.Name))
		})
		latest := ""
		if w.sts != nil {
			latest = w.sts.Status.UpdateRevision
		} else if len(revs) > 0 {
			latest = revs[0].Name
		}
		for i := range revs {
			cr := &revs[i]
			var mine []*corev1.Pod
			for j := range pods {
				if !placed[pods[j].Name] && revisionMatches(cr, w.target.Name, pods[j].Labels[appsv1.ControllerRevisionHashLabelKey]) {
					mine = append(mine, &pods[j])
				}
			}
			if len(mine) == 0 && cr.Name != latest {
				continue
			}
			ready := 0
			for _, p := range mine {
				if podHealthy(p) {
					ready++
				}
			}
			status := fmt.Sprintf("revision %d, %d/%d pods ready", cr.Revision, ready, len(mine))
			if cr.Name == latest {
				status += ", latest"
			}
			n := TreeNode{Kind: "ControllerRevision", Name: cr.Name, Health: healthOf(ready == len(mine)), Status: status}
			for _, p := range mine {
				n.Children = append(n.Children, b.pod(ctx, p))
				placed[p.Name] = true
			}
			nodes = append(nodes, n)
		}
	}
	for i := range pods {
		if !placed[pods[i].Name] {
			nodes = append(nodes, b.pod(ctx, &pods[i]))
		}
	}
	return nodes, nil
}

func podsOwnedBy(pods []corev1.Pod, uid types.UID) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range pods {
		if ref := metav1.GetControllerOf(&pods[i]); ref != nil && ref.UID == uid {
			out = append(out, &pods[i])
		}
	}
	return out
}

// revisionMatches reports whether a pod's controller-revision-hash label
// names the revision. StatefulSets put the revision's name in the label and
// DaemonSets its hash, which the revision is named after.
func revisionMatches(cr *appsv1.ControllerRevision, owner, hash string) bool {
	return hash != "" && (cr.Name == hash || cr.Name == owner+"-"+hash || cr.Labels[appsv1.ControllerRevisionHashLabelKey] == hash)
}

// services returns the Services that select the workload's pods, or would
// select them when it has none.
func (b *treeBuilder) services(ctx context.Context, w *workload, pods []corev1.Pod) ([]TreeNode, error) {
	list, err := b.cs.CoreV1().Services(b.ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return nil, nil // the tree is still useful without them
	}
	if err != nil {
		return nil, fmt.Errorf("listing Services in %s: %w", b.ns, err)
	}
	var template labels.Set
	if t := w.template(); t != nil {
		template = t.Labels
	}
	var nodes []TreeNode
	for _, svc := range list.Items {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		matched, ready := 0, 0
		for i := range pods {
			if sel.Matches(labels.Set(pods[i].Labels)) {
				matched++
				if resolve.PodReady(&pods[i]) {
					ready++
				}
			}
		}
		if matched == 0 && (template == nil || !sel.Matches(template)) {
			continue
		}
		var ports []string
		for _, p := range svc.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, cmp.Or(p.Protocol, corev1.ProtocolTCP)))
		}
		kind := string(cmp.Or(svc.Spec.Type, corev1.ServiceTypeClusterIP))
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			kind = "headless"
		}
		status := strings.TrimSpace(kind + " " + strings.Join(ports, ","))
		nodes = append(nodes, TreeNode{
			Kind: "Service", Name: svc.Name, Health: healthOf(ready > 0),
			Status: fmt.Sprintf("%s, %d/%d pods ready", status, ready, matched),
		})
	}
	slices.SortFunc(nodes, func(a, b TreeNode) int { return strings.Compare(a.Name, b.Name) })
	return nodes, nil
}

// pod returns a pod's node with its containers and what it mounts.
func (b *treeBuilder) pod(ctx context.Context, p *corev1.Pod) TreeNode {
	n := TreeNode{Kind: "Pod", Name: p.Name, Health: healthOf(podHealthy(p)), Status: podStatus(p)}
	if r := restarts(p); r > 0 {
		n.Status += ", " + plural(r, "restart")
	}
	statuses := map[string]*corev1.ContainerStatus{}
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses} {
		for i := range list {
			statuses[list[i].Name] = &list[i]
		}
	}
	runsOnce := p.Spec.RestartPolicy != "" && p.Spec.RestartPolicy != corev1.RestartPolicyAlways
	for _, c := range p.Spec.InitContainers {
		sidecar := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		n.Children = append(n.Children, containerNode(KindInitContainer, c.Name, statuses[c.Name], !sidecar))
	}
	for _, c := range p.Spec.Containers {
		n.Children = append(n.Children, containerNode(KindContainer, c.Name, statuses[c.Name], runsOnce))
	}
	for _, c := range p.Spec.EphemeralContainers {
		n.Children = append(n.Children, containerNode(KindEphemeralContainer, c.Name, statuses[c.Name], true))
	}
	for _, ref := range mountRefs(p) {
		n.Children = append(n.Children, b.mount(ctx, ref))
	}
	return n
}

// containerNode describes a container. finishes says whether it is meant to
// exit: an init container, a container of a pod that runs once, or a debug
// container.
func containerNode(kind, name string, st *corev1.ContainerStatus, finishes bool) TreeNode {
	n := TreeNode{Kind: kind, Name: name}
	switch {
	case st == nil:
		n.Health, n.Status = HealthNotReady, "not started"
	case st.State.Running != nil:
		n.Status = "running"
		switch kind {
		case KindContainer:
			n.Health = healthOf(st.Ready)
			if !st.Ready {
				n.Status = "running, not ready"
			}
		case KindInitContainer:
			// A sidecar runs on; any other init container is still at work.
			n.Health = healthOf(!finishes && st.Started != nil && *st.Started)
		default:
			n.Health = HealthReady
		}
	case st.State.Terminated != nil:
		t := st.State.Terminated
		n.Status = "exited " + exitCode(t)
		if t.ExitCode == 0 {
			n.Status = "completed"
		}
		n.Health = healthOf(kind == KindEphemeralContainer || finishes && t.ExitCode == 0)
	default:
		n.Health, n.Status = HealthNotReady, "waiting"
		if w := st.State.Waiting; w != nil && w.Reason != "" {
			n.Status = w.Reason
		}
	}
	if st != nil && st.RestartCount > 0 {
		n.Status += ", " + plural(st.RestartCount, "restart")
	}
	return n
}

// mountRef is a ConfigMap, Secret or PVC that a pod uses.
type mountRef struct {
	kind, name string
	// optional is true when every reference to it is optional.
	optional bool
	// keys are the keys the pod needs from a ConfigMap or Secret.
	keys []string
}

// mountRefs lists what a pod mounts or reads into its environment:
// ConfigMaps, then Secrets, then PVCs, each by name. The service account
// token volume Kubernetes adds to every pod is left out.
func mountRefs(p *corev1.Pod) []mountRef {
	refs := map[string]*mountRef{}
	use := func(kind, name string, optional *bool, keys ...string) {
		if name == "" {
			return
		}
		opt := optional != nil && *optional
		r, ok := refs[kind+"/"+name]
		if !ok {
			r = &mountRef{kind: kind, name: name, optional: true}
			refs[kind+"/"+name] = r
		}
		r.optional = r.optional && opt
		if !opt {
			for _, k := range keys {
				if !slices.Contains(r.keys, k) {
					r.keys = append(r.keys, k)
				}
			}
		}
	}
	itemKeys := func(items []corev1.KeyToPath) []string {
		var keys []string
		for _, it := range items {
			keys = append(keys, it.Key)
		}
		return keys
	}
	for _, v := range p.Spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			use("ConfigMap", v.ConfigMap.Name, v.ConfigMap.Optional, itemKeys(v.ConfigMap.Items)...)
		case v.Secret != nil:
			use("Secret", v.Secret.SecretName, v.Secret.Optional, itemKeys(v.Secret.Items)...)
		case v.PersistentVolumeClaim != nil:
			use("PersistentVolumeClaim", v.PersistentVolumeClaim.ClaimName, nil)
		case v.Ephemeral != nil:
			use("PersistentVolumeClaim", p.Name+"-"+v.Name, nil)
		case v.Projected != nil && !strings.HasPrefix(v.Name, "kube-api-access-"):
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					use("ConfigMap", s.ConfigMap.Name, s.ConfigMap.Optional, itemKeys(s.ConfigMap.Items)...)
				}
				if s.Secret != nil {
					use("Secret", s.Secret.Name, s.Secret.Optional, itemKeys(s.Secret.Items)...)
				}
			}
		}
	}
	for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				use("ConfigMap", e.ConfigMapRef.Name, e.ConfigMapRef.Optional)
			}
			if e.SecretRef != nil {
				use("Secret", e.SecretRef.Name, e.SecretRef.Optional)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				use("ConfigMap", r.Name, r.Optional, r.Key)
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				use("Secret", r.Name, r.Optional, r.Key)
			}
		}
	}
	order := map[string]int{"ConfigMap": 0, "Secret": 1, "PersistentVolumeClaim": 2}
	out := make([]mountRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b mountRef) int {
		return cmp.Or(cmp.Compare(order[a.kind], order[b.kind]), strings.Compare(a.name, b.name))
	})
	return out
}

// mount returns the node for something a pod uses, reading it once per tree.
// Only a Secret's key names and type are kept, never its values.
func (b *treeBuilder) mount(ctx context.Context, ref mountRef) TreeNode {
	n := TreeNode{Kind: ref.kind, Name: ref.name}
	key := ref.kind + "/" + ref.name
	l, ok := b.lookups[key]
	if !ok {
		l = b.read(ctx, ref.kind, ref.name)
		b.lookups[key] = l
	}
	switch {
	case apierrors.IsNotFound(l.err) && ref.optional:
		n.Health, n.Status = HealthReady, "not found (optional)"
	case apierrors.IsNotFound(l.err):
		n.Health, n.Status = HealthNotReady, "not found"
	case apierrors.IsForbidden(l.err):
		n.Health, n.Status = HealthUnknown, "no permission to read it"
	case l.err != nil:
		n.Health, n.Status = HealthUnknown, "could not be read"
	default:
		var missing []string
		for _, k := range ref.keys {
			if l.keys != nil && !l.keys[k] {
				missing = append(missing, k)
			}
		}
		n.Health, n.Status = healthOf(l.ready && len(missing) == 0), l.detail
		if len(missing) > 0 {
			n.Status = "missing key " + strings.Join(missing, ", ")
		}
	}
	return n
}

func (b *treeBuilder) read(ctx context.Context, kind, name string) lookup {
	opts := metav1.GetOptions{}
	switch kind {
	case "ConfigMap":
		cm, err := b.cs.CoreV1().ConfigMaps(b.ns).Get(ctx, name, opts)
		if err != nil {
			return lookup{err: err}
		}
		keys := map[string]bool{}
		for k := range cm.Data {
			keys[k] = true
		}
		for k := range cm.BinaryData {
			keys[k] = true
		}
		return lookup{keys: keys, ready: true, detail: plural(int32(len(keys)), "key")}
	case "Secret":
		s, err := b.cs.CoreV1().Secrets(b.ns).Get(ctx, name, opts)
		if err != nil {
			return lookup{err: err}
		}
		keys := map[string]bool{}
		for k := range s.Data {
			keys[k] = true
		}
		for k := range s.StringData {
			keys[k] = true
		}
		return lookup{keys: keys, ready: true, detail: fmt.Sprintf("%s, %s", cmp.Or(s.Type, corev1.SecretTypeOpaque), plural(int32(len(keys)), "key"))}
	default:
		pvc, err := b.cs.CoreV1().PersistentVolumeClaims(b.ns).Get(ctx, name, opts)
		if err != nil {
			return lookup{err: err}
		}
		detail := string(pvc.Status.Phase)
		if size, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			detail += ", " + size.String()
		}
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			detail += ", class " + *pvc.Spec.StorageClassName
		}
		return lookup{ready: pvc.Status.Phase == corev1.ClaimBound, detail: detail}
	}
}
