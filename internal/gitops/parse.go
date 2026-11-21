package gitops

import (
	"cmp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/paingha/tugcli/internal/ops"
)

// parse reads what an object of a kind reports about itself.
func parse(k kind, r schema.GroupVersionResource, u *unstructured.Unstructured) *Object {
	o := &Object{
		Tool: k.tool, Kind: k.name, Name: u.GetName(), Namespace: u.GetNamespace(),
		Generation: u.GetGeneration(), ObservedGeneration: num(u.Object, "status", "observedGeneration"),
		Conditions: conditions(u),
		k:          k, resource: r, u: u,
	}
	if k.tool == ArgoCD {
		parseApplication(o)
	} else {
		parseFlux(o)
	}
	return o
}

func parseFlux(o *Object) {
	u := o.u
	o.Suspended, _, _ = unstructured.NestedBool(u.Object, "spec", "suspend")
	o.Interval = text(u.Object, "spec", "interval")
	o.Ready = fluxReadiness(o)
	if c := o.condition("Ready"); c != nil {
		o.Reason, o.Message = c.Reason, compact(c.Message)
	}
	for _, c := range o.Conditions {
		if c.LastTransition.After(o.LastReconcile) {
			o.LastReconcile = c.LastTransition
		}
	}
	if o.k.source {
		o.Revision = text(u.Object, "status", "artifact", "revision")
		if t := timestamp(u.Object, "status", "artifact", "lastUpdateTime"); t.After(o.LastReconcile) {
			o.LastReconcile = t
		}
	}
	switch o.Kind {
	case KindHelmChart:
		o.Source = sourceRef(u.Object, "", o.Namespace, "spec", "sourceRef")
	case KindKustomization:
		o.Source = sourceRef(u.Object, KindGitRepository, o.Namespace, "spec", "sourceRef")
		o.Applied = text(u.Object, "status", "lastAppliedRevision")
		o.Attempted = text(u.Object, "status", "lastAttemptedRevision")
		o.DependsOn = dependsOn(u.Object, KindKustomization, o.Namespace)
	case KindHelmRelease:
		o.Source = helmReleaseSource(o)
		o.Attempted = text(u.Object, "status", "lastAttemptedRevision")
		o.Applied = text(u.Object, "status", "lastAppliedRevision") // v2beta1
		for _, h := range maps(u.Object, "status", "history") {
			if str(h["status"]) == "deployed" {
				o.Applied = str(h["chartVersion"])
				if t := parseTime(str(h["lastDeployed"])); t.After(o.LastReconcile) {
					o.LastReconcile = t
				}
				break
			}
		}
		o.DependsOn = dependsOn(u.Object, KindHelmRelease, o.Namespace)
	}
}

// fluxReadiness reads a Flux object's state from its Ready, Stalled and
// Reconciling conditions and its generation.
func fluxReadiness(o *Object) ops.Health {
	ready := o.condition("Ready")
	switch {
	case len(o.Conditions) == 0:
		return ops.HealthNone // an OCI HelmRepository has no status
	case o.conditionTrue("Stalled"), ready != nil && ready.Status == string(metav1.ConditionFalse):
		return ops.HealthNotReady
	case ready == nil, ready.Status != string(metav1.ConditionTrue), o.conditionTrue("Reconciling"), o.lagging():
		return ops.HealthUnknown
	default:
		return ops.HealthReady
	}
}

// lagging reports whether the object's controller has yet to reconcile its
// latest generation.
func (o *Object) lagging() bool {
	return o.Generation > 0 && o.ObservedGeneration > 0 && o.ObservedGeneration < o.Generation
}

// helmReleaseSource returns where a HelmRelease's chart comes from: the
// object spec.chartRef names, or the HelmChart helm-controller makes from
// spec.chart, which is named after the HelmRelease in its source's
// namespace.
func helmReleaseSource(o *Object) *Ref {
	u := o.u
	if r := sourceRef(u.Object, KindOCIRepository, o.Namespace, "spec", "chartRef"); r != nil {
		return r
	}
	if ns, name, ok := strings.Cut(text(u.Object, "status", "helmChart"), "/"); ok {
		return &Ref{Kind: KindHelmChart, Name: name, Namespace: ns}
	}
	src := sourceRef(u.Object, KindHelmRepository, o.Namespace, "spec", "chart", "spec", "sourceRef")
	if src == nil {
		return nil
	}
	return &Ref{Kind: KindHelmChart, Name: o.Namespace + "-" + o.Name, Namespace: src.Namespace}
}

// chartSource returns the repository a HelmRelease's HelmChart comes from,
// as spec.chart.spec.sourceRef names it.
func chartSource(o *Object) *Ref {
	return sourceRef(o.u.Object, KindHelmRepository, o.Namespace, "spec", "chart", "spec", "sourceRef")
}

func parseApplication(o *Object) {
	u := o.u.Object
	o.Sync = text(u, "status", "sync", "status")
	o.Health = text(u, "status", "health", "status")
	o.Revision = cmp.Or(text(u, "status", "sync", "revision"), strings.Join(strs(u, "status", "sync", "revisions"), ","))
	if h := maps(u, "status", "history"); len(h) > 0 {
		last := h[len(h)-1] // oldest first
		o.Applied = cmp.Or(str(last["revision"]), joinAny(last["revisions"]))
	}
	o.Attempted = cmp.Or(text(u, "status", "operationState", "syncResult", "revision"),
		strings.Join(strs(u, "status", "operationState", "syncResult", "revisions"), ","))
	o.LastReconcile = timestamp(u, "status", "reconciledAt")
	o.Repo = text(u, "spec", "source", "repoURL")
	if o.Repo == "" {
		if s := maps(u, "spec", "sources"); len(s) > 0 {
			o.Repo = str(s[0]["repoURL"])
		}
	}
	o.Destination = &Destination{
		Server: text(u, "spec", "destination", "server"), Name: text(u, "spec", "destination", "name"),
		Namespace: text(u, "spec", "destination", "namespace"),
	}
	o.Ready, o.Reason, o.Message = applicationState(o)
}

// applicationState reads an Application's state: ready when it is synced
// and healthy; not ready when it is degraded or missing, Argo CD reports an
// error, or its last sync failed; and unknown while it progresses or waits
// to be synced.
func applicationState(o *Object) (ops.Health, string, string) {
	for _, c := range o.Conditions {
		if strings.HasSuffix(c.Type, "Error") {
			return ops.HealthNotReady, c.Type, compact(c.Message)
		}
	}
	u := o.u.Object
	phase := text(u, "status", "operationState", "phase")
	if (phase == "Failed" || phase == "Error") && o.Sync != "Synced" {
		return ops.HealthNotReady, "Sync" + phase, compact(text(u, "status", "operationState", "message"))
	}
	msg := firstLine(text(u, "status", "health", "message"))
	switch {
	case o.Health == "Degraded" || o.Health == "Missing":
		return ops.HealthNotReady, o.Health, msg
	case o.Sync == "Synced" && o.Health == "Healthy":
		return ops.HealthReady, "", ""
	case o.Sync == "" && o.Health == "":
		return ops.HealthNone, "", ""
	case o.Sync == "OutOfSync":
		return ops.HealthUnknown, o.Sync, msg
	default:
		return ops.HealthUnknown, cmp.Or(o.Health, o.Sync), msg
	}
}

// remote reports whether a Kustomization or HelmRelease deploys to another
// cluster, through spec.kubeConfig.
func (o *Object) remote() bool {
	_, ok, _ := unstructured.NestedMap(o.u.Object, "spec", "kubeConfig")
	return ok
}

// inCluster reports whether an Application deploys to the cluster Argo CD
// runs in, whose objects tug can then read.
func (o *Object) inCluster() bool {
	d := o.Destination
	return d == nil || d.Server == "https://kubernetes.default.svc" || d.Name == "in-cluster" || (d.Server == "" && d.Name == "")
}

// conditions reads an object's status conditions. Argo CD's have no status:
// each is there because it holds, so tug reads them as true.
func conditions(u *unstructured.Unstructured) []ops.Condition {
	var out []ops.Condition
	for _, m := range maps(u.Object, "status", "conditions") {
		c := ops.Condition{
			Type: str(m["type"]), Status: cmp.Or(str(m["status"]), string(metav1.ConditionTrue)),
			Reason: str(m["reason"]), Message: str(m["message"]),
			ObservedGeneration: toInt64(m["observedGeneration"]), LastTransition: parseTime(str(m["lastTransitionTime"])),
		}
		if c.Type != "" {
			out = append(out, c)
		}
	}
	return out
}

// sourceRef reads a reference to a Flux object, such as spec.sourceRef,
// whose kind and namespace default to those given. It is nil when the
// field is missing.
func sourceRef(obj map[string]any, kind, ns string, fields ...string) *Ref {
	m, ok, _ := unstructured.NestedMap(obj, fields...)
	if !ok || str(m["name"]) == "" {
		return nil
	}
	return &Ref{Kind: cmp.Or(str(m["kind"]), kind), Name: str(m["name"]), Namespace: cmp.Or(str(m["namespace"]), ns)}
}

// dependsOn reads spec.dependsOn, whose entries are of the same kind as
// the object and in its namespace unless they say otherwise.
func dependsOn(obj map[string]any, kind, ns string) []Ref {
	var out []Ref
	for _, m := range maps(obj, "spec", "dependsOn") {
		if name := str(m["name"]); name != "" {
			out = append(out, Ref{Kind: cmp.Or(str(m["kind"]), kind), Name: name, Namespace: cmp.Or(str(m["namespace"]), ns)})
		}
	}
	return out
}

// objectRef names an object Flux or Argo CD manages.
type objectRef struct {
	Group, Version, Kind, Namespace, Name string
}

func (r objectRef) String() string { return strings.ToLower(r.Kind) + "/" + r.Name }

// inventory reads a Flux object's status.inventory: entries whose id is
// namespace_name_group_kind, with RBAC names' colons written as "__".
func inventory(u *unstructured.Unstructured) []objectRef {
	var out []objectRef
	for _, e := range maps(u.Object, "status", "inventory", "entries") {
		if r, ok := parseInventoryID(str(e["id"]), str(e["v"])); ok {
			out = append(out, r)
		}
	}
	return out
}

func parseInventoryID(id, version string) (objectRef, bool) {
	ns, rest, ok := strings.Cut(id, "_")
	if !ok {
		return objectRef{}, false
	}
	i := strings.LastIndex(rest, "_")
	if i < 0 {
		return objectRef{}, false
	}
	rest, kindName := rest[:i], rest[i+1:]
	i = strings.LastIndex(rest, "_")
	if i < 0 {
		return objectRef{}, false
	}
	name, group := rest[:i], rest[i+1:]
	if name == "" || kindName == "" {
		return objectRef{}, false
	}
	return objectRef{Group: group, Version: version, Kind: kindName, Namespace: ns, Name: strings.ReplaceAll(name, "__", ":")}, true
}

// argoResource is one entry of an Application's status.resources.
type argoResource struct {
	objectRef
	Status          string
	Health          string
	HealthMessage   string
	RequiresPruning bool
	Hook            bool
}

func argoResources(u *unstructured.Unstructured) []argoResource {
	var out []argoResource
	for _, m := range maps(u.Object, "status", "resources") {
		r := argoResource{
			objectRef: objectRef{Group: str(m["group"]), Version: str(m["version"]), Kind: str(m["kind"]), Namespace: str(m["namespace"]), Name: str(m["name"])},
			Status:    str(m["status"]),
		}
		if h, ok := m["health"].(map[string]any); ok {
			r.Health, r.HealthMessage = str(h["status"]), str(h["message"])
		}
		r.RequiresPruning, _ = m["requiresPruning"].(bool)
		r.Hook, _ = m["hook"].(bool)
		if r.Kind != "" && r.Name != "" {
			out = append(out, r)
		}
	}
	return out
}

func maps(obj map[string]any, fields ...string) []map[string]any {
	raw, _, _ := unstructured.NestedSlice(obj, fields...)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func text(obj map[string]any, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}

func strs(obj map[string]any, fields ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(obj, fields...)
	return s
}

func num(obj map[string]any, fields ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	return toInt64(v)
}

func timestamp(obj map[string]any, fields ...string) time.Time {
	return parseTime(text(obj, fields...))
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func joinAny(v any) string {
	l, _ := v.([]any)
	parts := make([]string, 0, len(l))
	for _, x := range l {
		if s := str(x); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ",")
}

// toInt64 reads a number that JSON may have decoded as either an integer
// or a float.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
