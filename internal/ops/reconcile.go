package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// Basis says how tug knows something. Kubernetes records what objects are
// now and a little of what happened to them, but nothing says which
// controller acted on which object and when, so some of what tug says about
// reconciling is its reading of indirect evidence.
type Basis string

// Bases.
const (
	// BasisObserved is read from the API as it is: a condition, a
	// timestamp, an event, a field manager's name.
	BasisObserved Basis = "observed"
	// BasisInferred is tug's reading of evidence that could mislead, such
	// as a field manager that is named after a deployment.
	BasisInferred Basis = "inferred"
)

// Claim is a finding together with how tug knows it.
type Claim struct {
	Finding
	Basis Basis `json:"basis"`
}

func observed(f Finding) Claim { return Claim{Finding: f, Basis: BasisObserved} }
func inferred(f Finding) Claim { return Claim{Finding: f, Basis: BasisInferred} }

// claims labels a diagnosis's findings, which restate what the API reports,
// as observed.
func claims(fs []Finding) []Claim {
	out := make([]Claim, 0, len(fs))
	for _, f := range fs {
		out = append(out, observed(f))
	}
	return out
}

// sortClaims orders claims worst first, keeping the order of those of the
// same severity.
func sortClaims(cs []Claim) {
	slices.SortStableFunc(cs, func(a, b Claim) int { return a.Severity.rank() - b.Severity.rank() })
}

func hasProblems(cs []Claim) bool {
	return slices.ContainsFunc(cs, func(c Claim) bool { return c.Severity != SeverityInfo })
}

// ControllerStatus is the state of a controller: the health of its
// workload, its leader election, its restarts and the kinds it reconciles.
type ControllerStatus struct {
	Target    string   `json:"target"`
	Namespace string   `json:"namespace"`
	Healthy   bool     `json:"healthy"`
	Summary   string   `json:"summary"`
	Rollout   *Rollout `json:"rollout,omitempty"`
	// ServiceAccount is the namespace/name its pods run as.
	ServiceAccount string     `json:"serviceAccount"`
	Pods           []PodState `json:"pods"`
	// Restarts are its containers that have restarted, the latest first.
	Restarts []ContainerRestart `json:"restarts"`
	// Leader is its pod that holds its leader Lease, when one does.
	Leader string `json:"leader,omitempty"`
	// Leases are the Leases in its namespace that its pods hold or that are
	// named after it.
	Leases []Lease `json:"leases"`
	// Reconciles are the kinds it reconciles, as far as its permissions
	// tell: the kinds whose status it may write.
	Reconciles []ReconciledKind `json:"reconciles"`
	Findings   []Claim          `json:"findings"`
	// Events are the most recent Warning events about it, newest first.
	Events []Event `json:"events"`
}

// ContainerRestart is a container that has restarted.
type ContainerRestart struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Count     int32  `json:"count"`
	// LastReason and LastExitCode say how its last run ended.
	LastReason   string    `json:"lastReason,omitempty"`
	LastExitCode int32     `json:"lastExitCode"`
	LastFinished time.Time `json:"lastFinished,omitzero"`
	// Recent is true when its last run ended within the last hour.
	Recent bool `json:"recent"`
}

// Lease is a leader-election Lease, as controllers hold one so that only one
// replica reconciles at a time.
type Lease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Holder is the holder's identity, which controllers usually start with
	// their pod's name; "" when no one holds it.
	Holder string `json:"holder"`
	// HolderPod is the controller's pod that holds it, when one does.
	HolderPod string `json:"holderPod,omitempty"`
	// Basis is observed when one of the controller's pods holds it, and
	// inferred when tug took it for the controller's because of its name.
	Basis    Basis     `json:"basis"`
	Acquired time.Time `json:"acquired,omitzero"`
	Renewed  time.Time `json:"renewed,omitzero"`
	// DurationSeconds is how long the holder may go without renewing it.
	DurationSeconds int32 `json:"durationSeconds,omitempty"`
	Transitions     int32 `json:"transitions"`
	// Expired is true when it has not been renewed within its duration.
	Expired bool `json:"expired"`
}

// ReconciledKind is a kind a controller reconciles, as tug can tell it.
type ReconciledKind struct {
	Kind string `json:"kind,omitempty"`
	// Resource is the resource and group, such as
	// "certificates.cert-manager.io".
	Resource string `json:"resource"`
	Basis    Basis  `json:"basis"`
	Evidence string `json:"evidence"`
}

// ControllerStatusOf reports on the controller a workload or pod runs: Why's
// diagnosis of it, which of its pods holds its leader Lease and how recently
// that was renewed, which containers restarted, and the kinds whose status
// its service account may write, which are the kinds it most likely
// reconciles. It only reads.
func ControllerStatusOf(ctx context.Context, c Clients, t resolve.Target, opts WhyOptions) (*ControllerStatus, error) {
	now := nowOf(opts)()
	v, err := viewController(ctx, c.Typed, t, opts)
	if err != nil {
		return nil, err
	}
	d := v.diag
	out := &ControllerStatus{
		Target: d.Target, Namespace: t.Namespace, Rollout: d.Rollout, Summary: d.Summary,
		ServiceAccount: v.account.String(), Pods: d.Pods, Restarts: restartsOf(v.pods, now),
		Leases: v.leases, Leader: v.leader(), Reconciles: []ReconciledKind{},
		Findings: claims(d.Findings), Events: d.Events,
	}
	if out.Leases == nil {
		out.Leases = []Lease{}
	}
	out.Findings = append(out.Findings, v.leaseClaims(now)...)
	kinds, kindClaims, err := reconciledKinds(ctx, c, v.account)
	if err != nil {
		return nil, err
	}
	out.Reconciles = kinds
	out.Findings = append(out.Findings, kindClaims...)
	sortClaims(out.Findings)
	out.Healthy = d.Healthy && !hasProblems(out.Findings)
	out.Summary = v.summary()
	return out, nil
}

// controllerView is what tug reads about a controller's workload.
type controllerView struct {
	diag    *Diagnosis
	w       *workload
	pods    []corev1.Pod
	account serviceAccount
	leases  []Lease
	// leaseErr is why the Leases could not be read.
	leaseErr error
}

// viewController reads a controller's workload, its pods and its Leases.
func viewController(ctx context.Context, cs kubernetes.Interface, t resolve.Target, opts WhyOptions) (*controllerView, error) {
	now := nowOf(opts)()
	d, err := Why(ctx, cs, t, opts)
	if err != nil {
		return nil, err
	}
	w, err := fetchWorkload(ctx, cs, t)
	if err != nil {
		return nil, err
	}
	pods, err := w.pods(ctx, cs)
	if err != nil {
		return nil, err
	}
	v := &controllerView{diag: d, w: w, pods: pods, account: accountOf(w)}
	v.leases, v.leaseErr = findLeases(ctx, cs, t.Namespace, baseName(w), pods, now)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return v, nil
}

// leader returns the controller's pod that holds a live Lease, or "".
func (v *controllerView) leader() string {
	for _, l := range v.leases {
		if l.HolderPod != "" && !l.Expired {
			return l.HolderPod
		}
	}
	return ""
}

// running reports whether at least one of the controller's pods is ready,
// as the one that reconciles must be.
func (v *controllerView) running() bool {
	return slices.ContainsFunc(v.pods, func(p corev1.Pod) bool { return resolve.PodReady(&p) })
}

// leaderless reports whether the controller seems to use a Lease that none
// of its pods holds now.
func (v *controllerView) leaderless() bool {
	return len(v.leases) > 0 && v.leader() == ""
}

// summary is the workload's summary with its leader.
func (v *controllerView) summary() string {
	switch {
	case v.leader() != "":
		return v.diag.Summary + ", leader " + v.leader()
	case v.leaderless():
		return v.diag.Summary + ", no leader"
	default:
		return v.diag.Summary
	}
}

// leaseClaims says what the controller's Leases tell about its leader.
func (v *controllerView) leaseClaims(now time.Time) []Claim {
	ref := v.diag.Target
	ns := v.w.target.Namespace
	if v.leaseErr != nil {
		return []Claim{observed(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "LeasesUnreadable",
			Message: "its leader election is unknown: " + firstLine(v.leaseErr.Error())})}
	}
	if len(v.leases) == 0 {
		return []Claim{observed(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "NoLease",
			Message: fmt.Sprintf("no Lease in %s is held by its pods or named after it, so it may not use leader election, or keeps its lock elsewhere", ns)})}
	}
	var out []Claim
	for _, l := range v.leases {
		lease := "lease/" + l.Name
		logs := func(pod string) string { return "`tug logs pod/" + pod + " -n " + ns + "`" }
		switch {
		case l.HolderPod != "" && l.Expired:
			out = append(out, observed(Finding{Severity: SeverityError, Objects: []string{lease}, Reason: "LeaseExpired",
				Message: fmt.Sprintf("its leader, pod %s, has not renewed it since %s, longer than the %ds it may go without; the leader may be stuck, so nothing may be reconciling",
					l.HolderPod, l.Renewed.UTC().Format(time.RFC3339), l.DurationSeconds),
				Next: "read the leader's logs: " + logs(l.HolderPod)}))
		case l.HolderPod != "":
			if !l.Acquired.IsZero() && now.Sub(l.Acquired) <= recentWindow && l.Transitions > 0 {
				out = append(out, observed(Finding{Severity: SeverityInfo, Objects: []string{lease}, Reason: "LeaderChanged",
					Message: fmt.Sprintf("pod %s became the leader at %s, %s in all",
						l.HolderPod, l.Acquired.UTC().Format(time.RFC3339), plural(l.Transitions, "change"))}))
			}
		case l.Holder == "":
			out = append(out, inferred(Finding{Severity: SeverityWarning, Objects: []string{lease}, Reason: "NoLeader",
				Message: "is named after it but has no holder, so none of its replicas leads",
				Next:    "check that its pods run and can reach the API server: " + "`tug logs " + ref + " -n " + ns + "`"}))
		case l.Expired:
			out = append(out, inferred(Finding{Severity: SeverityError, Objects: []string{lease}, Reason: "NoLeader",
				Message: fmt.Sprintf("is named after it, but %s, which is not one of its pods, last renewed it at %s and it has expired, so none of its replicas leads",
					l.Holder, l.Renewed.UTC().Format(time.RFC3339)),
				Next: "check that its pods run and can reach the API server: " + "`tug logs " + ref + " -n " + ns + "`"}))
		default:
			out = append(out, inferred(Finding{Severity: SeverityWarning, Objects: []string{lease}, Reason: "LeaderElsewhere",
				Message: fmt.Sprintf("is named after it but held by %s, which is not one of its pods; another copy of the controller may be leading", l.Holder),
				Next:    "check whether the controller runs twice, in another namespace or cluster: `tug get deploy -A`"}))
		}
	}
	return out
}

// accountOf returns the service account a workload's pods run as.
func accountOf(w *workload) serviceAccount {
	var spec corev1.PodSpec
	if w.pod != nil {
		spec = w.pod.Spec
	} else if t := w.template(); t != nil {
		spec = t.Spec
	}
	return serviceAccount{namespace: w.target.Namespace, name: cmp.Or(spec.ServiceAccountName, "default")}
}

// baseName is the name a controller's Lease is likely named after: the
// workload's, or a static pod's without its node's.
func baseName(w *workload) string {
	if w.pod != nil {
		return strings.TrimSuffix(w.pod.Name, "-"+w.pod.Spec.NodeName)
	}
	return w.target.Name
}

// findLeases returns the Leases in a namespace that the controller's pods
// hold or, when they hold none, that are named after it and not held by
// some other running pod, newest renewal first.
func findLeases(ctx context.Context, cs kubernetes.Interface, ns, base string, pods []corev1.Pod, now time.Time) ([]Lease, error) {
	list, err := cs.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the leases in %s: %w", ns, err)
	}
	var held, named []Lease
	var others []corev1.Pod // read only when a Lease is named after the controller
	othersRead := false
	for i := range list.Items {
		l := leaseOf(&list.Items[i], now)
		if pod := holderPod(l.Holder, pods); pod != "" {
			l.HolderPod, l.Basis = pod, BasisObserved
			held = append(held, l)
			continue
		}
		if !namedAfter(l.Name, base) {
			continue
		}
		if !othersRead {
			othersRead = true
			if all, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err == nil {
				others = all.Items
			}
		}
		if l.Holder != "" && !l.Expired && holderPod(l.Holder, others) != "" {
			continue // another workload's lease that happens to share a name
		}
		l.Basis = BasisInferred
		named = append(named, l)
	}
	out := held
	if len(out) == 0 {
		out = named
	}
	slices.SortStableFunc(out, func(a, b Lease) int { return b.Renewed.Compare(a.Renewed) })
	return out, nil
}

func leaseOf(l *coordinationv1.Lease, now time.Time) Lease {
	out := Lease{Name: l.Name, Namespace: l.Namespace}
	s := l.Spec
	if s.HolderIdentity != nil {
		out.Holder = *s.HolderIdentity
	}
	if s.AcquireTime != nil {
		out.Acquired = s.AcquireTime.Time
	}
	if s.RenewTime != nil {
		out.Renewed = s.RenewTime.Time
	}
	if s.LeaseDurationSeconds != nil {
		out.DurationSeconds = *s.LeaseDurationSeconds
	}
	if s.LeaseTransitions != nil {
		out.Transitions = *s.LeaseTransitions
	}
	out.Expired = out.Holder != "" && !out.Renewed.IsZero() && out.DurationSeconds > 0 &&
		now.Sub(out.Renewed) > time.Duration(out.DurationSeconds)*time.Second
	return out
}

// holderPod returns the pod whose identity a Lease holder is. Controllers
// built on client-go identify themselves by their hostname, which is their
// pod's name, often followed by "_" and a random ID; a pod on the host's
// network, as the control plane's static pods are, has its node's name.
func holderPod(holder string, pods []corev1.Pod) string {
	if holder == "" {
		return ""
	}
	for i := range pods {
		p := &pods[i]
		if startsWithName(holder, p.Name) || p.Spec.HostNetwork && p.Spec.NodeName != "" && startsWithName(holder, p.Spec.NodeName) {
			return p.Name
		}
	}
	return ""
}

// startsWithName reports whether s is name or starts with it followed by a
// character that cannot be part of a name.
func startsWithName(s, name string) bool {
	if !strings.HasPrefix(s, name) {
		return false
	}
	return len(s) == len(name) || !isNameChar(s[len(name)])
}

func isNameChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.'
}

// namedAfter reports whether a Lease's name contains a controller's. Short
// names are left out, since they turn up inside unrelated names.
func namedAfter(lease, base string) bool {
	return len(base) >= 4 && strings.Contains(lease, base)
}

// restartsOf returns the containers that have restarted, the latest first.
func restartsOf(pods []corev1.Pod, now time.Time) []ContainerRestart {
	out := []ContainerRestart{}
	for i := range pods {
		p := &pods[i]
		for _, st := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			if st.RestartCount == 0 {
				continue
			}
			r := ContainerRestart{Pod: p.Name, Container: st.Name, Count: st.RestartCount}
			if last := st.LastTerminationState.Terminated; last != nil {
				r.LastReason, r.LastExitCode, r.LastFinished = last.Reason, last.ExitCode, last.FinishedAt.Time
				r.Recent = !r.LastFinished.IsZero() && now.Sub(r.LastFinished) <= recentWindow
			}
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b ContainerRestart) int { return b.LastFinished.Compare(a.LastFinished) })
	return out
}

// reconciledKinds returns the kinds whose status a service account may
// write, which are the kinds its controller most likely reconciles, with
// what limits the answer.
func reconciledKinds(ctx context.Context, c Clients, sa serviceAccount) ([]ReconciledKind, []Claim, error) {
	x, err := loadRBAC(ctx, c.Typed)
	if err != nil {
		return nil, nil, err
	}
	ref := "serviceaccount/" + sa.name
	var notes []Claim
	if len(x.unreadable) > 0 {
		notes = append(notes, observed(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "RBACUnreadable",
			Message: fmt.Sprintf("could not list %s, so the kinds it reconciles may be missing", strings.Join(x.unreadable, ", ")),
			Next:    "check what you may list: `kubectl auth can-i --list`"}))
	}
	kinds := map[schema.GroupResource]string{}
	if resources, _, err := listableResources(ctx, c); err == nil {
		for _, r := range resources {
			kinds[r.resource.GroupResource()] = r.kind
		}
	} else if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	out := []ReconciledKind{}
	seen := map[schema.GroupResource]bool{}
	var broad []string
	for _, g := range x.of(sa) {
		if g.wildcard() {
			if !slices.Contains(broad, g.role) {
				broad = append(broad, g.role)
			}
			continue
		}
		gr := schema.GroupResource{Group: g.group, Resource: g.resource}
		if seen[gr] {
			continue
		}
		seen[gr] = true
		evidence := fmt.Sprintf("its service account may update %s/status through %s", g.resource, g.role)
		if g.namespace != "" {
			evidence += " in namespace " + g.namespace
		}
		out = append(out, ReconciledKind{Kind: kinds[gr], Resource: gr.String(), Basis: BasisInferred, Evidence: evidence})
	}
	slices.SortFunc(out, func(a, b ReconciledKind) int { return strings.Compare(a.Resource, b.Resource) })
	switch {
	case len(broad) > 0:
		notes = append(notes, inferred(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "BroadPermissions",
			Message: fmt.Sprintf("may write the status of every kind, through %s, so its permissions do not tell which kinds it reconciles", strings.Join(broad, ", "))}))
	case len(out) == 0 && len(x.unreadable) == 0:
		notes = append(notes, observed(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "NoStatusPermissions",
			Message: "may not write any kind's status, so its permissions do not tell what it reconciles"}))
	}
	return out, notes, nil
}

// controllerWorkload is a deployment, statefulset or daemonset that may run
// a controller.
type controllerWorkload struct {
	target     resolve.Target
	account    serviceAccount
	containers []string
}

func (w controllerWorkload) key() string { return w.target.Namespace + "/" + w.target.String() }

// ControllerTargets lists the deployments, statefulsets and daemonsets in
// every namespace, to find a controller by name wherever it runs.
func ControllerTargets(ctx context.Context, cs kubernetes.Interface) ([]resolve.Target, error) {
	ws, err := listControllers(ctx, cs)
	if err != nil {
		return nil, err
	}
	out := make([]resolve.Target, len(ws))
	for i, w := range ws {
		out[i] = w.target
	}
	return out, nil
}

// listControllers lists the workloads in every namespace that may run a
// controller.
func listControllers(ctx context.Context, cs kubernetes.Interface) ([]controllerWorkload, error) {
	var out []controllerWorkload
	add := func(kind resolve.Kind, m metav1.ObjectMeta, sel *metav1.LabelSelector, tpl corev1.PodTemplateSpec) {
		t, ok := target(kind, m.Name, m.Namespace, sel)
		if !ok {
			return
		}
		w := controllerWorkload{target: t, account: serviceAccount{namespace: m.Namespace, name: cmp.Or(tpl.Spec.ServiceAccountName, "default")}}
		for _, c := range tpl.Spec.Containers {
			w.containers = append(w.containers, c.Name)
		}
		out = append(out, w)
	}
	all := metav1.ListOptions{}
	deploys, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("listing deployments in every namespace: %w", err)
	}
	for _, d := range deploys.Items {
		add(resolve.KindDeployment, d.ObjectMeta, d.Spec.Selector, d.Spec.Template)
	}
	sets, err := cs.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets in every namespace: %w", err)
	}
	for _, s := range sets.Items {
		add(resolve.KindStatefulSet, s.ObjectMeta, s.Spec.Selector, s.Spec.Template)
	}
	daemons, err := cs.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets in every namespace: %w", err)
	}
	for _, d := range daemons.Items {
		add(resolve.KindDaemonSet, d.ObjectMeta, d.Spec.Selector, d.Spec.Template)
	}
	return out, nil
}

func nowOf(o WhyOptions) func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}
