package ops

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeWatch makes the fake clientset's event watches deliver what the
// test sends.
func newFakeWatch(c Clients) *watch.FakeWatcher {
	fw := watch.NewFake()
	c.Typed.(*fake.Clientset).PrependWatchReactor("events", k8stesting.DefaultWatchReactor(fw, nil))
	return fw
}

func TestTrace(t *testing.T) {
	op := dbOperator()
	c, db := reconcileCluster(t, op, statusWrittenBy("db-operator"))
	logs := strings.Join([]string{
		"2026-11-20T11:56:00.123456789Z {\"msg\":\"reconciling\",\"name\":\"orders\",\"namespace\":\"web\"}",
		"2026-11-20T11:56:30Z reconciling web/orders-nightly", // another object
		"2026-11-20T11:57:00Z backup failed for uid database-orders",
		"not a timestamp orders",
	}, "\n") + "\n"
	c.Typed.(*fake.Clientset).PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(logs)}, nil
	})
	tl, err := Trace(context.Background(), c, objectOf(db, databases), TraceOptions{Logs: true})
	if err != nil {
		t.Fatal(err)
	}
	if tl.Notes[0] != NoReconcileHistory || tl.Generation != 4 || tl.ObservedGeneration != 3 {
		t.Errorf("notes %q, generation %d/%d", tl.Notes, tl.Generation, tl.ObservedGeneration)
	}
	if tl.Controller != "deploy/db-operator" || tl.ControllerNamespace != "db-system" || tl.ControllerBasis != BasisInferred {
		t.Errorf("controller %s in %s (%s), want deploy/db-operator in db-system, inferred", tl.Controller, tl.ControllerNamespace, tl.ControllerBasis)
	}
	var got []string
	for _, e := range tl.Entries {
		got = append(got, string(e.Source)+" "+e.Object+" "+string(e.Basis)+": "+e.Message)
	}
	for _, want := range []string{
		"managedFields database/orders observed: kubectl-client-side-apply last wrote metadata and spec (Update)",
		"managedFields database/orders observed: db-operator last wrote status (Update)",
		"condition database/orders observed: Ready became False: BackupFailing: the latest backup failed",
		"owned pod/orders-db-0 observed: created, owned by statefulset/orders-db",
		"container pod/orders-db-0 observed: container api exited with code 1 (Error)",
		"event database/orders observed: backup orders-nightly failed",
		`log pod/db-operator-5d4f-x2x4z inferred: {"msg":"reconciling","name":"orders","namespace":"web"}`,
		"log pod/db-operator-5d4f-x2x4z observed: backup failed for uid database-orders",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("timeline lacks %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, e := range got {
		if strings.Contains(e, "orders-nightly") && strings.HasPrefix(e, "log") {
			t.Errorf("timeline has a log line about another object: %s", e)
		}
	}
	if !slices.IsSortedFunc(tl.Entries, func(a, b TimelineEntry) int { return a.Time.Compare(b.Time) }) {
		t.Errorf("timeline is not oldest first:\n%s", strings.Join(got, "\n"))
	}
	for _, e := range tl.Entries {
		if e.Source == SourceEvent && e.Reason == "BackupFailed" && e.Count == 3 && e.Type != corev1.EventTypeWarning {
			t.Errorf("event entry = %+v", e)
		}
	}
}

func TestTraceWithANamedControllerThatLoggedNothing(t *testing.T) {
	c, db := reconcileCluster(t, dbOperator(), nil)
	tl, err := Trace(context.Background(), c, objectOf(db, databases), TraceOptions{Logs: true, Controller: &operatorTarget})
	if err != nil {
		t.Fatal(err)
	}
	if tl.ControllerBasis != BasisObserved {
		t.Errorf("controller basis = %q, want observed for one named", tl.ControllerBasis)
	}
	if !slices.ContainsFunc(tl.Notes, func(n string) bool { return strings.Contains(n, "logged nothing that mentions it in the last 1h") }) {
		t.Errorf("notes = %q", tl.Notes)
	}
}

func TestContainsName(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{`"name":"orders"`, true},
		{"web/orders reconciled", true},
		{"orders", true},
		{"Orders", false},
		{"reconciling orders-nightly", false},
		{"preorders", false},
		{"orders.v2", false},
		{"orders: done", true},
	} {
		if got := containsName(tt.line, "orders"); got != tt.want {
			t.Errorf("containsName(%q, orders) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestEventFeed(t *testing.T) {
	typed, customs, db := ordersObjects()
	c, md := fakeClients(t, typed, customs...)
	f, err := NewEventFeed(context.Background(), c, objectOf(db, databases))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range f.Records {
		got = append(got, r.Type+" "+r.Object+" "+r.Reason)
	}
	want := []string{"Warning database/orders BackupFailed", "Warning database/orders BackupFailed", "Normal database/orders Provisioned"}
	if !slices.Equal(got, want) {
		t.Errorf("records = %q, want %q (oldest first)", got, want)
	}

	// A pod the StatefulSet starts after the feed opened is found through
	// its owner.
	newPod := whyPod("orders-db-1", func(p *corev1.Pod) {
		p.OwnerReferences = controlledBy("StatefulSet", "orders-db", "sts-orders-db")
	})
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newPod)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: u}
	obj.SetAPIVersion("v1")
	obj.SetKind("Pod")
	if err := md.Tracker().Add(partial(obj)); err != nil {
		t.Fatal(err)
	}

	fw := newFakeWatch(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan EventRecord, 10)
	done := make(chan error, 1)
	go func() {
		done <- f.Follow(ctx, func(r EventRecord) { sent <- r })
	}()
	podEvent := warningEvent("Pod", "orders-db-1", "", "BackOff", "back-off restarting failed container", 1, 0)
	podEvent.UID, podEvent.InvolvedObject.APIVersion, podEvent.InvolvedObject.UID = "ev-pod", "v1", newPod.UID
	other := warningEvent("Pod", "unrelated", "", "BackOff", "not ours", 1, 0)
	other.UID, other.InvolvedObject.APIVersion, other.InvolvedObject.UID = "ev-other", "v1", "pod-unrelated"
	fw.Add(other)
	fw.Add(podEvent)
	fw.Modify(podEvent) // the same again: not sent twice
	again := podEvent.DeepCopy()
	again.Count = 2
	fw.Modify(again)

	for _, want := range []int32{1, 2} {
		select {
		case r := <-sent:
			if r.Object != "pod/orders-db-1" || r.Reason != "BackOff" || r.Count != want {
				t.Errorf("sent %+v, want pod/orders-db-1's BackOff, count %d", r, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Follow sent nothing")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Follow after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not end when its context did")
	}
}
