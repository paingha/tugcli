// Package bundle collects diagnostic evidence about one object, or a whole
// namespace, into a support bundle to share with whoever helps: the objects
// as YAML, tug's diagnosis and tree, events, container logs, the nodes the
// pods run on, the cluster's version and resources, and tug doctor's
// checks, with a manifest of every file and a README that says what was
// redacted. Every file is redacted before the bundle keeps it.
//
// It only reads from the cluster. It copies Secrets from the metadata API,
// which serves no data, so no Secret's data is in a bundle. It returns the
// bundle and never prints; progress goes through a callback.
package bundle

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/doctor"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/version"
)

// Limits that keep a bundle of a large namespace to a size worth sending.
const (
	// maxLogPods is how many pods' logs a bundle holds, those not ready
	// first.
	maxLogPods = 20
	// maxLogBytes caps each log.
	maxLogBytes = 10 << 20
	// maxDiagnosed is how many workloads of a namespace get a diagnosis
	// and a tree.
	maxDiagnosed = 50
	// maxNodes is how many nodes a bundle describes.
	maxNodes = 50

	listConcurrency = 8
	logConcurrency  = 4
)

// Options say what to collect.
type Options struct {
	// Name is the directory the archive's files are under; DefaultName
	// makes one.
	Name string
	// Archive is the archive's file name, for the README: Name and Suffix
	// when it is empty.
	Archive string
	// Cluster says which cluster the bundle is about.
	Cluster ClusterInfo
	// Since is how far back events and current logs go.
	Since time.Duration
	// LogsTail is the most lines kept from the end of each log; 0 leaves
	// logs out.
	LogsTail int64
	// Include, when set, keeps only objects of these kinds; Exclude leaves
	// these kinds out. Each is a kind or resource as kubectl names it,
	// such as deploy or certificates.cert-manager.io. They apply to the
	// objects, events (Event) and nodes (Node), not to logs or diagnoses.
	Include, Exclude []string
	// Redactor redacts every file; nil uses the built-in rules alone.
	Redactor *Redactor
	// Doctor runs tug doctor's checks; nil leaves them out.
	Doctor func(ctx context.Context) (doctor.Report, error)
	// Now defaults to time.Now.
	Now func() time.Time
	// Progress, when set, is told what is being collected, a step at a
	// time.
	Progress func(step string)
}

// ClusterInfo names the cluster a bundle is about, as tug's catalog knows
// it.
type ClusterInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Region   string `json:"region,omitempty"`
}

// File is one file of a bundle.
type File struct {
	// Path is the file's path under the bundle's directory, with forward
	// slashes, as in the archive.
	Path string
	// Source says where its content came from: an API request or a tug
	// command.
	Source string
	Data   []byte
	// Redactions counts the values redacted from it.
	Redactions int
}

// Bundle is a collected support bundle, ready to preview or write.
type Bundle struct {
	// Name is the directory the files are under in the archive.
	Name string
	// Files are README.md, manifest.json and then the rest by path.
	Files []File
	// Problems say what could not be collected, and why.
	Problems []string
	// Created is when the bundle was collected.
	Created time.Time
}

// Size adds up the sizes of the bundle's files, before compression.
func (b *Bundle) Size() int64 {
	var n int64
	for _, f := range b.Files {
		n += int64(len(f.Data))
	}
	return n
}

var (
	namespaces = schema.GroupResource{Resource: "namespaces"}
	secrets    = schema.GroupResource{Resource: "secrets"}
	podKind    = schema.GroupKind{Kind: "Pod"}
	eventKind  = schema.GroupKind{Kind: "Event"}
	nodeKind   = schema.GroupKind{Kind: "Node"}
)

// skipped are resources a namespace bundle does not list: events, which
// events.json holds, and resources derived by the hundreds from objects the
// bundle already has, or that are not stored objects.
var (
	skippedGroups    = []string{"metrics.k8s.io"}
	skippedResources = []schema.GroupResource{
		{Resource: "events"},
		{Group: "events.k8s.io", Resource: "events"},
		{Resource: "endpoints"},
		{Group: "discovery.k8s.io", Resource: "endpointslices"},
	}
)

// Collect gathers a bundle about an object, with what it owns, or, for a
// Namespace, about everything in it. What cannot be read, for want of
// permission or because it has gone, is noted in the bundle's problems
// rather than failing the whole; Collect fails only when the cluster's
// resources cannot be discovered, a kind in Include or Exclude is unknown,
// or ctx ends.
func Collect(ctx context.Context, c ops.Clients, target resolve.Object, opts Options) (*Bundle, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Redactor == nil {
		opts.Redactor = &Redactor{}
	}
	col := &collector{
		c: c, opts: opts, red: opts.Redactor, now: opts.Now(), target: target,
		visited: map[string]bool{}, kept: map[string]bool{},
		uids: map[types.UID]bool{}, refs: map[string]bool{}, namespaces: map[string]bool{},
	}
	if col.opts.Name == "" {
		col.opts.Name = DefaultName(opts.Cluster.Name, target.String(), col.now)
	}
	col.progress("discovering the cluster's resources")
	ix, failed, err := loadIndex(ctx, c.Discovery)
	if err != nil {
		return nil, err
	}
	col.ix = ix
	if len(failed) > 0 {
		col.problems = append(col.problems, "could not discover "+strings.Join(failed, ", ")+", so objects of those groups are missing")
	}
	if col.filter, err = newKindFilter(ix, opts.Include, opts.Exclude); err != nil {
		return nil, err
	}

	steps := []func(context.Context) error{col.cluster}
	if target.Resource.GroupResource() == namespaces {
		steps = append(steps, col.namespace)
	} else {
		steps = append(steps, col.object)
	}
	steps = append(steps, col.events, col.nodes, col.logs, col.doctor)
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	return col.finish()
}

// collector gathers one bundle.
type collector struct {
	c      ops.Clients
	opts   Options
	red    *Redactor
	now    time.Time
	target resolve.Object
	ix     *index
	filter kindFilter

	files    []File
	problems []string
	// visited are the tree nodes already looked up, by kind, namespace
	// and name; kept, the objects already in the bundle.
	visited, kept map[string]bool
	// uids, refs and namespaces are what events are matched against:
	// the objects' UIDs, their kind/namespace/name, and their namespaces.
	uids       map[types.UID]bool
	refs       map[string]bool
	namespaces map[string]bool
	// clusterScoped are the cluster-scoped objects, whose events may be in
	// any namespace.
	clusterScoped []objectRef
	pods          []*corev1.Pod
}

// objectRef names an object as an event's involvedObject does.
type objectRef struct {
	kind, namespace, name string
	uid                   types.UID
}

func (r objectRef) key() string { return r.kind + "/" + r.namespace + "/" + r.name }

func (col *collector) progress(step string) {
	if col.opts.Progress != nil {
		col.opts.Progress(step)
	}
}

// problem notes what could not be collected, unless ctx has ended, in which
// case it returns ctx's error to stop.
func (col *collector) problem(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := what
	if err != nil {
		msg += ": " + firstLine(err.Error())
	}
	msg, _ = col.red.Text(msg)
	col.problems = append(col.problems, msg)
	return nil
}

func (col *collector) add(p, source string, data []byte, redactions int) {
	col.files = append(col.files, File{Path: p, Source: source, Data: data, Redactions: redactions})
}

// addJSON adds a value as indented JSON, redacted.
func (col *collector) addJSON(p, source string, v any) error {
	data, n, err := col.redactedJSON(v)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", p, err)
	}
	col.add(p, source, data, n)
	return nil
}

// addYAML adds a value as YAML, redacted.
func (col *collector) addYAML(p, source string, v any) error {
	data, n, err := col.redactedJSON(v)
	if err == nil {
		data, err = yaml.JSONToYAML(data)
	}
	if err != nil {
		return fmt.Errorf("encoding %s: %w", p, err)
	}
	col.add(p, source, data, n)
	return nil
}

// redactedJSON encodes a value as JSON with its strings redacted. The
// redacted JSON is decoded back into a value of v's type before it is
// encoded, so its fields keep their order, as tug's -o json has them.
func (col *collector) redactedJSON(v any) ([]byte, int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, 0, err
	}
	generic, n := col.red.Value(generic)
	if raw, err = json.Marshal(generic); err != nil {
		return nil, 0, err
	}
	typed := reflect.New(reflect.TypeOf(v))
	if err := json.Unmarshal(raw, typed.Interface()); err != nil {
		return nil, 0, err
	}
	data, err := encodeJSON(typed.Elem().Interface())
	return data, n, err
}

// encodeJSON indents JSON and leaves <, > and & as they are, so that the
// placeholder reads as it does everywhere else.
func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// cluster adds the API server's version and the resources it serves.
func (col *collector) cluster(ctx context.Context) error {
	col.progress("reading the cluster's version")
	if v, err := col.c.Discovery.ServerVersion(); err != nil {
		if err := col.problem(ctx, "could not read the cluster's version", err); err != nil {
			return err
		}
	} else if err := col.addJSON("cluster/version.json", "GET /version", v); err != nil {
		return err
	}
	type apiResource struct {
		Group      string   `json:"group,omitempty"`
		Version    string   `json:"version"`
		Resource   string   `json:"resource"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Names      []string `json:"names"`
	}
	out := make([]apiResource, 0, len(col.ix.resources))
	for _, r := range col.ix.resources {
		out = append(out, apiResource{r.gvr.Group, r.gvr.Version, r.gvr.Resource, r.kind, r.namespaced, r.names})
	}
	return col.addJSON("cluster/api-resources.json", "API discovery (GET /api, /apis)", out)
}

// object collects the target, what it owns, its diagnosis and its tree.
func (col *collector) object(ctx context.Context) error {
	t := col.target
	r, ok := col.ix.find(t.Resource.GroupResource())
	if !ok {
		r = resourceInfo{gvr: t.Resource, kind: t.Kind, namespaced: t.Namespace != ""}
	}
	col.progress("reading " + t.String() + " and what it owns")
	obj, uid, err := col.fetch(ctx, r, t.Namespace, t.Name)
	if err != nil {
		return col.problem(ctx, "could not read "+t.String(), err)
	}
	col.keep(r, t.Namespace, t.Name, uid, obj, col.source(r, t.Namespace, t.Name))
	root, err := col.diagnose(ctx, t)
	if err != nil {
		return err
	}
	if root != nil {
		return col.walk(ctx, parentRef{namespace: t.Namespace, namespaced: r.namespaced, uid: uid, group: r.gvr.Group}, root.Children)
	}
	return nil
}

// diagnose adds an object's diagnosis and tree, and returns the tree.
func (col *collector) diagnose(ctx context.Context, o resolve.Object) (*ops.TreeNode, error) {
	base := strings.ToLower(o.Kind) + "-" + safe(o.Name) + ".json"
	command := func(name string) string {
		cmd := "tug " + name + " " + o.Ref()
		if o.Namespace != "" {
			cmd += " -n " + o.Namespace
		}
		return cmd + " -o json"
	}
	d, err := ops.WhyObject(ctx, col.c, o, ops.WhyOptions{Now: col.opts.Now, ExplicitNamespace: true})
	if err != nil {
		if err := col.problem(ctx, "could not diagnose "+o.String(), err); err != nil {
			return nil, err
		}
	} else if err := col.addJSON("why/"+base, command("why"), d); err != nil {
		return nil, err
	}
	root, unlisted, err := ops.ObjectTree(ctx, col.c, o)
	if err != nil {
		return nil, col.problem(ctx, "could not read the tree of "+o.String(), err)
	}
	if len(unlisted) > 0 {
		if err := col.problem(ctx, fmt.Sprintf("could not list %s, so what %s owns of those kinds is missing", strings.Join(unlisted, ", "), o), nil); err != nil {
			return nil, err
		}
	}
	return root, col.addJSON("tree/"+base, command("tree"), root)
}

// parentRef is the object whose tree children are being looked up.
type parentRef struct {
	namespace  string
	namespaced bool
	uid        types.UID
	group      string
}

// walk collects the objects of a tree beneath a parent.
func (col *collector) walk(ctx context.Context, parent parentRef, nodes []ops.TreeNode) error {
	for _, n := range nodes {
		if n.Kind == ops.KindContainer || n.Kind == ops.KindInitContainer || n.Kind == ops.KindEphemeralContainer {
			continue
		}
		key := n.Kind + "/" + parent.namespace + "/" + n.Name
		if col.visited[key] {
			continue
		}
		col.visited[key] = true
		found, err := col.locate(ctx, parent, n)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if err := col.problem(ctx, fmt.Sprintf("could not read %s/%s", strings.ToLower(n.Kind), n.Name), err); err != nil {
				return err
			}
			continue
		case found == nil:
			continue // gone since the tree was read, or named but missing, as a Secret a pod mounts may be
		}
		col.keep(found.r, found.namespace, n.Name, found.uid, found.obj, col.source(found.r, found.namespace, n.Name))
		next := parentRef{namespace: found.namespace, namespaced: found.r.namespaced, uid: found.uid, group: found.r.gvr.Group}
		if err := col.walk(ctx, next, n.Children); err != nil {
			return err
		}
	}
	return nil
}

// located is a tree node found in the API.
type located struct {
	r         resourceInfo
	namespace string
	obj       map[string]any
	uid       types.UID
}

// locate finds the object a tree node names. A tree has kinds and names
// only: the resource is the one serving the kind (preferring the core
// group, then the parent's), and the namespace is the parent's or, beneath
// a cluster-scoped parent, the one holding an object of that name that the
// parent owns. It returns nil when there is no such object.
func (col *collector) locate(ctx context.Context, parent parentRef, n ops.TreeNode) (*located, error) {
	var firstErr error
	for _, r := range col.ix.ofKind(n.Kind, parent.group) {
		ns := ""
		switch {
		case r.namespaced && parent.namespaced:
			ns = parent.namespace
		case r.namespaced:
			var err error
			if ns, err = col.findNamespace(ctx, r, n.Name, parent.uid); err != nil {
				firstErr = cmp.Or(firstErr, err)
				continue
			}
			if ns == "" {
				continue
			}
		case parent.namespaced:
			continue // a namespaced object owns nothing cluster-scoped
		}
		obj, uid, err := col.fetch(ctx, r, ns, n.Name)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		return &located{r: r, namespace: ns, obj: obj, uid: uid}, nil
	}
	return nil, firstErr
}

// findNamespace returns the namespace of the object of a resource with a
// name that owner owns, or "" when there is none.
func (col *collector) findNamespace(ctx context.Context, r resourceInfo, name string, owner types.UID) (string, error) {
	list, err := col.c.Metadata.Resource(r.gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String(),
	})
	if err != nil {
		return "", err
	}
	var only string
	for _, it := range list.Items {
		if it.Name != name {
			continue
		}
		for _, ref := range it.OwnerReferences {
			if ref.UID == owner {
				return it.Namespace, nil
			}
		}
		only = it.Namespace
	}
	if len(list.Items) == 1 {
		return only, nil
	}
	return "", nil
}

// fetch reads an object. A Secret is read through the metadata API, so the
// read brings none of its data.
func (col *collector) fetch(ctx context.Context, r resourceInfo, ns, name string) (map[string]any, types.UID, error) {
	if r.gvr.GroupResource() == secrets {
		p, err := col.c.Metadata.Resource(r.gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, "", err
		}
		obj, err := secretObject(p)
		return obj, p.UID, err
	}
	u, err := col.c.Dynamic.Resource(r.gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	return u.Object, u.GetUID(), nil
}

// secretObject turns a Secret's metadata into an object with no data.
func secretObject(p *metav1.PartialObjectMetadata) (map[string]any, error) {
	md, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&p.ObjectMeta)
	if err != nil {
		return nil, fmt.Errorf("reading the metadata of secret %s: %w", p.Name, err)
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": md}, nil
}

// secretHeader opens the YAML of every Secret, which has metadata only.
const secretHeader = "# Metadata only: tug copied this Secret from the metadata API, which serves\n# no data, so none of its data is in this bundle.\n"

// keep adds an object to the bundle, redacted, unless it is there already
// or its kind is left out, and remembers what events and logs need of it.
func (col *collector) keep(r resourceInfo, ns, name string, uid types.UID, obj map[string]any, source string) {
	gr := r.gvr.GroupResource()
	key := gr.String() + "/" + ns + "/" + name
	if col.kept[key] {
		return
	}
	col.kept[key] = true
	ref := objectRef{kind: r.kind, namespace: ns, name: name, uid: uid}
	col.uids[uid] = true
	col.refs[ref.key()] = true
	if ns != "" {
		col.namespaces[ns] = true
	} else {
		col.clusterScoped = append(col.clusterScoped, ref)
	}
	if gr == (schema.GroupResource{Resource: "pods"}) {
		var p corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &p); err == nil {
			col.pods = append(col.pods, &p)
		}
	}
	if !col.filter.keeps(r.groupKind()) {
		return
	}
	n := col.red.Object(obj, gr)
	data, err := yaml.Marshal(obj)
	if err != nil {
		col.problems = append(col.problems, fmt.Sprintf("could not encode %s/%s: %v", strings.ToLower(r.kind), name, err))
		return
	}
	if gr == secrets {
		data = append([]byte(secretHeader), data...)
	}
	col.add(objectPath(gr, ns, name), source, data, n)
}

// source says which API request an object came from.
func (col *collector) source(r resourceInfo, ns, name string) string {
	s := "GET " + apiPath(r.gvr, ns, name)
	if r.gvr.GroupResource() == secrets {
		s += " (metadata only)"
	}
	return s
}

// namespace collects a namespace: the Namespace, everything in it, and the
// diagnosis and tree of each of its workloads.
func (col *collector) namespace(ctx context.Context) error {
	ns := col.target.Name
	col.namespaces[ns] = true
	if r, ok := col.ix.find(namespaces); ok {
		obj, uid, err := col.fetch(ctx, r, "", ns)
		if err != nil {
			if err := col.problem(ctx, "could not read namespace "+ns, err); err != nil {
				return err
			}
		} else {
			col.keep(r, "", ns, uid, obj, col.source(r, "", ns))
		}
	}

	var sources []resourceInfo
	for _, r := range col.ix.resources {
		gk := r.groupKind()
		if !r.namespaced || !r.list || slices.Contains(skippedGroups, r.gvr.Group) || slices.Contains(skippedResources, r.gvr.GroupResource()) {
			continue
		}
		// Pods are listed even when left out: their logs and nodes are not.
		if col.filter.keeps(gk) || gk == podKind {
			sources = append(sources, r)
		}
	}
	col.progress(fmt.Sprintf("listing %s in namespace %s", plural(len(sources), "kind"), ns))
	lists := make([][]listedObject, len(sources))
	errs := make([]error, len(sources))
	var wg sync.WaitGroup
	sem := make(chan struct{}, listConcurrency)
	for i, r := range sources {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			lists[i], errs[i] = col.list(ctx, r, ns)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, r := range sources {
		if errs[i] != nil {
			if err := col.problem(ctx, fmt.Sprintf("could not list %s in %s", r.gvr.GroupResource(), ns), errs[i]); err != nil {
				return err
			}
			continue
		}
		for _, o := range lists[i] {
			source := "LIST " + apiPath(r.gvr, ns, "")
			if r.gvr.GroupResource() == secrets {
				source += " (metadata only)"
			}
			col.keep(r, ns, o.name, o.uid, o.obj, source)
		}
	}

	workloads, err := resolve.Workloads(ctx, col.c.Typed, ns, "")
	if err != nil {
		return col.problem(ctx, "could not list the workloads in "+ns, err)
	}
	if len(workloads) > maxDiagnosed {
		if err := col.problem(ctx, fmt.Sprintf("only the first %d of %d workloads were diagnosed", maxDiagnosed, len(workloads)), nil); err != nil {
			return err
		}
		workloads = workloads[:maxDiagnosed]
	}
	col.progress("diagnosing " + plural(len(workloads), "workload"))
	for _, w := range workloads {
		if _, err := col.diagnose(ctx, resolve.ObjectOf(w)); err != nil {
			return err
		}
	}
	return nil
}

// listedObject is one object of a list.
type listedObject struct {
	name string
	uid  types.UID
	obj  map[string]any
}

// list lists a resource's objects in a namespace, a page at a time, Secrets
// as metadata only.
func (col *collector) list(ctx context.Context, r resourceInfo, ns string) ([]listedObject, error) {
	var out []listedObject
	opts := metav1.ListOptions{Limit: 500}
	for {
		var next string
		if r.gvr.GroupResource() == secrets {
			l, err := col.c.Metadata.Resource(r.gvr).Namespace(ns).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for i := range l.Items {
				obj, err := secretObject(&l.Items[i])
				if err != nil {
					return nil, err
				}
				out = append(out, listedObject{l.Items[i].Name, l.Items[i].UID, obj})
			}
			next = l.Continue
		} else {
			l, err := col.c.Dynamic.Resource(r.gvr).Namespace(ns).List(ctx, opts)
			if err != nil {
				return nil, err
			}
			for _, u := range l.Items {
				out = append(out, listedObject{u.GetName(), u.GetUID(), u.Object})
			}
			next = l.GetContinue()
		}
		if next == "" {
			return out, nil
		}
		opts.Continue = next
	}
}

// eventRecord is an event as events.json has it.
type eventRecord struct {
	LastSeen  time.Time `json:"lastSeen"`
	FirstSeen time.Time `json:"firstSeen,omitzero"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Object    string    `json:"object"`
	Namespace string    `json:"namespace,omitempty"`
	Message   string    `json:"message"`
	Count     int32     `json:"count,omitempty"`
	Source    string    `json:"source,omitempty"`
}

// events adds the events of the window about the collected objects, or,
// for a namespace, all of the namespace's, oldest first.
func (col *collector) events(ctx context.Context) error {
	if !col.filter.keeps(eventKind) {
		return nil
	}
	col.progress("reading events")
	whole := col.target.Resource.GroupResource() == namespaces
	cutoff := col.now.Add(-col.opts.Since)
	out := []eventRecord{}
	var sources []string
	seen := map[types.UID]bool{}
	add := func(e *corev1.Event, ref objectRef) {
		if e.UID != "" && seen[e.UID] {
			return
		}
		about := (e.InvolvedObject.UID != "" && col.uids[e.InvolvedObject.UID]) || col.refs[ref.key()]
		if !whole && !about {
			return
		}
		last := lastSeen(e)
		if last.Before(cutoff) {
			return
		}
		seen[e.UID] = true
		out = append(out, eventRecord{
			LastSeen: last, FirstSeen: e.FirstTimestamp.Time, Type: e.Type, Reason: e.Reason,
			Object:    strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name,
			Namespace: e.InvolvedObject.Namespace, Message: e.Message, Count: count(e),
			Source: cmp.Or(e.ReportingController, e.Source.Component),
		})
	}
	for _, ns := range slices.Sorted(maps.Keys(col.namespaces)) {
		items, err := col.listEvents(ctx, ns, "")
		if err != nil {
			if err := col.problem(ctx, "could not list the events in "+ns, err); err != nil {
				return err
			}
			continue
		}
		sources = append(sources, "LIST /api/v1/namespaces/"+ns+"/events")
		for i := range items {
			e := &items[i]
			add(e, objectRef{kind: e.InvolvedObject.Kind, namespace: e.InvolvedObject.Namespace, name: e.InvolvedObject.Name})
		}
	}
	if !whole {
		for _, o := range col.clusterScoped {
			sel := fields.Set{"involvedObject.kind": o.kind, "involvedObject.name": o.name}.AsSelector().String()
			items, err := col.listEvents(ctx, metav1.NamespaceAll, sel)
			if err != nil {
				if err := col.problem(ctx, fmt.Sprintf("could not list the events about %s/%s", strings.ToLower(o.kind), o.name), err); err != nil {
					return err
				}
				continue
			}
			sources = append(sources, "LIST /api/v1/events?fieldSelector="+sel)
			for i := range items {
				e := &items[i]
				add(e, objectRef{kind: e.InvolvedObject.Kind, name: e.InvolvedObject.Name})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b eventRecord) int {
		return cmp.Or(a.LastSeen.Compare(b.LastSeen), strings.Compare(a.Object, b.Object))
	})
	source := strings.Join(sources, "; ")
	if source == "" {
		source = "no events could be listed"
	}
	return col.addJSON("events.json", source, out)
}

// listEvents lists events a page at a time.
func (col *collector) listEvents(ctx context.Context, ns, fieldSelector string) ([]corev1.Event, error) {
	var out []corev1.Event
	opts := metav1.ListOptions{Limit: 500, FieldSelector: fieldSelector}
	for {
		l, err := col.c.Typed.CoreV1().Events(ns).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, l.Items...)
		if l.Continue == "" {
			return out, nil
		}
		opts.Continue = l.Continue
	}
}

// lastSeen is when an event last happened, whichever API wrote it.
func lastSeen(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	default:
		return e.CreationTimestamp.Time
	}
}

func count(e *corev1.Event) int32 {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	return e.Count
}

// nodeSummary is what a bundle keeps of a node: whether it works and what
// it has room for, without its images or its annotations.
type nodeSummary struct {
	Name          string                  `json:"name"`
	Labels        map[string]string       `json:"labels,omitempty"`
	Unschedulable bool                    `json:"unschedulable,omitempty"`
	Taints        []corev1.Taint          `json:"taints,omitempty"`
	Conditions    []corev1.NodeCondition  `json:"conditions"`
	Capacity      corev1.ResourceList     `json:"capacity,omitempty"`
	Allocatable   corev1.ResourceList     `json:"allocatable,omitempty"`
	NodeInfo      corev1.NodeSystemInfo   `json:"nodeInfo"`
	Addresses     []corev1.NodeAddress    `json:"addresses,omitempty"`
	Volumes       []corev1.AttachedVolume `json:"volumesAttached,omitempty"`
}

// nodes adds the conditions of the nodes the collected pods run on.
func (col *collector) nodes(ctx context.Context) error {
	if !col.filter.keeps(nodeKind) {
		return nil
	}
	var names []string
	for _, p := range col.pods {
		if p.Spec.NodeName != "" && !slices.Contains(names, p.Spec.NodeName) {
			names = append(names, p.Spec.NodeName)
		}
	}
	slices.Sort(names)
	if len(names) > maxNodes {
		if err := col.problem(ctx, fmt.Sprintf("only %d of the %d nodes the pods run on are described", maxNodes, len(names)), nil); err != nil {
			return err
		}
		names = names[:maxNodes]
	}
	if len(names) > 0 {
		col.progress("reading " + plural(len(names), "node"))
	}
	for _, name := range names {
		n, err := col.c.Typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if err := col.problem(ctx, "could not read node "+name, err); err != nil {
				return err
			}
			continue
		}
		s := nodeSummary{
			Name: n.Name, Labels: n.Labels, Unschedulable: n.Spec.Unschedulable, Taints: n.Spec.Taints,
			Conditions: n.Status.Conditions, Capacity: n.Status.Capacity, Allocatable: n.Status.Allocatable,
			NodeInfo: n.Status.NodeInfo, Addresses: n.Status.Addresses, Volumes: n.Status.VolumesAttached,
		}
		if err := col.addYAML("nodes/"+safe(name)+".yaml", "GET /api/v1/nodes/"+name+" (conditions, capacity and taints only)", s); err != nil {
			return err
		}
	}
	return nil
}

// logRequest is one log to read.
type logRequest struct {
	pod       *corev1.Pod
	container string
	previous  bool
}

// logs adds the logs of the collected pods' containers: the current run's
// over the window, and the previous run's when a container has restarted.
// Pods that are not ready come first, up to maxLogPods.
func (col *collector) logs(ctx context.Context) error {
	if col.opts.LogsTail == 0 || len(col.pods) == 0 {
		return nil
	}
	pods := slices.Clone(col.pods)
	slices.SortStableFunc(pods, func(a, b *corev1.Pod) int {
		return cmp.Or(cmp.Compare(rankPod(a), rankPod(b)), strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	if len(pods) > maxLogPods {
		if err := col.problem(ctx, fmt.Sprintf("only the logs of %d of %d pods are included, those not ready first", maxLogPods, len(pods)), nil); err != nil {
			return err
		}
		pods = pods[:maxLogPods]
	}
	var reqs []logRequest
	for _, p := range pods {
		statuses := map[string]corev1.ContainerStatus{}
		for _, s := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			statuses[s.Name] = s
		}
		for _, c := range slices.Concat(p.Spec.InitContainers, p.Spec.Containers) {
			s, ok := statuses[c.Name]
			if !ok {
				continue // never started, so it has no log
			}
			if s.State.Running != nil || s.State.Terminated != nil || s.RestartCount > 0 {
				reqs = append(reqs, logRequest{pod: p, container: c.Name})
			}
			if s.RestartCount > 0 || s.LastTerminationState.Terminated != nil {
				reqs = append(reqs, logRequest{pod: p, container: c.Name, previous: true})
			}
		}
	}
	if len(reqs) == 0 {
		return nil
	}
	col.progress(fmt.Sprintf("reading %s from %s", plural(len(reqs), "log"), plural(len(pods), "pod")))
	files := make([]File, len(reqs))
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, logConcurrency)
	for i, req := range reqs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			files[i], errs[i] = col.readLog(ctx, req)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, req := range reqs {
		if errs[i] != nil {
			what := "log"
			if req.previous {
				what = "previous log"
			}
			if err := col.problem(ctx, fmt.Sprintf("could not read the %s of pod %s, container %s", what, req.pod.Name, req.container), errs[i]); err != nil {
				return err
			}
			continue
		}
		col.files = append(col.files, files[i])
	}
	return nil
}

// readLog reads and redacts one log.
func (col *collector) readLog(ctx context.Context, req logRequest) (File, error) {
	tail := col.opts.LogsTail
	limit := int64(maxLogBytes)
	opts := &corev1.PodLogOptions{Container: req.container, Previous: req.previous, Timestamps: true, TailLines: &tail, LimitBytes: &limit}
	query := fmt.Sprintf("container=%s&tailLines=%d&timestamps=true&limitBytes=%d", req.container, tail, limit)
	name := req.container + ".log"
	if req.previous {
		// The previous run may have ended long before the window, and its
		// last lines are what explain why.
		query += "&previous=true"
		name = req.container + ".previous.log"
	} else {
		since := int64(col.opts.Since.Seconds())
		opts.SinceSeconds = &since
		query += fmt.Sprintf("&sinceSeconds=%d", since)
	}
	rc, err := col.c.Typed.CoreV1().Pods(req.pod.Namespace).GetLogs(req.pod.Name, opts).Stream(ctx)
	if err != nil {
		return File{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxLogBytes))
	if err != nil {
		return File{}, err
	}
	text, n := col.red.Text(string(data))
	return File{
		Path:   path.Join("logs", safe(req.pod.Namespace), safe(req.pod.Name), safe(name)),
		Source: "GET /api/v1/namespaces/" + req.pod.Namespace + "/pods/" + req.pod.Name + "/log?" + query,
		Data:   []byte(text), Redactions: n,
	}, nil
}

// rankPod orders pods for their logs: those that are not ready, then those
// that have restarted, then the rest.
func rankPod(p *corev1.Pod) int {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
			return 0
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.RestartCount > 0 {
			return 1
		}
	}
	return 2
}

// doctor adds tug doctor's checks.
func (col *collector) doctor(ctx context.Context) error {
	if col.opts.Doctor == nil {
		return nil
	}
	col.progress("running tug doctor's checks")
	report, err := col.opts.Doctor(ctx)
	if err != nil {
		return col.problem(ctx, "could not run tug doctor's checks", err)
	}
	return col.addJSON("doctor.json", "tug doctor -o json", report)
}

// finish orders the files and adds the README and manifest.
func (col *collector) finish() (*Bundle, error) {
	slices.SortFunc(col.files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	readme, err := col.readme()
	if err != nil {
		return nil, err
	}
	files := append([]File{{Path: "README.md", Source: "tug: what is in this bundle and how to review it", Data: readme}}, col.files...)
	m := manifest{
		Format: manifestFormat, Tug: version.Get(), Created: col.now.UTC(), Cluster: col.opts.Cluster,
		Target: col.target.String(), Namespace: col.target.Namespace, Since: formatDuration(col.opts.Since),
		LogsTail: col.opts.LogsTail, Include: col.opts.Include, Exclude: col.opts.Exclude,
		RedactPatterns: col.red.Patterns(), Files: make([]manifestFile, 0, len(files)), Problems: col.problems,
	}
	for _, f := range files {
		m.Files = append(m.Files, manifestFile{Path: f.Path, Size: len(f.Data), Source: f.Source, Redactions: f.Redactions})
	}
	data, err := encodeJSON(m)
	if err != nil {
		return nil, fmt.Errorf("encoding the manifest: %w", err)
	}
	mf := File{Path: "manifest.json", Source: "tug: every other file, its size and where it came from", Data: data}
	files = slices.Insert(files, 1, mf)
	return &Bundle{Name: col.opts.Name, Files: files, Problems: col.problems, Created: col.now}, nil
}

// manifestFormat versions manifest.json for tools that read it.
const manifestFormat = "tug.support-bundle/v1"

// manifest is manifest.json: what the bundle is about, how it was
// collected, and every other file in it.
type manifest struct {
	Format    string       `json:"format"`
	Tug       version.Info `json:"tug"`
	Created   time.Time    `json:"created"`
	Cluster   ClusterInfo  `json:"cluster"`
	Target    string       `json:"target"`
	Namespace string       `json:"namespace,omitempty"`
	Since     string       `json:"since"`
	LogsTail  int64        `json:"logsTail"`
	Include   []string     `json:"include,omitempty"`
	Exclude   []string     `json:"exclude,omitempty"`
	// RedactPatterns counts the --redact-pattern patterns, which are not
	// recorded: they may spell out what they hide.
	RedactPatterns int            `json:"redactPatterns"`
	Files          []manifestFile `json:"files"`
	Problems       []string       `json:"problems,omitempty"`
}

type manifestFile struct {
	Path       string `json:"path"`
	Size       int    `json:"size"`
	Source     string `json:"source"`
	Redactions int    `json:"redactions"`
}

// apiPath is the API server's path for a resource, or one of its objects.
func apiPath(gvr schema.GroupVersionResource, ns, name string) string {
	p := "/apis/" + gvr.Group + "/" + gvr.Version
	if gvr.Group == "" {
		p = "/api/" + gvr.Version
	}
	if ns != "" {
		p += "/namespaces/" + ns
	}
	p += "/" + gvr.Resource
	if name != "" {
		p += "/" + name
	}
	return p
}

// objectPath is where an object's YAML goes in a bundle:
// objects/<namespace>/<resource>/<name>.yaml, with _cluster for a
// cluster-scoped object, which no namespace can be named. Archive paths
// always use forward slashes, so this is package path, not path/filepath.
func objectPath(gr schema.GroupResource, ns, name string) string {
	scope := cmp.Or(ns, "_cluster")
	return path.Join("objects", safe(scope), safe(gr.String()), safe(name)+".yaml")
}

// safe makes a name safe as one part of a path on every platform: RBAC
// names may hold colons, which Windows refuses.
func safe(name string) string {
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	s := string(b)
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// plural counts things: "1 pod", "3 pods".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
