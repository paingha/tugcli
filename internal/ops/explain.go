package ops

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"

	"github.com/paingha/tugcli/internal/resolve"
)

// Explanation is what tug can tell about how an object is reconciled:
// WhyObject's diagnosis, who has written it, which controller most likely
// reconciles it and on what evidence, and whether that controller runs.
type Explanation struct {
	Target    string `json:"target"`
	Namespace string `json:"namespace"`
	Healthy   bool   `json:"healthy"`
	Summary   string `json:"summary"`
	// Generation is metadata.generation, which the API server raises with
	// each change to the spec. ObservedGeneration is the generation the
	// controller says it last reconciled, from status.observedGeneration
	// or the Ready condition; 0 when it says none.
	Generation         int64       `json:"generation,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
	Owners             []Owner     `json:"owners"`
	// Managers are the field managers that have written it, the latest
	// first.
	Managers []FieldManager `json:"managers"`
	// Controllers are the controllers that may reconcile it, the likeliest
	// first.
	Controllers []ControllerCandidate `json:"controllers"`
	Findings    []Claim               `json:"findings"`
	// Events are the most recent Warning events about it and what it owns,
	// newest first.
	Events []Event `json:"events"`
}

// Owner is one of an object's ownerReferences.
type Owner struct {
	// Object is the owner's kind in lower case and its name, such as
	// "database/orders".
	Object string `json:"object"`
	// Controller is true for the owner that manages the object.
	Controller bool `json:"controller"`
}

// FieldManager is an entry of an object's managedFields: a client that has
// written some of its fields, and when it last did.
type FieldManager struct {
	Manager string `json:"manager"`
	// Operation is Apply or Update.
	Operation   string `json:"operation"`
	Subresource string `json:"subresource,omitempty"`
	// Fields are the top-level fields it owns, such as spec and status.
	Fields []string  `json:"fields,omitempty"`
	Time   time.Time `json:"time,omitzero"`
}

// WritesStatus reports whether the manager writes the object's status, as
// the controller that reconciles it does.
func (m FieldManager) WritesStatus() bool {
	return m.Subresource == "status" || slices.Contains(m.Fields, "status")
}

// ControllerCandidate is a controller that may reconcile an object, with the
// evidence for it.
type ControllerCandidate struct {
	// Name is how the controller names itself, as a field manager or an
	// event reporter, or the name of the workload tug found for it.
	Name string `json:"name"`
	// Workload is the deployment, statefulset or daemonset that tug takes
	// to run the controller, when it found one.
	Workload  string `json:"workload,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Basis is observed when tug names the controller only as it names
	// itself, and inferred when tug ties it to a workload.
	Basis    Basis      `json:"basis"`
	Evidence []Evidence `json:"evidence"`
	// Healthy, Summary and Leader describe the workload of the likeliest
	// candidate that has one.
	Healthy *bool  `json:"healthy,omitempty"`
	Summary string `json:"summary,omitempty"`
	Leader  string `json:"leader,omitempty"`
}

// Evidence is one reason to take a controller for an object's.
type Evidence struct {
	Basis Basis  `json:"basis"`
	Text  string `json:"text"`
}

// Explain explains how an object is reconciled. It starts from WhyObject's
// diagnosis, each of whose findings restates what the API reports, then
// looks for the controller that reconciles the object: the field managers
// that write its status, who reported events about it, the service accounts
// whose roles let them update its status, and the operators that usually
// serve its API group. Each controller candidate and each finding is
// labelled as observed or inferred. For the likeliest candidate it finds a
// workload for, it checks that the workload runs and leads, and ties that
// to the object's state. It only reads, and never reads a Secret's data.
func Explain(ctx context.Context, c Clients, o resolve.Object, opts WhyOptions) (*Explanation, error) {
	now := nowOf(opts)()
	d, err := WhyObject(ctx, c, o, opts)
	if err != nil {
		return nil, err
	}
	u, err := getObject(ctx, c, o)
	if err != nil {
		return nil, err
	}
	st := readStatus(u)
	gen, seen, lagging := st.lag()
	out := &Explanation{
		Target: d.Target, Namespace: d.Namespace, Summary: d.Summary,
		Generation: gen, ObservedGeneration: seen, Conditions: d.Conditions,
		Owners: ownersOf(u), Managers: fieldManagers(u), Controllers: []ControllerCandidate{},
		Findings: claims(d.Findings), Events: d.Events,
	}
	var extra []Claim
	add := func(cl Claim) { extra = append(extra, cl) }

	reporters, err := eventReporters(ctx, c, o, u.GetUID())
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	id := &identifier{c: c, o: o, byKey: map[string]*candidate{}}
	if err := id.identify(ctx, out.Managers, reporters); err != nil {
		return nil, err
	}
	cands := id.ranked()
	for _, cl := range id.notes {
		add(cl)
	}

	ref := o.String()
	self := func(args ...string) string { return suggest(o, opts.ExplicitNamespace, args...) }
	if dt := u.GetDeletionTimestamp(); dt != nil && len(u.GetFinalizers()) > 0 && now.Sub(dt.Time) > time.Minute {
		f := Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Terminating",
			Message: fmt.Sprintf("was deleted at %s, but finalizers %s keep it until the controller that set them removes them",
				dt.UTC().Format(time.RFC3339), strings.Join(u.GetFinalizers(), ", ")),
			Next: "check that its controller runs; remove a finalizer by hand only once you know what it guards"}
		add(observed(f))
	}
	if _, ok := o.Workload(); !ok && o.Resource.GroupResource() != cronJobs {
		for _, field := range []string{"suspend", "paused"} {
			if on, _, _ := unstructured.NestedBool(u.Object, "spec", field); on {
				add(observed(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Suspended",
					Message: fmt.Sprintf("spec.%s is true, so its controller leaves it as it is", field),
					Next:    fmt.Sprintf("set spec.%s to false for its controller to reconcile it again", field)}))
			}
		}
	}

	var top *candidate
	for _, cand := range cands {
		if cand.workload != nil {
			top = cand
			break
		}
	}
	switch {
	case len(cands) == 0:
		add(observed(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "ControllerUnknown",
			Message: fmt.Sprintf("tug could not tell which controller reconciles it: no field manager has written its status, no event names who reported it, and no service account may update %s/status by name",
				o.Resource.Resource),
			Next: "list the controllers in the cluster: `tug get deploy,sts -A`"}))
	case top != nil:
		cl, err := id.check(ctx, top, now)
		if err != nil {
			return nil, err
		}
		for _, x := range cl {
			add(x)
		}
		if top.working && lagging {
			add(inferred(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "NotPickedUp",
				Message: fmt.Sprintf("%s in %s runs, but it has not reconciled generation %d (it last reconciled %d); it may be failing on it, backed up, or not watching it",
					top.Workload, top.Namespace, gen, seen),
				Next: "see what it logged about it: " + self("reconcile", "trace", o.Ref(), "--logs")}))
		}
	}
	for _, cand := range cands {
		out.Controllers = append(out.Controllers, cand.ControllerCandidate)
	}
	sortClaims(extra)
	out.Findings = append(out.Findings, extra...)
	sortClaims(out.Findings)
	out.Healthy = d.Healthy && !hasProblems(extra)
	return out, nil
}

// suggest formats a tug command to suggest, in backticks, with -n when the
// object is in another namespace than the session's.
func suggest(o resolve.Object, explicit bool, args ...string) string {
	cmd := "tug " + strings.Join(args, " ")
	if o.Namespace != "" && explicit {
		cmd += " -n " + o.Namespace
	}
	return "`" + cmd + "`"
}

func ownersOf(u *unstructured.Unstructured) []Owner {
	out := []Owner{}
	for _, r := range u.GetOwnerReferences() {
		out = append(out, Owner{Object: strings.ToLower(r.Kind) + "/" + r.Name, Controller: r.Controller != nil && *r.Controller})
	}
	return out
}

// fieldManagers reads an object's managedFields, the latest write first.
func fieldManagers(u *unstructured.Unstructured) []FieldManager {
	out := []FieldManager{}
	for _, e := range u.GetManagedFields() {
		m := FieldManager{Manager: e.Manager, Operation: string(e.Operation), Subresource: e.Subresource}
		if e.Time != nil {
			m.Time = e.Time.Time
		}
		if raw := e.FieldsV1.GetRawBytes(); len(raw) > 0 {
			var top map[string]json.RawMessage
			if json.Unmarshal(raw, &top) == nil {
				for k := range top {
					if f, ok := strings.CutPrefix(k, "f:"); ok {
						m.Fields = append(m.Fields, f)
					}
				}
				slices.Sort(m.Fields)
			}
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b FieldManager) int { return b.Time.Compare(a.Time) })
	return out
}

// eventReporters returns who reported events about an object: the reporting
// controller the events API records, or the older source component.
func eventReporters(ctx context.Context, c Clients, o resolve.Object, uid types.UID) ([]string, error) {
	evs, err := eventsAbout(ctx, c, o.Namespace, map[types.UID]bool{uid: true})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range evs {
		r := cmp.Or(evs[i].ReportingController, evs[i].Source.Component)
		if r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// eventsAbout lists the events about any of a set of objects in a
// namespace, or in every namespace for "", matching them by UID.
func eventsAbout(ctx context.Context, c Clients, ns string, uids map[types.UID]bool) ([]corev1.Event, error) {
	opts := metav1.ListOptions{}
	if len(uids) == 1 {
		for uid := range uids {
			opts.FieldSelector = fields.OneTermEqualSelector("involvedObject.uid", string(uid)).String()
		}
	}
	list, err := c.Typed.CoreV1().Events(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	var out []corev1.Event
	for _, e := range list.Items {
		if uids[e.InvolvedObject.UID] {
			out = append(out, e)
		}
	}
	return out, nil
}

// candidate is a controller candidate as identify builds it.
type candidate struct {
	ControllerCandidate
	// score weighs the evidence: a status write counts most.
	score    int
	workload *controllerWorkload
	// working is true once check finds the workload with a ready pod and
	// a leader, or no lease to lead with.
	working bool
}

// Weights of the evidence for a candidate.
const (
	scoreStatusWriter = 4
	scoreReporter     = 2
	scoreRBAC         = 2
	scoreWellKnown    = 1
)

// identifier looks for the controllers that may reconcile an object.
type identifier struct {
	c Clients
	o resolve.Object
	// ws are the workloads in every namespace, listed once when first
	// needed.
	ws     []controllerWorkload
	wsRead bool
	byKey  map[string]*candidate
	order  []string
	// notes say what limited the search.
	notes []Claim
}

// pendingName is a name that matched no workload, kept to tie to a
// candidate that runs a container of that name.
type pendingName struct {
	name     string
	score    int
	evidence Evidence
}

func (id *identifier) workloads(ctx context.Context) []controllerWorkload {
	if id.wsRead {
		return id.ws
	}
	id.wsRead = true
	ws, err := listControllers(ctx, id.c.Typed)
	if err != nil && ctx.Err() == nil {
		id.notes = append(id.notes, observed(Finding{Severity: SeverityInfo, Objects: []string{id.o.String()}, Reason: "WorkloadsUnlisted",
			Message: "tug could not find which workloads run its controllers: " + firstLine(err.Error()),
			Next:    "check what you may list: `kubectl auth can-i --list`"}))
	}
	id.ws = ws
	return id.ws
}

// add records evidence for a candidate, named or tied to a workload.
func (id *identifier) add(name string, w *controllerWorkload, score int, evidence ...Evidence) *candidate {
	key := "name/" + name
	if w != nil {
		key = "workload/" + w.key()
	}
	cand := id.byKey[key]
	if cand == nil {
		cand = &candidate{ControllerCandidate: ControllerCandidate{Name: name, Basis: BasisObserved}}
		if w != nil {
			cand.Workload, cand.Namespace, cand.workload, cand.Basis = w.target.String(), w.target.Namespace, w, BasisInferred
		}
		id.byKey[key] = cand
		id.order = append(id.order, key)
	}
	cand.score += score
	for _, e := range evidence {
		if !slices.Contains(cand.Evidence, e) {
			cand.Evidence = append(cand.Evidence, e)
		}
	}
	return cand
}

// identify gathers the candidates from each kind of evidence.
func (id *identifier) identify(ctx context.Context, managers []FieldManager, reporters []string) error {
	var pending []pendingName
	for _, m := range managers {
		if !m.WritesStatus() || isClientTool(m.Manager) {
			continue
		}
		text := "field manager " + m.Manager + " has written its status"
		if !m.Time.IsZero() {
			text += ", last at " + m.Time.UTC().Format(time.RFC3339)
		}
		id.byName(ctx, m.Manager, scoreStatusWriter, Evidence{BasisObserved, text}, &pending)
	}
	for _, r := range reporters {
		id.byName(ctx, r, scoreReporter, Evidence{BasisObserved, r + " reported events about it"}, &pending)
	}

	gr := id.o.Resource.GroupResource()
	x, err := loadRBAC(ctx, id.c.Typed)
	if err != nil {
		return err
	}
	if len(x.unreadable) > 0 {
		id.notes = append(id.notes, observed(Finding{Severity: SeverityInfo, Objects: []string{id.o.String()}, Reason: "RBACUnreadable",
			Message: fmt.Sprintf("could not list %s, so service accounts that may update its status are missing", strings.Join(x.unreadable, ", ")),
			Next:    "check what you may list: `kubectl auth can-i --list`"}))
	}
	if writers := x.writers(gr, id.o.Namespace); len(writers) > 0 {
		ws := id.workloads(ctx)
		for _, g := range writers {
			for i := range ws {
				if ws[i].account == g.account {
					id.add(ws[i].target.Name, &ws[i], scoreRBAC, Evidence{BasisInferred,
						fmt.Sprintf("it runs as %s, which may update %s/status through %s", g.account, gr.Resource, g.role)})
				}
			}
		}
	}
	for _, name := range wellKnown(gr.Group, id.o.Kind) {
		for _, w := range id.named(ctx, name) {
			id.add(name, w, scoreWellKnown, Evidence{BasisInferred, fmt.Sprintf("%s is usually reconciled by %s", gr, name)})
		}
	}
	for _, p := range pending {
		attached := false
		for _, key := range id.order {
			cand := id.byKey[key]
			if cand.workload != nil && slices.Contains(cand.workload.containers, p.name) {
				id.add(cand.Name, cand.workload, p.score, p.evidence,
					Evidence{BasisInferred, fmt.Sprintf("%s runs a container named %s", cand.Workload, p.name)})
				attached = true
			}
		}
		if !attached {
			id.add(p.name, nil, p.score, p.evidence)
		}
	}
	return ctx.Err()
}

// byName records evidence from a name a controller gave itself, tying it to
// the control plane or to the workloads named after it.
func (id *identifier) byName(ctx context.Context, name string, score int, ev Evidence, pending *[]pendingName) {
	if process, ok := controlPlane(name); ok {
		facts := []Evidence{ev, {BasisObserved, process + " is part of Kubernetes' control plane"}}
		if process != name {
			facts[1] = Evidence{BasisObserved, name + " is one of the controllers in " + process + ", part of Kubernetes' control plane"}
		}
		id.add(process, nil, score, facts...)
		return
	}
	matches := id.named(ctx, name)
	if len(matches) == 0 {
		*pending = append(*pending, pendingName{name: name, score: score, evidence: ev})
		return
	}
	for _, w := range matches {
		link := fmt.Sprintf("%s in %s has the same name", w.target, w.target.Namespace)
		if w.target.Name != name {
			link = fmt.Sprintf("%s starts with the name of %s in %s", name, w.target, w.target.Namespace)
		}
		id.add(name, w, score, ev, Evidence{BasisInferred, link})
	}
}

// named returns the workloads a controller's name points to: those with the
// same name or, failing that, the longest name that it starts with, as
// cert-manager-certificates-issuing does deploy/cert-manager's.
func (id *identifier) named(ctx context.Context, name string) []*controllerWorkload {
	ws := id.workloads(ctx)
	var exact, prefixed []*controllerWorkload
	longest := 0
	for i := range ws {
		n := ws[i].target.Name
		switch {
		case n == name:
			exact = append(exact, &ws[i])
		case len(n) >= 4 && strings.HasPrefix(name, n+"-"):
			if len(n) > longest {
				prefixed, longest = nil, len(n)
			}
			if len(n) == longest {
				prefixed = append(prefixed, &ws[i])
			}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return prefixed
}

// ranked returns the candidates, the best supported first.
func (id *identifier) ranked() []*candidate {
	out := make([]*candidate, 0, len(id.order))
	for _, key := range id.order {
		out = append(out, id.byKey[key])
	}
	slices.SortStableFunc(out, func(a, b *candidate) int { return b.score - a.score })
	return out
}

// check reads the workload of a candidate and says what its state means for
// the object.
func (id *identifier) check(ctx context.Context, cand *candidate, now time.Time) ([]Claim, error) {
	ref := id.o.String()
	status := "`tug reconcile status " + cand.Workload + " -n " + cand.Namespace + "`"
	v, err := viewController(ctx, id.c.Typed, cand.workload.target, WhyOptions{Now: func() time.Time { return now }, ExplicitNamespace: true})
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return []Claim{observed(Finding{Severity: SeverityInfo, Objects: []string{cand.Workload}, Reason: "ControllerUnreadable",
			Message: "the likeliest controller could not be read: " + firstLine(err.Error())})}, nil
	}
	healthy := v.diag.Healthy
	cand.Healthy, cand.Summary, cand.Leader = &healthy, v.summary(), v.leader()
	cand.working = v.running() && !v.leaderless()
	var out []Claim
	switch {
	case !v.running():
		out = append(out, inferred(Finding{Severity: SeverityError, Objects: []string{cand.Workload}, Reason: "ControllerNotReady",
			Message: fmt.Sprintf("%s in %s, which most likely reconciles %s, has no ready pod (%s), so nothing changes %s's status until it runs",
				cand.Workload, cand.Namespace, ref, v.diag.Summary, ref),
			Next: "see what is wrong with it: " + status}))
	case !healthy:
		out = append(out, inferred(Finding{Severity: SeverityWarning, Objects: []string{cand.Workload}, Reason: "ControllerDegraded",
			Message: fmt.Sprintf("%s in %s, which most likely reconciles %s, runs but is not healthy: %s",
				cand.Workload, cand.Namespace, ref, v.diag.Summary),
			Next: "see what is wrong with it: " + status}))
	}
	if v.leaderless() {
		out = append(out, inferred(Finding{Severity: SeverityError, Objects: []string{cand.Workload}, Reason: "NoLeader",
			Message: fmt.Sprintf("%s in %s, which most likely reconciles %s, has no leader: none of its pods holds a live lease, so none of them reconciles",
				cand.Workload, cand.Namespace, ref),
			Next: "see its leases and pods: " + status}))
	}
	return out, nil
}

// clientTools are field managers that people and their tools write as;
// they are not controllers even when they write a status.
var clientTools = []string{"kubectl", "k9s", "helm", "Go-http-client", "Mozilla", "unknown"}

func isClientTool(manager string) bool {
	return slices.ContainsFunc(clientTools, func(t string) bool { return strings.HasPrefix(manager, t) })
}

// controlPlaneProcesses are the parts of Kubernetes that write status and
// report events under their own names.
var controlPlaneProcesses = []string{"kube-controller-manager", "kube-scheduler", "kube-apiserver", "kubelet", "cloud-controller-manager"}

// builtinControllers are the controllers in kube-controller-manager, as they
// name themselves when they report events.
var builtinControllers = []string{
	"deployment-controller", "replicaset-controller", "replication-controller", "statefulset-controller",
	"daemonset-controller", "job-controller", "cronjob-controller", "endpoint-controller",
	"endpointslice-controller", "endpointslice-mirroring-controller", "persistentvolume-controller",
	"persistentvolume-expander", "attachdetach-controller", "node-controller", "horizontal-pod-autoscaler",
	"disruption-controller", "namespace-controller", "garbage-collector-controller", "taint-eviction-controller",
	"ttl-after-finished-controller", "resource-claim-controller", "service-account-controller",
}

// controlPlane returns the control-plane process a field manager or event
// reporter is part of.
func controlPlane(name string) (string, bool) {
	switch {
	case slices.Contains(controlPlaneProcesses, name):
		return name, true
	case name == "default-scheduler":
		return "kube-scheduler", true
	case slices.Contains(builtinControllers, name):
		return "kube-controller-manager", true
	}
	return "", false
}

// wellKnownControllers are the workloads that usually reconcile the kinds of
// some popular operators, by API group and, where one group is served by
// several, kind.
var wellKnownControllers = []struct {
	group string
	kinds []string // nil for every kind in the group
	names []string
}{
	{group: "cert-manager.io", names: []string{"cert-manager"}},
	{group: "acme.cert-manager.io", names: []string{"cert-manager"}},
	{group: "kustomize.toolkit.fluxcd.io", names: []string{"kustomize-controller"}},
	{group: "helm.toolkit.fluxcd.io", names: []string{"helm-controller"}},
	{group: "source.toolkit.fluxcd.io", names: []string{"source-controller"}},
	{group: "notification.toolkit.fluxcd.io", names: []string{"notification-controller"}},
	{group: "image.toolkit.fluxcd.io", kinds: []string{"ImageRepository", "ImagePolicy"}, names: []string{"image-reflector-controller"}},
	{group: "image.toolkit.fluxcd.io", kinds: []string{"ImageUpdateAutomation"}, names: []string{"image-automation-controller"}},
	{group: "argoproj.io", kinds: []string{"Application", "AppProject"}, names: []string{"argocd-application-controller"}},
	{group: "argoproj.io", kinds: []string{"ApplicationSet"}, names: []string{"argocd-applicationset-controller"}},
	{group: "argoproj.io", kinds: []string{"Rollout", "AnalysisRun", "Experiment"}, names: []string{"argo-rollouts"}},
	{group: "argoproj.io", kinds: []string{"Workflow", "CronWorkflow"}, names: []string{"workflow-controller"}},
	{group: "external-secrets.io", names: []string{"external-secrets"}},
	{group: "monitoring.coreos.com", names: []string{"prometheus-operator", "kube-prometheus-stack-operator"}},
	{group: "postgresql.cnpg.io", names: []string{"cnpg-controller-manager"}},
	{group: "cluster.x-k8s.io", names: []string{"capi-controller-manager"}},
	{group: "karpenter.sh", names: []string{"karpenter"}},
	{group: "keda.sh", names: []string{"keda-operator"}},
	{group: "kafka.strimzi.io", names: []string{"strimzi-cluster-operator"}},
	{group: "networking.istio.io", names: []string{"istiod"}},
	{group: "pkg.crossplane.io", names: []string{"crossplane"}},
	{group: "velero.io", names: []string{"velero"}},
}

// wellKnown returns the workload names that usually reconcile a kind.
func wellKnown(group, kind string) []string {
	var out []string
	for _, w := range wellKnownControllers {
		if w.group == group && (w.kinds == nil || slices.Contains(w.kinds, kind)) {
			out = append(out, w.names...)
		}
	}
	return out
}
