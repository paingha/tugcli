package ops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/paingha/tugcli/internal/resolve"
)

// NoReconcileHistory says what a timeline cannot show, for the CLI's help
// and for every timeline's notes.
const NoReconcileHistory = "Kubernetes keeps no history of reconciles. This timeline is pieced together from " +
	"the timestamps objects carry now (when they were created, when each condition last changed, when each " +
	"field manager last wrote) and from events, which the API server keeps for an hour by default, so older " +
	"changes, and objects deleted since, leave no trace."

// Source says where a timeline entry comes from.
type Source string

// Sources of timeline entries.
const (
	// SourceMetadata is the object's own creation and deletion timestamps.
	SourceMetadata Source = "metadata"
	// SourceManagedFields is when each field manager last wrote the object.
	SourceManagedFields Source = "managedFields"
	// SourceCondition is when each status condition last changed.
	SourceCondition Source = "condition"
	// SourceEvent is an event about the object or something it owns.
	SourceEvent Source = "event"
	// SourceOwned is the creation and deletion timestamps of what the
	// object owns.
	SourceOwned Source = "owned"
	// SourceContainer is when the containers of pods it owns started and
	// stopped.
	SourceContainer Source = "container"
	// SourceLog is a line the controller logged that mentions the object.
	SourceLog Source = "log"
)

// Timeline is what tug can piece together of what happened to an object
// and what it owns, oldest first.
type Timeline struct {
	Target    string `json:"target"`
	Namespace string `json:"namespace"`
	// Generation and ObservedGeneration are as in Explanation.
	Generation         int64 `json:"generation,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Controller is the workload whose logs were read, in
	// ControllerNamespace; ControllerBasis is observed when it was named
	// and inferred when tug found it.
	Controller          string          `json:"controller,omitempty"`
	ControllerNamespace string          `json:"controllerNamespace,omitempty"`
	ControllerBasis     Basis           `json:"controllerBasis,omitempty"`
	Entries             []TimelineEntry `json:"entries"`
	// Notes say what the timeline cannot show.
	Notes []string `json:"notes"`
}

// TimelineEntry is one thing that happened, with where tug learned it.
type TimelineEntry struct {
	Time   time.Time `json:"time"`
	Source Source    `json:"source"`
	// Object is what it happened to, such as "pod/api-7d9f-x2x4z", or for a
	// log line, the pod that logged it.
	Object  string `json:"object"`
	Message string `json:"message"`
	// Basis is inferred where tug links the entry to the object on
	// evidence that could mislead, such as a log line that has its name but
	// not its UID.
	Basis Basis `json:"basis"`
	// Type, Reason, Count and Until describe an event: Normal or Warning,
	// its reason, how many times it happened and when it last did.
	Type   string    `json:"type,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Count  int32     `json:"count,omitempty"`
	Until  time.Time `json:"until,omitzero"`
}

// TraceOptions tune Trace.
type TraceOptions struct {
	// Logs adds the lines the controller logged that mention the object's
	// name or UID.
	Logs bool
	// Controller is the workload whose logs to read; nil to find it as
	// Explain does.
	Controller *resolve.Target
	// Since is how far back to read the logs; it defaults to an hour.
	Since time.Duration
}

// maxLogEntries is how many log lines a timeline keeps, the newest.
const maxLogEntries = 200

// Trace pieces together a timeline of an object: its creation and deletion,
// when each field manager last wrote it, when each condition last changed,
// the events about it and everything it owns, when what it owns was created
// or marked for deletion, when the containers of pods it owns started and
// stopped and, with opts.Logs, what its controller logged about it. It only
// reads, and never reads a Secret's data.
func Trace(ctx context.Context, c Clients, o resolve.Object, opts TraceOptions) (*Timeline, error) {
	u, err := getObject(ctx, c, o)
	if err != nil {
		return nil, err
	}
	g, err := loadGraph(ctx, c, o)
	if err != nil {
		return nil, err
	}
	st := readStatus(u)
	gen, seen, _ := st.lag()
	tl := &timeline{Timeline: Timeline{
		Target: o.String(), Namespace: o.Namespace, Generation: gen, ObservedGeneration: seen,
		Entries: []TimelineEntry{}, Notes: []string{NoReconcileHistory},
	}}
	ref := o.String()

	tl.add(u.GetCreationTimestamp().Time, SourceMetadata, ref, "created", BasisObserved)
	if dt := u.GetDeletionTimestamp(); dt != nil {
		msg := "marked for deletion"
		if f := u.GetFinalizers(); len(f) > 0 {
			msg += "; finalizers " + strings.Join(f, ", ") + " keep it"
		}
		tl.add(dt.Time, SourceMetadata, ref, msg, BasisObserved)
	}
	for _, m := range fieldManagers(u) {
		tl.add(m.Time, SourceManagedFields, ref, managerMessage(m), BasisObserved)
	}
	for _, cond := range st.conditions {
		msg := cond.Type + " became " + cond.Status
		if text := conditionText(cond); text != cond.Type+"="+cond.Status {
			msg += ": " + text
		}
		if cond.ObservedGeneration > 0 {
			msg += fmt.Sprintf(" (at generation %d)", cond.ObservedGeneration)
		}
		tl.add(cond.LastTransition, SourceCondition, ref, msg, BasisObserved)
	}

	uids := map[types.UID]bool{u.GetUID(): true}
	pods := map[types.UID]bool{}
	if o.Resource.GroupResource() == (schema.GroupResource{Resource: "pods"}) {
		pods[u.GetUID()] = true
	}
	for _, d := range g.descendants(o, u.GetUID()) {
		uids[d.uid] = true
		child := d.obj.String()
		tl.add(d.created, SourceOwned, child, "created, owned by "+d.owner.String(), BasisObserved)
		if !d.deleting.IsZero() {
			tl.add(d.deleting, SourceOwned, child, "marked for deletion", BasisObserved)
		}
		if d.obj.Resource.GroupResource() == (schema.GroupResource{Resource: "pods"}) {
			pods[d.uid] = true
		}
	}
	if err := tl.containers(ctx, c, o.Namespace, pods); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tl.note("the pods' containers could not be read, so their starts and exits are missing: " + firstLine(err.Error()))
	}
	evs, err := eventsAbout(ctx, c, o.Namespace, uids)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		tl.note("events could not be read, so they are missing: " + firstLine(err.Error()))
	}
	for i := range evs {
		e := &evs[i]
		r := eventRecord(e)
		entry := TimelineEntry{Time: r.FirstSeen, Source: SourceEvent, Object: r.Object, Message: r.Message, Basis: BasisObserved,
			Type: r.Type, Reason: r.Reason, Count: r.Count}
		if r.Count > 1 && r.LastSeen.After(r.FirstSeen) {
			entry.Until = r.LastSeen
		}
		tl.entry(entry)
	}
	if len(g.unlisted) > 0 {
		tl.note(fmt.Sprintf("could not list %s, so anything of those kinds it owns is missing", strings.Join(g.unlisted, ", ")))
	}
	if opts.Logs {
		if err := tl.logs(ctx, c, o, u, opts); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(tl.Entries, func(a, b TimelineEntry) int { return a.Time.Compare(b.Time) })
	return &tl.Timeline, nil
}

// timeline builds a Timeline.
type timeline struct{ Timeline }

func (tl *timeline) entry(e TimelineEntry) {
	if !e.Time.IsZero() {
		tl.Entries = append(tl.Entries, e)
	}
}

func (tl *timeline) add(t time.Time, src Source, object, msg string, b Basis) {
	tl.entry(TimelineEntry{Time: t, Source: src, Object: object, Message: msg, Basis: b})
}

func (tl *timeline) note(s string) { tl.Notes = append(tl.Notes, s) }

// managerMessage says what a field manager wrote: "kustomize-controller
// wrote status (Update)".
func managerMessage(m FieldManager) string {
	what := "fields"
	if len(m.Fields) > 0 {
		what = strings.Join(m.Fields, " and ")
	}
	if m.Subresource != "" && !slices.Contains(m.Fields, m.Subresource) {
		what = m.Subresource
	}
	return fmt.Sprintf("%s last wrote %s (%s)", m.Manager, what, m.Operation)
}

// containers adds when the containers of some pods started and stopped.
func (tl *timeline) containers(ctx context.Context, c Clients, ns string, pods map[types.UID]bool) error {
	if len(pods) == 0 {
		return nil
	}
	list, err := c.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if !pods[p.UID] {
			continue
		}
		ref := "pod/" + p.Name
		statuses := slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses, p.Status.EphemeralContainerStatuses)
		for _, s := range statuses {
			if r := s.State.Running; r != nil {
				msg := "container " + s.Name + " started"
				if s.RestartCount > 0 {
					msg += fmt.Sprintf(" (restart %d)", s.RestartCount)
				}
				tl.add(r.StartedAt.Time, SourceContainer, ref, msg, BasisObserved)
			}
			for _, t := range []*corev1.ContainerStateTerminated{s.State.Terminated, s.LastTerminationState.Terminated} {
				if t == nil {
					continue
				}
				msg := "container " + s.Name + " exited with code " + exitCode(t)
				if m := firstLine(t.Message); m != "" {
					msg += ": " + m
				}
				tl.add(t.FinishedAt.Time, SourceContainer, ref, msg, BasisObserved)
			}
		}
	}
	return nil
}

// logs adds the lines that the controller of an object logged about it.
func (tl *timeline) logs(ctx context.Context, c Clients, o resolve.Object, u *unstructured.Unstructured, opts TraceOptions) error {
	t := opts.Controller
	tl.ControllerBasis = BasisObserved
	if t == nil {
		reporters, err := eventReporters(ctx, c, o, u.GetUID())
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		id := &identifier{c: c, o: o, byKey: map[string]*candidate{}}
		if err := id.identify(ctx, fieldManagers(u), reporters); err != nil {
			return err
		}
		for _, cand := range id.ranked() {
			if cand.workload != nil {
				t = &cand.workload.target
				break
			}
		}
		if t == nil {
			tl.note("tug could not tell which controller reconciles it, so it read no logs; name one with --controller")
			return nil
		}
		tl.ControllerBasis = BasisInferred
	}
	tl.Controller, tl.ControllerNamespace = t.String(), t.Namespace
	var pods []corev1.Pod
	w, err := fetchWorkload(ctx, c.Typed, *t)
	if err == nil {
		pods, err = w.pods(ctx, c.Typed)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tl.note(fmt.Sprintf("the pods of %s could not be listed, so its logs are missing: %s", t, firstLine(err.Error())))
		return nil
	}
	if len(pods) == 0 {
		tl.note(fmt.Sprintf("%s has no pods, so it has no logs to read", t))
		return nil
	}
	since := cmp.Or(opts.Since, recentWindow)
	secs := int64(since.Seconds())
	var found []TimelineEntry
	for i := range pods {
		p := &pods[i]
		container := resolve.DefaultContainer(p)
		rc, err := c.Typed.CoreV1().Pods(t.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{
			Container: container, Timestamps: true, SinceSeconds: &secs,
		}).Stream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			tl.note(fmt.Sprintf("the logs of pod %s could not be read: %s", p.Name, firstLine(err.Error())))
			continue
		}
		err = scanLines(rc, func(line string) {
			ts, text, ok := strings.Cut(line, " ")
			when, perr := time.Parse(time.RFC3339Nano, ts)
			if !ok || perr != nil {
				return
			}
			b, ok := mentions(text, o.Name, u.GetUID())
			if !ok {
				return
			}
			found = append(found, TimelineEntry{Time: when, Source: SourceLog, Object: "pod/" + p.Name, Message: firstLine(text), Basis: b})
		})
		_ = rc.Close()
		if err != nil && !errors.Is(err, context.Canceled) {
			tl.note(fmt.Sprintf("the logs of pod %s were cut short: %s", p.Name, firstLine(err.Error())))
		}
	}
	slices.SortStableFunc(found, func(a, b TimelineEntry) int { return a.Time.Compare(b.Time) })
	if n := len(found) - maxLogEntries; n > 0 {
		tl.note(fmt.Sprintf("%d earlier log lines that mention it are left out; narrow --since to see them", n))
		found = found[n:]
	}
	if len(found) == 0 {
		tl.note(fmt.Sprintf("%s logged nothing that mentions it in the last %s", t, shortDuration(since)))
	}
	tl.Entries = append(tl.Entries, found...)
	return nil
}

// shortDuration writes a duration without its zero parts: "1h", not
// "1h0m0s".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// mentions reports whether a log line is about an object: observed when it
// has the object's UID, inferred when it has only its name, which another
// object may share.
func mentions(line, name string, uid types.UID) (Basis, bool) {
	if uid != "" && strings.Contains(line, string(uid)) {
		return BasisObserved, true
	}
	if containsName(line, name) {
		return BasisInferred, true
	}
	return "", false
}

// containsName reports whether s has name as a whole word, so that "orders"
// is not found in "orders-nightly".
func containsName(s, name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(name)
		if (j == 0 || !isNameChar(lowerASCII(s[j-1]))) && (end == len(s) || !isNameChar(lowerASCII(s[end]))) {
			return true
		}
		i = j + 1
	}
	return false
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// EventRecord is an event about an object or something it owns.
type EventRecord struct {
	// Object is what it is about, such as "pod/api-7d9f-x2x4z".
	Object string `json:"object"`
	// Type is Normal or Warning.
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// Reporter is the controller or component that reported it.
	Reporter string `json:"reporter,omitempty"`
}

func eventRecord(e *corev1.Event) EventRecord {
	return EventRecord{
		Object: ShortKind(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name, Type: e.Type, Reason: e.Reason,
		Message: e.Message, Count: eventCount(e), FirstSeen: firstSeen(e), LastSeen: lastSeen(e),
		Reporter: cmp.Or(e.ReportingController, e.Source.Component),
	}
}

// firstSeen returns when an event first happened, whichever API wrote it.
func firstSeen(e *corev1.Event) time.Time {
	for _, t := range []time.Time{e.FirstTimestamp.Time, e.EventTime.Time, e.LastTimestamp.Time, e.CreationTimestamp.Time} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// EventFeed is the events about an object and everything it owns.
type EventFeed struct {
	// Records are the events there were when it was opened, oldest first by
	// when they last happened.
	Records []EventRecord
	// Unlisted names the resources that could not be listed, whose objects'
	// events may be missing.
	Unlisted []string

	c  Clients
	ns string
	// resourceVersion is where Follow starts.
	resourceVersion string
	resources       map[schema.GroupKind]apiResource
	// known says, by UID, whether an object is the object or beneath it.
	known map[types.UID]bool
	// sent is what was last sent of each event, by the event's UID.
	sent map[types.UID]string
}

// maxOwnerDepth is how far up its owners Follow looks to tell whether a
// new object is beneath the one it follows.
const maxOwnerDepth = 8

// NewEventFeed lists the events about an object and everything it owns, in
// its namespace or, for a cluster-scoped object, in every namespace. It only
// reads.
func NewEventFeed(ctx context.Context, c Clients, o resolve.Object) (*EventFeed, error) {
	u, err := getObject(ctx, c, o)
	if err != nil {
		return nil, err
	}
	g, err := loadGraph(ctx, c, o)
	if err != nil {
		return nil, err
	}
	f := &EventFeed{
		Records: []EventRecord{}, Unlisted: g.unlisted, c: c, ns: o.Namespace,
		resources: g.resources, known: map[types.UID]bool{u.GetUID(): true}, sent: map[types.UID]string{},
	}
	for _, d := range g.descendants(o, u.GetUID()) {
		f.known[d.uid] = true
	}
	list, err := c.Typed.CoreV1().Events(f.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	f.resourceVersion = list.ResourceVersion
	for i := range list.Items {
		e := &list.Items[i]
		if f.known[e.InvolvedObject.UID] {
			r := eventRecord(e)
			f.sent[e.UID] = sentKey(r)
			f.Records = append(f.Records, r)
		}
	}
	slices.SortStableFunc(f.Records, func(a, b EventRecord) int {
		return cmp.Or(a.LastSeen.Compare(b.LastSeen), strings.Compare(a.Object, b.Object), strings.Compare(a.Reason, b.Reason))
	})
	return f, nil
}

func sentKey(r EventRecord) string { return fmt.Sprint(r.Count, " ", r.LastSeen.UnixNano()) }

// Follow watches for new events about the object and everything it owns,
// including what it comes to own after the feed was opened, and sends each
// to sink, from the goroutine that called Follow. An event seen again sends
// again only when it has happened again. It returns nil when ctx ends, as
// following ends on Ctrl-C.
func (f *EventFeed) Follow(ctx context.Context, sink func(EventRecord)) error {
	const pause = time.Second
	rv := f.resourceVersion
	for {
		w, err := f.c.Typed.CoreV1().Events(f.ns).Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("watching events: %w", err)
		}
		rv, err = f.drain(ctx, w, rv, sink)
		w.Stop()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		// The API server ends watches every few minutes; start another.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pause):
		}
	}
}

// drain sends a watch's events until it ends, returning where to watch from
// next: "" to start again from now when the watch fell too far behind.
func (f *EventFeed) drain(ctx context.Context, w watch.Interface, rv string, sink func(EventRecord)) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return rv, nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return rv, nil
			}
			switch ev.Type {
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return "", nil
				}
				return rv, fmt.Errorf("watching events: %w", err)
			case watch.Bookmark:
				if m, err := meta.Accessor(ev.Object); err == nil {
					rv = m.GetResourceVersion()
				}
			case watch.Added, watch.Modified:
				e, ok := ev.Object.(*corev1.Event)
				if !ok {
					continue
				}
				if e.ResourceVersion != "" {
					rv = e.ResourceVersion
				}
				if !f.about(ctx, e.InvolvedObject) {
					continue
				}
				r := eventRecord(e)
				if key := sentKey(r); f.sent[e.UID] != key {
					f.sent[e.UID] = key
					sink(r)
				}
			}
		}
	}
}

// about reports whether an object is the one followed or beneath it,
// looking up the owners of one it has not seen before.
func (f *EventFeed) about(ctx context.Context, ref corev1.ObjectReference) bool {
	if known, ok := f.known[ref.UID]; ok {
		return known
	}
	ok := f.beneath(ctx, ref.APIVersion, ref.Kind, ref.Name, ref.Namespace, maxOwnerDepth)
	if ref.UID != "" {
		f.known[ref.UID] = ok
	}
	return ok
}

// beneath reports whether an object's owners, or theirs in turn, include
// one the feed knows to be the object or beneath it.
func (f *EventFeed) beneath(ctx context.Context, apiVersion, kind, name, ns string, depth int) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	r, ok := f.resources[schema.GroupKind{Group: gv.Group, Kind: kind}]
	if !ok {
		return false
	}
	if !r.namespaced {
		ns = ""
	}
	m, err := f.c.Metadata.Resource(r.resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, owner := range m.OwnerReferences {
		known, seen := f.known[owner.UID]
		if !seen && depth > 0 {
			known = f.beneath(ctx, owner.APIVersion, owner.Kind, owner.Name, m.Namespace, depth-1)
			f.known[owner.UID] = known
		}
		if known {
			return true
		}
	}
	return false
}
