package ops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/paingha/tugcli/internal/resolve"
)

// Clients are what the diagnostics of any object need: the typed client for
// workloads and events, the dynamic client for objects' status, the metadata
// client to find what an object owns without reading any Secret's data, and
// discovery to know which resources the cluster serves.
type Clients struct {
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Metadata  metadata.Interface
	Discovery discovery.DiscoveryInterface
}

// ObjectTree returns the tree beneath any object. A workload or pod gets
// Tree's tree. Beneath any other object is what it owns, found through
// ownerReferences in its namespace or, for a cluster-scoped object, in the
// whole cluster; each workload it owns is shown as Tree shows it. Each
// object's health comes from its status conditions. It also returns the
// resources it could not list, whose objects may be missing. It only reads,
// and never reads a Secret.
func ObjectTree(ctx context.Context, c Clients, o resolve.Object) (*TreeNode, []string, error) {
	if t, ok := o.Workload(); ok {
		n, err := Tree(ctx, c.Typed, t)
		return n, nil, err
	}
	u, err := getObject(ctx, c, o)
	if err != nil {
		return nil, nil, err
	}
	g, err := loadGraph(ctx, c, o)
	if err != nil {
		return nil, nil, err
	}
	a := assess(o, u)
	root := TreeNode{Kind: o.Kind, Name: o.Name, Health: a.health, Status: a.status}
	b := &objectTreeBuilder{c: c, g: g, seen: map[types.UID]bool{u.GetUID(): true}}
	if root.Children, err = b.children(ctx, o, u.GetUID()); err != nil {
		return nil, nil, err
	}
	return &root, g.unlisted, nil
}

// objectTreeBuilder builds the tree of one object, visiting each owned
// object once.
type objectTreeBuilder struct {
	c    Clients
	g    *graph
	seen map[types.UID]bool
}

func (b *objectTreeBuilder) children(ctx context.Context, owner resolve.Object, uid types.UID) ([]TreeNode, error) {
	var out []TreeNode
	for _, m := range b.g.ownedBy(owner, uid, false) {
		if b.seen[m.uid] {
			continue
		}
		b.seen[m.uid] = true
		if t, ok := m.obj.Workload(); ok {
			n, err := Tree(ctx, b.c.Typed, t)
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case apierrors.IsNotFound(err):
				// deleted since it was listed
			case err != nil:
				out = append(out, TreeNode{Kind: m.obj.Kind, Name: m.obj.Name, Health: HealthUnknown, Status: unreadableStatus(err)})
			default:
				out = append(out, *n)
			}
			continue
		}
		n := TreeNode{Kind: m.obj.Kind, Name: m.obj.Name, Health: HealthNone}
		if !statusless(m.obj) {
			u, err := getObject(ctx, b.c, m.obj)
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case apierrors.IsNotFound(err):
				continue
			case err != nil:
				n.Health, n.Status = HealthUnknown, unreadableStatus(err)
			default:
				a := assess(m.obj, u)
				n.Health, n.Status = a.health, a.status
			}
		}
		var err error
		if n.Children, err = b.children(ctx, m.obj, m.uid); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// unreadableStatus says why an object could not be read, for a tree.
func unreadableStatus(err error) string {
	if apierrors.IsForbidden(err) {
		return "not allowed to read it"
	}
	return "could not be read: " + firstLine(err.Error())
}

// WhyObject explains why any object is not ready. A workload or pod gets
// Why's diagnosis. Any other object is judged by its status conditions,
// whether its controller has caught up with its latest generation, the
// health of what it owns (with Why's diagnosis of each workload it owns),
// and recent Warning events about it and what it owns. It only reads, and
// never reads a Secret.
func WhyObject(ctx context.Context, c Clients, o resolve.Object, opts WhyOptions) (*Diagnosis, error) {
	if t, ok := o.Workload(); ok {
		return Why(ctx, c.Typed, t, opts)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	u, err := getObject(ctx, c, o)
	if err != nil {
		return nil, err
	}
	g, err := loadGraph(ctx, c, o)
	if err != nil {
		return nil, err
	}
	st := readStatus(u)
	d := &objectDiagnoser{
		c: c, g: g, o: o, opts: opts, now: now(),
		seen:     map[types.UID]bool{u.GetUID(): true},
		subjects: []member{{obj: o, uid: u.GetUID()}},
		out: &Diagnosis{
			Target: o.String(), Namespace: o.Namespace, Conditions: st.conditions,
			Pods: []PodState{}, Findings: []Finding{}, Events: []Event{},
		},
	}
	d.self(u, st)
	if err := d.walk(ctx, o, u.GetUID()); err != nil {
		return nil, err
	}
	if len(g.unlisted) > 0 {
		d.add(Finding{Severity: SeverityInfo, Objects: []string{o.String()}, Reason: "PartialView",
			Message: fmt.Sprintf("could not list %s, so anything of those kinds it owns is missing", strings.Join(g.unlisted, ", ")),
			Next:    "check what you may list: `kubectl auth can-i --list`"})
	}
	if err := d.events(ctx); err != nil {
		d.add(Finding{Severity: SeverityInfo, Objects: []string{o.String()}, Reason: "EventsUnavailable",
			Message: fmt.Sprintf("events could not be read, so warnings about it may be missing: %v", err)})
	}
	d.finish(assess(o, u))
	return d.out, nil
}

// objectDiagnoser collects the findings of one WhyObject call.
type objectDiagnoser struct {
	c    Clients
	g    *graph
	o    resolve.Object
	opts WhyOptions
	now  time.Time
	seen map[types.UID]bool
	// subjects are the objects whose events count: the object and what it
	// owns, apart from workloads, whose events Why reads.
	subjects []member
	// owned counts what the object owns; notReady, those that are not.
	owned, notReady int
	childEvents     []Event
	out             *Diagnosis
}

func (d *objectDiagnoser) add(f Finding) { d.out.Findings = append(d.out.Findings, f) }

// tug formats a tug command about an object to suggest, in backticks, with
// -n when the object is in another namespace than the session's.
func (d *objectDiagnoser) tug(about resolve.Object, args ...string) string {
	cmd := "tug " + strings.Join(args, " ")
	if about.Namespace != "" && (d.opts.ExplicitNamespace || d.o.Namespace == "") {
		cmd += " -n " + about.Namespace
	}
	return "`" + cmd + "`"
}

// self checks the object's own status.
func (d *objectDiagnoser) self(u *unstructured.Unstructured, st objectStatus) {
	ref := d.o.String()
	describe := "see its status and events: " + d.tug(d.o, "describe", d.o.Ref())
	said := map[string]bool{}
	for _, c := range st.conditions {
		var sev Severity
		switch v := verdict(c); {
		case v == HealthNotReady:
			sev = SeverityError
		case v == HealthUnknown && polarity(c.Type) == busyWhenTrue:
			sev = SeverityInfo
		case v == HealthUnknown:
			sev = SeverityWarning
		default:
			continue
		}
		// Controllers often say the same thing in two conditions, such as
		// Ready=False and Stalled=True.
		key := c.Reason + "\x00" + c.Message
		if c.Message != "" && said[key] {
			continue
		}
		said[key] = true
		d.add(Finding{Severity: sev, Objects: []string{ref}, Reason: conditionReason(c), Message: conditionMessage(c), Next: describe})
	}
	if gen, seen, ok := st.lag(); ok {
		d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "NotReconciled",
			Message: fmt.Sprintf("its controller has not caught up with its latest change: it is at generation %d but was last reconciled at %d, so its conditions may be out of date", gen, seen),
			Next:    "if this lasts, check that the controller that manages it is running, and read its logs"})
	}
	switch d.o.Resource.GroupResource() {
	case cronJobs:
		if suspended, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); suspended {
			d.add(Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Suspended",
				Message: "is suspended, so it starts no jobs", Next: "set spec.suspend to false to resume it"})
		}
	case persistentVolumeClaims:
		if phase, _, _ := unstructured.NestedString(u.Object, "status", "phase"); phase != string(corev1.ClaimBound) {
			d.add(Finding{Severity: SeverityError, Objects: []string{ref}, Reason: cmp.Or(phase, "Pending"),
				Message: "no volume is bound to it", Next: describe})
		}
	default:
		if len(st.conditions) == 0 && st.observed == 0 {
			d.add(Finding{Severity: SeverityInfo, Objects: []string{ref}, Reason: "NoConditions",
				Message: "reports no status conditions, so tug judges it by what it owns and by its events", Next: describe})
		}
	}
}

// walk checks what an object owns, and what that owns in turn. Workloads
// get Why's diagnosis; other objects are judged by their status.
func (d *objectDiagnoser) walk(ctx context.Context, owner resolve.Object, uid types.UID) error {
	for _, m := range d.g.ownedBy(owner, uid, true) {
		if d.seen[m.uid] {
			continue
		}
		d.seen[m.uid] = true
		d.owned++
		ref := m.obj.String()
		if t, ok := m.obj.Workload(); ok {
			opts := d.opts
			opts.ExplicitNamespace = opts.ExplicitNamespace || d.o.Namespace == ""
			wd, err := Why(ctx, d.c.Typed, t, opts)
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case apierrors.IsNotFound(err):
				d.owned--
			case err != nil:
				d.notReady++
				d.add(unreadableFinding(ref, err))
			default:
				if !wd.Healthy {
					d.notReady++
				}
				for _, f := range wd.Findings {
					d.add(f)
				}
				d.out.Pods = append(d.out.Pods, wd.Pods...)
				d.childEvents = append(d.childEvents, wd.Events...)
			}
			continue
		}
		if !statusless(m.obj) {
			u, err := getObject(ctx, d.c, m.obj)
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case apierrors.IsNotFound(err):
				d.owned--
				continue
			case err != nil:
				d.notReady++
				d.add(unreadableFinding(ref, err))
			default:
				d.subjects = append(d.subjects, m)
				d.ownedHealth(m.obj, assess(m.obj, u))
			}
		}
		if err := d.walk(ctx, m.obj, m.uid); err != nil {
			return err
		}
	}
	return nil
}

// ownedHealth turns the health of an object the object owns into a finding.
func (d *objectDiagnoser) ownedHealth(o resolve.Object, a assessment) {
	if a.health == HealthReady || a.health == HealthNone {
		return
	}
	d.notReady++
	f := Finding{Objects: []string{o.String()}, Message: a.status, Next: "see why: " + d.tug(o, "why", o.Ref())}
	switch {
	case a.health == HealthNotReady:
		f.Severity = SeverityError
	case a.busy:
		f.Severity = SeverityInfo
	default:
		f.Severity = SeverityWarning
	}
	f.Reason = "NotReady"
	if a.worst != nil {
		f.Reason, f.Message = conditionReason(*a.worst), conditionMessage(*a.worst)
	} else if a.busy {
		f.Reason = "NotReconciled"
	}
	d.add(f)
}

func unreadableFinding(ref string, err error) Finding {
	return Finding{Severity: SeverityWarning, Objects: []string{ref}, Reason: "Unreadable",
		Message: "it could not be read, so its health is unknown: " + firstLine(err.Error())}
}

// events gathers the recent Warning events about the object and what it
// owns, newest first, with those Why found about the workloads it owns.
func (d *objectDiagnoser) events(ctx context.Context) error {
	var found []Event
	for _, s := range d.subjects {
		ns := s.obj.Namespace
		if ns == "" {
			ns = metav1.NamespaceAll // a cluster-scoped object's events are often in default
		}
		sel := fields.OneTermEqualSelector("involvedObject.uid", string(s.uid)).String()
		list, err := d.c.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
		if err != nil {
			return err
		}
		for _, e := range list.Items {
			if e.InvolvedObject.UID != s.uid || e.Type != corev1.EventTypeWarning || d.now.Sub(lastSeen(&e)) > recentWindow {
				continue
			}
			found = append(found, Event{
				Object: ShortKind(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name, Reason: e.Reason,
				Message: e.Message, Count: eventCount(&e), LastSeen: lastSeen(&e),
			})
		}
	}
	d.out.Events = append(found, d.childEvents...)
	return nil
}

// finish groups the findings, orders the events and decides whether the
// object is healthy.
func (d *objectDiagnoser) finish(a assessment) {
	out := d.out
	out.Findings = groupFindings(out.Findings)
	slices.SortStableFunc(out.Events, func(a, b Event) int { return b.LastSeen.Compare(a.LastSeen) })
	if len(out.Events) > maxEvents {
		out.Events = out.Events[:maxEvents]
	}
	problems := slices.ContainsFunc(out.Findings, func(f Finding) bool { return f.Severity != SeverityInfo })
	out.Healthy = !problems && d.notReady == 0 && (a.health == HealthReady || a.health == HealthNone)
	out.Summary = cmp.Or(a.status, "no status conditions")
	if d.owned > 0 {
		out.Summary += fmt.Sprintf(" (owns %d, %d not ready)", d.owned, d.notReady)
	}
}

// graph indexes objects by the UIDs of their owners.
type graph struct {
	owned map[types.UID][]member
	// unlisted names the resources that could not be listed, whose objects
	// may be missing.
	unlisted []string
}

// member is an object found while listing, with what the graph keeps of it.
type member struct {
	obj     resolve.Object
	uid     types.UID
	created time.Time
}

var (
	cronJobs               = schema.GroupResource{Group: "batch", Resource: "cronjobs"}
	jobs                   = schema.GroupResource{Group: "batch", Resource: "jobs"}
	persistentVolumeClaims = schema.GroupResource{Resource: "persistentvolumeclaims"}
	services               = schema.GroupResource{Resource: "services"}
)

// ownedBy returns what an object owns, by kind and then name. With latest,
// a CronJob owns only its newest Job: older ones are history, and a failure
// among them says nothing about the CronJob now.
func (g *graph) ownedBy(owner resolve.Object, uid types.UID, latest bool) []member {
	ms := g.owned[uid]
	if !latest || owner.Resource.GroupResource() != cronJobs {
		return ms
	}
	var newest *member
	var out []member
	for i, m := range ms {
		if m.obj.Resource.GroupResource() != jobs {
			out = append(out, m)
		} else if newest == nil || m.created.After(newest.created) {
			newest = &ms[i]
		}
	}
	if newest != nil {
		out = append(out, *newest)
	}
	return out
}

// skippedGroups and skippedResources are never searched for what an object
// owns: they are not stored objects, or they are derived by the hundreds
// from objects tug already shows.
var (
	skippedGroups    = []string{"metrics.k8s.io"}
	skippedResources = []schema.GroupResource{
		{Resource: "events"},
		{Group: "events.k8s.io", Resource: "events"},
		{Resource: "endpoints"},
		{Group: "discovery.k8s.io", Resource: "endpointslices"},
	}
)

// listConcurrency bounds how many resources loadGraph lists at once.
const listConcurrency = 8

// loadGraph lists the metadata of everything an object could own: every
// resource in its namespace or, for a cluster-scoped object, in the whole
// cluster. A resource that cannot be listed, for want of permission or
// because its API is down, is noted rather than failing the whole.
func loadGraph(ctx context.Context, c Clients, root resolve.Object) (*graph, error) {
	g := &graph{owned: map[types.UID][]member{}}
	lists, err := discovery.ServerPreferredResourcesWithContext(ctx, discovery.ToDiscoveryInterfaceWithContext(c.Discovery))
	if err != nil {
		var failed *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &failed) {
			return nil, fmt.Errorf("discovering the cluster's resources: %w", err)
		}
		for gv := range failed.Groups {
			g.unlisted = append(g.unlisted, gv.String())
		}
	}
	type source struct {
		resource schema.GroupVersionResource
		kind     string
		ns       string
	}
	var sources []source
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil || slices.Contains(skippedGroups, gv.Group) {
			continue
		}
		for _, r := range l.APIResources {
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || slices.Contains(skippedResources, gr) {
				continue
			}
			if root.Namespace != "" && !r.Namespaced {
				continue // a namespaced object owns only objects in its own namespace
			}
			ns := ""
			if r.Namespaced {
				ns = root.Namespace // every namespace for a cluster-scoped owner
			}
			sources = append(sources, source{gv.WithResource(r.Name), r.Kind, ns})
		}
	}

	var (
		mu   sync.Mutex
		seen = map[types.UID]bool{}
		sem  = make(chan struct{}, listConcurrency)
		wg   sync.WaitGroup
	)
	for _, s := range sources {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			items, err := listMetadata(ctx, c.Metadata, s.resource, s.ns)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				g.unlisted = append(g.unlisted, s.resource.GroupResource().String())
				return
			}
			for _, it := range items {
				if seen[it.UID] {
					continue
				}
				seen[it.UID] = true
				m := member{
					obj:     resolve.Object{Resource: s.resource, Kind: s.kind, Name: it.Name, Namespace: it.Namespace},
					uid:     it.UID,
					created: it.CreationTimestamp.Time,
				}
				for _, ref := range it.OwnerReferences {
					g.owned[ref.UID] = append(g.owned[ref.UID], m)
				}
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(g.unlisted)
	for _, ms := range g.owned {
		slices.SortFunc(ms, func(a, b member) int {
			return cmp.Or(strings.Compare(a.obj.Kind, b.obj.Kind), strings.Compare(a.obj.Name, b.obj.Name))
		})
	}
	return g, nil
}

// listMetadata lists the metadata of a resource's objects, a page at a time.
func listMetadata(ctx context.Context, md metadata.Interface, r schema.GroupVersionResource, ns string) ([]metav1.PartialObjectMetadata, error) {
	var out []metav1.PartialObjectMetadata
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := md.Resource(r).Namespace(ns).List(ctx, opts)
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

func getObject(ctx context.Context, c Clients, o resolve.Object) (*unstructured.Unstructured, error) {
	u, err := c.Dynamic.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting %s: %w", o, err)
	}
	return u, nil
}

// statuslessResources have no status of their own, so tug does not read
// them, which also keeps every Secret's data on the API server.
var statuslessResources = []schema.GroupResource{
	{Resource: "configmaps"},
	{Resource: "secrets"},
	{Resource: "serviceaccounts"},
	{Group: "rbac.authorization.k8s.io", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings"},
	{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"},
	{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
}

func statusless(o resolve.Object) bool {
	return slices.Contains(statuslessResources, o.Resource.GroupResource())
}

// objectStatus is what an object reports about itself.
type objectStatus struct {
	conditions []Condition
	generation int64
	// observed is status.observedGeneration; 0 when the object does not
	// report one.
	observed int64
}

func readStatus(u *unstructured.Unstructured) objectStatus {
	s := objectStatus{generation: u.GetGeneration()}
	s.observed = toInt64(nested(u.Object, "status", "observedGeneration"))
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		c := Condition{
			Type: str(m["type"]), Status: str(m["status"]), Reason: str(m["reason"]), Message: str(m["message"]),
			ObservedGeneration: toInt64(m["observedGeneration"]),
		}
		if c.Type == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, str(m["lastTransitionTime"])); err == nil {
			c.LastTransition = t
		}
		s.conditions = append(s.conditions, c)
	}
	return s
}

// lag reports whether the object's controller has yet to reconcile its
// latest generation, by status.observedGeneration or, without one, by the
// observedGeneration of its Ready condition.
func (s objectStatus) lag() (generation, observed int64, ok bool) {
	observed = s.observed
	if observed == 0 {
		if c := s.summary(); c != nil {
			observed = c.ObservedGeneration
		}
	}
	return s.generation, observed, s.generation > 0 && observed > 0 && observed < s.generation
}

// summaryTypes are the conditions that say whether an object as a whole
// works, in the order they are trusted.
var summaryTypes = []string{"Ready", "Available", "Healthy", "Synced", "Established"}

// summary returns the condition that speaks for the object as a whole, or
// nil.
func (s objectStatus) summary() *Condition {
	for _, t := range summaryTypes {
		for i, c := range s.conditions {
			if c.Type == t {
				return &s.conditions[i]
			}
		}
	}
	return nil
}

// Condition polarities.
const (
	unknownPolarity = iota
	// goodWhenTrue conditions mean the object works when true, as Ready
	// does.
	goodWhenTrue
	// badWhenTrue conditions mean trouble when true, as Stalled does.
	badWhenTrue
	// busyWhenTrue conditions mean work is under way when true, as
	// Reconciling does.
	busyWhenTrue
)

// Condition types by polarity: exact types, or the ends of types, such as
// Cluster API's ControlPlaneReady or a node's MemoryPressure. The bad ones
// are matched first, so NetworkUnavailable is not taken for Available.
var (
	badTypes  = []string{"Stalled", "Degraded", "Failed", "Failure", "Error", "Pressure", "Unavailable", "Unhealthy", "NotReady"}
	goodTypes = []string{"Ready", "Available", "Healthy", "Synced", "Established", "Accepted", "Programmed", "ResolvedRefs", "Progressing", "Succeeded", "Complete", "Initialized"}
	busyTypes = []string{"Reconciling"}
)

func polarity(conditionType string) int {
	matches := func(types []string) bool {
		return slices.ContainsFunc(types, func(t string) bool { return strings.HasSuffix(conditionType, t) })
	}
	switch {
	case matches(badTypes):
		return badWhenTrue
	case matches(goodTypes):
		return goodWhenTrue
	case matches(busyTypes):
		return busyWhenTrue
	default:
		return unknownPolarity
	}
}

// verdict says what a condition means for an object's health: "" when it
// says nothing tug understands.
func verdict(c Condition) Health {
	switch polarity(c.Type) {
	case goodWhenTrue:
		switch c.Status {
		case string(metav1.ConditionTrue):
			return HealthReady
		case string(metav1.ConditionFalse):
			return HealthNotReady
		default:
			return HealthUnknown
		}
	case badWhenTrue:
		if c.Status == string(metav1.ConditionTrue) {
			return HealthNotReady
		}
	case busyWhenTrue:
		if c.Status == string(metav1.ConditionTrue) {
			return HealthUnknown
		}
	}
	return ""
}

// assessment is tug's reading of an object's health.
type assessment struct {
	health Health
	// status says why in a few words.
	status string
	// worst is the condition behind a health other than ready, if one is.
	worst *Condition
	// busy is true when the object is unknown only because its controller
	// is still at work on it.
	busy bool
}

// assess reads an object's health from its status conditions and
// generation, or for a few built-in kinds without conditions, from what they
// have instead.
func assess(o resolve.Object, u *unstructured.Unstructured) assessment {
	switch o.Resource.GroupResource() {
	case persistentVolumeClaims:
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		return assessment{health: healthOf(phase == string(corev1.ClaimBound)), status: cmp.Or(phase, "Pending")}
	case cronJobs:
		schedule, _, _ := unstructured.NestedString(u.Object, "spec", "schedule")
		if suspended, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); suspended {
			return assessment{health: HealthUnknown, status: "suspended, schedule " + schedule}
		}
		return assessment{health: HealthNone, status: "schedule " + schedule}
	case services:
		typ, _, _ := unstructured.NestedString(u.Object, "spec", "type")
		return assessment{health: HealthNone, status: typ}
	}

	st := readStatus(u)
	summary := st.summary()
	find := func(h Health) *Condition {
		if summary != nil && verdict(*summary) == h {
			return summary
		}
		for i, c := range st.conditions {
			if verdict(c) == h {
				return &st.conditions[i]
			}
		}
		return nil
	}
	if c := find(HealthNotReady); c != nil {
		return assessment{health: HealthNotReady, status: conditionText(*c), worst: c}
	}
	if gen, _, ok := st.lag(); ok {
		return assessment{health: HealthUnknown, status: fmt.Sprintf("generation %d not yet reconciled", gen), busy: true}
	}
	if c := find(HealthUnknown); c != nil {
		// Flux, for one, sets Ready to Unknown and Reconciling to True
		// while it works.
		busy := slices.ContainsFunc(st.conditions, func(c Condition) bool {
			return polarity(c.Type) == busyWhenTrue && c.Status == string(metav1.ConditionTrue)
		})
		return assessment{health: HealthUnknown, status: conditionText(*c), worst: c, busy: busy}
	}
	if c := find(HealthReady); c != nil {
		return assessment{health: HealthReady, status: conditionText(*c)}
	}
	if len(st.conditions) > 0 {
		c := st.conditions[0]
		return assessment{health: HealthNone, status: c.Type + "=" + c.Status}
	}
	return assessment{health: HealthNone}
}

// conditionText says what a condition says in a few words, for a tree or a
// summary: "DoesNotExist: Issuer "letsencrypt" not found".
func conditionText(c Condition) string {
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
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

func conditionReason(c Condition) string { return cmp.Or(c.Reason, c.Type) }

// conditionMessage says what a condition says in full: "Ready is False:
// Issuer "letsencrypt" not found".
func conditionMessage(c Condition) string {
	s := c.Type + " is " + c.Status
	if c.Message != "" {
		s += ": " + firstLine(c.Message)
	}
	return s
}

func nested(obj map[string]any, fields ...string) any {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
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
