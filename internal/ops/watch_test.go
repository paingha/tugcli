package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/resolve"
)

// watchResources is what the watch tests' fake cluster serves.
var watchResources = []*metav1.APIResourceList{
	{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		{Name: "events", Kind: "Event", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
	}},
	{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
		{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
	}},
	{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
		{Name: "jobs", Kind: "Job", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
	}},
	{GroupVersion: "db.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "databases", Kind: "Database", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
		{Name: "backups", Kind: "Backup", Namespaced: true, Verbs: []string{"get", "list", "watch"}},
	}},
}

var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	backupsGVR     = schema.GroupVersionResource{Group: "db.example.com", Version: "v1", Resource: "backups"}
	// watchListKinds are what the fake dynamic client lists. Secrets are
	// missing on purpose: listing them through it would panic, and watch
	// must never read them.
	watchListKinds = map[schema.GroupVersionResource]string{
		podResource: "PodList", replicaSetResource: "ReplicaSetList", jobResource: "JobList",
		deploymentsGVR: "DeploymentList", databases: "DatabaseList", backupsGVR: "BackupList",
	}
)

// watchCluster is a fake cluster whose watches the test drives: each
// call to Watch gets a new fake watcher, which the test takes with watcher.
type watchCluster struct {
	Clients
	typed *fake.Clientset
	dyn   *dynamicfake.FakeDynamicClient
	md    *metadatafake.FakeMetadataClient

	mu       sync.Mutex
	watchers map[string]chan *watch.FakeWatcher
	// failWatch are errors the next calls to Watch for a resource return.
	failWatch map[string][]error
}

// newWatchCluster serves Events through the typed client, Secrets through
// the metadata client only, and everything else through the dynamic and
// metadata clients.
func newWatchCluster(t *testing.T, objs ...runtime.Object) *watchCluster {
	t.Helper()
	var typed, dyn, md []runtime.Object
	for _, o := range objs {
		if _, ok := o.(*corev1.Event); ok {
			typed = append(typed, o)
			continue
		}
		u := toUnstructured(t, o)
		if u.GetKind() != "Secret" {
			dyn = append(dyn, u)
		}
		md = append(md, partial(u))
	}
	sch := metadatafake.NewTestScheme()
	for _, l := range watchResources {
		gv, _ := schema.ParseGroupVersion(l.GroupVersion)
		for _, r := range l.APIResources {
			sch.AddKnownTypeWithName(gv.WithKind(r.Kind), &metav1.PartialObjectMetadata{})
		}
	}
	c := &watchCluster{
		typed:     fake.NewSimpleClientset(typed...),
		dyn:       dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), watchListKinds, dyn...),
		md:        metadatafake.NewSimpleMetadataClient(sch, md...),
		watchers:  map[string]chan *watch.FakeWatcher{},
		failWatch: map[string][]error{},
	}
	c.typed.Resources = watchResources
	c.typed.PrependWatchReactor("*", c.react)
	c.dyn.PrependWatchReactor("*", c.react)
	c.md.PrependWatchReactor("*", c.react)
	c.Clients = Clients{Typed: c.typed, Dynamic: c.dyn, Metadata: c.md, Discovery: c.typed.Discovery()}
	return c
}

func (c *watchCluster) react(a k8stesting.Action) (bool, watch.Interface, error) {
	r := a.GetResource().Resource
	c.mu.Lock()
	defer c.mu.Unlock()
	if errs := c.failWatch[r]; len(errs) > 0 {
		c.failWatch[r] = errs[1:]
		return true, nil, errs[0]
	}
	fw := watch.NewFakeWithChanSize(32, false)
	c.chanFor(r) <- fw
	return true, fw, nil
}

func (c *watchCluster) chanFor(resource string) chan *watch.FakeWatcher {
	if c.watchers[resource] == nil {
		c.watchers[resource] = make(chan *watch.FakeWatcher, 8)
	}
	return c.watchers[resource]
}

// watcher returns the next watch of a resource that the code under test
// opens.
func (c *watchCluster) watcher(t *testing.T, resource string) *watch.FakeWatcher {
	t.Helper()
	c.mu.Lock()
	ch := c.chanFor(resource)
	c.mu.Unlock()
	select {
	case fw := <-ch:
		return fw
	case <-time.After(5 * time.Second):
		t.Fatalf("nothing watched %s", resource)
		return nil
	}
}

func (c *watchCluster) failNextWatch(resource string, errs ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failWatch[resource] = append(c.failWatch[resource], errs...)
}

// count counts the actions of a verb on a resource through a client.
func count(actions []k8stesting.Action, verb, resource string) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func toUnstructured(t *testing.T, o runtime.Object) *unstructured.Unstructured {
	t.Helper()
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u
	}
	gvks, _, err := scheme.Scheme.ObjectKinds(o)
	if err != nil {
		t.Fatal(err)
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(gvks[0])
	return u
}

// watching is a Watch call running in the background.
type watching struct {
	changes chan Change
	errs    chan error
	cancel  context.CancelFunc
	done    chan error
	result  *error
}

func startWatch(t *testing.T, c Clients, o resolve.Object, opts WatchOptions) *watching {
	t.Helper()
	return startWatcher(t, newWatcher(c, o, opts, WatchSink{}))
}

// startWatcher runs a watcher whose timings a test may have changed.
func startWatcher(t *testing.T, w *watcher) *watching {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &watching{changes: make(chan Change, 100), errs: make(chan error, 100), cancel: cancel, done: make(chan error, 1)}
	w.sink = WatchSink{Change: func(c Change) { r.changes <- c }, Error: func(err error) { r.errs <- err }}
	go func() { r.done <- w.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		r.wait(t)
	})
	return r
}

// wait returns what Watch returned.
func (r *watching) wait(t *testing.T) error {
	t.Helper()
	if r.result != nil {
		return *r.result
	}
	select {
	case err := <-r.done:
		r.result = &err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return")
		return nil
	}
}

func describe(c Change) string {
	return fmt.Sprintf("%s %s [%s] %s", c.Type, c.Object, c.Health, c.Message)
}

// expect checks the next changes, each as "type object [health] message".
func (r *watching) expect(t *testing.T, want ...string) {
	t.Helper()
	for i, w := range want {
		select {
		case c := <-r.changes:
			if got := describe(c); got != w {
				t.Errorf("change %d = %q\nwant         %q", i, got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no change %d; want %q", i, w)
		}
	}
}

// quiet checks that nothing more has been reported.
func (r *watching) quiet(t *testing.T) {
	t.Helper()
	select {
	case c := <-r.changes:
		t.Errorf("unexpected change %q", describe(c))
	case err := <-r.errs:
		t.Errorf("unexpected problem: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func controller(kind, name string, uid types.UID) []metav1.OwnerReference {
	yes := true
	group := "apps/v1"
	if kind == "Job" || kind == "CronJob" {
		group = "batch/v1"
	}
	return []metav1.OwnerReference{{APIVersion: group, Kind: kind, Name: name, UID: uid, Controller: &yes}}
}

func rollingDeployment(gen, observed int64, total, updated, ready int32) *appsv1.Deployment {
	two := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", UID: "deploy-api", Generation: gen},
		Spec:       appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: observed, Replicas: total, UpdatedReplicas: updated,
			ReadyReplicas: ready, AvailableReplicas: ready},
	}
}

func apiReplicaSet(name, revision string, desired, ready int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", UID: types.UID("rs-" + name),
			Annotations:     map[string]string{revisionAnnotation: revision},
			OwnerReferences: controller("Deployment", "api", "deploy-api"),
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &desired},
		Status: appsv1.ReplicaSetStatus{Replicas: ready, ReadyReplicas: ready},
	}
}

// readyPod is a running pod with one ready container, owned by a
// ReplicaSet.
func readyPod(name, rs string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID("pod-" + name),
			Labels: map[string]string{"app": "api"}, OwnerReferences: controller("ReplicaSet", rs, types.UID("rs-"+rs))},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}
}

func TestWatchRollout(t *testing.T) {
	c := newWatchCluster(t,
		rollingDeployment(1, 1, 2, 2, 2),
		apiReplicaSet("api-old", "1", 2, 2),
		readyPod("api-old-a", "api-old"), readyPod("api-old-b", "api-old"),
		readyPod("other", "someone-else"),
	)
	api := resolve.ObjectOf(resolve.Target{Kind: resolve.KindDeployment, Name: "api", Namespace: "shop"})
	w := startWatch(t, c.Clients, api, WatchOptions{Events: true})
	w.expect(t, "state deploy/api [ready] 2/2 ready (owns 3, 0 not ready)")
	deploys, sets, pods, events := c.watcher(t, "deployments"), c.watcher(t, "replicasets"), c.watcher(t, "pods"), c.watcher(t, "events")

	steps := []struct {
		name string
		send func()
		want []string
	}{
		{
			name: "the spec changes",
			send: func() { deploys.Modify(toUnstructured(t, rollingDeployment(2, 1, 2, 2, 2))) },
			want: []string{"generation deploy/api [not-ready] generation 1 → 2, not yet reconciled"},
		},
		{
			name: "a new ReplicaSet appears",
			send: func() { sets.Add(toUnstructured(t, apiReplicaSet("api-new", "2", 1, 0))) },
			want: []string{"added rs/api-new [not-ready] appeared: 0/1 ready, revision 2"},
		},
		{
			name: "the controller catches up",
			send: func() { deploys.Modify(toUnstructured(t, rollingDeployment(2, 2, 2, 0, 2))) },
			want: []string{
				"generation deploy/api [not-ready] generation 2 reconciled",
				"rollout deploy/api [not-ready] 2/2 ready → 2/2 ready, 0/2 updated, 2 old",
			},
		},
		{
			name: "a new pod no node can take",
			send: func() {
				p := readyPod("api-new-a", "api-new")
				p.Spec.NodeName = ""
				p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
					Message: "0/3 nodes are available: 3 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
				}}}
				pods.Add(toUnstructured(t, p))
			},
			want: []string{
				"added pod/api-new-a [not-ready] appeared: Pending",
				"scheduling pod/api-new-a [not-ready] no node can run it: 0/3 nodes are available: 3 Insufficient cpu",
			},
		},
		{
			name: "a node takes it",
			send: func() {
				p := readyPod("api-new-a", "api-new")
				p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "main",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}
				pods.Modify(toUnstructured(t, p))
			},
			want: []string{
				"scheduling pod/api-new-a [not-ready] scheduled on node node-a",
				"status pod/api-new-a [not-ready] Pending → ContainerCreating",
			},
		},
		{
			name: "it becomes ready",
			send: func() { pods.Modify(toUnstructured(t, readyPod("api-new-a", "api-new"))) },
			want: []string{"ready pod/api-new-a [ready] became ready: Running"},
		},
		{
			name: "it runs out of memory",
			send: func() {
				p := readyPod("api-new-a", "api-new")
				p.Status.Conditions[1].Status = corev1.ConditionFalse
				st := &p.Status.ContainerStatuses[0]
				st.Ready, st.RestartCount = false, 1
				st.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}
				pods.Modify(toUnstructured(t, p))
			},
			want: []string{
				"restart pod/api-new-a [not-ready] container main restarted (1 restart), last exit code 137 (OOMKilled)",
				"not-ready pod/api-new-a [not-ready] is no longer ready: Running",
			},
		},
		{
			name: "a warning about a pod that has not arrived yet",
			send: func() {
				events.Add(&corev1.Event{
					ObjectMeta:     metav1.ObjectMeta{Name: "api-new-b.1", Namespace: "shop", UID: "event-1"},
					InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-new-b", Namespace: "shop", UID: "pod-api-new-b"},
					Type:           corev1.EventTypeWarning, Reason: "FailedMount", Message: "secret \"tls\" not found\nmore", Count: 3,
				})
				events.Add(&corev1.Event{
					ObjectMeta:     metav1.ObjectMeta{Name: "other.1", Namespace: "shop", UID: "event-2"},
					InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "other", UID: "pod-other"},
					Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "not ours",
				})
				p := readyPod("api-new-b", "api-new")
				p.Status = corev1.PodStatus{Phase: corev1.PodPending}
				pods.Add(toUnstructured(t, p))
			},
			want: []string{
				"added pod/api-new-b [not-ready] appeared: Pending",
				"event pod/api-new-b [not-ready] FailedMount: secret \"tls\" not found (x3)",
			},
		},
		{
			name: "an old pod goes",
			send: func() {
				p := readyPod("api-old-a", "api-old")
				p.DeletionTimestamp = &metav1.Time{Time: whyNow}
				pods.Modify(toUnstructured(t, p))
				pods.Delete(toUnstructured(t, p))
			},
			want: []string{
				"terminating pod/api-old-a [ready] is terminating",
				"deleted pod/api-old-a [ready] was deleted",
			},
		},
		{
			name: "the rollout completes",
			send: func() { deploys.Modify(toUnstructured(t, rollingDeployment(2, 2, 2, 2, 2))) },
			want: []string{"ready deploy/api [ready] became ready: 2/2 ready"},
		},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			s.send()
			w.expect(t, s.want...)
			w.quiet(t)
		})
	}
	w.cancel()
	if err := w.wait(t); !errors.Is(err, context.Canceled) {
		t.Errorf("Watch after cancelling = %v, want context.Canceled", err)
	}
}

func TestWatchCustomResourceUntilReady(t *testing.T) {
	orders := custom("db.example.com/v1", "Database", "shop", "orders", "Ready=False BackupFailing: the latest backup failed")
	nightly := ownedBy(custom("db.example.com/v1", "Backup", "shop", "orders-nightly", `Ready=False UploadFailed: bucket "orders-backups" not found`), orders)
	weekly := ownedBy(custom("db.example.com/v1", "Backup", "shop", "orders-weekly", "Ready=True Uploaded: done"), orders)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-credentials", Namespace: "shop", UID: "secret-orders-credentials", OwnerReferences: ownerRefsOf(orders)},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	old := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "orders.1", Namespace: "shop", UID: "event-old"},
		InvolvedObject: corev1.ObjectReference{Kind: "Database", Name: "orders", UID: orders.GetUID()},
		Type:           corev1.EventTypeWarning, Reason: "BackupFailed", Message: "reported before the watch", Count: 4,
	}
	c := newWatchCluster(t, orders, nightly, weekly, creds, old)
	root := resolve.Object{Resource: databases, Kind: "Database", Name: "orders", Namespace: "shop"}
	w := startWatch(t, c.Clients, root, WatchOptions{Events: true, Until: UntilReady})
	w.expect(t,
		"state database/orders [not-ready] BackupFailing: the latest backup failed (owns 3, 1 not ready)",
		`state backup/orders-nightly [not-ready] UploadFailed: bucket "orders-backups" not found`,
	)
	dbs, backups, secrets, events := c.watcher(t, "databases"), c.watcher(t, "backups"), c.watcher(t, "secrets"), c.watcher(t, "events")

	// A Warning event, and an old one modified without happening again.
	events.Modify(old)
	events.Add(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "orders-nightly.1", Namespace: "shop", UID: "event-new"},
		InvolvedObject: corev1.ObjectReference{Kind: "Backup", Name: "orders-nightly", UID: nightly.GetUID()},
		Type:           corev1.EventTypeWarning, Reason: "UploadFailed", Message: "bucket missing", Count: 2,
	})
	w.expect(t, "event backup/orders-nightly [not-ready] UploadFailed: bucket missing (x2)")
	w.quiet(t)

	// While the watch of backups expires, one is mended and another goes;
	// the watch after the list expires too.
	mended := ownedBy(custom("db.example.com/v1", "Backup", "shop", "orders-nightly", "Ready=True Uploaded: uploaded to s3"), orders)
	if err := c.dyn.Tracker().Update(backupsGVR, mended, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := c.dyn.Tracker().Delete(backupsGVR, "shop", "orders-weekly"); err != nil {
		t.Fatal(err)
	}
	c.failNextWatch("backups", apierrors.NewResourceExpired("too old resource version: 1 (5)"))
	backups.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired, Message: "too old resource version"})
	w.expect(t,
		"deleted backup/orders-weekly [ready] was deleted",
		"condition backup/orders-nightly [ready] Ready False → True (Uploaded: uploaded to s3)",
	)
	c.watcher(t, "backups")
	if n := count(c.dyn.Actions(), "list", "backups"); n != 3 {
		t.Errorf("backups were listed %d times, want 3: at the start and after each expired watch", n)
	}
	w.quiet(t)

	tls := partial(toUnstructured(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "orders-tls", Namespace: "shop", UID: "secret-orders-tls", OwnerReferences: ownerRefsOf(orders)}}))
	secrets.Add(tls)
	w.expect(t, "added secret/orders-tls [none] appeared")

	dbs.Modify(custom("db.example.com/v1", "Database", "shop", "orders", "Ready=True Serving: accepting connections"))
	w.expect(t, "condition database/orders [ready] Ready False → True (Serving: accepting connections)")
	if err := w.wait(t); err != nil {
		t.Errorf("Watch until ready = %v, want nil once it is ready", err)
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if n := count(c.dyn.Actions(), verb, "secrets") + count(c.typed.Actions(), verb, "secrets"); n > 0 {
			t.Errorf("Watch read Secrets with %s %d times; it may read only their metadata", verb, n)
		}
	}
}

func TestWatchObjectCreatedAgain(t *testing.T) {
	orders := custom("db.example.com/v1", "Database", "shop", "orders", "Ready=True Serving: accepting connections")
	c := newWatchCluster(t, orders)
	root := resolve.Object{Resource: databases, Kind: "Database", Name: "orders", Namespace: "shop"}
	w := startWatch(t, c.Clients, root, WatchOptions{Until: UntilDeleted})
	w.expect(t, "state database/orders [ready] Serving: accepting connections")

	// While the watch expires, the Database is deleted and created again:
	// it was not gone for long enough to be seen gone, so the watch goes on.
	again := custom("db.example.com/v1", "Database", "shop", "orders", "Ready=False Provisioning: creating replicas")
	again.SetUID("database-orders-2")
	if err := c.dyn.Tracker().Delete(databases, "shop", "orders"); err != nil {
		t.Fatal(err)
	}
	if err := c.dyn.Tracker().Add(again); err != nil {
		t.Fatal(err)
	}
	c.watcher(t, "databases").Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonGone})
	w.expect(t,
		"deleted database/orders [ready] was deleted",
		"added database/orders [not-ready] appeared: Provisioning: creating replicas",
	)
	w.quiet(t)
	c.watcher(t, "databases").Delete(again)
	w.expect(t, "deleted database/orders [not-ready] was deleted")
	if err := w.wait(t); err != nil {
		t.Errorf("Watch until deleted = %v, want nil", err)
	}
}

func ownerRefsOf(owner *unstructured.Unstructured) []metav1.OwnerReference {
	return ownedBy(&unstructured.Unstructured{Object: map[string]any{}}, owner).GetOwnerReferences()
}

func TestWatchFindsNewKinds(t *testing.T) {
	orders := custom("db.example.com/v1", "Database", "shop", "orders", "Ready=True Serving: accepting connections")
	c := newWatchCluster(t, orders)
	root := resolve.Object{Resource: databases, Kind: "Database", Name: "orders", Namespace: "shop"}
	wr := newWatcher(c.Clients, root, WatchOptions{}, WatchSink{})
	wr.discoverEvery = 10 * time.Millisecond
	w := startWatcher(t, wr)
	w.expect(t, "state database/orders [ready] Serving: accepting connections")
	c.watcher(t, "databases")

	backup := ownedBy(custom("db.example.com/v1", "Backup", "shop", "orders-nightly", "Ready=Unknown Uploading: started"), orders)
	if err := c.dyn.Tracker().Add(backup); err != nil {
		t.Fatal(err)
	}
	if err := c.md.Tracker().Add(partial(backup)); err != nil {
		t.Fatal(err)
	}
	w.expect(t, "added backup/orders-nightly [unknown] appeared: Uploading: started")
}

func TestWatchUntilDeleted(t *testing.T) {
	pod := readyPod("api-old-a", "api-old")
	root := resolve.ObjectOf(resolve.Target{Kind: resolve.KindPod, Name: "api-old-a", Namespace: "shop"})

	t.Run("deleted while watched", func(t *testing.T) {
		c := newWatchCluster(t, pod)
		w := startWatch(t, c.Clients, root, WatchOptions{Until: UntilDeleted})
		w.expect(t, "state pod/api-old-a [ready] Running")
		c.watcher(t, "pods").Delete(toUnstructured(t, pod))
		w.expect(t, "deleted pod/api-old-a [ready] was deleted")
		if err := w.wait(t); err != nil {
			t.Errorf("Watch until deleted = %v, want nil", err)
		}
	})
	t.Run("already gone", func(t *testing.T) {
		w := startWatch(t, newWatchCluster(t).Clients, root, WatchOptions{Until: UntilDeleted})
		if err := w.wait(t); err != nil {
			t.Errorf("Watch until deleted of nothing = %v, want nil", err)
		}
		w.quiet(t)
	})
	t.Run("gone without waiting for it", func(t *testing.T) {
		w := startWatch(t, newWatchCluster(t).Clients, root, WatchOptions{})
		if err := w.wait(t); !apierrors.IsNotFound(err) {
			t.Errorf("Watch of nothing = %v, want not found", err)
		}
	})
}

func migrateJob(conditions ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "shop", UID: "job-migrate", Generation: 1},
		Status:     batchv1.JobStatus{Active: 1, Conditions: conditions},
	}
}

func TestWatchUntilReadyEnds(t *testing.T) {
	job := resolve.ObjectOf(resolve.Target{Kind: resolve.KindJob, Name: "migrate", Namespace: "shop"})
	complete := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	failed := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}

	t.Run("ready already", func(t *testing.T) {
		done := migrateJob(complete)
		done.Status.Active, done.Status.Succeeded = 0, 1
		w := startWatch(t, newWatchCluster(t, done).Clients, job, WatchOptions{Until: UntilReady})
		w.expect(t, "state job/migrate [ready] complete, 1/1 succeeded")
		if err := w.wait(t); err != nil {
			t.Errorf("Watch until ready of a complete job = %v, want nil", err)
		}
	})
	t.Run("fails", func(t *testing.T) {
		c := newWatchCluster(t, migrateJob())
		w := startWatch(t, c.Clients, job, WatchOptions{Until: UntilReady})
		w.expect(t, "state job/migrate [ready] running, 1 active, 0/1 succeeded")
		c.watcher(t, "jobs").Modify(toUnstructured(t, migrateJob(failed)))
		w.expect(t,
			"condition job/migrate [not-ready] Failed is True (BackoffLimitExceeded)",
			"not-ready job/migrate [not-ready] is no longer ready: failed: BackoffLimitExceeded",
		)
		err := w.wait(t)
		if !errors.Is(err, ErrStalled) || !strings.HasPrefix(err.Error(), "job/migrate has failed: BackoffLimitExceeded; ") {
			t.Errorf("Watch until ready of a failed job = %v, want ErrStalled", err)
		}
	})
	t.Run("deleted first", func(t *testing.T) {
		c := newWatchCluster(t, migrateJob())
		w := startWatch(t, c.Clients, job, WatchOptions{Until: UntilReady})
		w.expect(t, "state job/migrate [ready] running, 1 active, 0/1 succeeded")
		c.watcher(t, "jobs").Delete(toUnstructured(t, migrateJob()))
		w.expect(t, "deleted job/migrate [ready] was deleted")
		if err := w.wait(t); err == nil || err.Error() != "job/migrate was deleted before it was ready" {
			t.Errorf("Watch until ready of a deleted job = %v", err)
		}
	})
}

func TestWatchRetriesAndReports(t *testing.T) {
	orders := custom("db.example.com/v1", "Database", "shop", "orders", "Ready=True Serving: accepting connections")
	c := newWatchCluster(t, orders)
	c.dyn.PrependReactor("list", "backups", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "db.example.com", Resource: "backups"}, "", errors.New("no"))
	})
	c.failNextWatch("databases", apierrors.NewServiceUnavailable("etcd is restarting"))
	backup := ownedBy(custom("db.example.com/v1", "Backup", "shop", "orders-nightly", "Ready=True Uploaded: done"), orders)
	if err := c.md.Tracker().Add(partial(backup)); err != nil {
		t.Fatal(err)
	}
	root := resolve.Object{Resource: databases, Kind: "Database", Name: "orders", Namespace: "shop"}
	wr := newWatcher(c.Clients, root, WatchOptions{}, WatchSink{})
	wr.firstRetry = time.Millisecond
	w := startWatcher(t, wr)

	// The Backups cannot be listed, which does not hold up the start, and
	// the first watch of the Database fails and is tried again.
	w.expect(t, "state database/orders [ready] Serving: accepting connections")
	var problems []string
	for range 2 {
		select {
		case err := <-w.errs:
			problems = append(problems, err.Error())
		case <-time.After(5 * time.Second):
			t.Fatalf("problems = %q, want two", problems)
		}
	}
	want := []string{
		"could not watch backups.db.example.com, so changes to them are missing: backups.db.example.com is forbidden: no",
		"watching database/orders: etcd is restarting; trying again",
	}
	for _, p := range want {
		if !strings.Contains(strings.Join(problems, "\n"), p) {
			t.Errorf("problems = %q, want %q", problems, p)
		}
	}
	c.watcher(t, "databases").Modify(custom("db.example.com/v1", "Database", "shop", "orders", "Ready=False Lost: replica down"))
	w.expect(t, "condition database/orders [not-ready] Ready True → False (Lost: replica down)")
}

func TestDiff(t *testing.T) {
	ready := Condition{Type: "Ready", Status: "True", Reason: "Serving"}
	tests := []struct {
		name     string
		old, cur snapshot
		want     []string
	}{
		{
			name: "a message changing alone is not news",
			old:  snapshot{judge: judgedByConditions, conditions: []Condition{{Type: "Ready", Status: "True", Reason: "Serving", Message: "at 12:00"}}},
			cur:  snapshot{judge: judgedByConditions, conditions: []Condition{{Type: "Ready", Status: "True", Reason: "Serving", Message: "at 12:01"}}},
		},
		{
			name: "a reason changing is",
			old:  snapshot{judge: judgedByConditions, conditions: []Condition{ready}},
			cur:  snapshot{judge: judgedByConditions, conditions: []Condition{{Type: "Ready", Status: "True", Reason: "Degraded"}}},
			want: []string{"condition Ready is True (Serving → Degraded)"},
		},
		{
			name: "a ReplicaSet's counts are quiet; its health is not",
			old:  snapshot{judge: judgedByRollout, quiet: true, health: HealthNotReady, status: "1/2 ready"},
			cur:  snapshot{judge: judgedByRollout, quiet: true, health: HealthReady, status: "2/2 ready"},
			want: []string{"ready became ready: 2/2 ready"},
		},
		{
			name: "nor is a ReplicaSet's generation",
			old:  snapshot{judge: judgedByRollout, quiet: true, health: HealthReady, status: "2/2 ready", generation: 3, observed: 3},
			cur:  snapshot{judge: judgedByRollout, quiet: true, health: HealthReady, status: "2/3 ready", generation: 4, observed: 3},
		},
		{
			name: "a claim's phase",
			old:  snapshot{judge: judgedByStatus, health: HealthNotReady, status: "Pending"},
			cur:  snapshot{judge: judgedByStatus, health: HealthNotReady, status: "Lost"},
			want: []string{"status Pending → Lost"},
		},
		{
			name: "terminating says it all",
			old:  snapshot{judge: judgedByPod, health: HealthReady, status: "Running"},
			cur:  snapshot{judge: judgedByPod, health: HealthNotReady, status: "Terminating", terminating: true},
			want: []string{"terminating is terminating"},
		},
		{
			name: "a new generation reconciled at once",
			old:  snapshot{judge: judgedByConditions, generation: 3, observed: 3},
			cur:  snapshot{judge: judgedByConditions, generation: 4, observed: 4},
			want: []string{"generation generation 3 → 4, reconciled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, c := range diff(tt.old, tt.cur) {
				got = append(got, string(c.Type)+" "+c.Message)
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("diff = %q, want %q", got, tt.want)
			}
		})
	}
}
