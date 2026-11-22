package policy

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/paingha/tugcli/internal/ops"
)

// kind is a kind of object as a pack names it.
type kind struct {
	Group string
	Kind  string
}

// String writes the kind as a pack would: Deployment for a kind built into
// Kubernetes, Certificate.cert-manager.io for any other.
func (k kind) String() string {
	if b, ok := builtinKind(k.Kind); ok && b.Group == k.Group {
		return b.Kind
	}
	if k.Group == "" {
		return k.Kind
	}
	return k.Kind + "." + k.Group
}

// of reports whether gk is this kind.
func (k kind) of(gk schema.GroupKind) bool {
	return strings.EqualFold(k.Kind, gk.Kind) && k.Group == gk.Group
}

// Kinds built into Kubernetes, which packs may name without their group.
var (
	kindPod                   = kind{"", "Pod"}
	kindPersistentVolumeClaim = kind{"", "PersistentVolumeClaim"}
	kindSecret                = kind{"", "Secret"}
	kindDeployment            = kind{"apps", "Deployment"}
	kindStatefulSet           = kind{"apps", "StatefulSet"}
	kindDaemonSet             = kind{"apps", "DaemonSet"}
	kindReplicaSet            = kind{"apps", "ReplicaSet"}
	kindJob                   = kind{"batch", "Job"}
	kindCronJob               = kind{"batch", "CronJob"}
)

var builtinKinds = []kind{
	kindPod, kindPersistentVolumeClaim, kindSecret,
	{"", "ConfigMap"}, {"", "Service"}, {"", "ServiceAccount"}, {"", "Namespace"}, {"", "Node"}, {"", "PersistentVolume"},
	kindDeployment, kindStatefulSet, kindDaemonSet, kindReplicaSet,
	kindJob, kindCronJob,
	{"policy", "PodDisruptionBudget"},
	{"autoscaling", "HorizontalPodAutoscaler"},
	{"networking.k8s.io", "Ingress"}, {"networking.k8s.io", "NetworkPolicy"}, {"networking.k8s.io", "IngressClass"},
	{"storage.k8s.io", "StorageClass"},
	{"rbac.authorization.k8s.io", "Role"}, {"rbac.authorization.k8s.io", "RoleBinding"},
	{"rbac.authorization.k8s.io", "ClusterRole"}, {"rbac.authorization.k8s.io", "ClusterRoleBinding"},
	{"apiextensions.k8s.io", "CustomResourceDefinition"},
	{"gateway.networking.k8s.io", "Gateway"}, {"gateway.networking.k8s.io", "HTTPRoute"},
}

func builtinKind(name string) (kind, bool) {
	for _, k := range builtinKinds {
		if strings.EqualFold(k.Kind, name) {
			return k, true
		}
	}
	return kind{}, false
}

var kindPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// parseKind reads a kind as a pack names it: Deployment, deployment, or
// Certificate.cert-manager.io with its group.
func parseKind(s string) (kind, error) {
	name, group, hasGroup := strings.Cut(strings.TrimSpace(s), ".")
	if !kindPattern.MatchString(name) || (hasGroup && group == "") {
		return kind{}, fmt.Errorf("kind %q must be a kind such as Deployment, or Kind.group such as Certificate.cert-manager.io", s)
	}
	group = strings.ToLower(group)
	if b, ok := builtinKind(name); ok && (!hasGroup || group == b.Group) {
		return b, nil
	}
	if !hasGroup {
		return kind{}, fmt.Errorf("kind %s is not built into Kubernetes; name its API group too, as %s.example.com", name, name)
	}
	return kind{Group: group, Kind: name}, nil
}

// kindScope is which kinds a type of check can check.
type kindScope int

const (
	// anyKind checks look only at metadata or status, which every kind
	// has.
	anyKind kindScope = iota
	// podKinds checks look at a pod template.
	podKinds
	// replicatedKinds checks look at a pod template and a replica count.
	replicatedKinds
	// storageKinds checks look at volume claims.
	storageKinds
)

// withTemplate are the kinds that have pods, through a template or, for a
// pod, of their own.
var withTemplate = []kind{kindDeployment, kindStatefulSet, kindDaemonSet, kindReplicaSet, kindJob, kindCronJob, kindPod}

// withReplicas are the kinds with a replica count.
var withReplicas = []kind{kindDeployment, kindStatefulSet, kindReplicaSet}

// withClaims are the kinds that are, or have templates of, volume claims.
var withClaims = []kind{kindPersistentVolumeClaim, kindStatefulSet}

func (s kindScope) accepts(k kind) bool {
	switch s {
	case podKinds:
		return containsKind(withTemplate, k)
	case replicatedKinds:
		return containsKind(withReplicas, k)
	case storageKinds:
		return containsKind(withClaims, k)
	default:
		return true
	}
}

func (s kindScope) hint() string {
	var ks []kind
	switch s {
	case podKinds:
		ks = withTemplate
	case replicatedKinds:
		ks = withReplicas
	case storageKinds:
		ks = withClaims
	default:
		return "any kind will do"
	}
	names := make([]string, len(ks))
	for i, k := range ks {
		names[i] = k.Kind
	}
	return "this type checks only " + joinWords(names, "and")
}

func containsKind(ks []kind, k kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// Default kinds for each type of check that has them.
var (
	// workloadKinds are the kinds most checks of pods look at. Pods,
	// ReplicaSets and Jobs that a workload controls are checked through
	// its template instead.
	workloadKinds = []kind{kindDeployment, kindStatefulSet, kindDaemonSet, kindJob, kindCronJob, kindPod}
	// serviceKinds are the workloads that run until stopped, which need
	// probes.
	serviceKinds = []kind{kindDeployment, kindStatefulSet, kindDaemonSet}
	// scaledKinds are the workloads whose replicas are worth a budget and
	// spreading.
	scaledKinds = []kind{kindDeployment, kindStatefulSet}
	claimKinds  = []kind{kindPersistentVolumeClaim}
)

// object is one object being checked, with its pod template read out.
type object struct {
	u  *unstructured.Unstructured
	gk schema.GroupKind
	// template is the object's pod template, or the pod itself; nil for
	// kinds without one.
	template *corev1.PodTemplateSpec
	// templateErr says why a template could not be read.
	templateErr error
	// replicas is the desired replica count, for kinds with one.
	replicas int32
}

// templatePaths are where kinds keep their pod template.
var templatePaths = map[kind][]string{
	kindDeployment:  {"spec", "template"},
	kindStatefulSet: {"spec", "template"},
	kindDaemonSet:   {"spec", "template"},
	kindReplicaSet:  {"spec", "template"},
	kindJob:         {"spec", "template"},
	kindCronJob:     {"spec", "jobTemplate", "spec", "template"},
}

func newObject(u *unstructured.Unstructured) *object {
	o := &object{u: u, gk: u.GroupVersionKind().GroupKind()}
	k := kind{Group: o.gk.Group, Kind: o.gk.Kind}
	switch path, ok := templatePaths[k]; {
	case k == kindPod:
		var p corev1.Pod
		o.templateErr = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p)
		o.template = &corev1.PodTemplateSpec{ObjectMeta: p.ObjectMeta, Spec: p.Spec}
	case ok:
		o.template = &corev1.PodTemplateSpec{}
		if m, found, err := unstructured.NestedMap(u.Object, path...); err != nil {
			o.templateErr = err
		} else if found {
			o.templateErr = runtime.DefaultUnstructuredConverter.FromUnstructured(m, o.template)
		}
	}
	if o.templateErr != nil {
		o.templateErr = fmt.Errorf("reading its pod template: %w", o.templateErr)
	}
	if containsKind(withReplicas, k) {
		o.replicas = 1 // the API server's default
		if v, found, err := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas"); err == nil && found {
			switch n := v.(type) {
			case int64:
				o.replicas = int32(n)
			case float64:
				o.replicas = int32(n)
			}
		}
	}
	return o
}

// ref names the object as tug's findings do: deploy/api, database/orders.
func (o *object) ref() string { return ops.ShortKind(o.gk.Kind) + "/" + o.u.GetName() }

// controlled reports whether a workload built into Kubernetes controls m,
// so that its template is what to fix: a Deployment's ReplicaSets, their
// pods, a CronJob's Jobs.
func controlled(m metav1.Object) bool {
	ref := metav1.GetControllerOf(m)
	if ref == nil {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	return containsKind(withTemplate, kind{Group: gv.Group, Kind: ref.Kind})
}

// joinWords lists words as "a, b and c", with conj between the last two.
func joinWords(words []string, conj string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}
