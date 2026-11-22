package diff

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"

	"github.com/paingha/tugcli/internal/resolve"
)

// DefaultFieldManager is the field manager tug's dry runs apply as.
const DefaultFieldManager = "tug"

// Clients are what diff needs to read one cluster.
type Clients struct {
	Dynamic  dynamic.Interface
	Metadata metadata.Interface
}

// IsSecret reports whether a resource is the core group's Secrets, whose
// data tug never reads.
func IsSecret(r schema.GroupVersionResource) bool {
	return r.Group == "" && r.Resource == "secrets"
}

// Get returns an object as the API server has it, with its managedFields,
// or nil when it does not exist. A Secret is read through the metadata API,
// so its data never leaves the API server.
func Get(ctx context.Context, c Clients, o resolve.Object) (map[string]any, error) {
	if IsSecret(o.Resource) {
		p, err := c.Metadata.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", o, err)
		}
		return secretMetadata(p)
	}
	u, err := c.Dynamic.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", o, err)
	}
	return u.Object, nil
}

// secretMetadata turns a Secret's metadata, read through the metadata API,
// into an object of kind Secret with nothing but its metadata.
func secretMetadata(p *metav1.PartialObjectMetadata) (map[string]any, error) {
	md, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&p.ObjectMeta)
	if err != nil {
		return nil, fmt.Errorf("reading the metadata of secret %s: %w", p.Name, err)
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": md}, nil
}

// Applied is what a server-side dry-run apply would make of an object.
type Applied struct {
	Object map[string]any
	// Conflicts are the fields the apply would take from other managers.
	// The dry run was then forced to get Object.
	Conflicts []Conflict
}

// DryRunApply asks the API server what a server-side apply of manifest
// would make of object o, as manager, without changing anything: admission,
// including mutating webhooks, and defaulting run, but nothing is stored.
// When the apply would conflict with other field managers, the conflicts
// are returned and the dry run is repeated with force, as
// --force-conflicts would apply it.
func DryRunApply(ctx context.Context, c Clients, o resolve.Object, manifest map[string]any, manager string) (*Applied, error) {
	if IsSecret(o.Resource) {
		return nil, fmt.Errorf("not applying %s: tug never reads a Secret's data, which a dry run returns", o)
	}
	res := c.Dynamic.Resource(o.Resource).Namespace(o.Namespace)
	opts := metav1.ApplyOptions{FieldManager: manager, DryRun: []string{metav1.DryRunAll}}
	u := &unstructured.Unstructured{Object: manifest}
	out, err := res.Apply(ctx, o.Name, u, opts)
	var conflicts []Conflict
	if apierrors.IsConflict(err) {
		if conflicts = conflictsOf(err); len(conflicts) > 0 {
			opts.Force = true
			out, err = res.Apply(ctx, o.Name, u, opts)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dry-run applying %s: %w", o, err)
	}
	return &Applied{Object: out.Object, Conflicts: conflicts}, nil
}

// conflictsOf reads the field manager conflicts out of an apply's error.
func conflictsOf(err error) []Conflict {
	var st apierrors.APIStatus
	if !errors.As(err, &st) || st.Status().Details == nil {
		return nil
	}
	var out []Conflict
	for _, c := range st.Status().Details.Causes {
		if c.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		out = append(out, Conflict{
			Manager: conflictManager(c.Message),
			Field:   strings.TrimPrefix(c.Field, "."),
			Message: c.Message,
		})
	}
	return out
}

// conflictManager takes the manager's name from a conflict's message, such
// as `conflict with "helm" using apps/v1`.
func conflictManager(msg string) string {
	_, rest, ok := strings.Cut(msg, `conflict with "`)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

// Locate finds where the API server keeps the object a manifest describes.
// A namespaced object without a namespace is in ns.
func Locate(mapper meta.RESTMapper, manifest map[string]any, ns string) (resolve.Object, error) {
	u := unstructured.Unstructured{Object: manifest}
	gvk := u.GroupVersionKind()
	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		return resolve.Object{}, fmt.Errorf("the cluster serves no %s in %s; check the apiVersion, or install the CustomResourceDefinition that defines it", gvk.Kind, gvk.GroupVersion())
	}
	if err != nil {
		return resolve.Object{}, fmt.Errorf("looking up %s %s: %w", gvk.GroupVersion(), gvk.Kind, err)
	}
	o := resolve.Object{Resource: m.Resource, Kind: gvk.Kind, Name: u.GetName()}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		o.Namespace = u.GetNamespace()
		if o.Namespace == "" {
			o.Namespace = ns
		}
	}
	return o, nil
}

// File compares object o as it is with what applying manifest as
// opts.FieldManager would make of it, by a server-side dry run. Each
// difference is attributed to its owners on both sides and to what set the
// new value: the manifest, admission or the API server's defaults. A dry
// run says nothing about what controllers do once the apply is made.
func File(ctx context.Context, c Clients, o resolve.Object, manifest map[string]any, opts Options) (*Result, error) {
	live, err := Get(ctx, c, o)
	if err != nil {
		return nil, err
	}
	applied, err := DryRunApply(ctx, c, o, manifest, opts.FieldManager)
	if err != nil {
		return nil, err
	}
	r, err := compareSides(sides{from: live, to: applied.Object, manifest: manifest, text: true}, opts)
	if err != nil {
		return nil, fmt.Errorf("comparing %s: %w", o, err)
	}
	r.Object, r.Namespace = o.String(), o.Namespace
	r.Conflicts = applied.Conflicts
	markConflicts(r)
	return r, nil
}

// Against compares object o on one cluster with the same object, by
// resource, namespace and name, on another. Fields that only the clusters
// set themselves are left out unless opts.AllFields; each other difference
// is attributed to its owners on both clusters, and flagged when an apply
// of the other cluster's value here, as opts.FieldManager, would conflict
// with a manager here.
func Against(ctx context.Context, here, there Clients, o resolve.Object, opts Options) (*Result, error) {
	a, err := Get(ctx, here, o)
	if err != nil {
		return nil, err
	}
	b, err := Get(ctx, there, o)
	if err != nil {
		return nil, err
	}
	r, err := compareSides(sides{from: a, to: b, acrossClusters: true, text: true}, opts)
	if err != nil {
		return nil, fmt.Errorf("comparing %s: %w", o, err)
	}
	r.Object, r.Namespace = o.String(), o.Namespace
	return r, nil
}
