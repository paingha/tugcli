package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// Annotations shared with kubectl, so its rollout history stays meaningful.
const (
	restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"
	revisionAnnotation    = "deployment.kubernetes.io/revision"
	podTemplateHashLabel  = "pod-template-hash"
)

// Restart triggers a rolling restart, as `kubectl rollout restart` does, by
// stamping the pod template.
func Restart(ctx context.Context, cs kubernetes.Interface, t resolve.Target, now time.Time) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{restartedAtAnnotation: now.UTC().Format(time.RFC3339)},
		}}},
	})
	if err != nil {
		return err
	}
	opts := metav1.PatchOptions{FieldManager: "tug"}
	switch t.Kind {
	case resolve.KindDeployment:
		_, err = cs.AppsV1().Deployments(t.Namespace).Patch(ctx, t.Name, types.StrategicMergePatchType, patch, opts)
	case resolve.KindStatefulSet:
		_, err = cs.AppsV1().StatefulSets(t.Namespace).Patch(ctx, t.Name, types.StrategicMergePatchType, patch, opts)
	case resolve.KindDaemonSet:
		_, err = cs.AppsV1().DaemonSets(t.Namespace).Patch(ctx, t.Name, types.StrategicMergePatchType, patch, opts)
	default:
		return fmt.Errorf("cannot restart %s; restart its deployment, statefulset or daemonset", t)
	}
	return err
}

// Scale sets the replica count of a deployment or statefulset.
func Scale(ctx context.Context, cs kubernetes.Interface, t resolve.Target, replicas int32) error {
	if replicas < 0 {
		return fmt.Errorf("replicas must be zero or more, got %d", replicas)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	opts := metav1.PatchOptions{FieldManager: "tug"}
	var err error
	switch t.Kind {
	case resolve.KindDeployment:
		_, err = cs.AppsV1().Deployments(t.Namespace).Patch(ctx, t.Name, types.MergePatchType, patch, opts)
	case resolve.KindStatefulSet:
		_, err = cs.AppsV1().StatefulSets(t.Namespace).Patch(ctx, t.Name, types.MergePatchType, patch, opts)
	default:
		return fmt.Errorf("cannot scale %s; only deployments and statefulsets scale", t)
	}
	return err
}

// Rollback returns a deployment to an earlier revision: the one before the
// current one when toRevision is 0. It returns the revision rolled back to.
func Rollback(ctx context.Context, cs kubernetes.Interface, t resolve.Target, toRevision int64) (int64, error) {
	if t.Kind != resolve.KindDeployment {
		return 0, fmt.Errorf("rollback supports deployments; for %s use `tug rollout undo %s`", t, t)
	}
	d, err := cs.AppsV1().Deployments(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	if d.Spec.Paused {
		return 0, fmt.Errorf("%s is paused; resume it before rolling back", t)
	}
	current := revision(&d.ObjectMeta)

	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return 0, err
	}
	rsList, err := cs.AppsV1().ReplicaSets(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return 0, err
	}
	var target *appsv1.ReplicaSet
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !metav1.IsControlledBy(rs, d) {
			continue
		}
		rev := revision(&rs.ObjectMeta)
		switch {
		case toRevision > 0 && rev == toRevision:
			target = rs
		case toRevision == 0 && rev < current && (target == nil || rev > revision(&target.ObjectMeta)):
			target = rs
		}
	}
	if target == nil {
		if toRevision > 0 {
			return 0, fmt.Errorf("%s has no revision %d", t, toRevision)
		}
		return 0, fmt.Errorf("%s has no earlier revision to roll back to", t)
	}
	rev := revision(&target.ObjectMeta)
	if rev == current {
		return rev, fmt.Errorf("%s is already at revision %d", t, rev)
	}

	tmpl := target.Spec.Template.DeepCopy()
	delete(tmpl.Labels, podTemplateHashLabel)
	patch, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template", "value": tmpl}})
	if err != nil {
		return 0, err
	}
	_, err = cs.AppsV1().Deployments(t.Namespace).Patch(ctx, t.Name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "tug"})
	return rev, err
}

func revision(m *metav1.ObjectMeta) int64 {
	n, err := strconv.ParseInt(m.Annotations[revisionAnnotation], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
