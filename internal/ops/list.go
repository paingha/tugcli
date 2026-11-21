package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/paingha/tugcli/internal/resolve"
)

// ObjectState is an object as a list of any kind shows it: its health, a
// few words on how it stands, its controller and its age.
type ObjectState struct {
	Object resolve.Object
	Health Health
	// Status is what kubectl's STATUS column would show for a pod, how a
	// workload's rollout stands, or what another object's conditions say;
	// "" for objects with no status, such as ConfigMaps.
	Status string
	// Owner is the object's controller, such as "rs/api-server-7d9f", or
	// "" when it has none.
	Owner   string
	Created time.Time
}

var (
	pods         = schema.GroupResource{Resource: "pods"}
	deployments  = schema.GroupResource{Group: "apps", Resource: "deployments"}
	statefulSets = schema.GroupResource{Group: "apps", Resource: "statefulsets"}
	daemonSets   = schema.GroupResource{Group: "apps", Resource: "daemonsets"}
)

// ListObjects lists the objects of a resource in a namespace, or in the
// cluster when the resource is cluster-scoped, with their state, sorted by
// namespace and name. Objects with no status of their own, Secrets among
// them, are read as metadata only, so a Secret's data never leaves the API
// server. It only reads.
func ListObjects(ctx context.Context, c Clients, m *meta.RESTMapping, ns string) ([]ObjectState, error) {
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}
	kind := m.GroupVersionKind.Kind
	var out []ObjectState
	if statusless(resolve.Object{Resource: m.Resource}) {
		items, err := listMetadata(ctx, c.Metadata, m.Resource, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, ObjectState{
				Object: resolve.Object{Resource: m.Resource, Kind: kind, Name: it.Name, Namespace: it.Namespace},
				Health: HealthNone, Owner: controllerName(it.OwnerReferences), Created: it.CreationTimestamp.Time,
			})
		}
	} else {
		opts := metav1.ListOptions{Limit: 500}
		for {
			list, err := c.Dynamic.Resource(m.Resource).Namespace(ns).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				u := &list.Items[i]
				o := resolve.Object{Resource: m.Resource, Kind: cmp.Or(u.GetKind(), kind), Name: u.GetName(), Namespace: u.GetNamespace()}
				health, status := stateOf(o, u)
				out = append(out, ObjectState{
					Object: o, Health: health, Status: status,
					Owner: controllerName(u.GetOwnerReferences()), Created: u.GetCreationTimestamp().Time,
				})
			}
			if list.GetContinue() == "" {
				break
			}
			opts.Continue = list.GetContinue()
		}
	}
	slices.SortStableFunc(out, func(a, b ObjectState) int {
		return cmp.Or(cmp.Compare(a.Object.Namespace, b.Object.Namespace), cmp.Compare(a.Object.Name, b.Object.Name))
	})
	return out, nil
}

// stateOf reads an object's health and status: as the workloads list does
// for pods and workloads, and from its conditions for anything else.
func stateOf(o resolve.Object, u *unstructured.Unstructured) (Health, string) {
	gr := o.Resource.GroupResource()
	switch gr {
	case pods:
		var p corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
			return unreadable(err)
		}
		return healthOf(podHealthy(&p)), podStatus(&p)
	case deployments:
		var d appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &d); err != nil {
			return unreadable(err)
		}
		r := deploymentRollout(&d)
		return healthOf(r.Complete), r.Summary
	case statefulSets:
		var s appsv1.StatefulSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &s); err != nil {
			return unreadable(err)
		}
		r := statefulSetRollout(&s)
		return healthOf(r.Complete), r.Summary
	case daemonSets:
		var d appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &d); err != nil {
			return unreadable(err)
		}
		r := daemonSetRollout(&d)
		return healthOf(r.Complete), r.Summary
	case jobs:
		var j batchv1.Job
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &j); err != nil {
			return unreadable(err)
		}
		return healthOf(jobCondition(&j, batchv1.JobFailed) == nil), jobRollout(&j).Summary
	}
	a := assess(o, u)
	return a.health, a.status
}

func unreadable(err error) (Health, string) {
	return HealthUnknown, fmt.Sprintf("could not be read: %s", firstLine(err.Error()))
}

// controllerName names the controller among an object's owners as tug
// prints objects, "rs/api-server-7d9f", or returns "".
func controllerName(refs []metav1.OwnerReference) string {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return ShortKind(r.Kind) + "/" + r.Name
		}
	}
	return ""
}
