package resolve

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

// Object is any Kubernetes object, named by its resource, for the commands
// that work on more than workloads, such as why and tree.
type Object struct {
	Resource schema.GroupVersionResource
	// Kind is the object's kind, such as Certificate.
	Kind string
	Name string
	// Namespace is "" for a cluster-scoped object.
	Namespace string
}

// String returns the kind in lower case and the name, as tug prints objects:
// "certificate/web-tls".
func (o Object) String() string { return strings.ToLower(o.Kind) + "/" + o.Name }

// Ref returns a name for the object that kubectl resolves to its resource
// whatever else the cluster serves: "certificates.cert-manager.io/web-tls",
// or "pods/api-0" for the core group.
func (o Object) Ref() string {
	r := o.Resource.Resource
	if o.Resource.Group != "" {
		r += "." + o.Resource.Group
	}
	return r + "/" + o.Name
}

// workloadResource is where the API serves a workload kind.
type workloadResource struct {
	resource schema.GroupVersionResource
	kind     string
}

var workloadResources = map[Kind]workloadResource{
	KindDeployment:  {schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "Deployment"},
	KindStatefulSet: {schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "StatefulSet"},
	KindDaemonSet:   {schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, "DaemonSet"},
	KindJob:         {schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "Job"},
	KindPod:         {schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "Pod"},
}

// ObjectOf returns the object behind a workload target.
func ObjectOf(t Target) Object {
	r := workloadResources[t.Kind]
	return Object{Resource: r.resource, Kind: r.kind, Name: t.Name, Namespace: t.Namespace}
}

// WorkloadKind returns the workload kind served by a group and resource, if
// tug knows it as a workload.
func WorkloadKind(gr schema.GroupResource) (Kind, bool) {
	for k, r := range workloadResources {
		if r.resource.GroupResource() == gr {
			return k, true
		}
	}
	return "", false
}

// Workload returns the workload target of an object whose kind tug knows as
// a workload. The target has no selector; the workload's own is read when
// it is fetched.
func (o Object) Workload() (Target, bool) {
	k, ok := WorkloadKind(o.Resource.GroupResource())
	if !ok {
		return Target{}, false
	}
	return Target{Kind: k, Name: o.Name, Namespace: o.Namespace}, true
}

// FindResource maps a resource as kubectl accepts it, plural, singular,
// short name or kind, with an optional group and version
// ("kustomizations.kustomize.toolkit.fluxcd.io", "deployments.v1.apps"), to
// the resource the cluster serves.
func FindResource(mapper meta.RESTMapper, resource string) (*meta.RESTMapping, error) {
	fully, gr := schema.ParseResourceArg(strings.ToLower(resource))
	var (
		gvk schema.GroupVersionKind
		err error
	)
	if fully != nil {
		gvk, err = mapper.KindFor(*fully)
	}
	if gvk.Empty() {
		gvk, err = mapper.KindFor(gr.WithVersion(""))
	}
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("the cluster has no resource type %q; list them with `tug api-resources`", resource)
		}
		return nil, fmt.Errorf("looking up resource type %q: %w", resource, err)
	}
	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("looking up resource type %q: %w", resource, err)
	}
	return m, nil
}

// Objects lists the objects of a resource in a namespace, or in the cluster
// when the resource is cluster-scoped. It reads metadata only, so a
// Secret's data never leaves the API server.
func Objects(ctx context.Context, md metadata.Interface, m *meta.RESTMapping, ns string) ([]Object, error) {
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}
	var out []Object
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := md.Resource(m.Resource).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for _, it := range list.Items {
			out = append(out, Object{Resource: m.Resource, Kind: m.GroupVersionKind.Kind, Name: it.Name, Namespace: it.Namespace})
		}
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}
