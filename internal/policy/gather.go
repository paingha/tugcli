package policy

import (
	"context"
	"fmt"
	"strings"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// Inventory is what checks look at: the objects, and the
// PodDisruptionBudgets in their namespaces.
type Inventory struct {
	Objects              []*unstructured.Unstructured
	PodDisruptionBudgets []policyv1.PodDisruptionBudget
	// BudgetsUnknown is set when the budgets could not be listed;
	// podDisruptionBudget checks then report nothing.
	BudgetsUnknown bool
	// Unlisted names the resources the cluster refused to list.
	Unlisted []string
}

// Scope says what Gather lists.
type Scope struct {
	// Namespace to list in; "" for every namespace, which also takes in
	// the kinds that are not namespaced.
	Namespace string
	// Object, when set, is the only object to check.
	Object *resolve.Object
}

// pageSize is how many objects one list request asks for.
const pageSize = 500

// lastApplied is the annotation where kubectl apply keeps an object's last
// configuration, which for a Secret holds its data.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Gather lists the objects in scope that the checks of packs apply to,
// and the PodDisruptionBudgets that podDisruptionBudget checks need. Pods,
// ReplicaSets and Jobs that a workload controls are left out, as their
// workload's template is checked instead, and so are kinds the cluster
// does not serve. Resources the cluster refuses to list are named in
// Unlisted. Gather only reads, and reads only the metadata of Secrets.
func Gather(ctx context.Context, c ops.Clients, mapper meta.RESTMapper, packs []*Pack, s Scope) (*Inventory, error) {
	inv := &Inventory{}
	var kinds []kind
	budgets := false
	for _, p := range packs {
		for _, ch := range p.Checks {
			for _, k := range ch.kinds {
				if !containsKind(kinds, k) {
					kinds = append(kinds, k)
				}
			}
			if _, ok := ch.rule.(budgetRule); ok {
				budgets = true
			}
		}
	}

	budgetNamespace := s.Namespace
	if o := s.Object; o != nil {
		u, err := getObject(ctx, c, *o)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", o, err)
		}
		inv.Objects = []*unstructured.Unstructured{u}
		budgetNamespace = o.Namespace
		budgets = budgets && containsKind(withReplicas, kind{Group: o.Resource.Group, Kind: o.Kind})
	} else {
		for _, k := range kinds {
			m, err := mapping(mapper, k)
			if meta.IsNoMatchError(err) {
				continue // the cluster does not serve the kind, so nothing can fail
			}
			if err != nil {
				return nil, fmt.Errorf("looking up kind %s: %w", k, err)
			}
			if m.Scope.Name() == meta.RESTScopeNameRoot && s.Namespace != "" {
				continue // cluster-scoped kinds are checked across all namespaces
			}
			objs, err := listKind(ctx, c, m, k, s.Namespace)
			if refused(err) {
				inv.Unlisted = append(inv.Unlisted, m.Resource.GroupResource().String())
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("listing %s: %w", m.Resource.GroupResource(), err)
			}
			inv.Objects = append(inv.Objects, objs...)
		}
	}

	if budgets {
		list, err := listBudgets(ctx, c, budgetNamespace)
		switch {
		case refused(err):
			inv.BudgetsUnknown = true
			inv.Unlisted = append(inv.Unlisted, "poddisruptionbudgets.policy")
		case err != nil:
			return nil, fmt.Errorf("listing PodDisruptionBudgets: %w", err)
		default:
			inv.PodDisruptionBudgets = list
		}
	}
	return inv, nil
}

// refused reports whether a list failed because the cluster will not or
// cannot serve it to this user, rather than because it cannot be reached.
func refused(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err)
}

// mapping finds where the cluster serves a kind. Kinds built into
// Kubernetes are looked up exactly; others by their group and their name
// in any case.
func mapping(mapper meta.RESTMapper, k kind) (*meta.RESTMapping, error) {
	if b, ok := builtinKind(k.Kind); ok && b.Group == k.Group {
		return mapper.RESTMapping(schema.GroupKind{Group: k.Group, Kind: b.Kind})
	}
	gvk, err := mapper.KindFor(schema.GroupVersionResource{Group: k.Group, Resource: strings.ToLower(k.Kind)})
	if err != nil {
		return nil, err
	}
	return mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
}

// listKind lists the objects of a kind that checks look at in ns, or in
// every namespace for "".
func listKind(ctx context.Context, c ops.Clients, m *meta.RESTMapping, k kind, ns string) ([]*unstructured.Unstructured, error) {
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}
	switch k {
	case kindSecret:
		items, err := listMetadata(ctx, c.Metadata, m.Resource, ns)
		if err != nil {
			return nil, err
		}
		out := make([]*unstructured.Unstructured, len(items))
		for i := range items {
			out[i] = fromMetadata(&items[i], m.GroupVersionKind)
		}
		return out, nil
	case kindPod:
		// Most pods belong to a workload, so read their metadata first and
		// only the pods no workload controls in full.
		items, err := listMetadata(ctx, c.Metadata, m.Resource, ns)
		if err != nil {
			return nil, err
		}
		var out []*unstructured.Unstructured
		for i := range items {
			if controlled(&items[i]) {
				continue
			}
			pods, err := listObjects(ctx, c.Dynamic, m.Resource, m.GroupVersionKind, items[i].Namespace, items[i].Name)
			if err != nil {
				return nil, err
			}
			out = append(out, pods...)
		}
		return out, nil
	}
	items, err := listObjects(ctx, c.Dynamic, m.Resource, m.GroupVersionKind, ns, "")
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, u := range items {
		if !controlled(u) {
			out = append(out, u)
		}
	}
	return out, nil
}

// getObject reads the one object a target names.
func getObject(ctx context.Context, c ops.Clients, o resolve.Object) (*unstructured.Unstructured, error) {
	gvk := o.Resource.GroupVersion().WithKind(o.Kind)
	if (kind{Group: o.Resource.Group, Kind: o.Kind}) == kindSecret {
		p, err := c.Metadata.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return fromMetadata(p, gvk), nil
	}
	items, err := listObjects(ctx, c.Dynamic, o.Resource, gvk, o.Namespace, o.Name)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, apierrors.NewNotFound(o.Resource.GroupResource(), o.Name)
	}
	return items[0], nil
}

// listObjects lists the objects of a resource in ns, all of them or only
// the one called name. The API server leaves the kind out of the items of
// a list, so it is set from gvk.
func listObjects(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, gvk schema.GroupVersionKind, ns, name string) ([]*unstructured.Unstructured, error) {
	opts := metav1.ListOptions{Limit: pageSize}
	if name != "" {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}
	var out []*unstructured.Unstructured
	for {
		list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			u := &list.Items[i]
			if name != "" && u.GetName() != name {
				continue // a server that ignores the field selector
			}
			u.SetGroupVersionKind(gvk)
			out = append(out, u)
		}
		if list.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = list.GetContinue()
	}
}

// listMetadata lists only the metadata of a resource's objects in ns.
func listMetadata(ctx context.Context, md metadata.Interface, gvr schema.GroupVersionResource, ns string) ([]metav1.PartialObjectMetadata, error) {
	opts := metav1.ListOptions{Limit: pageSize}
	var out []metav1.PartialObjectMetadata
	for {
		list, err := md.Resource(gvr).Namespace(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

// fromMetadata makes an object of only p's metadata, without the last
// configuration kubectl applied, which for a Secret holds its data.
func fromMetadata(p *metav1.PartialObjectMetadata, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetName(p.Name)
	u.SetNamespace(p.Namespace)
	u.SetUID(p.UID)
	u.SetLabels(p.Labels)
	u.SetOwnerReferences(p.OwnerReferences)
	if len(p.Annotations) > 0 {
		annotations := make(map[string]string, len(p.Annotations))
		for k, v := range p.Annotations {
			if k == lastApplied {
				v = "-"
			}
			annotations[k] = v
		}
		u.SetAnnotations(annotations)
	}
	return u
}

// listBudgets lists the PodDisruptionBudgets in ns, or in every namespace
// for "".
func listBudgets(ctx context.Context, c ops.Clients, ns string) ([]policyv1.PodDisruptionBudget, error) {
	opts := metav1.ListOptions{Limit: pageSize}
	var out []policyv1.PodDisruptionBudget
	for {
		list, err := c.Typed.PolicyV1().PodDisruptionBudgets(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}
