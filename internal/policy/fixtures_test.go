package policy

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
)

// The objects here live in the shop namespace. Each starts out passing
// every check of the baseline pack, and options break it as a test needs.

// goodContainer passes every check of the baseline pack.
func goodContainer(name string) corev1.Container {
	yes, no := true, false
	return corev1.Container{
		Name:  name,
		Image: "registry.example.com/shop/" + name + ":1.4.2",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}}},
		LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/live"}}},
		SecurityContext: &corev1.SecurityContext{RunAsNonRoot: &yes, ReadOnlyRootFilesystem: &yes, Privileged: &no},
	}
}

// goodTemplate is a pod template whose one container, main, passes every
// check of the baseline pack, spread across zones.
func goodTemplate(app string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{goodContainer("main")},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway,
			}},
		},
	}
}

type templateOption func(*corev1.PodTemplateSpec)

// withMain changes the template's first container.
func withMain(f func(*corev1.Container)) templateOption {
	return func(t *corev1.PodTemplateSpec) { f(&t.Spec.Containers[0]) }
}

func deployment(name string, replicas int32, opts ...templateOption) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID(name), Labels: map[string]string{"team": "shop"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: goodTemplate(name),
		},
	}
	for _, o := range opts {
		o(&d.Spec.Template)
	}
	return d
}

func statefulSet(name string, replicas int32, opts ...templateOption) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID(name)},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: goodTemplate(name),
		},
	}
	for _, o := range opts {
		o(&s.Spec.Template)
	}
	return s
}

func cronJob(name string, opts ...templateOption) *batchv1.CronJob {
	c := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID(name)},
		Spec:       batchv1.CronJobSpec{Schedule: "@daily", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: goodTemplate(name)}}},
	}
	for _, o := range opts {
		o(&c.Spec.JobTemplate.Spec.Template)
	}
	return c
}

func pod(name string, owner []metav1.OwnerReference, opts ...templateOption) *corev1.Pod {
	t := goodTemplate(name)
	for _, o := range opts {
		o(&t)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID(name), Labels: t.Labels, OwnerReferences: owner},
		Spec:       t.Spec,
	}
}

func controller(apiVersion, kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(name), Controller: &yes}}
}

func claim(name string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID(name)},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: class},
	}
}

func budget(name, ns string, selector *metav1.LabelSelector) policyv1.PodDisruptionBudget {
	return policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selector},
	}
}

// database returns a db.example.com Database with conditions, each given
// as "Type=Status Reason: message".
func database(name string, conditions ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("db.example.com/v1")
	u.SetKind("Database")
	u.SetNamespace("shop")
	u.SetName(name)
	u.SetUID(types.UID("database-" + name))
	var conds []any
	for _, c := range conditions {
		typeStatus, rest, _ := strings.Cut(c, " ")
		typ, status, _ := strings.Cut(typeStatus, "=")
		reason, msg, _ := strings.Cut(rest, ": ")
		conds = append(conds, map[string]any{"type": typ, "status": status, "reason": reason, "message": msg})
	}
	if conds != nil {
		u.Object["status"] = map[string]any{"conditions": conds}
	}
	return u
}

// toUnstructured turns built-in objects into what the dynamic client
// returns, with their kind set.
func toUnstructured(t *testing.T, objs ...runtime.Object) []*unstructured.Unstructured {
	t.Helper()
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u)
			continue
		}
		gvks, _, err := scheme.Scheme.ObjectKinds(o)
		if err != nil {
			t.Fatal(err)
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: m}
		u.SetGroupVersionKind(gvks[0])
		out = append(out, u)
	}
	return out
}

// packOf parses a pack of the given checks, each a check in YAML's flow
// style: {id: x, severity: warning, ...}.
func packOf(t *testing.T, checks ...string) *Pack {
	t.Helper()
	p, err := parseChecks(checks...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func parseChecks(checks ...string) (*Pack, error) {
	var b strings.Builder
	b.WriteString("apiVersion: tug.policy/v1alpha1\nkind: PolicyPack\nname: test\nversion: 1.0.0\nchecks:\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	return Parse([]byte(b.String()), "test.yaml")
}

// findings describes a report's findings as "objects | container | message",
// with "[blocking]" after a blocking check's.
func findings(r *Report) []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		s := fmt.Sprintf("%s | %s | %s", strings.Join(f.Objects, ","), f.Container, f.Message)
		if f.Blocking {
			s += " [blocking]"
		}
		out = append(out, s)
	}
	return out
}
