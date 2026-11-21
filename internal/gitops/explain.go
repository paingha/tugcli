package gitops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/ops"
)

// Stage is where in delivering an object a failure sits.
type Stage string

// The stages, in the order delivery passes through them.
const (
	// StageNone means nothing is wrong.
	StageNone Stage = "none"
	// StageSuspended means Flux has been told not to reconcile it.
	StageSuspended Stage = "suspended"
	// StageDependency means something it depends on is not ready.
	StageDependency Stage = "dependency"
	// StageSource means its source cannot be fetched.
	StageSource Stage = "source"
	// StageReconcile means its latest change or revision is not applied
	// yet.
	StageReconcile Stage = "reconcile"
	// StageAdmission means the cluster's admission control rejects what
	// it applies.
	StageAdmission Stage = "admission"
	// StageApply means building, rendering or applying it fails, or its
	// Helm chart does not install or upgrade.
	StageApply Stage = "apply"
	// StageHealth means what it manages is not healthy.
	StageHealth Stage = "health"
)

var stageOrder = []Stage{StageSuspended, StageDependency, StageSource, StageReconcile, StageAdmission, StageApply, StageHealth}

// Finding is one thing wrong with delivering an object, and where it sits.
type Finding struct {
	Stage Stage `json:"stage"`
	ops.Finding
}

// Explanation is what Explain found out about a Kustomization, HelmRelease
// or Application.
type Explanation struct {
	Object *Object `json:"object"`
	// Healthy is true when it is ready, nothing it manages is unhealthy
	// and nothing else is wrong.
	Healthy bool `json:"healthy"`
	// Stage is where the failure sits: the earliest stage with an error,
	// or else with a warning.
	Stage   Stage  `json:"stage"`
	Summary string `json:"summary"`
	// Findings are in the order of their stages.
	Findings []Finding `json:"findings"`
	// Managed are the objects it manages, with their health as far as tug
	// could check it.
	Managed []Managed `json:"managed"`
}

// ExplainOptions tune Explain.
type ExplainOptions struct {
	// Now is the time ages are measured from; it defaults to time.Now.
	Now func() time.Time
}

// staleAfter is how long Argo CD may go without comparing an Application
// with its source before tug says so; it does so every three minutes by
// default.
const staleAfter = 15 * time.Minute

// Explain decides where delivering a Kustomization, HelmRelease or
// Application fails: whether it is suspended, something it depends on is
// not ready, its source cannot be fetched, its latest change is not
// reconciled, admission control rejects it, building or applying it fails,
// or what it manages is unhealthy, which ops.WhyObject diagnoses. It only
// reads.
func Explain(ctx context.Context, c Clients, s *Snapshot, o *Object, opts ExplainOptions) (*Explanation, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	e := &explainer{
		c: c, s: s, o: o, now: now(),
		out: &Explanation{Object: o, Findings: []Finding{}, Managed: []Managed{}},
		k:   &checker{c: c, s: s, owner: o, opts: ops.WhyOptions{Now: opts.Now, ExplicitNamespace: true}},
	}
	var err error
	if o.Tool == ArgoCD {
		err = e.application(ctx)
	} else {
		err = e.flux(ctx)
	}
	if err != nil {
		return nil, err
	}
	e.finish()
	return e.out, nil
}

// explainer collects the findings of one Explain call.
type explainer struct {
	c   Clients
	s   *Snapshot
	o   *Object
	now time.Time
	out *Explanation
	k   *checker
}

// add records a finding about the object, or about the objects given.
func (e *explainer) add(stage Stage, sev ops.Severity, reason, msg, next string, objects ...string) {
	if len(objects) == 0 {
		objects = []string{e.o.String()}
	}
	e.out.Findings = append(e.out.Findings, Finding{Stage: stage, Finding: ops.Finding{
		Severity: sev, Objects: objects, Reason: reason, Message: msg, Next: next,
	}})
}

// has reports whether a finding of a severity sits at a stage.
func (e *explainer) has(stage Stage, sev ops.Severity) bool {
	return slices.ContainsFunc(e.out.Findings, func(f Finding) bool { return f.Stage == stage && f.Severity == sev })
}

// name names another object for a message, with its namespace when that is
// not the object's.
func (e *explainer) name(r Ref) string {
	if r.Namespace != "" && r.Namespace != e.o.Namespace {
		return r.String() + " in " + r.Namespace
	}
	return r.String()
}

// why suggests tug why about a Flux object, which shows its conditions and
// events.
func why(o *Object) string {
	return tugCommand(o.Namespace, "why", o.FullName())
}

func (e *explainer) flux(ctx context.Context) error {
	o := e.o
	if o.Suspended {
		e.add(StageSuspended, ops.SeverityWarning, "Suspended",
			"is suspended, so Flux does not reconcile it: changes to it and to its source wait, and the result of its last reconcile stands",
			fmt.Sprintf("set spec.suspend to false where it is defined, or resume it: `flux resume %s %s -n %s`", strings.ToLower(o.Kind), o.Name, o.Namespace))
	}
	e.dependencies()
	e.fluxSources()
	e.fluxRevision()
	e.fluxConditions()
	return e.fluxManaged(ctx)
}

// dependencies checks what the object depends on through spec.dependsOn,
// following each chain that is not ready to where it stops.
func (e *explainer) dependencies() {
	o := e.o
	blocked := false
	for _, d := range o.DependsOn {
		dep := e.s.get(d)
		if dep == nil {
			msg := fmt.Sprintf("depends on %s, which is not in the cluster", e.name(d))
			if e.s.Namespace != "" && d.Namespace != e.s.Namespace {
				msg = fmt.Sprintf("depends on %s, which tug may not read", e.name(d))
			}
			e.add(StageDependency, ops.SeverityError, "DependencyNotFound", msg, "check its spec.dependsOn")
			blocked = true
			continue
		}
		if dep.Ready == ops.HealthReady {
			continue
		}
		blocked = true
		sev := ops.SeverityError
		if dep.Ready == ops.HealthUnknown {
			sev = ops.SeverityWarning
		}
		msg := fmt.Sprintf("depends on %s, which is not ready: %s", e.name(dep.Ref()), statusText(dep))
		next := tugCommand(dep.Namespace, "gitops", "explain", dep.String())
		roots := e.s.rootCauses(dep)
		if len(roots) > 0 && !slices.Contains(roots, dep) {
			r := roots[0]
			msg += fmt.Sprintf("; the chain stops at %s: %s", e.name(r.Ref()), statusText(r))
			next = tugCommand(r.Namespace, "gitops", "explain", r.String())
		}
		e.add(StageDependency, sev, "DependencyNotReady", msg, "see why: "+next, o.String(), dep.String())
	}
	if !blocked && o.Reason == "DependencyNotReady" {
		e.add(StageDependency, ops.SeverityInfo, "DependencyNotReady",
			"its dependencies are ready now, so Flux goes on at its next attempt", "")
	}
}

// rootCauses follows what a not-ready object depends on to the objects
// that are not ready though everything they depend on is.
func (s *Snapshot) rootCauses(o *Object) []*Object {
	var out []*Object
	seen := map[*Object]bool{}
	var walk func(n *Object)
	walk = func(n *Object) {
		if seen[n] {
			return
		}
		seen[n] = true
		deeper := false
		for _, d := range n.DependsOn {
			if dep := s.get(d); dep != nil && dep.Ready != ops.HealthReady {
				deeper = true
				walk(dep)
			}
		}
		if !deeper {
			out = append(out, n)
		}
	}
	walk(o)
	return out
}

// sourceChain returns the sources a Flux object applies, upstream first: a
// HelmRelease's chart repository before its HelmChart.
func (e *explainer) sourceChain() []Ref {
	o := e.o
	if o.Source == nil {
		return nil
	}
	if o.Kind == KindHelmRelease && o.Source.Kind == KindHelmChart {
		if repo := chartSource(o); repo != nil {
			return []Ref{*repo, *o.Source}
		}
	}
	return []Ref{*o.Source}
}

// fluxSources checks the sources it applies, stopping at the first that
// fails.
func (e *explainer) fluxSources() {
	o := e.o
	for _, r := range e.sourceChain() {
		src := e.s.get(r)
		if src == nil {
			switch {
			case !e.s.served(r.Kind):
				e.add(StageSource, ops.SeverityError, "SourceNotFound",
					fmt.Sprintf("its source %s is of a kind the cluster does not serve", e.name(r)), "check its source reference")
			case r.Kind == KindHelmChart && o.ObservedGeneration == 0:
				e.add(StageSource, ops.SeverityInfo, "ChartNotCreated", "helm-controller has not yet made the HelmChart for its chart", "")
			default:
				e.add(StageSource, ops.SeverityError, "SourceNotFound",
					fmt.Sprintf("its source %s is not in the cluster", e.name(r)), "check its source reference and that the source is defined")
			}
			return
		}
		what := "its source " + e.name(r)
		if r.Kind == KindHelmChart {
			what = "its chart, " + e.name(r) + ","
		}
		switch src.Ready {
		case ops.HealthNotReady:
			e.add(StageSource, ops.SeverityError, cmp.Or(src.Reason, "SourceNotReady"),
				fmt.Sprintf("%s cannot be fetched: %s", what, readyMessage(src)),
				"see the source's conditions and events: "+why(src), o.String(), src.String())
			return
		case ops.HealthUnknown:
			e.add(StageSource, ops.SeverityInfo, "SourceNotReady",
				fmt.Sprintf("%s is being fetched: %s", what, statusText(src)), "", o.String(), src.String())
		}
		if src.Suspended {
			e.add(StageSource, ops.SeverityWarning, "SourceSuspended",
				fmt.Sprintf("%s is suspended, so it fetches no new revisions", what),
				fmt.Sprintf("resume it: `flux resume source %s %s -n %s`", sourceCLIKind(src.Kind), src.Name, src.Namespace), o.String(), src.String())
		}
	}
}

// sourceCLIKind is what the flux CLI calls a source kind.
func sourceCLIKind(k string) string {
	switch k {
	case KindGitRepository:
		return "git"
	case KindOCIRepository:
		return "oci"
	case KindHelmRepository:
		return "helm"
	case KindHelmChart:
		return "chart"
	default:
		return strings.ToLower(k)
	}
}

// fluxRevision checks that its controller has caught up with its latest
// change and that it has applied what its source offers.
func (e *explainer) fluxRevision() {
	o := e.o
	reconciling := o.conditionTrue("Reconciling")
	if o.lagging() {
		sev := ops.SeverityWarning
		if reconciling {
			sev = ops.SeverityInfo
		}
		controller := "kustomize-controller"
		if o.Kind == KindHelmRelease {
			controller = "helm-controller"
		}
		e.add(StageReconcile, sev, "NotReconciled",
			fmt.Sprintf("Flux has not reconciled its latest change: it is at generation %d, but was last reconciled at %d", o.Generation, o.ObservedGeneration),
			fmt.Sprintf("if this lasts, check that %s is running: `tug why deploy/%s -n flux-system`", controller, controller))
	}
	if o.Revision == "" || revisionApplied(o.Revision, o.Applied) || (o.Attempted == o.Revision && o.Ready == ops.HealthNotReady) {
		return
	}
	what := "revision"
	if o.Kind == KindHelmRelease {
		what = "chart version"
	}
	msg := fmt.Sprintf("its source offers %s %s, but it last applied %s", what, o.Revision, cmp.Or(o.Applied, "none"))
	sev := ops.SeverityInfo
	switch {
	case o.Suspended:
		msg += ", and it is suspended"
		sev = ops.SeverityWarning
	case reconciling:
		msg += "; Flux is applying it now"
	case o.Ready == ops.HealthNotReady:
		msg += "; Flux applies it once the failure here clears"
	case o.Interval != "":
		msg += "; Flux applies it at its next reconcile, every " + o.Interval
	}
	e.add(StageReconcile, sev, "RevisionNotApplied", msg, "")
}

// revisionApplied reports whether what a source offers is what was
// applied, allowing for an OCI artifact's digest after a chart version.
func revisionApplied(offered, applied string) bool {
	return offered == applied || (applied != "" && strings.HasPrefix(offered, applied+"@"))
}

// fluxConditions turns its Ready condition, and the others Flux sets, into
// findings, unless one already says the same.
func (e *explainer) fluxConditions() {
	o := e.o
	said := map[string]bool{}
	report := func(c *ops.Condition, sev ops.Severity, stage Stage) {
		key := c.Reason + "\x00" + c.Message
		if said[key] {
			return
		}
		said[key] = true
		if e.has(stage, ops.SeverityError) {
			return // a finding above says where it fails, in more detail
		}
		e.add(stage, sev, cmp.Or(c.Reason, c.Type), conditionMessage(*c), e.next(stage, c.Reason))
	}
	ready := o.condition("Ready")
	switch {
	case ready == nil:
	case ready.Status == string(metav1.ConditionFalse):
		report(ready, ops.SeverityError, fluxStage(ready.Reason, ready.Message))
	case ready.Status != string(metav1.ConditionTrue) || o.conditionTrue("Reconciling"):
		e.add(StageReconcile, ops.SeverityInfo, "Reconciling", "Flux is reconciling it now: "+cmp.Or(firstLine(ready.Message), ready.Status), "")
	}
	if c := o.condition("Released"); c != nil && c.Status == string(metav1.ConditionFalse) {
		report(c, ops.SeverityError, fluxStage(c.Reason, c.Message))
	}
	if c := o.condition("TestSuccess"); c != nil && c.Status == string(metav1.ConditionFalse) {
		report(c, ops.SeverityError, StageApply)
	}
	if c := o.condition("Healthy"); c != nil && c.Status == string(metav1.ConditionFalse) {
		report(c, ops.SeverityError, StageHealth)
	}
	if c := o.condition("Remediated"); c != nil && c.Status == string(metav1.ConditionTrue) {
		e.add(StageApply, ops.SeverityWarning, cmp.Or(c.Reason, "Remediated"),
			"its release failed, and helm-controller remediated it: "+compact(c.Message), e.next(StageApply, c.Reason))
	}
	if o.conditionTrue("Stalled") {
		e.add(StageReconcile, ops.SeverityInfo, "Stalled", "Flux has stopped retrying it until it or its source changes", "")
	}
}

// fluxStage says where the failure a Flux reason and message report sits.
func fluxStage(reason, msg string) Stage {
	switch reason {
	case "DependencyNotReady":
		return StageDependency
	case "ArtifactFailed", "SourceNotReady", "HelmChartNotReady":
		return StageSource
	case "HealthCheckFailed":
		return StageHealth
	case "Progressing", "ProgressingWithRetry":
		return StageReconcile
	}
	if admissionRejected(msg) {
		return StageAdmission
	}
	return StageApply
}

// admissionMarkers are what the API server says when admission control
// rejects an object: a webhook such as Kyverno's or Gatekeeper's, a
// ValidatingAdmissionPolicy, or Pod Security admission.
var admissionMarkers = []string{"admission webhook", "denied the request", "denied request", "validatingadmissionpolicy", "violates podsecurity"}

func admissionRejected(msg string) bool {
	m := strings.ToLower(msg)
	return slices.ContainsFunc(admissionMarkers, func(s string) bool { return strings.Contains(m, s) })
}

// next suggests what to do about a failure of the object at a stage.
func (e *explainer) next(stage Stage, reason string) string {
	o := e.o
	switch stage {
	case StageDependency:
		return "see what it depends on: " + tugCommand(o.Namespace, "gitops", "dependencies", o.String())
	case StageSource:
		if o.Source != nil {
			if src := e.s.get(*o.Source); src != nil {
				return "see the source's conditions and events: " + why(src)
			}
		}
		return "see its conditions and events: " + why(o)
	case StageAdmission:
		return "change the manifest so that the policy named above allows it, or ask the policy's owners for an exception"
	case StageHealth:
		return "" // what it manages follows
	}
	switch o.Kind {
	case KindKustomization:
		if reason == "BuildFailed" {
			path := cmp.Or(text(o.u.Object, "spec", "path"), ".")
			return fmt.Sprintf("build it as Flux does, in a checkout of its source: `flux build kustomization %s -n %s --path %s`", o.Name, o.Namespace, path)
		}
	case KindHelmRelease:
		name, ns := helmRelease(o)
		return fmt.Sprintf("see the release's history: `helm history %s -n %s`", name, ns)
	}
	return "see its conditions and events: " + why(o)
}

// helmRelease returns the name and namespace of a HelmRelease's Helm
// release.
func helmRelease(o *Object) (string, string) {
	u := o.u.Object
	if h := maps(u, "status", "history"); len(h) > 0 && str(h[0]["name"]) != "" {
		return str(h[0]["name"]), cmp.Or(str(h[0]["namespace"]), o.Namespace)
	}
	target := text(u, "spec", "targetNamespace")
	name := text(u, "spec", "releaseName")
	if name == "" {
		name = o.Name
		if target != "" {
			name = target + "-" + o.Name
		}
	}
	return name, cmp.Or(text(u, "spec", "storageNamespace"), text(u, "status", "storageNamespace"), target, o.Namespace)
}

// fluxManaged checks what a Kustomization or HelmRelease manages, by its
// inventory or, for a HelmRelease without one, by helm-controller's labels.
func (e *explainer) fluxManaged(ctx context.Context) error {
	o := e.o
	if o.remote() {
		e.add(StageHealth, ops.SeverityInfo, "Remote",
			"it deploys to another cluster through spec.kubeConfig, so tug cannot check what it manages from here", "")
		return nil
	}
	refs := inventory(o.u)
	if _, ok, _ := unstructured.NestedMap(o.u.Object, "status", "inventory"); !ok && o.Kind == KindHelmRelease {
		var err error
		if refs, err = e.k.helmObjects(ctx, o); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.add(StageHealth, ops.SeverityInfo, "PartialView", "what it manages could not be listed: "+firstLine(err.Error()), "")
			return nil
		}
	}
	results, err := e.k.checkAll(ctx, refs, e.k.check)
	if err != nil {
		return err
	}
	e.record(results)
	return nil
}

// record keeps what checking the managed objects found.
func (e *explainer) record(results []checked) {
	for _, r := range results {
		e.out.Managed = append(e.out.Managed, r.managed)
		for _, f := range r.findings {
			e.out.Findings = append(e.out.Findings, Finding{Stage: StageHealth, Finding: f})
		}
	}
}

func (e *explainer) application(ctx context.Context) error {
	o := e.o
	for _, c := range o.Conditions {
		msg := compact(c.Message)
		switch {
		case c.Type == "ComparisonError":
			stage := StageApply
			next := "see how Argo CD renders it: `argocd app manifests " + o.Name + " --source git`"
			if sourceFailure(c.Message) {
				stage = StageSource
				next = "check the repository's URL and credentials in Argo CD: `argocd repo list`"
			}
			e.add(stage, ops.SeverityError, c.Type, "Argo CD cannot compare it with its source: "+msg, next)
		case c.Type == "InvalidSpecError":
			e.add(StageApply, ops.SeverityError, c.Type, msg, "check its spec.project, spec.source and spec.destination: its AppProject must allow them")
		case strings.HasSuffix(c.Type, "Error"):
			e.add(fluxStage("", msg), ops.SeverityError, c.Type, msg, "see the sync result: `argocd app get "+o.Name+" --show-operation`")
		case c.Type == "SharedResourceWarning":
			e.add(StageApply, ops.SeverityWarning, c.Type, msg, "make sure each object is managed by one Application")
		case strings.HasSuffix(c.Type, "Warning"):
			e.add(StageApply, ops.SeverityInfo, c.Type, msg, "")
		}
	}
	e.operation()
	e.sync()
	return e.applicationHealth(ctx)
}

// sourceMarkers are what Argo CD says when it cannot fetch a repository or
// chart, rather than render what it fetched.
var sourceMarkers = []string{
	"authentication required", "repository not found", "failed to list refs", "unable to resolve", "could not read username",
	"permission denied (publickey)", "ssh: handshake failed", "no such host", "x509:", "connection refused",
	"unable to fetch", "failed to fetch", "couldn't find remote ref", "failed to get index",
}

func sourceFailure(msg string) bool {
	m := strings.ToLower(msg)
	return slices.ContainsFunc(sourceMarkers, func(s string) bool { return strings.Contains(m, s) })
}

// operation checks its last sync.
func (e *explainer) operation() {
	o := e.o
	u := o.u.Object
	phase := text(u, "status", "operationState", "phase")
	msg := compact(text(u, "status", "operationState", "message"))
	rev := cmp.Or(text(u, "status", "operationState", "operation", "sync", "revision"), o.Attempted)
	switch phase {
	case "Failed", "Error":
		sev := ops.SeverityError
		if o.Sync == "Synced" {
			sev = ops.SeverityInfo // a later comparison found it in sync after all
		}
		var failed []argoSyncResult
		for _, r := range syncResults(o.u) {
			if r.Status == "SyncFailed" || r.HookPhase == "Failed" || r.HookPhase == "Error" {
				failed = append(failed, r)
			}
		}
		stage := fluxStage("", msg)
		if slices.ContainsFunc(failed, func(r argoSyncResult) bool { return admissionRejected(r.Message) }) {
			stage = StageAdmission
		}
		e.add(stage, sev, "Sync"+phase, fmt.Sprintf("its last sync, of revision %s, failed: %s", cmp.Or(rev, "unknown"), msg),
			"see the sync result: `argocd app get "+o.Name+" --show-operation`")
		for _, r := range failed {
			e.add(fluxStage("", r.Message), sev, "SyncFailed", compact(r.Message), "", r.String())
		}
	case "Running", "Terminating":
		started := timestamp(u, "status", "operationState", "startedAt")
		since := ""
		if !started.IsZero() {
			since = " for " + duration.HumanDuration(e.now.Sub(started))
		}
		e.add(StageReconcile, ops.SeverityInfo, "Syncing", fmt.Sprintf("a sync of revision %s has been running%s: %s", cmp.Or(rev, "unknown"), since, cmp.Or(msg, phase)), "")
	}
}

// argoSyncResult is one entry of an Application's last sync result.
type argoSyncResult struct {
	objectRef
	Status, HookPhase, Message string
}

func syncResults(u *unstructured.Unstructured) []argoSyncResult {
	var out []argoSyncResult
	for _, m := range maps(u.Object, "status", "operationState", "syncResult", "resources") {
		out = append(out, argoSyncResult{
			objectRef: objectRef{Group: str(m["group"]), Version: str(m["version"]), Kind: str(m["kind"]), Namespace: str(m["namespace"]), Name: str(m["name"])},
			Status:    str(m["status"]), HookPhase: str(m["hookPhase"]), Message: str(m["message"]),
		})
	}
	return out
}

// automated returns whether an Application syncs on its own, and whether it
// also undoes changes made in the cluster.
func automated(o *Object) (auto, selfHeal bool) {
	a, ok, _ := unstructured.NestedMap(o.u.Object, "spec", "syncPolicy", "automated")
	if !ok {
		return false, false
	}
	if enabled, set := a["enabled"].(bool); set && !enabled {
		return false, false
	}
	selfHeal, _ = a["selfHeal"].(bool)
	return true, selfHeal
}

// sync checks whether it is in sync with its source, and has been compared
// with it lately.
func (e *explainer) sync() {
	o := e.o
	phase := text(o.u.Object, "status", "operationState", "phase")
	drift := tugCommand(o.Namespace, "gitops", "drift", o.String())
	auto, selfHeal := automated(o)
	switch o.Sync {
	case "OutOfSync":
		n := 0
		for _, r := range argoResources(o.u) {
			if r.Status == "OutOfSync" && !r.Hook {
				n++
			}
		}
		differ := fmt.Sprintf("%d of its objects differ from its source", n)
		switch {
		case phase == "Running" || phase == "Terminating":
		case !auto:
			e.add(StageReconcile, ops.SeverityWarning, "OutOfSync",
				differ+", and it has no automated sync policy, so Argo CD changes nothing until someone syncs it", "see what differs: "+drift)
		case phase == "Failed" || phase == "Error":
			// the failed sync above says why
		case !selfHeal && o.Attempted != "" && o.Attempted == o.Revision:
			e.add(StageReconcile, ops.SeverityWarning, "OutOfSync",
				fmt.Sprintf("%s, though Argo CD synced revision %s: they changed in the cluster since, and automated sync applies only new revisions while spec.syncPolicy.automated.selfHeal is off", differ, o.Revision),
				"see what differs: "+drift)
		default:
			e.add(StageReconcile, ops.SeverityInfo, "OutOfSync", differ+"; Argo CD syncs it automatically", "see what differs: "+drift)
		}
	case "Unknown", "":
		if !e.has(StageSource, ops.SeverityError) && !e.has(StageApply, ops.SeverityError) {
			e.add(StageReconcile, ops.SeverityWarning, "SyncUnknown", "Argo CD has not compared it with its source", "")
		}
	}
	if !o.LastReconcile.IsZero() && e.now.Sub(o.LastReconcile) > staleAfter {
		e.add(StageReconcile, ops.SeverityWarning, "NotReconciled",
			fmt.Sprintf("Argo CD last compared it with its source %s ago, though it does so every three minutes by default, so its application controller may be stopped or overloaded",
				duration.HumanDuration(e.now.Sub(o.LastReconcile))),
			"check the application controller: `tug why sts/argocd-application-controller -n argocd`")
	}
}

// applicationHealth checks what an Application manages: Argo CD's health of
// each, with ops.WhyObject's diagnosis of those it says are unhealthy when
// they are in this cluster.
func (e *explainer) applicationHealth(ctx context.Context) error {
	o := e.o
	var resources []argoResource
	for _, r := range argoResources(o.u) {
		if !r.Hook {
			resources = append(resources, r)
		}
	}
	switch o.Health {
	case "Suspended":
		e.add(StageHealth, ops.SeverityInfo, "Suspended", "something it manages is suspended, such as a paused rollout or a suspended CronJob", "")
	case "Progressing":
		e.add(StageHealth, ops.SeverityInfo, "Progressing", "something it manages is still progressing: "+cmp.Or(firstLine(text(o.u.Object, "status", "health", "message")), "rolling out"), "")
	}
	inCluster := o.inCluster()
	if !inCluster {
		e.add(StageHealth, ops.SeverityInfo, "Remote",
			fmt.Sprintf("it deploys to %s, another cluster, so tug relies on Argo CD for the health of what it manages", cmp.Or(o.Destination.Name, o.Destination.Server)), "")
	}
	refs := make([]objectRef, len(resources))
	byRef := make(map[objectRef]argoResource, len(resources))
	for i, r := range resources {
		refs[i], byRef[r.objectRef] = r.objectRef, r
	}
	results, err := e.k.checkAll(ctx, refs, func(ctx context.Context, ref objectRef) checked {
		return e.k.checkArgo(ctx, byRef[ref], inCluster)
	})
	if err != nil {
		return err
	}
	before := len(e.out.Findings)
	e.record(results)
	if o.Health == "Degraded" && len(e.out.Findings) == before {
		e.add(StageHealth, ops.SeverityError, "Degraded",
			"Argo CD reports it degraded: "+cmp.Or(firstLine(text(o.u.Object, "status", "health", "message")), "something it manages is not healthy"), "")
	}
	return nil
}

// checkArgo checks one object an Application manages.
func (k *checker) checkArgo(ctx context.Context, r argoResource, inCluster bool) checked {
	m := Managed{Kind: r.Kind, Group: r.Group, Name: r.Name, Namespace: r.Namespace, Sync: r.Status, Status: cmp.Or(firstLine(r.HealthMessage), r.Health)}
	switch r.Health {
	case "Healthy":
		m.Health = ops.HealthReady
	case "Degraded", "Missing":
		m.Health = ops.HealthNotReady
	case "Progressing", "Suspended", "Unknown":
		m.Health = ops.HealthUnknown
	default:
		m.Health = ops.HealthNone
	}
	if inCluster {
		if o := k.gitopsObject(r.objectRef); o != nil {
			return k.nested(m, o)
		}
	}
	// Argo CD's own word, for a degraded object tug cannot diagnose.
	argoSays := func() []ops.Finding {
		if r.Health != "Degraded" {
			return nil
		}
		return []ops.Finding{{Severity: ops.SeverityError, Objects: []string{r.String()}, Reason: "Degraded",
			Message: "Argo CD reports it degraded" + prefixed(": ", firstLine(r.HealthMessage))}}
	}
	switch {
	case r.Health == "Missing":
		return checked{managed: m, findings: []ops.Finding{{Severity: ops.SeverityWarning, Objects: []string{r.String()}, Reason: "Missing",
			Message: "is in its source but not in the cluster; Argo CD creates it at the next sync"}}}
	case r.Health != "Degraded" && r.Health != "Progressing":
		return checked{managed: m}
	case !inCluster:
		return checked{managed: m, findings: argoSays()}
	}
	obj, err := k.object(r.objectRef)
	if err != nil {
		return checked{managed: m, findings: argoSays()}
	}
	if _, ok := obj.Workload(); !ok && !k.takeDeep() {
		return checked{managed: m, findings: argoSays()}
	}
	c := k.diagnose(ctx, m, obj, m.Health)
	if c.managed.Health == ops.HealthReady {
		c.findings = argoSays() // healthy to tug, but not by Argo CD's health checks
	}
	c.managed.Sync, c.managed.Health = r.Status, m.Health
	return c
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// finish orders the findings by stage and decides where the failure sits.
func (e *explainer) finish() {
	out := e.out
	rank := func(s Stage) int { return slices.Index(stageOrder, s) }
	slices.SortStableFunc(out.Findings, func(a, b Finding) int { return cmp.Compare(rank(a.Stage), rank(b.Stage)) })
	out.Stage = StageNone
	for _, sev := range []ops.Severity{ops.SeverityError, ops.SeverityWarning, ops.SeverityInfo} {
		if i := slices.IndexFunc(out.Findings, func(f Finding) bool { return f.Severity == sev }); i >= 0 {
			out.Stage = out.Findings[i].Stage
			break
		}
	}
	problems := slices.ContainsFunc(out.Findings, func(f Finding) bool { return f.Severity != ops.SeverityInfo })
	out.Healthy = e.o.Ready == ops.HealthReady && !problems
	if out.Healthy {
		out.Stage = StageNone
		out.Summary = statusText(e.o)
		return
	}
	out.Summary = e.stageSummary(out.Stage)
}

// stageSummary says in a few words where a failure sits.
func (e *explainer) stageSummary(s Stage) string {
	switch s {
	case StageSuspended:
		return "it is suspended"
	case StageDependency:
		return "something it depends on is not ready"
	case StageSource:
		return "its source cannot be fetched"
	case StageReconcile:
		return "its latest change is not applied yet"
	case StageAdmission:
		return "the cluster's admission control rejects what it applies"
	case StageApply:
		switch e.o.Kind {
		case KindHelmRelease:
			return "its Helm chart fails to install or upgrade"
		case KindApplication:
			return "rendering or applying its manifests fails"
		default:
			return "building or applying it fails"
		}
	case StageHealth:
		return "what it manages is not healthy"
	default:
		return statusText(e.o)
	}
}

// statusText says what an object reports about its state in a few words:
// "DependencyNotReady: dependency 'flux-system/infra' is not ready", or
// "OutOfSync, Degraded" for an Application.
func statusText(o *Object) string {
	if o.Tool == ArgoCD {
		s := strings.Join(nonEmpty(o.Sync, o.Health), ", ")
		if o.Ready == ops.HealthNotReady && o.Message != "" {
			s = strings.Join(nonEmpty(s, o.Message), ": ")
		}
		return cmp.Or(s, "no status")
	}
	if o.Suspended && o.Ready == ops.HealthReady {
		return "suspended"
	}
	c := o.condition("Ready")
	if c == nil {
		if o.Ready == ops.HealthNone {
			return "no status"
		}
		return string(o.Ready)
	}
	return conditionText(*c)
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// conditionText says what a condition says in a few words: "BuildFailed:
// kustomize build failed".
func conditionText(c ops.Condition) string {
	msg := firstLine(c.Message)
	var s string
	switch {
	case c.Reason != "" && msg != "":
		s = c.Reason + ": " + msg
	case msg != "":
		s = msg
	case c.Reason != "":
		s = c.Reason
	default:
		s = c.Type + "=" + c.Status
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// conditionMessage says what a condition says in full: "Ready is False:
// kustomize build failed".
func conditionMessage(c ops.Condition) string {
	s := c.Type + " is " + c.Status
	if msg := compact(c.Message); msg != "" {
		s += ": " + msg
	}
	return s
}

// maxMessage bounds how much of a controller's message a finding repeats.
const maxMessage = 1000

// compact puts a message of several lines, such as the policy report a
// Kyverno webhook gives, on one line.
func compact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMessage {
		s = s[:maxMessage] + "…"
	}
	return s
}

// readyMessage returns the whole message of a Flux object's Ready
// condition, on one line.
func readyMessage(o *Object) string {
	if c := o.condition("Ready"); c != nil && c.Message != "" {
		return compact(c.Message)
	}
	return statusText(o)
}

// tugCommand formats a tug command to suggest, in backticks, with -n for the
// namespace of what it is about, which is often not the session's.
func tugCommand(ns string, args ...string) string {
	cmd := "tug " + strings.Join(args, " ")
	if ns != "" {
		cmd += " -n " + ns
	}
	return "`" + cmd + "`"
}
