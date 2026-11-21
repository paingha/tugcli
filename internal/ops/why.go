package ops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/resolve"
)

// Severity says how much a finding matters.
type Severity string

// Severities, from worst to least.
const (
	// SeverityError stops the workload being ready.
	SeverityError Severity = "error"
	// SeverityWarning degrades it or will soon.
	SeverityWarning Severity = "warning"
	// SeverityInfo explains its state without anything being wrong.
	SeverityInfo Severity = "info"
)

func (s Severity) rank() int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// Finding is one reason a workload is not ready, and what to do about it.
type Finding struct {
	Severity Severity `json:"severity"`
	// Objects are what the finding is about, such as "pod/api-7d9f-x2x4z".
	// Pods with the same problem share one finding.
	Objects []string `json:"objects"`
	// Container names the container, when the finding is about one.
	Container string `json:"container,omitempty"`
	// Reason is the cause in a word or two, often Kubernetes' own, such as
	// CrashLoopBackOff.
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// Next suggests what to do, usually with a tug command in backticks.
	Next string `json:"next,omitempty"`
}

// Event is a recent Warning event about the workload or one of its parts.
type Event struct {
	Object   string    `json:"object"`
	Reason   string    `json:"reason"`
	Message  string    `json:"message"`
	Count    int32     `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

// PodState summarises one pod.
type PodState struct {
	Name string `json:"name"`
	// Phase is the pod's phase; Status is what kubectl's STATUS column
	// would show, such as CrashLoopBackOff.
	Phase           string    `json:"phase"`
	Status          string    `json:"status"`
	ReadyContainers int       `json:"readyContainers"`
	Containers      int       `json:"containers"`
	Restarts        int32     `json:"restarts"`
	Node            string    `json:"node,omitempty"`
	Created         time.Time `json:"created"`
	// Conditions are the pod's conditions that are not true.
	Conditions []Condition `json:"conditions,omitempty"`
}

// Condition is a status condition: one of a pod's that is not true, or any
// of another object's.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// LastTransition is when the status last changed, when the object
	// says.
	LastTransition time.Time `json:"lastTransition,omitzero"`
	// ObservedGeneration is the generation of the object the condition was
	// set for, when the object says.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Diagnosis is what Why found out about a workload or pod.
type Diagnosis struct {
	Target    string `json:"target"`
	Namespace string `json:"namespace"`
	// Healthy is true when the workload has rolled out, its pods are ready
	// (or, for a job, it has not failed) and nothing is wrong.
	Healthy bool   `json:"healthy"`
	Summary string `json:"summary"`
	// Rollout is nil for a bare pod and for objects other than workloads.
	Rollout *Rollout `json:"rollout,omitempty"`
	// Conditions are the status conditions of an object other than a
	// workload or pod.
	Conditions []Condition `json:"conditions,omitempty"`
	// Pods are the workload's pods or, for another object, the pods of the
	// workloads it owns.
	Pods     []PodState `json:"pods"`
	Findings []Finding  `json:"findings"`
	// Events are the most recent Warning events, newest first.
	Events []Event `json:"events"`
}

// WhyOptions tune Why.
type WhyOptions struct {
	// Now is the time ages are measured from; it defaults to time.Now.
	Now func() time.Time
	// ExplicitNamespace adds -n <namespace> to the commands findings
	// suggest, for a namespace that is not the session's.
	ExplicitNamespace bool
}

const (
	// recentWindow is how far back events and restarts count. Kubernetes
	// keeps events for an hour by default.
	recentWindow = time.Hour
	// maxEvents is how many recent Warning events a diagnosis lists.
	maxEvents = 10
	// slowStart is how long a pod may take to start before it is worth a
	// warning.
	slowStart = 2 * time.Minute
)

// Why explains why a workload or bare pod is not ready: its rollout, its
// pods' phases, conditions and containers, failing probes, scheduling
// failures, PersistentVolumeClaims stuck in Pending and recent Warning
// events. Each finding has a severity and a suggested next step. It only
// reads.
func Why(ctx context.Context, cs kubernetes.Interface, t resolve.Target, o WhyOptions) (*Diagnosis, error) {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	w, err := fetchWorkload(ctx, cs, t)
	if err != nil {
		return nil, err
	}
	pods, err := w.pods(ctx, cs)
	if err != nil {
		return nil, err
	}
	d := &diagnoser{
		w:       w,
		now:     now(),
		ns:      t.Namespace,
		nsFlag:  o.ExplicitNamespace,
		pods:    pods,
		objects: map[string]bool{objectKey(kindOf(t.Kind), t.Name): true},
		out: &Diagnosis{
			Target:    t.String(),
			Namespace: t.Namespace,
			Rollout:   w.rollout(),
			Pods:      []PodState{},
			Findings:  []Finding{},
			Events:    []Event{},
		},
	}
	if w.deploy != nil {
		if err := d.replicaSets(ctx, cs); err != nil {
			return nil, err
		}
	}
	for i := range pods {
		d.objects[objectKey("Pod", pods[i].Name)] = true
	}
	if err := d.loadEvents(ctx, cs); err != nil {
		d.add(Finding{
			Severity: SeverityInfo, Objects: []string{"namespace/" + d.ns}, Reason: "EventsUnavailable",
			Message: fmt.Sprintf("events could not be read, so probe and scheduling failures may be missing: %v", err),
		})
	}

	d.workload()
	for i := range pods {
		d.pod(&pods[i])
	}
	d.claims(ctx, cs)
	d.controllerEvents()
	d.finish()
	return d.out, nil
}

// diagnoser collects the findings of one Why call.
type diagnoser struct {
	w      *workload
	now    time.Time
	ns     string
	nsFlag bool
	pods   []corev1.Pod
	// objects holds "Kind/name" for the workload and its parts; events
	// about anything else are ignored.
	objects map[string]bool
	events  map[string][]corev1.Event
	out     *Diagnosis
}

func objectKey(kind, name string) string { return kind + "/" + name }

// kindOf returns the Kubernetes kind of a target kind.
func kindOf(k resolve.Kind) string {
	switch k {
	case resolve.KindDeployment:
		return "Deployment"
	case resolve.KindStatefulSet:
		return "StatefulSet"
	case resolve.KindDaemonSet:
		return "DaemonSet"
	case resolve.KindJob:
		return "Job"
	default:
		return "Pod"
	}
}

func (d *diagnoser) add(f Finding) { d.out.Findings = append(d.out.Findings, f) }

// tug formats a tug command to suggest, in backticks.
func (d *diagnoser) tug(args ...string) string {
	cmd := "tug " + strings.Join(args, " ")
	if d.nsFlag {
		cmd += " -n " + d.ns
	}
	return "`" + cmd + "`"
}

// replicaSets loads a deployment's ReplicaSets, whose events explain pods
// that were never created.
func (d *diagnoser) replicaSets(ctx context.Context, cs kubernetes.Interface) error {
	list, err := cs.AppsV1().ReplicaSets(d.ns).List(ctx, metav1.ListOptions{LabelSelector: d.w.target.Selector.String()})
	if err != nil {
		return fmt.Errorf("listing the ReplicaSets of %s: %w", d.w.target, err)
	}
	for _, rs := range list.Items {
		if metav1.IsControlledBy(&rs, d.w.deploy) {
			d.objects[objectKey("ReplicaSet", rs.Name)] = true
		}
	}
	return nil
}

// loadEvents lists the namespace's events once and keeps those about the
// workload and its parts, oldest first.
func (d *diagnoser) loadEvents(ctx context.Context, cs kubernetes.Interface) error {
	d.events = map[string][]corev1.Event{}
	list, err := cs.CoreV1().Events(d.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, e := range list.Items {
		key := objectKey(e.InvolvedObject.Kind, e.InvolvedObject.Name)
		if d.objects[key] || e.InvolvedObject.Kind == "PersistentVolumeClaim" {
			d.events[key] = append(d.events[key], e)
		}
	}
	for _, evs := range d.events {
		slices.SortStableFunc(evs, func(a, b corev1.Event) int { return lastSeen(&a).Compare(lastSeen(&b)) })
	}
	return nil
}

// recentEvents returns an object's events from the last hour, oldest first.
func (d *diagnoser) recentEvents(kind, name string) []corev1.Event {
	var out []corev1.Event
	for _, e := range d.events[objectKey(kind, name)] {
		if d.now.Sub(lastSeen(&e)) <= recentWindow {
			out = append(out, e)
		}
	}
	return out
}

// latestEvent returns the newest recent event about an object with one of
// the reasons, or nil.
func (d *diagnoser) latestEvent(kind, name string, reasons ...string) *corev1.Event {
	evs := d.recentEvents(kind, name)
	for i := len(evs) - 1; i >= 0; i-- {
		if slices.Contains(reasons, evs[i].Reason) {
			return &evs[i]
		}
	}
	return nil
}

// lastSeen returns when an event last happened, whichever API wrote it.
func lastSeen(e *corev1.Event) time.Time {
	t := e.LastTimestamp.Time
	if e.Series != nil && e.Series.LastObservedTime.After(t) {
		t = e.Series.LastObservedTime.Time
	}
	for _, alt := range []time.Time{e.EventTime.Time, e.FirstTimestamp.Time, e.CreationTimestamp.Time} {
		if t.IsZero() {
			t = alt
		}
	}
	return t
}

func eventCount(e *corev1.Event) int32 {
	switch {
	case e.Series != nil && e.Series.Count > 0:
		return e.Series.Count
	case e.Count > 0:
		return e.Count
	default:
		return 1
	}
}

// workload checks the controller's own state.
func (d *diagnoser) workload() {
	ref := d.w.target.String()
	r := d.out.Rollout
	switch {
	case d.w.deploy != nil:
		dep := d.w.deploy
		if r.Desired == 0 {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "ScaledToZero",
				Message: "is scaled to zero replicas, so nothing runs", Next: "scale it up: " + d.tug("scale", ref, "1")})
		}
		if dep.Spec.Paused {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Paused",
				Message: "its rollout is paused, so changes to the pod template do not roll out",
				Next:    "resume it: " + d.tug("rollout", "resume", ref)})
		}
		for _, c := range dep.Status.Conditions {
			switch {
			case c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse:
				d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: c.Reason, Message: c.Message,
					Next: "fix what stops the new pods (see the other findings), or roll back: " + d.tug("rollback", ref)})
			case c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue:
				d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: c.Reason, Message: c.Message,
					Next: "check ResourceQuotas and admission policies in the namespace: " + d.tug("describe", "quota")})
			}
		}
	case d.w.sts != nil:
		sts := d.w.sts
		if r.Desired == 0 {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "ScaledToZero",
				Message: "is scaled to zero replicas, so nothing runs", Next: "scale it up: " + d.tug("scale", ref, "1")})
		}
		if sts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType && sts.Status.UpdateRevision != sts.Status.CurrentRevision {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "OnDelete",
				Message: fmt.Sprintf("uses the OnDelete update strategy, so only %d/%d pods run the latest revision", r.Updated, r.Desired),
				Next:    "delete the old pods one at a time to update them"})
		}
	case d.w.ds != nil:
		ds := d.w.ds
		if r.Desired == 0 {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "NoNodes",
				Message: "matches no nodes, so it runs no pods",
				Next:    "check its nodeSelector, affinity and tolerations against the nodes: " + d.tug("get", "nodes", "--show-labels")})
		}
		if n := ds.Status.NumberMisscheduled; n > 0 {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Misscheduled",
				Message: fmt.Sprintf("%d pods run on nodes they should not", n),
				Next:    "check its nodeSelector and the labels of the nodes it runs on"})
		}
	case d.w.job != nil:
		j := d.w.job
		if c := jobCondition(j, batchv1.JobFailed); c != nil {
			d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: c.Reason, Message: lowerFirst(c.Message),
				Next: "read the failed pods' logs: " + d.tug("logs", ref) + "; a failed job does not retry, so recreate it once fixed"})
		}
		if jobCondition(j, batchv1.JobSuspended) != nil {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Suspended",
				Message: "is suspended, so it starts no pods", Next: "set spec.suspend to false to resume it"})
		}
	}
}

// pod checks one pod: deletion, phase, scheduling, containers and probes.
func (d *diagnoser) pod(p *corev1.Pod) {
	ref := "pod/" + p.Name
	d.out.Pods = append(d.out.Pods, podState(p))
	before := len(d.out.Findings)

	if p.DeletionTimestamp != nil {
		// DeletionTimestamp is when the grace period ends.
		if d.now.Sub(p.DeletionTimestamp.Time) > time.Minute {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Terminating",
				Message: fmt.Sprintf("has been terminating since %s, past its grace period", p.DeletionTimestamp.UTC().Format(time.RFC3339)),
				Next:    "check its finalizers and its node: " + d.tug("describe", ref)})
		}
		return
	}
	if p.Status.Phase == corev1.PodSucceeded {
		return
	}
	if p.Status.Phase == corev1.PodFailed && p.Status.Reason != "" {
		sev := SeverityError
		next := "see what happened on its node: " + d.tug("describe", ref)
		if d.w.target.IsWorkload() {
			sev = SeverityWarning // the controller replaces it
			next = "the controller replaces it; " + next
		}
		d.add(Finding{Severity: sev, Objects: []string{ref}, Reason: p.Status.Reason, Message: lowerFirst(p.Status.Message), Next: next})
	}

	d.scheduling(p)
	statuses := map[string]corev1.ContainerStatus{}
	for _, st := range p.Status.InitContainerStatuses {
		statuses[st.Name] = st
		d.container(p, st, true)
	}
	for _, st := range p.Status.ContainerStatuses {
		statuses[st.Name] = st
		d.container(p, st, false)
	}
	d.probes(p, statuses)

	if len(d.out.Findings) > before || podHealthy(p) || p.Status.Phase == corev1.PodFailed {
		return
	}
	// Nothing specific explains it; say what the pod's conditions say.
	age := d.now.Sub(p.CreationTimestamp.Time)
	sev := SeverityWarning
	if age < slowStart {
		sev = SeverityInfo
	}
	msg := fmt.Sprintf("is %s and not ready", podStatus(p))
	if c := podCondition(p, corev1.PodReady); c != nil && c.Message != "" {
		msg += ": " + c.Message
	}
	d.add(Finding{Severity: sev, Objects: []string{ref}, Reason: "NotReady", Message: msg,
		Next: "see its events and conditions: " + d.tug("describe", ref)})
}

func podState(p *corev1.Pod) PodState {
	s := PodState{
		Name:            p.Name,
		Phase:           string(p.Status.Phase),
		Status:          podStatus(p),
		ReadyContainers: readyContainers(p),
		Containers:      len(p.Spec.Containers),
		Restarts:        restarts(p),
		Node:            p.Spec.NodeName,
		Created:         p.CreationTimestamp.Time,
	}
	for _, c := range p.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			s.Conditions = append(s.Conditions, Condition{
				Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			})
		}
	}
	return s
}

// scheduling explains a pod that no node has taken.
func (d *diagnoser) scheduling(p *corev1.Pod) {
	if p.Spec.NodeName != "" {
		return
	}
	ref := "pod/" + p.Name
	var msg string
	if c := podCondition(p, corev1.PodScheduled); c != nil && c.Status == corev1.ConditionFalse {
		if c.Reason == corev1.PodReasonSchedulingGated {
			var gates []string
			for _, g := range p.Spec.SchedulingGates {
				gates = append(gates, g.Name)
			}
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: c.Reason,
				Message: "waits for scheduling gates to be removed: " + strings.Join(gates, ", "),
				Next:    "the controller that added the gates removes them; check that it runs"})
			return
		}
		msg = c.Message
	}
	if msg == "" {
		if e := d.latestEvent("Pod", p.Name, "FailedScheduling"); e != nil {
			msg = e.Message
		}
	}
	if msg == "" {
		return
	}
	msg, _, _ = strings.Cut(msg, " preemption:")
	msg = strings.TrimSuffix(strings.TrimSpace(msg), ".")
	d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: "FailedScheduling",
		Message: "no node can run it: " + msg, Next: d.schedulingNext(ref, msg)})
}

// schedulingNext suggests fixes for the reasons in a scheduler message such
// as "0/3 nodes are available: 1 Insufficient nvidia.com/gpu, 2 node(s) had
// untolerated taint {dedicated: gpu}".
func (d *diagnoser) schedulingNext(ref, msg string) string {
	var next []string
	add := func(s string) {
		if !slices.Contains(next, s) {
			next = append(next, s)
		}
	}
	_, reasons, _ := strings.Cut(msg, "available: ")
	for _, r := range strings.Split(reasons, ", ") {
		lr := strings.ToLower(r)
		switch {
		case strings.Contains(lr, "insufficient") && strings.Contains(lr, "gpu"):
			add("add nodes with free GPUs, or lower the pod's GPU request")
		case strings.Contains(lr, "insufficient cpu"), strings.Contains(lr, "insufficient memory"),
			strings.Contains(lr, "insufficient ephemeral-storage"):
			add("lower the pod's resource requests, or add capacity to the cluster")
		case strings.Contains(lr, "insufficient"):
			add("free up or add the resource the pod requests")
		case strings.Contains(lr, "taint"):
			add("add a toleration for the taint, or run the pod on nodes without it")
		case strings.Contains(lr, "node affinity/selector"), strings.Contains(lr, "node selector"):
			add("check the pod's nodeSelector and affinity against the nodes' labels: " + d.tug("get", "nodes", "--show-labels"))
		case strings.Contains(lr, "persistentvolumeclaim"):
			add("check the pod's volume claims: " + d.tug("get", "pvc"))
		case strings.Contains(lr, "volume node affinity conflict"):
			add("its volume is in a zone with no free node; add capacity there")
		case strings.Contains(lr, "free ports"):
			add("another pod on each node uses the same hostPort")
		case strings.Contains(lr, "anti-affinity"), strings.Contains(lr, "pod affinity"):
			add("check the pod's affinity and anti-affinity rules")
		case strings.Contains(lr, "too many pods"):
			add("the nodes are at their pod limit; add nodes")
		case strings.Contains(lr, "unschedulable"):
			add("the nodes are cordoned; uncordon one or add nodes")
		}
	}
	if len(next) == 0 {
		return "read the scheduler's full message: " + d.tug("describe", ref)
	}
	return strings.Join(next, "; ")
}

// container checks one container's state.
func (d *diagnoser) container(p *corev1.Pod, st corev1.ContainerStatus, init bool) {
	ref := "pod/" + p.Name
	// Messages describe the container, which the finding names; restart
	// counts are left to the pod table so that pods with the same problem
	// share a finding.
	what := ""
	if init {
		what = "as an init container, it "
	}
	logs := func(previous bool) string {
		args := []string{"logs", ref}
		if init || len(p.Spec.Containers) > 1 {
			args = append(args, "-c", st.Name)
		}
		if previous {
			args = append(args, "--previous")
		}
		return d.tug(args...)
	}
	f := Finding{Objects: []string{ref}, Container: st.Name}
	last := st.LastTerminationState.Terminated

	switch {
	case st.State.Waiting != nil:
		w := st.State.Waiting
		f.Severity, f.Reason = SeverityError, w.Reason
		switch w.Reason {
		case "CrashLoopBackOff":
			if last != nil && last.Reason == "OOMKilled" {
				d.oom(p, st, SeverityError)
				return
			}
			f.Message = what + "keeps crashing and waits to restart"
			if last != nil {
				f.Message += "; last exit code " + exitCode(last)
				if m := firstLine(last.Message); m != "" {
					f.Message += ": " + m
				}
			}
			f.Next = "read the crashed container's logs: " + logs(true)
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
			f.Message = what + "cannot pull image " + imageOf(p, st)
			if w.Message != "" {
				f.Message += ": " + w.Message
			}
			f.Next = "check that the image and tag exist and that the pod may pull from the registry (imagePullSecrets)"
		case "CreateContainerConfigError":
			f.Message = what + "cannot start: " + w.Message
			f.Next = "create the missing ConfigMap or Secret, or fix the reference; " + d.tug("tree", d.w.target.String()) + " shows which is missing"
		case "CreateContainerError", "RunContainerError", "PreStartHookError", "PostStartHookError":
			f.Message = what + "cannot start: " + w.Message
			f.Next = "check the container's command, volume mounts and security context: " + d.tug("describe", ref)
		case "", "ContainerCreating", "PodInitializing":
			// Still starting: volume and sandbox problems show up as events.
			return
		default:
			f.Severity = SeverityWarning
			f.Message = what + "is waiting: " + w.Message
			f.Next = "see its events: " + d.tug("describe", ref)
		}
	case st.State.Terminated != nil:
		t := st.State.Terminated
		if t.ExitCode == 0 {
			return // finished, as init containers and job pods do
		}
		if t.Reason == "OOMKilled" {
			d.oom(p, st, SeverityError)
			return
		}
		f.Severity, f.Reason = SeverityError, "Failed"
		f.Message = what + "exited with code " + exitCode(t)
		if m := firstLine(t.Message); m != "" {
			f.Message += ": " + m
		}
		f.Next = "read its logs: " + logs(false)
	case st.State.Running != nil:
		if st.RestartCount == 0 || last == nil || d.now.Sub(last.FinishedAt.Time) > recentWindow {
			return
		}
		if last.Reason == "OOMKilled" {
			d.oom(p, st, SeverityWarning)
			return
		}
		f.Severity, f.Reason = SeverityWarning, "Restarted"
		f.Message = what + "has restarted in the last hour; last exit code " + exitCode(last)
		f.Next = "read the logs from before the restart: " + logs(true)
	default:
		return
	}
	d.add(f)
}

// oom reports a container killed for running out of memory.
func (d *diagnoser) oom(p *corev1.Pod, st corev1.ContainerStatus, sev Severity) {
	limit := ""
	for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
		if c.Name == st.Name {
			if m := c.Resources.Limits.Memory(); m != nil && !m.IsZero() {
				limit = m.String()
			}
		}
	}
	msg := "was killed for using more memory than its limit of " + limit
	next := "raise its memory limit, or find what uses the memory: " + d.tug("top", d.w.target.String())
	if limit == "" {
		msg = "was killed because its node ran out of memory (it has no memory limit)"
		next = "set a memory request and limit that fit what it uses: " + d.tug("top", d.w.target.String())
	}
	d.add(Finding{Severity: sev, Objects: []string{"pod/" + p.Name}, Container: st.Name, Reason: "OOMKilled",
		Message: msg, Next: next})
}

// probes reports failing readiness, liveness and startup probes from the
// kubelet's Unhealthy events.
func (d *diagnoser) probes(p *corev1.Pod, statuses map[string]corev1.ContainerStatus) {
	ref := "pod/" + p.Name
	seen := map[string]bool{}
	evs := d.recentEvents("Pod", p.Name)
	for i := len(evs) - 1; i >= 0; i-- { // newest first
		e := &evs[i]
		if e.Reason != "Unhealthy" {
			continue
		}
		probe, detail, ok := strings.Cut(e.Message, " probe failed: ")
		if !ok {
			continue
		}
		container := containerOf(e.InvolvedObject.FieldPath)
		key := probe + "/" + container
		if seen[key] {
			continue
		}
		seen[key] = true
		st := statuses[container]
		logs := []string{"logs", ref}
		if len(p.Spec.Containers) > 1 && container != "" {
			logs = append(logs, "-c", container)
		}
		f := Finding{Objects: []string{ref}, Container: container}
		count := plural(eventCount(e), "time")
		switch probe {
		case "Readiness":
			if st.Ready {
				continue
			}
			f.Severity, f.Reason = SeverityWarning, "ReadinessProbeFailed"
			f.Message = fmt.Sprintf("readiness probe failed %s, so the pod gets no traffic: %s", count, detail)
			f.Next = "check the probe's path, port and timing against the app, and its logs: " + d.tug(logs...)
		case "Liveness":
			f.Severity, f.Reason = SeverityWarning, "LivenessProbeFailed"
			if st.RestartCount > 0 {
				f.Severity = SeverityError
			}
			f.Message = fmt.Sprintf("liveness probe failed %s, so the kubelet restarts the container: %s", count, detail)
			f.Next = "check the probe against the app; the logs from before the restart: " + d.tug(append(logs, "--previous")...)
		case "Startup":
			if st.Started != nil && *st.Started {
				continue
			}
			f.Severity, f.Reason = SeverityWarning, "StartupProbeFailed"
			f.Message = fmt.Sprintf("startup probe failed %s: %s", count, detail)
			f.Next = "give the app longer to start (failureThreshold × periodSeconds), or check its logs: " + d.tug(logs...)
		default:
			continue
		}
		d.add(f)
	}
}

// containerOf returns "main" from an event's field path "spec.containers{main}".
func containerOf(fieldPath string) string {
	_, rest, ok := strings.Cut(fieldPath, "{")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "}")
	return name
}

// claims checks the PersistentVolumeClaims the pods mount.
func (d *diagnoser) claims(ctx context.Context, cs kubernetes.Interface) {
	seen := map[string]bool{}
	for i := range d.pods {
		p := &d.pods[i]
		for _, v := range p.Spec.Volumes {
			name := ""
			switch {
			case v.PersistentVolumeClaim != nil:
				name = v.PersistentVolumeClaim.ClaimName
			case v.Ephemeral != nil:
				name = p.Name + "-" + v.Name
			}
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			d.claim(ctx, cs, name)
		}
	}
}

func (d *diagnoser) claim(ctx context.Context, cs kubernetes.Interface, name string) {
	ref := "pvc/" + name
	pvc, err := cs.CoreV1().PersistentVolumeClaims(d.ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: "NotFound",
			Message: "is mounted by the pods but does not exist", Next: "create it, or fix the pod's volume"})
		return
	case err != nil:
		d.add(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "Unreadable",
			Message: fmt.Sprintf("could not be read: %v", err)})
		return
	}
	class := ""
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	switch pvc.Status.Phase {
	case corev1.ClaimPending:
		warn := d.latestEvent("PersistentVolumeClaim", name, "ProvisioningFailed", "FailedBinding", "ExternalProvisioning")
		if warn == nil && d.latestEvent("PersistentVolumeClaim", name, "WaitForFirstConsumer", "WaitForPodScheduled") != nil {
			d.add(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "WaitForFirstConsumer",
				Message: "is Pending until a pod that uses it is scheduled, as its storage class asks"})
			return
		}
		msg := "is Pending: no volume is bound to it"
		if warn != nil {
			msg += ": " + warn.Message
		}
		next := "check its storage class and provisioner: " + d.tug("describe", ref)
		if class != "" {
			next = fmt.Sprintf("check storage class %s and its provisioner: %s", class, d.tug("describe", ref))
		}
		d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: "Pending", Message: msg, Next: next})
	case corev1.ClaimLost:
		d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: "Lost",
			Message: "has lost its volume (" + pvc.Spec.VolumeName + ")", Next: "restore the volume or recreate the claim"})
	}
}

// eventNext says what to do about a Warning event that explains a problem
// on its own, and whether the reason is one of those.
func (d *diagnoser) eventNext(reason, ref string) (string, bool) {
	switch reason {
	case "FailedMount":
		return "check the volumes the pods mount: " + d.tug("tree", d.w.target.String()), true
	case "FailedAttachVolume", "FailedMapVolume":
		return "the volume may still be attached to another node: " + d.tug("describe", ref), true
	case "FailedCreatePodSandBox":
		return "the node could not set up the pod's network or sandbox: " + d.tug("describe", ref), true
	case "FailedCreate":
		return "check ResourceQuotas, LimitRanges and admission policies: " + d.tug("describe", ref), true
	case "FailedPostStartHook", "FailedPreStopHook":
		return "check the container's lifecycle hooks: " + d.tug("describe", ref), true
	default:
		return "", false
	}
}

// controllerEvents turns Warning events with a clear meaning into findings
// and lists the most recent Warning events.
func (d *diagnoser) controllerEvents() {
	type keyed struct {
		e   corev1.Event
		ref string
	}
	var warnings []keyed
	for _, evs := range d.events {
		for _, e := range evs {
			if e.Type != corev1.EventTypeWarning || d.now.Sub(lastSeen(&e)) > recentWindow {
				continue
			}
			if e.InvolvedObject.Kind == "PersistentVolumeClaim" && !d.mounted(e.InvolvedObject.Name) {
				continue
			}
			warnings = append(warnings, keyed{e, shortKind(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name})
		}
	}
	slices.SortStableFunc(warnings, func(a, b keyed) int { return lastSeen(&b.e).Compare(lastSeen(&a.e)) })

	explained := map[string]bool{}
	for _, k := range warnings {
		next, ok := d.eventNext(k.e.Reason, k.ref)
		if !ok || explained[k.e.Reason+"/"+k.ref] {
			continue
		}
		explained[k.e.Reason+"/"+k.ref] = true
		d.add(Finding{Severity: SeverityError, Objects: []string{k.ref}, Container: containerOf(k.e.InvolvedObject.FieldPath),
			Reason: k.e.Reason, Message: k.e.Message, Next: next})
	}
	for i, k := range warnings {
		if i == maxEvents {
			break
		}
		d.out.Events = append(d.out.Events, Event{
			Object: k.ref, Reason: k.e.Reason, Message: k.e.Message, Count: eventCount(&k.e), LastSeen: lastSeen(&k.e),
		})
	}
}

// mounted reports whether one of the pods mounts the claim.
func (d *diagnoser) mounted(claim string) bool {
	for i := range d.pods {
		for _, v := range d.pods[i].Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim ||
				v.Ephemeral != nil && d.pods[i].Name+"-"+v.Name == claim {
				return true
			}
		}
	}
	return false
}

// finish groups findings that pods share, orders them worst first and
// decides whether the workload is healthy.
func (d *diagnoser) finish() {
	out := d.out
	if len(d.pods) == 0 && out.Rollout != nil && out.Rollout.Desired > 0 && !out.Rollout.Complete &&
		!slices.ContainsFunc(out.Findings, func(f Finding) bool { return f.Severity == SeverityError }) &&
		(d.w.job == nil || jobCondition(d.w.job, batchv1.JobFailed) == nil) {
		d.add(Finding{Severity: SeverityWarning, Objects: []string{d.w.target.String()}, Reason: "NoPods",
			Message: "has no pods yet", Next: "see the controller's events: " + d.tug("describe", d.w.target.String())})
	}

	grouped := groupFindings(out.Findings)
	out.Findings = grouped

	problems := slices.ContainsFunc(grouped, func(f Finding) bool { return f.Severity != SeverityInfo })
	switch r := out.Rollout; {
	case d.w.pod != nil:
		out.Healthy = !problems && podHealthy(d.w.pod)
		out.Summary = fmt.Sprintf("%s, %d/%d containers ready", podStatus(d.w.pod), readyContainers(d.w.pod), len(d.w.pod.Spec.Containers))
	case d.w.job != nil:
		out.Healthy = !problems && jobCondition(d.w.job, batchv1.JobFailed) == nil
		out.Summary = r.Summary
	default:
		out.Healthy = !problems && r.Complete
		out.Summary = r.Summary
		if !r.Complete && !problems {
			d.out.Findings = append(d.out.Findings, Finding{Severity: SeverityInfo, Objects: []string{d.w.target.String()},
				Reason: "RolloutInProgress", Message: "is still rolling out: " + r.Summary,
				Next: "watch it: " + d.tug("rollout", "status", d.w.target.String())})
		}
	}
}

// groupFindings merges findings that differ only in their objects, as pods
// with the same problem do, and orders them worst first.
func groupFindings(findings []Finding) []Finding {
	var grouped []Finding
	index := map[string]int{}
	for _, f := range findings {
		key := strings.Join([]string{string(f.Severity), f.Reason, f.Container, f.Message}, "\x00")
		if i, ok := index[key]; ok {
			for _, o := range f.Objects {
				if !slices.Contains(grouped[i].Objects, o) {
					grouped[i].Objects = append(grouped[i].Objects, o)
				}
			}
			continue
		}
		index[key] = len(grouped)
		grouped = append(grouped, f)
	}
	slices.SortStableFunc(grouped, func(a, b Finding) int { return a.Severity.rank() - b.Severity.rank() })
	return grouped
}

func plural(n int32, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// exitCode describes how a container stopped: "137 (OOMKilled)".
func exitCode(t *corev1.ContainerStateTerminated) string {
	s := fmt.Sprint(t.ExitCode)
	if t.Reason != "" {
		s += " (" + t.Reason + ")"
	} else if t.Signal != 0 {
		s += fmt.Sprintf(" (signal %d)", t.Signal)
	}
	return s
}

// imageOf returns the image a container runs, from its spec when the status
// has none yet.
func imageOf(p *corev1.Pod, st corev1.ContainerStatus) string {
	for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
		if c.Name == st.Name {
			return c.Image
		}
	}
	return st.Image
}

// firstLine returns the first line of a termination message, shortened.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// lowerFirst lowers the first letter of a Kubernetes message such as "Job
// has reached the specified backoff limit" so it reads as part of a
// sentence. A first word with other capitals, such as a kind, is kept.
func lowerFirst(s string) string {
	word, _, _ := strings.Cut(s, " ")
	if len(word) < 2 || strings.ToLower(word[1:]) != word[1:] {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
