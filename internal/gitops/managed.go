package gitops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// Managed is an object a Kustomization, HelmRelease or Application manages,
// with its health.
type Managed struct {
	Kind      string `json:"kind"`
	Group     string `json:"group,omitempty"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Health is ready, not-ready, unknown, or none for an object that
	// reports no health, such as a ConfigMap.
	Health ops.Health `json:"health"`
	Status string     `json:"status"`
	// Sync is what Argo CD says of it: Synced or OutOfSync.
	Sync string `json:"sync,omitempty"`
}

// String returns the kind in lower case and the name: "deployment/api".
func (m Managed) String() string { return strings.ToLower(m.Kind) + "/" + m.Name }

const (
	// checkConcurrency bounds how many managed objects are checked at
	// once.
	checkConcurrency = 8
	// maxDeepChecks bounds how many managed objects other than workloads
	// get ops.WhyObject's diagnosis, each of which lists their whole
	// namespace.
	maxDeepChecks = 8
)

// statuslessResources report no health of their own, so tug only checks
// that they exist, through their metadata, which also keeps every Secret's
// data on the API server.
var statuslessResources = []schema.GroupResource{
	{Resource: "configmaps"},
	{Resource: "secrets"},
	{Resource: "serviceaccounts"},
	{Resource: "services"},
	{Resource: "limitranges"},
	{Resource: "resourcequotas"},
	{Group: "networking.k8s.io", Resource: "networkpolicies"},
	{Group: "rbac.authorization.k8s.io", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings"},
	{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"},
	{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
}

// checked is the outcome of checking one managed object.
type checked struct {
	managed  Managed
	findings []ops.Finding
}

// checker checks the health of the objects a GitOps object manages.
type checker struct {
	c    Clients
	s    *Snapshot
	opts ops.WhyOptions
	// owner names the GitOps object in findings' next steps.
	owner *Object

	mu   sync.Mutex
	deep int
}

// checkAll checks objects at most checkConcurrency at a time, returning
// their results in order.
func (k *checker) checkAll(ctx context.Context, refs []objectRef, check func(context.Context, objectRef) checked) ([]checked, error) {
	out := make([]checked, len(refs))
	sem := make(chan struct{}, checkConcurrency)
	var wg sync.WaitGroup
	for i, r := range refs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			out[i] = check(ctx, r)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// takeDeep reports whether one more object may get a deep check.
func (k *checker) takeDeep() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.deep >= maxDeepChecks {
		return false
	}
	k.deep++
	return true
}

// check checks one object Flux manages: a workload gets ops.WhyObject's
// diagnosis, a Flux object its own state, an object without health a check
// that it exists, and any other object its status conditions, with
// ops.WhyObject's diagnosis when they are not ready.
func (k *checker) check(ctx context.Context, r objectRef) checked {
	m := Managed{Kind: r.Kind, Group: r.Group, Name: r.Name, Namespace: r.Namespace}
	if o := k.gitopsObject(r); o != nil {
		return k.nested(m, o)
	}
	obj, err := k.object(r)
	if meta.IsNoMatchError(err) {
		m.Health, m.Status = ops.HealthUnknown, "the cluster does not serve this kind"
		return checked{managed: m, findings: []ops.Finding{{
			Severity: ops.SeverityWarning, Objects: []string{r.String()}, Reason: "UnknownKind",
			Message: fmt.Sprintf("the cluster serves no %s in %s, so it cannot be there", r.Kind, groupVersion(r)),
			Next:    "check that the CRD it needs is installed: `tug get crds`",
		}}}
	}
	if err != nil {
		m.Health, m.Status = ops.HealthUnknown, "its kind could not be looked up: "+firstLine(err.Error())
		return checked{managed: m}
	}
	if _, ok := obj.Workload(); ok {
		return k.diagnose(ctx, m, obj, ops.HealthNotReady)
	}
	if slices.Contains(statuslessResources, obj.Resource.GroupResource()) {
		_, err := k.c.Metadata.Resource(obj.Resource).Namespace(obj.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
		return k.existence(m, err)
	}
	u, err := k.c.Dynamic.Resource(obj.Resource).Namespace(obj.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
	if err != nil {
		return k.existence(m, err)
	}
	m.Health, m.Status = quickHealth(obj, u)
	if (m.Health == ops.HealthNotReady || m.Health == ops.HealthUnknown) && k.takeDeep() {
		return k.diagnose(ctx, m, obj, m.Health)
	}
	return checked{managed: m}
}

// diagnose gives an object ops.WhyObject's diagnosis, keeping its findings
// when it is not healthy, as unhealthy says.
func (k *checker) diagnose(ctx context.Context, m Managed, obj resolve.Object, unhealthy ops.Health) checked {
	d, err := ops.WhyObject(ctx, k.c.Clients, obj, k.opts)
	if err != nil {
		return k.existence(m, err)
	}
	m.Status = d.Summary
	if d.Healthy {
		m.Health = ops.HealthReady
		return checked{managed: m}
	}
	m.Health = unhealthy
	var findings []ops.Finding
	for _, f := range d.Findings {
		if f.Severity != ops.SeverityInfo {
			findings = append(findings, f)
		}
	}
	if len(findings) == 0 {
		findings = []ops.Finding{{Severity: ops.SeverityWarning, Objects: []string{obj.String()}, Reason: "NotReady",
			Message: d.Summary, Next: "see why: " + tugCommand(obj.Namespace, "why", obj.Ref())}}
	}
	return checked{managed: m, findings: findings}
}

// existence turns the error of reading an object into its health: missing,
// not allowed, or unreadable; or, without one, present with no health.
func (k *checker) existence(m Managed, err error) checked {
	switch {
	case err == nil:
		m.Health = ops.HealthNone
		return checked{managed: m}
	case apierrors.IsNotFound(err):
		m.Health, m.Status = ops.HealthNotReady, "missing"
		msg := "is not in the cluster, though " + k.owner.String() + " manages it"
		switch k.owner.Tool {
		case Flux:
			msg += "; Flux creates it again at its next reconcile"
			if k.owner.Interval != "" {
				msg += ", every " + k.owner.Interval
			}
		case ArgoCD:
			msg += "; Argo CD creates it at the next sync"
		}
		return checked{managed: m, findings: []ops.Finding{{Severity: ops.SeverityWarning, Objects: []string{m.String()}, Reason: "Missing", Message: msg}}}
	case apierrors.IsForbidden(err):
		m.Health, m.Status = ops.HealthUnknown, "not allowed to read it"
	default:
		m.Health, m.Status = ops.HealthUnknown, unreadable(err)
	}
	return checked{managed: m}
}

// unreadable says why an object could not be read. It repeats only what the
// API server said or why it could not be reached: an error from decoding an
// answer may quote it, which for a Secret would be its data.
func unreadable(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return "could not be read: " + firstLine(status.Status().Message)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "could not be read: " + firstLine(netErr.Error())
	}
	return "could not be read"
}

// nested judges a Flux or Argo CD object that another manages, such as the
// Kustomizations a root Kustomization applies, by its own state.
func (k *checker) nested(m Managed, o *Object) checked {
	m.Health, m.Status = o.Ready, statusText(o)
	if o.Ready != ops.HealthNotReady {
		return checked{managed: m}
	}
	next := "see why: " + tugCommand(o.Namespace, "gitops", "explain", o.String())
	if !delivers(o.Kind) {
		next = "see its conditions and events: " + why(o) // explain takes no source
	}
	return checked{managed: m, findings: []ops.Finding{{
		Severity: ops.SeverityError, Objects: []string{o.String()}, Reason: cmp.Or(o.Reason, "NotReady"),
		Message: "is not ready: " + cmp.Or(o.Message, statusText(o)), Next: next,
	}}}
}

// gitopsObject returns the Flux or Argo CD object a reference names, if it
// names one.
func (k *checker) gitopsObject(r objectRef) *Object {
	kd, ok := kindNamed(r.Kind)
	if !ok || kd.group != r.Group {
		return nil
	}
	return k.s.get(Ref{Kind: r.Kind, Name: r.Name, Namespace: r.Namespace})
}

// object maps a reference to the resource that serves it.
func (k *checker) object(r objectRef) (resolve.Object, error) {
	var versions []string
	if r.Version != "" {
		versions = []string{r.Version}
	}
	m, err := k.c.Mapper.RESTMapping(schema.GroupKind{Group: r.Group, Kind: r.Kind}, versions...)
	if err != nil {
		return resolve.Object{}, err
	}
	ns := r.Namespace
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		ns = ""
	}
	return resolve.Object{Resource: m.Resource, Kind: r.Kind, Name: r.Name, Namespace: ns}, nil
}

func groupVersion(r objectRef) string {
	return schema.GroupVersion{Group: r.Group, Version: r.Version}.String()
}

// quickHealth reads an object's health from its status conditions and
// generation, without the cost of ops.WhyObject: it decides only whether
// that diagnosis is worth asking for.
func quickHealth(obj resolve.Object, u *unstructured.Unstructured) (ops.Health, string) {
	switch obj.Resource.GroupResource() {
	case schema.GroupResource{Resource: "persistentvolumeclaims"}:
		phase := text(u.Object, "status", "phase")
		if phase == "Bound" {
			return ops.HealthReady, phase
		}
		return ops.HealthNotReady, cmp.Or(phase, "Pending")
	case schema.GroupResource{Resource: "namespaces"}:
		phase := text(u.Object, "status", "phase")
		if phase == "Terminating" {
			return ops.HealthNotReady, phase
		}
		return ops.HealthNone, phase
	}
	o := &Object{Generation: u.GetGeneration(), ObservedGeneration: num(u.Object, "status", "observedGeneration"), Conditions: conditions(u)}
	for _, c := range o.Conditions {
		for _, bad := range []string{"Stalled", "Degraded", "Failed"} {
			if c.Type == bad && c.Status == string(metav1.ConditionTrue) {
				return ops.HealthNotReady, conditionText(c)
			}
		}
	}
	for _, t := range []string{"Ready", "Available", "Healthy", "Synced", "Established"} {
		c := o.condition(t)
		if c == nil {
			continue
		}
		switch {
		case c.Status == string(metav1.ConditionFalse):
			return ops.HealthNotReady, conditionText(*c)
		case c.Status != string(metav1.ConditionTrue):
			return ops.HealthUnknown, conditionText(*c)
		case o.lagging():
			return ops.HealthUnknown, fmt.Sprintf("generation %d not yet reconciled", o.Generation)
		default:
			return ops.HealthReady, conditionText(*c)
		}
	}
	return ops.HealthNone, ""
}

// helmObjects finds the workloads of a HelmRelease that records no
// inventory, by the labels helm-controller puts on everything it installs,
// in the release's namespace.
func (k *checker) helmObjects(ctx context.Context, hr *Object) ([]objectRef, error) {
	ns := cmp.Or(text(hr.u.Object, "spec", "targetNamespace"), hr.Namespace)
	sel := labels.SelectorFromSet(labels.Set{
		"helm.toolkit.fluxcd.io/name": hr.Name, "helm.toolkit.fluxcd.io/namespace": hr.Namespace,
	}).String()
	var out []objectRef
	for _, w := range []struct{ group, kind, resource string }{
		{"apps", "Deployment", "deployments"},
		{"apps", "StatefulSet", "statefulsets"},
		{"apps", "DaemonSet", "daemonsets"},
		{"batch", "Job", "jobs"},
	} {
		r := schema.GroupVersionResource{Group: w.group, Version: "v1", Resource: w.resource}
		l, err := k.c.Metadata.Resource(r).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, fmt.Errorf("listing the %s of %s: %w", w.resource, hr, err)
		}
		for _, it := range l.Items {
			out = append(out, objectRef{Group: w.group, Version: "v1", Kind: w.kind, Namespace: it.Namespace, Name: it.Name})
		}
	}
	return out, nil
}
