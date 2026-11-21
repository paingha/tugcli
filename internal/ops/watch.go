package ops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/paingha/tugcli/internal/resolve"
)

// ChangeType says what changed about an object that Watch follows.
type ChangeType string

// Change types.
const (
	// ChangeState is how an object was when the watch began: the watched
	// object, and each object beneath it that is not ready.
	ChangeState ChangeType = "state"
	// ChangeAdded is an object that appeared beneath the watched object, or
	// the watched object created again after it was deleted.
	ChangeAdded ChangeType = "added"
	// ChangeReady and ChangeNotReady are an object becoming ready, or no
	// longer being ready, for an object judged by its rollout or its
	// status rather than by its conditions.
	ChangeReady    ChangeType = "ready"
	ChangeNotReady ChangeType = "not-ready"
	// ChangeCondition is a status condition that appeared or changed its
	// status or reason.
	ChangeCondition ChangeType = "condition"
	// ChangeGeneration is a new generation of the object's spec, or its
	// controller catching up with one.
	ChangeGeneration ChangeType = "generation"
	// ChangeRollout is a workload's replica counts changing.
	ChangeRollout ChangeType = "rollout"
	// ChangeStatus is a pod's status, as kubectl's STATUS column shows it,
	// or the short status of an object without conditions, changing.
	ChangeStatus ChangeType = "status"
	// ChangeRestart is a container that restarted.
	ChangeRestart ChangeType = "restart"
	// ChangeScheduling is a pod that no node can take, or that a node took
	// after none could.
	ChangeScheduling ChangeType = "scheduling"
	// ChangeEvent is a new Warning event about the object or what it owns.
	ChangeEvent       ChangeType = "event"
	ChangeTerminating ChangeType = "terminating"
	ChangeDeleted     ChangeType = "deleted"
)

// Change is one meaningful change to the watched object or to something it
// owns.
type Change struct {
	Time time.Time `json:"time"`
	// Object is what changed, as tree names it: "deploy/api", "pod/api-7d9f-x2x4z".
	Object    string `json:"object"`
	Namespace string `json:"namespace,omitempty"`
	// Root is true when Object is the watched object itself.
	Root bool       `json:"root,omitempty"`
	Type ChangeType `json:"type"`
	// Health is the object's health after the change.
	Health Health `json:"health"`
	// Reason is the cause in a word, when there is one: a condition's or
	// an event's reason, or why a container stopped.
	Reason string `json:"reason,omitempty"`
	// Message says what changed, such as "Ready False → True (Serving)".
	Message string `json:"message"`
	// Condition is the condition's type, for a condition change.
	Condition string `json:"condition,omitempty"`
	// From and To are the value before and after, when one changed: a
	// condition's status, a generation, a status or a restart count.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Until is what Watch waits for before it returns.
type Until string

// What Watch can wait for.
const (
	// UntilStopped watches until the context ends.
	UntilStopped Until = ""
	// UntilReady waits for the object to be ready: a workload rolled out,
	// a job complete, a pod ready, or any other object ready by its
	// conditions; an object that reports no health of its own waits for
	// what it owns.
	UntilReady Until = "ready"
	// UntilDeleted waits for the object to be gone.
	UntilDeleted Until = "deleted"
)

// ErrStalled is returned when Watch waits for an object to be ready that
// cannot become ready as it is, such as a failed job.
var ErrStalled = errors.New("it will not become ready without a change")

// WatchOptions tune Watch.
type WatchOptions struct {
	// Events reports new Warning events about the object and what it owns.
	Events bool
	Until  Until
	// Now stamps each change; it defaults to time.Now.
	Now func() time.Time
}

// WatchSink receives what Watch finds. Watch calls it from one goroutine
// at a time.
type WatchSink struct {
	Change func(Change)
	// Error receives problems that do not end the watch, such as a kind
	// of object it could not watch, whose changes are then missing. It may
	// be nil.
	Error func(error)
}

// Watch follows an object and everything it owns, as tree finds it, and
// reports each meaningful change through sink: condition transitions, new
// generations and the controller catching up with them, replica counts,
// pods' status, restarts and scheduling failures, objects appearing,
// becoming ready or not ready, terminating and being deleted and, with
// opts.Events, new Warning events. It begins with the object's state.
//
// It lists and then watches each kind of object involved, and lists again
// when a watch expires. For objects other than workloads it searches the
// owner graph again from time to time for kinds of object it does not yet
// watch. It returns nil once opts.Until is reached, ctx's error when ctx
// ends first, and an error when it cannot watch the object at all. It only
// reads, and never reads a Secret's data.
func Watch(ctx context.Context, c Clients, o resolve.Object, opts WatchOptions, sink WatchSink) error {
	return newWatcher(c, o, opts, sink).run(ctx)
}

// Timing of the search for kinds of object not yet watched, and of retries.
const (
	discoverEvery = 30 * time.Second
	discoverSoon  = 5 * time.Second
	firstRetry    = time.Second
	lastRetry     = 30 * time.Second
	// maxHeldEvents bounds the events kept about objects not yet seen,
	// which a watch of another kind may still bring.
	maxHeldEvents = 100
)

// ownedKind is a kind of object that a kind of controller owns.
type ownedKind struct {
	resource schema.GroupVersionResource
	kind     string
}

var (
	podResource        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	replicaSetResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	jobResource        = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

	// knownOwned are what built-in controllers own, watched from the
	// start without searching the owner graph.
	knownOwned = map[schema.GroupResource][]ownedKind{
		{Group: "apps", Resource: "deployments"}:  {{replicaSetResource, "ReplicaSet"}, {podResource, "Pod"}},
		{Group: "apps", Resource: "replicasets"}:  {{podResource, "Pod"}},
		{Group: "apps", Resource: "statefulsets"}: {{podResource, "Pod"}},
		{Group: "apps", Resource: "daemonsets"}:   {{podResource, "Pod"}},
		jobs:                                      {{podResource, "Pod"}},
		cronJobs:                                  {{jobResource, "Job"}, {podResource, "Pod"}},
	}
)

// watcher is the state of one Watch call. Its maps belong to the goroutine
// running run; streams only send it messages.
type watcher struct {
	c    Clients
	root resolve.Object
	opts WatchOptions
	sink WatchSink
	now  func() time.Time

	// discover is true when the owner graph is searched for kinds to
	// watch, which workloads do not need.
	discover                    bool
	discoverEvery, discoverSoon time.Duration
	firstRetry                  time.Duration

	wg     sync.WaitGroup
	msgs   chan streamMsg
	graphs chan graphResult

	streams map[streamKey]streamSpec
	// pending are the streams whose first list has not arrived; the
	// watch begins once none are and the first search of the graph is
	// done.
	pending      map[streamKey]bool
	graphPending bool
	begun        bool
	lastErr      map[streamKey]string

	// rootUID is the watched object's, or "" while it does not exist.
	rootUID    types.UID
	rootListed bool
	objects    map[types.UID]*entry
	// children index every object seen by the UIDs of its owners, whether
	// or not they are watched, as objects arrive in any order.
	children map[types.UID]map[types.UID]bool
	byStream map[streamKey]map[types.UID]bool

	eventCounts map[types.UID]int32
	held        []*corev1.Event

	discovering bool
	timer       *time.Timer
	next        time.Time
}

func newWatcher(c Clients, o resolve.Object, opts WatchOptions, sink WatchSink) *watcher {
	_, workload := o.Workload()
	w := &watcher{
		c: c, root: o, opts: opts, sink: sink, now: opts.Now,
		discover: !workload, discoverEvery: discoverEvery, discoverSoon: discoverSoon, firstRetry: firstRetry,
		msgs: make(chan streamMsg, 64), graphs: make(chan graphResult),
		streams: map[streamKey]streamSpec{}, pending: map[streamKey]bool{}, lastErr: map[streamKey]string{},
		objects: map[types.UID]*entry{}, children: map[types.UID]map[types.UID]bool{},
		byStream: map[streamKey]map[types.UID]bool{}, eventCounts: map[types.UID]int32{},
	}
	w.graphPending = w.discover
	if opts.Now == nil {
		w.now = time.Now
	}
	return w
}

// entry is an object a stream has seen.
type entry struct {
	uid    types.UID
	key    streamKey
	ref    resolve.Object
	owners []types.UID
	obj    runtime.Object
	// member is true for the watched object and what it owns, directly
	// or through other members; only members are reported.
	member bool
	root   bool
	snap   snapshot
}

func (w *watcher) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer w.wg.Wait()
	defer cancel()
	defer func() {
		if w.timer != nil {
			w.timer.Stop()
		}
	}()

	w.startStream(ctx, w.rootSpec())
	for _, k := range knownOwned[w.root.Resource.GroupResource()] {
		w.watchKind(ctx, k.resource, k.kind)
	}
	if w.opts.Events {
		w.startStream(ctx, w.eventsSpec())
	}
	for {
		var tick <-chan time.Time
		if w.timer != nil {
			tick = w.timer.C
		}
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-w.msgs:
			err = w.handle(ctx, m)
		case r := <-w.graphs:
			err = w.handleGraph(ctx, r)
		case <-tick:
			w.next = time.Time{}
			w.discoverNow(ctx)
		}
		if err != nil {
			return err
		}
		if done, err := w.reached(); done || err != nil {
			return err
		}
	}
}

// reached reports whether what Watch waits for has happened, or an error
// when it never will.
func (w *watcher) reached() (bool, error) {
	switch w.opts.Until {
	case UntilDeleted:
		return w.rootListed && w.rootUID == "", nil
	case UntilReady:
		if !w.begun {
			return false, nil
		}
		root := w.objects[w.rootUID]
		if root == nil {
			return false, fmt.Errorf("%s was deleted before it was ready", w.root)
		}
		if root.snap.stalled != "" {
			return false, fmt.Errorf("%s %s; %w", w.root, root.snap.stalled, ErrStalled)
		}
		if !root.snap.ready || root.snap.terminating {
			return false, nil
		}
		if root.snap.health == HealthNone {
			for _, e := range w.objects {
				if e.member && unhealthy(e.snap.health) {
					return false, nil
				}
			}
		}
		return true, nil
	default:
		return false, nil
	}
}

func unhealthy(h Health) bool { return h == HealthNotReady || h == HealthUnknown }

// handle applies what a stream found.
func (w *watcher) handle(ctx context.Context, m streamMsg) error {
	switch {
	case m.err != nil:
		return w.streamFailed(m)
	case m.sync:
		first := w.pending[m.key]
		delete(w.pending, m.key)
		delete(w.lastErr, m.key)
		w.sync(m.key, m.objects)
		if m.key == w.rootKey() && !w.rootListed {
			w.rootListed = true
			if w.rootUID == "" && w.opts.Until != UntilDeleted {
				return fmt.Errorf("getting %s: %w", w.root, apierrors.NewNotFound(w.root.Resource.GroupResource(), w.root.Name))
			}
			if w.rootUID != "" && w.discover {
				w.discoverNow(ctx)
			}
		}
		if first {
			w.maybeBegin()
		}
	default:
		w.apply(m.key, m.event.Type, m.event.Object)
	}
	return nil
}

// streamFailed reports a stream's error. Only the watched object's own
// stream failing for good ends the watch.
func (w *watcher) streamFailed(m streamMsg) error {
	what := "events"
	if !m.key.events {
		what = m.key.resource.GroupResource().String()
	}
	if m.key == w.rootKey() {
		if m.final {
			return fmt.Errorf("watching %s: %w", w.root, m.err)
		}
		what = w.root.String()
	}
	if msg := m.err.Error(); w.lastErr[m.key] != msg {
		w.lastErr[m.key] = msg
		if m.final {
			w.problem(fmt.Errorf("could not watch %s, so changes to them are missing: %w", what, m.err))
		} else {
			w.problem(fmt.Errorf("watching %s: %w; trying again", what, m.err))
		}
	}
	// A stream that cannot list does not hold up the start; what it finds
	// later is reported as it arrives.
	if w.pending[m.key] && m.key != w.rootKey() {
		delete(w.pending, m.key)
		w.maybeBegin()
	}
	return nil
}

func (w *watcher) problem(err error) {
	if w.sink.Error != nil {
		w.sink.Error(err)
	}
}

// maybeBegin starts reporting once the first lists are in, with the state
// of the object and of what it owns that is not ready.
func (w *watcher) maybeBegin() {
	if w.begun || len(w.pending) > 0 || w.graphPending || w.rootUID == "" {
		return
	}
	w.begun = true
	root := w.objects[w.rootUID]
	var owned, notReady int
	var trouble []*entry
	for _, e := range sorted(w.objects) {
		if !e.member || e.root {
			continue
		}
		owned++
		if unhealthy(e.snap.health) {
			notReady++
			trouble = append(trouble, e)
		}
	}
	msg := cmp.Or(root.snap.status, "no status")
	if owned > 0 {
		msg += fmt.Sprintf(" (owns %d, %d not ready)", owned, notReady)
	}
	w.emit(root, Change{Type: ChangeState, Message: msg})
	for _, e := range trouble {
		w.emit(e, Change{Type: ChangeState, Message: cmp.Or(e.snap.status, string(e.snap.health))})
	}
	w.schedule(w.discoverEvery)
}

// sorted returns entries by kind and name, so that what is reported
// together comes in a stable order.
func sorted(m map[types.UID]*entry) []*entry {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b *entry) int {
		return cmp.Or(strings.Compare(a.ref.Kind, b.ref.Kind), strings.Compare(a.ref.Namespace, b.ref.Namespace),
			strings.Compare(a.ref.Name, b.ref.Name))
	})
	return out
}

// sync applies a stream's full list: what has gone since the last list,
// then what is new or changed, so that an object deleted and created again
// meanwhile reads in that order.
func (w *watcher) sync(key streamKey, items []runtime.Object) {
	if key.events {
		w.syncEvents(items)
		return
	}
	seen := map[types.UID]bool{}
	for _, obj := range items {
		if m, err := meta.Accessor(obj); err == nil && w.wanted(key, m) {
			seen[m.GetUID()] = true
		}
	}
	gone := map[types.UID]*entry{}
	for uid := range w.byStream[key] {
		if !seen[uid] {
			gone[uid] = w.objects[uid]
		}
	}
	for _, e := range sorted(gone) {
		w.remove(e.uid)
	}
	for _, obj := range items {
		w.upsert(key, obj)
	}
}

// apply applies one watch event.
func (w *watcher) apply(key streamKey, typ watch.EventType, obj runtime.Object) {
	if key.events {
		w.event(typ, obj)
		return
	}
	switch typ {
	case watch.Added, watch.Modified:
		w.upsert(key, obj)
	case watch.Deleted:
		if m, err := meta.Accessor(obj); err == nil && w.wanted(key, m) {
			w.remove(m.GetUID())
		}
	}
}

// wanted reports whether a stream's object is one the watcher follows: the
// root stream's field selector may not have been applied.
func (w *watcher) wanted(key streamKey, m metav1.Object) bool {
	return key.name == "" || m.GetName() == key.name && m.GetNamespace() == w.root.Namespace
}

// upsert records an object that is new or has changed, and reports what
// changed if it is a member.
func (w *watcher) upsert(key streamKey, obj runtime.Object) {
	m, err := meta.Accessor(obj)
	if err != nil || !w.wanted(key, m) {
		return
	}
	uid := m.GetUID()
	e := w.objects[uid]
	if e == nil {
		e = &entry{uid: uid, key: key, ref: resolve.Object{
			Resource: key.resource, Kind: w.streams[key].kind, Name: m.GetName(), Namespace: m.GetNamespace(),
		}}
		w.objects[uid] = e
		addTo(w.byStream, key, uid)
	}
	e.obj = obj
	owners := ownerUIDs(m)
	if !slices.Equal(owners, e.owners) {
		for _, o := range e.owners {
			delete(w.children[o], uid)
		}
		for _, o := range owners {
			addTo(w.children, o, uid)
		}
		e.owners = owners
	}
	if key.name != "" && uid != w.rootUID {
		w.rootUID, e.root = uid, true
	}
	switch {
	case e.member:
		w.update(e)
	case e.root || w.ownedByMember(e):
		w.join(e)
	}
}

func addTo[K comparable](m map[K]map[types.UID]bool, k K, uid types.UID) {
	if m[k] == nil {
		m[k] = map[types.UID]bool{}
	}
	m[k][uid] = true
}

func ownerUIDs(m metav1.Object) []types.UID {
	var out []types.UID
	for _, ref := range m.GetOwnerReferences() {
		out = append(out, ref.UID)
	}
	return out
}

func (w *watcher) ownedByMember(e *entry) bool {
	return slices.ContainsFunc(e.owners, func(uid types.UID) bool {
		o := w.objects[uid]
		return o != nil && o.member
	})
}

// join makes an object a member, and with it what it owns.
func (w *watcher) join(e *entry) {
	e.member = true
	e.snap = snapshotOf(e.ref, e.obj)
	if w.begun {
		w.emit(e, Change{Type: ChangeAdded, Message: withStatus("appeared", e.snap.status), To: e.snap.status})
		if e.snap.unschedulable != "" {
			w.emit(e, unschedulable(e.snap.unschedulable))
		}
		w.release(e)
		if e.root {
			w.soon()
		}
	}
	owned := map[types.UID]*entry{}
	for uid := range w.children[e.uid] {
		if c := w.objects[uid]; c != nil && !c.member {
			owned[uid] = c
		}
	}
	for _, c := range sorted(owned) {
		w.join(c)
	}
}

func withStatus(s, status string) string {
	if status == "" {
		return s
	}
	return s + ": " + status
}

// update reports what changed about a member.
func (w *watcher) update(e *entry) {
	cur := snapshotOf(e.ref, e.obj)
	changes := diff(e.snap, cur)
	e.snap = cur
	if !w.begun {
		return
	}
	for _, c := range changes {
		w.emit(e, c)
	}
	if e.root && len(changes) > 0 {
		w.soon()
	}
}

// remove forgets an object that is gone. What it owns stays watched until
// the garbage collector deletes it too.
func (w *watcher) remove(uid types.UID) {
	e := w.objects[uid]
	if e == nil {
		return
	}
	delete(w.objects, uid)
	delete(w.byStream[e.key], uid)
	delete(w.children, uid)
	for _, o := range e.owners {
		delete(w.children[o], uid)
	}
	if uid == w.rootUID {
		w.rootUID = "" // unless a list has already brought it back, created again
	}
	if e.member && w.begun {
		w.emit(e, Change{Type: ChangeDeleted, Message: "was deleted"})
	}
}

func (w *watcher) emit(e *entry, c Change) {
	c.Time = w.now()
	c.Object = ShortKind(e.ref.Kind) + "/" + e.ref.Name
	c.Namespace = e.ref.Namespace
	c.Root = e.root
	c.Health = e.snap.health
	w.sink.Change(c)
}

// syncEvents applies a full list of events.
func (w *watcher) syncEvents(items []runtime.Object) {
	seen := map[types.UID]bool{}
	for _, obj := range items {
		if ev, ok := obj.(*corev1.Event); ok {
			seen[ev.UID] = true
			w.event(watch.Modified, ev)
		}
	}
	for uid := range w.eventCounts {
		if !seen[uid] {
			delete(w.eventCounts, uid)
		}
	}
}

// event reports a Warning event that is new, or has happened again, since
// the watch began.
func (w *watcher) event(typ watch.EventType, obj runtime.Object) {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	if typ == watch.Deleted {
		delete(w.eventCounts, ev.UID)
		return
	}
	n := eventCount(ev)
	prev, known := w.eventCounts[ev.UID]
	w.eventCounts[ev.UID] = n
	if !w.begun || known && n <= prev || ev.Type != corev1.EventTypeWarning {
		return
	}
	if e := w.objects[ev.InvolvedObject.UID]; e != nil && e.member {
		w.emitEvent(e, ev)
		return
	}
	// Another kind's watch may yet bring the object, as a pod's events
	// can arrive before the pod.
	w.held = append(w.held, ev)
	if len(w.held) > maxHeldEvents {
		w.held = slices.Delete(w.held, 0, len(w.held)-maxHeldEvents)
	}
}

// release reports the held events about an object that has just joined.
func (w *watcher) release(e *entry) {
	w.held = slices.DeleteFunc(w.held, func(ev *corev1.Event) bool {
		if ev.InvolvedObject.UID != e.uid {
			return false
		}
		w.emitEvent(e, ev)
		return true
	})
}

func (w *watcher) emitEvent(e *entry, ev *corev1.Event) {
	msg := ev.Reason + ": " + firstLine(ev.Message)
	if n := eventCount(ev); n > 1 {
		msg += fmt.Sprintf(" (x%d)", n)
	}
	w.emit(e, Change{Type: ChangeEvent, Reason: ev.Reason, Message: msg})
}

// graphResult is a search of the owner graph.
type graphResult struct {
	g *graph
	// root is the UID of the watched object when the search began.
	root types.UID
	err  error
}

// discoverNow searches the owner graph for kinds of object the watched
// object owns that are not yet watched, unless a search is under way.
func (w *watcher) discoverNow(ctx context.Context) {
	if !w.discover || w.discovering || w.rootUID == "" {
		return
	}
	w.discovering = true
	c, root, uid, out := w.c, w.root, w.rootUID, w.graphs
	w.wg.Go(func() {
		g, err := loadGraph(ctx, c, root)
		select {
		case out <- graphResult{g: g, root: uid, err: err}:
		case <-ctx.Done():
		}
	})
}

// handleGraph starts watching the kinds of object a search found.
func (w *watcher) handleGraph(ctx context.Context, r graphResult) error {
	w.discovering = false
	first := w.graphPending
	if r.err != nil {
		if first {
			return fmt.Errorf("finding what %s owns: %w", w.root, r.err)
		}
		w.problem(fmt.Errorf("searching again for what %s owns: %w", w.root, r.err))
		w.schedule(w.discoverEvery)
		return nil
	}
	if first && len(r.g.unlisted) > 0 {
		w.problem(fmt.Errorf("could not list %s, so anything of those kinds it owns is missing", strings.Join(r.g.unlisted, ", ")))
	}
	kinds := ownedKinds(r.g, w.root, r.root)
	for _, res := range slices.SortedFunc(maps.Keys(kinds), func(a, b schema.GroupVersionResource) int {
		return strings.Compare(a.String(), b.String())
	}) {
		w.watchKind(ctx, res, kinds[res])
	}
	w.graphPending = false
	if first {
		w.maybeBegin()
	} else {
		w.schedule(w.discoverEvery)
	}
	return nil
}

// ownedKinds returns the kinds of everything an object owns, directly or
// through what it owns.
func ownedKinds(g *graph, root resolve.Object, uid types.UID) map[schema.GroupVersionResource]string {
	kinds := map[schema.GroupVersionResource]string{}
	seen := map[types.UID]bool{uid: true}
	queue := []member{{obj: root, uid: uid}}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		for _, c := range g.ownedBy(m.obj, m.uid, false) {
			if !seen[c.uid] {
				seen[c.uid] = true
				kinds[c.obj.Resource] = c.obj.Kind
				queue = append(queue, c)
			}
		}
	}
	return kinds
}

// schedule asks for a search of the owner graph after d, unless one is
// already due sooner.
func (w *watcher) schedule(d time.Duration) {
	if !w.discover || !w.begun {
		return
	}
	at := time.Now().Add(d)
	if !w.next.IsZero() && w.next.Before(at) {
		return
	}
	w.next = at
	if w.timer == nil {
		w.timer = time.NewTimer(d)
		return
	}
	w.timer.Reset(d)
}

// soon asks for a search shortly after the watched object changes, since
// its controller may have created objects of new kinds.
func (w *watcher) soon() { w.schedule(w.discoverSoon) }

// streamKey names a stream: a resource's objects, the watched object alone
// by name, or Warning events.
type streamKey struct {
	resource schema.GroupVersionResource
	name     string
	events   bool
}

func (w *watcher) rootKey() streamKey { return streamKey{resource: w.root.Resource, name: w.root.Name} }

// streamSpec says how to list and watch a stream.
type streamSpec struct {
	key  streamKey
	kind string
	lw   listWatch
	opts metav1.ListOptions
}

type listWatch struct {
	list  func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error)
	watch func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// objectsListWatch lists and watches a resource's objects: their metadata
// only for kinds without a status, which keeps every Secret's data on the
// API server.
func (w *watcher) objectsListWatch(r schema.GroupVersionResource, ns string) listWatch {
	if statusless(resolve.Object{Resource: r}) {
		ri := w.c.Metadata.Resource(r).Namespace(ns)
		return listWatch{
			list:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return ri.List(ctx, o) },
			watch: ri.Watch,
		}
	}
	ri := w.c.Dynamic.Resource(r).Namespace(ns)
	return listWatch{
		list:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return ri.List(ctx, o) },
		watch: ri.Watch,
	}
}

func (w *watcher) rootSpec() streamSpec {
	return streamSpec{
		key: w.rootKey(), kind: w.root.Kind,
		lw:   w.objectsListWatch(w.root.Resource, w.root.Namespace),
		opts: metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", w.root.Name).String()},
	}
}

func (w *watcher) eventsSpec() streamSpec {
	// A cluster-scoped object's events are often in default, so look in
	// every namespace.
	ev := w.c.Typed.CoreV1().Events(w.root.Namespace)
	return streamSpec{
		key: streamKey{events: true}, kind: "Event",
		lw: listWatch{
			list:  func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return ev.List(ctx, o) },
			watch: ev.Watch,
		},
		opts: metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("type", corev1.EventTypeWarning).String()},
	}
}

// watchKind starts watching a kind of object the watched object owns, in
// its namespace or, for a cluster-scoped object, everywhere.
func (w *watcher) watchKind(ctx context.Context, r schema.GroupVersionResource, kind string) {
	key := streamKey{resource: r}
	if _, ok := w.streams[key]; ok {
		return
	}
	w.startStream(ctx, streamSpec{key: key, kind: kind, lw: w.objectsListWatch(r, w.root.Namespace)})
}

func (w *watcher) startStream(ctx context.Context, s streamSpec) {
	w.streams[s.key] = s
	if !w.begun {
		w.pending[s.key] = true
	}
	first, msgs := w.firstRetry, w.msgs
	w.wg.Go(func() { listAndWatch(ctx, s, first, msgs) })
}

// streamMsg is what a stream tells the watcher: a full list, one watch
// event, or an error.
type streamMsg struct {
	key     streamKey
	sync    bool
	objects []runtime.Object
	event   watch.Event
	err     error
	// final is true when the stream has stopped for good.
	final bool
}

// listAndWatch lists a stream's objects, then watches them from the
// version listed, resuming where a watch that ends left off and listing
// again when the version has expired (410 Gone). It sends what it finds to
// out until ctx ends, or until an error that retrying does not mend.
func listAndWatch(ctx context.Context, s streamSpec, first time.Duration, out chan<- streamMsg) {
	send := func(m streamMsg) bool {
		select {
		case out <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
	retry := backoff{first: first}
	failed := func(err error) bool {
		if ctx.Err() != nil {
			return false
		}
		final := permanent(err)
		if !send(streamMsg{key: s.key, err: err, final: final}) || final {
			return false
		}
		return retry.wait(ctx)
	}
	var rv string
	relist := true
	for ctx.Err() == nil {
		if relist {
			items, version, err := listAll(ctx, s.lw, s.opts)
			if err != nil {
				if !failed(err) {
					return
				}
				continue
			}
			if !send(streamMsg{key: s.key, sync: true, objects: items}) {
				return
			}
			rv, relist = version, false
			retry.reset()
		}
		opts := s.opts
		opts.ResourceVersion, opts.AllowWatchBookmarks = rv, true
		wi, err := s.lw.watch(ctx, opts)
		switch {
		case expired(err):
			relist = true
			continue
		case err != nil:
			if !failed(err) {
				return
			}
			continue
		}
		began := time.Now()
		var events int
		rv, relist, events, err = forward(ctx, s.key, wi, rv, send)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !failed(err) {
				return
			}
			continue
		}
		switch {
		case events > 0:
			retry.reset()
		case !relist && time.Since(began) < time.Second:
			// A server that ends each watch at once is not asked again
			// straight away.
			if !retry.wait(ctx) {
				return
			}
		}
	}
}

// forward passes a watch's events on until the watch ends. It returns the
// resource version to resume from, whether to list again, how many events
// it passed on, and the error the watch ended with other than expiry.
func forward(ctx context.Context, key streamKey, wi watch.Interface, rv string, send func(streamMsg) bool) (string, bool, int, error) {
	defer wi.Stop()
	var n int
	for {
		select {
		case <-ctx.Done():
			return rv, false, n, nil
		case ev, ok := <-wi.ResultChan():
			if !ok {
				return rv, false, n, nil
			}
			switch ev.Type {
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				if expired(err) {
					return rv, true, n, nil
				}
				return rv, true, n, err
			case watch.Bookmark:
			default:
				if !send(streamMsg{key: key, event: ev}) {
					return rv, false, n, nil
				}
				n++
			}
			if m, err := meta.Accessor(ev.Object); err == nil && m.GetResourceVersion() != "" {
				rv = m.GetResourceVersion()
			}
		}
	}
}

// listAll lists every object of a stream, a page at a time, and returns
// them with the resource version to watch from.
func listAll(ctx context.Context, lw listWatch, base metav1.ListOptions) ([]runtime.Object, string, error) {
	opts := base
	opts.Limit = 500
	var items []runtime.Object
	for {
		list, err := lw.list(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		objs, err := meta.ExtractList(list)
		if err != nil {
			return nil, "", err
		}
		items = append(items, objs...)
		lm, err := meta.ListAccessor(list)
		if err != nil {
			return nil, "", err
		}
		if lm.GetContinue() == "" {
			return items, lm.GetResourceVersion(), nil
		}
		opts.Continue = lm.GetContinue()
	}
}

// expired reports whether a watch's resource version is too old to resume
// from.
func expired(err error) bool {
	return err != nil && (apierrors.IsResourceExpired(err) || apierrors.IsGone(err))
}

// permanent reports whether retrying would not mend an error.
func permanent(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) ||
		apierrors.IsMethodNotSupported(err) || apierrors.IsBadRequest(err)
}

// backoff doubles the wait between retries, up to lastRetry.
type backoff struct {
	first, d time.Duration
}

func (b *backoff) reset() { b.d = 0 }

// wait waits before the next try, and reports false if ctx ends first.
func (b *backoff) wait(ctx context.Context) bool {
	b.d = min(max(2*b.d, b.first), lastRetry)
	t := time.NewTimer(b.d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// judge is how an object's health is judged.
type judge int

const (
	// judgedByNothing objects report no health, as a Secret does.
	judgedByNothing judge = iota
	judgedByConditions
	// judgedByStatus objects have a short status instead of conditions,
	// as a PersistentVolumeClaim has its phase.
	judgedByStatus
	judgedByRollout
	judgedByPod
)

// snapshot is what Watch compares of an object from one version to the
// next.
type snapshot struct {
	judge  judge
	health Health
	status string
	// quiet objects do not report changes of their status or generation,
	// only of their health, as a ReplicaSet's counts follow its pods' and
	// its generation every scaling.
	quiet      bool
	conditions []Condition
	generation int64
	// observed is the generation the controller last reconciled; 0 when
	// the object does not say.
	observed    int64
	terminating bool
	// ready is what UntilReady waits for.
	ready bool
	// stalled says why the object cannot become ready as it is.
	stalled string

	// For pods.
	restarts      map[string]restartState
	node          string
	unschedulable string
}

type restartState struct {
	count int32
	last  *corev1.ContainerStateTerminated
}

// snapshotOf reads what Watch compares from an object: a pod by its status,
// a workload by its rollout, as tree and why judge them, and any other
// object by its conditions, as assess does.
func snapshotOf(o resolve.Object, obj runtime.Object) snapshot {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok { // metadata only
		s := snapshot{health: HealthNone, ready: true}
		if m, err := meta.Accessor(obj); err == nil {
			s.terminating = m.GetDeletionTimestamp() != nil
		}
		return s
	}
	s := snapshot{terminating: u.GetDeletionTimestamp() != nil, generation: u.GetGeneration()}
	switch gr := o.Resource.GroupResource(); {
	case gr == podResource.GroupResource():
		if p, ok := fromUnstructured[corev1.Pod](u); ok {
			podSnapshot(&s, p)
			return s
		}
	case gr == replicaSetResource.GroupResource():
		if rs, ok := fromUnstructured[appsv1.ReplicaSet](u); ok {
			desired := replicas(rs.Spec.Replicas)
			s.judge, s.quiet = judgedByRollout, true
			s.health = healthOf(rs.Status.ReadyReplicas >= desired)
			s.status = fmt.Sprintf("%d/%d ready, revision %d", rs.Status.ReadyReplicas, desired, revision(&rs.ObjectMeta))
			s.ready = s.health == HealthReady
			s.conditions, s.observed = readStatus(u).conditions, rs.Status.ObservedGeneration
			return s
		}
	default:
		if t, ok := o.Workload(); ok {
			if wl, ok := workloadOf(t, u); ok {
				workloadSnapshot(&s, wl, u)
				return s
			}
		}
	}
	a, st := assess(o, u), readStatus(u)
	s.health, s.status, s.conditions = a.health, a.status, st.conditions
	_, s.observed, _ = st.lag()
	s.judge = judgedByStatus
	if len(st.conditions) > 0 {
		s.judge = judgedByConditions
	}
	s.ready = a.health == HealthReady || a.health == HealthNone
	return s
}

func fromUnstructured[T any](u *unstructured.Unstructured) (*T, bool) {
	var t T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &t); err != nil {
		return nil, false
	}
	return &t, true
}

// workloadOf returns the workload behind a deployment, statefulset,
// daemonset or job.
func workloadOf(t resolve.Target, u *unstructured.Unstructured) (*workload, bool) {
	w := &workload{target: t}
	var ok bool
	switch t.Kind {
	case resolve.KindDeployment:
		w.deploy, ok = fromUnstructured[appsv1.Deployment](u)
	case resolve.KindStatefulSet:
		w.sts, ok = fromUnstructured[appsv1.StatefulSet](u)
	case resolve.KindDaemonSet:
		w.ds, ok = fromUnstructured[appsv1.DaemonSet](u)
	case resolve.KindJob:
		w.job, ok = fromUnstructured[batchv1.Job](u)
	}
	return w, ok
}

func workloadSnapshot(s *snapshot, w *workload, u *unstructured.Unstructured) {
	r, n := w.rollout(), workloadNode(w)
	st := readStatus(u)
	s.judge, s.health, s.status = judgedByRollout, n.Health, n.Status
	s.conditions, s.observed = st.conditions, st.observed
	s.ready = r.Complete
	switch {
	case w.job != nil:
		if c := jobCondition(w.job, batchv1.JobFailed); c != nil {
			s.stalled = withStatus("has failed", c.Reason)
		}
	case w.deploy != nil:
		for _, c := range w.deploy.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
				s.stalled = "has exceeded its progress deadline"
			}
		}
	}
}

func podSnapshot(s *snapshot, p *corev1.Pod) {
	s.judge, s.generation = judgedByPod, 0
	s.ready = podHealthy(p)
	s.health, s.status = healthOf(s.ready), podStatus(p)
	s.node = p.Spec.NodeName
	s.restarts = map[string]restartState{}
	for _, st := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		s.restarts[st.Name] = restartState{count: st.RestartCount, last: st.LastTerminationState.Terminated}
	}
	if p.Spec.NodeName == "" {
		if c := podCondition(p, corev1.PodScheduled); c != nil && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			msg, _, _ := strings.Cut(c.Message, " preemption:")
			s.unschedulable = strings.TrimSuffix(strings.TrimSpace(msg), ".")
		}
	}
	if p.Status.Phase == corev1.PodFailed {
		s.stalled = withStatus("has failed", p.Status.Reason)
	}
}

func unschedulable(msg string) Change {
	return Change{Type: ChangeScheduling, Reason: "FailedScheduling", Message: "no node can run it: " + msg}
}

// diff says what changed between two snapshots of an object, in the order
// a person reads them: the spec, the conditions, scheduling, restarts, and
// last the status or health that follows from those.
func diff(old, cur snapshot) []Change {
	var out []Change
	if cur.terminating && !old.terminating {
		// Its status and health now only say that it is going.
		return []Change{{Type: ChangeTerminating, Message: "is terminating"}}
	}

	// A new generation says why the health changed, until the controller
	// has caught up with it.
	generation := false
	lagging := cur.observed > 0 && cur.observed < cur.generation
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	switch {
	case cur.judge == judgedByPod || cur.quiet:
	case old.generation > 0 && cur.generation > old.generation:
		msg := fmt.Sprintf("generation %d → %d", old.generation, cur.generation)
		switch {
		case lagging:
			msg += ", not yet reconciled"
		case cur.observed >= cur.generation:
			msg += ", reconciled"
		}
		out = append(out, Change{Type: ChangeGeneration, Message: msg, From: itoa(old.generation), To: itoa(cur.generation)})
		generation = true
	case cur.generation > 0 && cur.observed >= cur.generation && old.observed < cur.generation:
		out = append(out, Change{Type: ChangeGeneration, Message: fmt.Sprintf("generation %d reconciled", cur.generation),
			From: itoa(old.observed), To: itoa(cur.observed)})
	}

	if cur.judge != judgedByPod {
		for _, c := range cur.conditions {
			if ch, ok := conditionChange(old.conditions, c); ok {
				out = append(out, ch)
			}
		}
	}

	if cur.unschedulable != "" && cur.unschedulable != old.unschedulable {
		out = append(out, unschedulable(cur.unschedulable))
	}
	if old.node == "" && cur.node != "" && old.unschedulable != "" {
		out = append(out, Change{Type: ChangeScheduling, Reason: "Scheduled", Message: "scheduled on node " + cur.node})
	}

	for _, name := range slices.Sorted(maps.Keys(cur.restarts)) {
		r, was := cur.restarts[name], old.restarts[name].count
		if r.count <= was {
			continue
		}
		ch := Change{Type: ChangeRestart, Message: fmt.Sprintf("container %s restarted (%s)", name, plural(r.count, "restart")),
			From: strconv.Itoa(int(was)), To: strconv.Itoa(int(r.count))}
		if r.last != nil {
			ch.Reason = terminatedReason(r.last)
			ch.Message += ", last exit code " + exitCode(r.last)
		}
		out = append(out, ch)
	}

	// An object judged by its conditions says why its health changed in
	// them.
	flipped := cur.health != old.health && (cur.health == HealthReady || old.health == HealthReady)
	if flipped && !generation && cur.judge != judgedByConditions && cur.judge != judgedByNothing {
		ch := Change{Type: ChangeNotReady, Message: withStatus("is no longer ready", cur.status), From: old.status, To: cur.status}
		if cur.health == HealthReady {
			ch.Type, ch.Message = ChangeReady, withStatus("became ready", cur.status)
		}
		if cur.judge == judgedByPod {
			ch.Reason = cur.status
		}
		return append(out, ch)
	}
	if cur.status != old.status && !cur.quiet && (cur.judge == judgedByRollout || cur.judge == judgedByPod || cur.judge == judgedByStatus) {
		ch := Change{Type: ChangeStatus, Message: cmp.Or(old.status, "-") + " → " + cur.status, From: old.status, To: cur.status}
		switch cur.judge {
		case judgedByRollout:
			ch.Type = ChangeRollout
		case judgedByPod:
			ch.Reason = cur.status
		}
		out = append(out, ch)
	}
	return out
}

// conditionChange reports a condition that is new, or whose status or
// reason changed. A message changing alone is not reported: controllers
// put times and counts in theirs.
func conditionChange(old []Condition, c Condition) (Change, bool) {
	ch := Change{Type: ChangeCondition, Condition: c.Type, Reason: c.Reason, To: c.Status}
	detail := ""
	if c.Reason != "" || c.Message != "" {
		detail = " (" + conditionText(c) + ")"
	}
	i := slices.IndexFunc(old, func(o Condition) bool { return o.Type == c.Type })
	switch {
	case i < 0:
		ch.Message = c.Type + " is " + c.Status + detail
	case old[i].Status != c.Status:
		ch.From = old[i].Status
		ch.Message = c.Type + " " + old[i].Status + " → " + c.Status + detail
	case old[i].Reason != c.Reason:
		ch.From = old[i].Status
		ch.Message = fmt.Sprintf("%s is %s (%s → %s)", c.Type, c.Status, cmp.Or(old[i].Reason, "no reason"), cmp.Or(c.Reason, "no reason"))
	default:
		return Change{}, false
	}
	return ch, true
}
