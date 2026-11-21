package ops

import (
	"context"
	"strings"
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
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/resolve"
)

// testResources is what the fake clusters' discovery serves.
var testResources = []*metav1.APIResourceList{
	{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
		{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "services", Kind: "Service", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "events", Kind: "Event", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: []string{"create"}},
	}},
	{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
		{Name: "statefulsets", Kind: "StatefulSet", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
		{Name: "jobs", Kind: "Job", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "cronjobs", Kind: "CronJob", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "db.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "databases", Kind: "Database", Namespaced: true, Verbs: []string{"get", "list"}},
		{Name: "backups", Kind: "Backup", Namespaced: true, Verbs: []string{"get", "list"}},
	}},
	{GroupVersion: "storage.example.com/v1", APIResources: []metav1.APIResource{
		{Name: "xstorages", Kind: "XStorage", Namespaced: false, Verbs: []string{"get", "list"}},
		{Name: "buckets", Kind: "Bucket", Namespaced: false, Verbs: []string{"get", "list"}},
	}},
}

var (
	databases = schema.GroupVersionResource{Group: "db.example.com", Version: "v1", Resource: "databases"}
	xstorages = schema.GroupVersionResource{Group: "storage.example.com", Version: "v1", Resource: "xstorages"}
	cronjobs  = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
)

// fakeClients serves the same objects through the typed, dynamic and
// metadata clients, with discovery of testResources. typed are built-in
// objects; custom are custom resources.
func fakeClients(t *testing.T, typed []runtime.Object, custom ...*unstructured.Unstructured) (Clients, *metadatafake.FakeMetadataClient) {
	t.Helper()
	cs := fake.NewSimpleClientset(typed...)
	cs.Resources = testResources

	var dyn, md []runtime.Object
	for _, o := range typed {
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
		dyn = append(dyn, u)
		md = append(md, partial(u))
	}
	for _, u := range custom {
		dyn = append(dyn, u)
		md = append(md, partial(u))
	}
	sch := metadatafake.NewTestScheme()
	for _, o := range md {
		if gvk := o.GetObjectKind().GroupVersionKind(); !sch.Recognizes(gvk) {
			sch.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
		}
	}
	meta := metadatafake.NewSimpleMetadataClient(sch, md...)
	return Clients{
		Typed:     cs,
		Dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), dyn...),
		Metadata:  meta,
		Discovery: cs.Discovery(),
	}, meta
}

func partial(u *unstructured.Unstructured) *metav1.PartialObjectMetadata {
	p := &metav1.PartialObjectMetadata{}
	p.SetGroupVersionKind(u.GroupVersionKind())
	p.Name, p.Namespace, p.UID = u.GetName(), u.GetNamespace(), u.GetUID()
	p.OwnerReferences = u.GetOwnerReferences()
	p.CreationTimestamp = u.GetCreationTimestamp()
	return p
}

// custom returns a custom resource with status conditions, each given as
// "Type=Status reason: message".
func custom(apiVersion, kind, ns, name string, conditions ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUID(types.UID(strings.ToLower(kind) + "-" + name))
	u.SetGeneration(1)
	var conds []any
	for _, c := range conditions {
		typeStatus, rest, _ := strings.Cut(c, " ")
		typ, status, _ := strings.Cut(typeStatus, "=")
		reason, msg, _ := strings.Cut(rest, ": ")
		conds = append(conds, map[string]any{
			"type": typ, "status": status, "reason": reason, "message": msg,
			"lastTransitionTime": whyNow.Add(-10 * time.Minute).Format(time.RFC3339),
		})
	}
	if conds != nil {
		u.Object["status"] = map[string]any{"conditions": conds, "observedGeneration": int64(1)}
	}
	return u
}

func ownedBy(u *unstructured.Unstructured, owner *unstructured.Unstructured) *unstructured.Unstructured {
	yes := true
	u.SetOwnerReferences(append(u.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes,
	}))
	return u
}

func ownerRef(owner *unstructured.Unstructured) []metav1.OwnerReference {
	return ownedBy(&unstructured.Unstructured{Object: map[string]any{}}, owner).GetOwnerReferences()
}

func objectOf(u *unstructured.Unstructured, r schema.GroupVersionResource) resolve.Object {
	return resolve.Object{Resource: r, Kind: u.GetKind(), Name: u.GetName(), Namespace: u.GetNamespace()}
}

// ordersCluster is a Database that owns a failing Backup, a Secret, a
// Service and a StatefulSet whose pod is crashing.
func ordersCluster(t *testing.T) (Clients, *unstructured.Unstructured) {
	db := custom("db.example.com/v1", "Database", "web", "orders",
		"Ready=False BackupFailing: the latest backup failed",
		"Stalled=True BackupFailing: the latest backup failed",
		"Provisioned=True Done: volumes are provisioned")
	db.SetGeneration(4)
	_ = unstructured.SetNestedField(db.Object, int64(3), "status", "observedGeneration")
	backup := ownedBy(custom("db.example.com/v1", "Backup", "web", "orders-nightly",
		`Ready=False UploadFailed: bucket "orders-backups" not found`), db)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-db", Namespace: "web", UID: "sts-orders-db", OwnerReferences: ownerRef(db)},
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "orders-db"}},
		},
		Status: appsv1.StatefulSetStatus{Replicas: 1, UpdatedReplicas: 1},
	}
	pod := whyPod("orders-db-0", waiting("CrashLoopBackOff", "back-off restarting failed container"), lastExit(1, "Error", time.Minute),
		func(p *corev1.Pod) {
			p.Labels = map[string]string{"app": "orders-db"}
			p.OwnerReferences = controlledBy("StatefulSet", "orders-db", "sts-orders-db")
		})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-credentials", Namespace: "web", UID: "secret-orders", OwnerReferences: ownerRef(db)},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "web", UID: "svc-orders", OwnerReferences: ownerRef(db)},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "orders-api"}},
	}
	dbEvent := warningEvent("Database", "orders", "", "BackupFailed", "backup orders-nightly failed", 3, 5*time.Minute)
	dbEvent.InvolvedObject.UID = db.GetUID()
	normal := warningEvent("Database", "orders", "", "Provisioned", "volumes are ready", 1, 5*time.Minute)
	normal.InvolvedObject.UID = db.GetUID()
	normal.Type = corev1.EventTypeNormal
	stale := warningEvent("Database", "orders", "", "BackupFailed", "an old failure", 1, 3*time.Hour)
	stale.InvolvedObject.UID = db.GetUID()

	c, _ := fakeClients(t, []runtime.Object{sts, pod, secret, svc, dbEvent, normal, stale}, db, backup)
	return c, db
}

func TestObjectTree(t *testing.T) {
	c, db := ordersCluster(t)
	root, unlisted, err := ObjectTree(context.Background(), c, objectOf(db, databases))
	if err != nil {
		t.Fatal(err)
	}
	want := `Database/orders not-ready: BackupFailing: the latest backup failed
  Backup/orders-nightly not-ready: UploadFailed: bucket "orders-backups" not found
` + "  Secret/orders-credentials none: \n" + // no status: a Secret is never read
		`  Service/orders none: ClusterIP
  StatefulSet/orders-db not-ready: 0/1 ready
    Pod/orders-db-0 not-ready: CrashLoopBackOff, 5 restarts
      Container/api not-ready: CrashLoopBackOff, 5 restarts
`
	if got := outline(*root); got != want {
		t.Errorf("ObjectTree(database/orders) =\n%s\nwant\n%s", got, want)
	}
	if len(unlisted) != 0 {
		t.Errorf("unlisted = %v, want none", unlisted)
	}
}

func TestObjectTreeNeverReadsSecrets(t *testing.T) {
	c, db := ordersCluster(t)
	if _, _, err := ObjectTree(context.Background(), c, objectOf(db, databases)); err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Dynamic.(*dynamicfake.FakeDynamicClient).Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Errorf("ObjectTree read a Secret through the dynamic client: %v", a)
		}
	}
}

func TestWhyObject(t *testing.T) {
	c, db := ordersCluster(t)
	d, err := WhyObject(context.Background(), c, objectOf(db, databases), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if d.Healthy || d.Target != "database/orders" || d.Namespace != "web" {
		t.Errorf("WhyObject(database/orders) = healthy %v, target %q in %q, want not healthy database/orders in web", d.Healthy, d.Target, d.Namespace)
	}
	if want := "BackupFailing: the latest backup failed (owns 4, 2 not ready)"; d.Summary != want {
		t.Errorf("Summary = %q, want %q", d.Summary, want)
	}
	if len(d.Conditions) != 3 || d.Conditions[0].Type != "Ready" || d.Conditions[0].LastTransition.IsZero() {
		t.Errorf("Conditions = %+v, want Ready, Stalled and Provisioned with their times", d.Conditions)
	}

	var got []string
	for _, f := range d.Findings {
		got = append(got, string(f.Severity)+" "+strings.Join(f.Objects, ",")+" "+f.Reason+": "+f.Message+" | "+f.Next)
	}
	want := []string{
		"error database/orders BackupFailing: Ready is False: the latest backup failed | see its status and events: `tug describe databases.db.example.com/orders`",
		`error backup/orders-nightly UploadFailed: Ready is False: bucket "orders-backups" not found | see why: ` + "`tug why backups.db.example.com/orders-nightly`",
		"warning database/orders NotReconciled: its controller has not caught up with its latest change: it is at generation 4 but was last reconciled at 3, so its conditions may be out of date | if this lasts, check that the controller that manages it is running, and read its logs",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("findings lack\n  %s\ngot\n  %s", w, strings.Join(got, "\n  "))
		}
	}
	// The StatefulSet's crashing pod comes from Why.
	crash := false
	for _, f := range d.Findings {
		crash = crash || f.Reason == "CrashLoopBackOff" && f.Objects[0] == "pod/orders-db-0"
	}
	if !crash {
		t.Errorf("findings lack the StatefulSet's crashing pod:\n  %s", strings.Join(got, "\n  "))
	}
	// Stalled says what Ready says, so it is not repeated.
	for _, g := range got {
		if strings.Contains(g, "Stalled is True") {
			t.Errorf("Stalled repeats Ready: %s", g)
		}
	}
	if len(d.Pods) != 1 || d.Pods[0].Name != "orders-db-0" {
		t.Errorf("Pods = %+v, want the StatefulSet's pod", d.Pods)
	}
	if len(d.Events) == 0 || d.Events[0].Object != "database/orders" || d.Events[0].Reason != "BackupFailed" || d.Events[0].Count != 3 {
		t.Errorf("Events = %+v, want the recent BackupFailed warning first", d.Events)
	}
	for _, e := range d.Events {
		if e.Message == "an old failure" || e.Reason == "Provisioned" {
			t.Errorf("Events include %+v, an old or normal event", e)
		}
	}
}

func TestWhyObjectHealthy(t *testing.T) {
	cert := custom("cert-manager.io/v1", "Certificate", "web", "web-tls", "Ready=True Ready: Certificate is up to date and has not expired")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "web", UID: "secret-web-tls", OwnerReferences: ownerRef(cert)}}
	c, _ := fakeClients(t, []runtime.Object{secret}, cert)
	certs := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

	d, err := WhyObject(context.Background(), c, objectOf(cert, certs), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy || len(d.Findings) != 0 {
		t.Errorf("WhyObject(certificate/web-tls) = healthy %v, findings %+v, want healthy with none", d.Healthy, d.Findings)
	}
	if want := "Ready: Certificate is up to date and has not expired (owns 1, 0 not ready)"; d.Summary != want {
		t.Errorf("Summary = %q, want %q", d.Summary, want)
	}
}

func TestWhyObjectPartialView(t *testing.T) {
	c, db := ordersCluster(t)
	meta := c.Metadata.(*metadatafake.FakeMetadataClient)
	meta.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", nil)
	})

	d, err := WhyObject(context.Background(), c, objectOf(db, databases), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range d.Findings {
		found = found || f.Reason == "PartialView" && f.Severity == SeverityInfo && strings.Contains(f.Message, "could not list secrets")
	}
	if !found {
		t.Errorf("findings = %+v, want PartialView naming secrets", d.Findings)
	}

	_, unlisted, err := ObjectTree(context.Background(), c, objectOf(db, databases))
	if err != nil || len(unlisted) != 1 || unlisted[0] != "secrets" {
		t.Errorf("ObjectTree unlisted = %v, %v, want [secrets]", unlisted, err)
	}
}

func TestWhyCronJobCountsOnlyItsNewestJob(t *testing.T) {
	yes := true
	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "web", UID: "cron-nightly"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
	}
	job := func(name string, age time.Duration, failed bool) *batchv1.Job {
		j := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web", UID: types.UID(name),
				CreationTimestamp: metav1.NewTime(whyNow.Add(-age)),
				OwnerReferences:   []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: "nightly", UID: "cron-nightly", Controller: &yes}}},
			Spec: batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": name}}},
		}
		if failed {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}
		} else {
			j.Status.Succeeded = 1
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}
		return j
	}
	c, _ := fakeClients(t, []runtime.Object{cron, job("nightly-1", 48*time.Hour, true), job("nightly-2", 24*time.Hour, false)})
	o := resolve.Object{Resource: cronjobs, Kind: "CronJob", Name: "nightly", Namespace: "web"}

	d, err := WhyObject(context.Background(), c, o, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.Findings {
		if f.Severity != SeverityInfo {
			t.Errorf("finding %+v; an older job's failure should not count", f)
		}
	}
	if !d.Healthy || d.Summary != "schedule 0 3 * * * (owns 1, 0 not ready)" {
		t.Errorf("WhyObject(cronjob/nightly) = healthy %v, %q, want healthy, owning its newest job", d.Healthy, d.Summary)
	}

	root, _, err := ObjectTree(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := outline(*root); !strings.Contains(got, "Job/nightly-1 not-ready") || !strings.Contains(got, "Job/nightly-2 ready") {
		t.Errorf("ObjectTree(cronjob/nightly) shows every job it owns; got\n%s", got)
	}
}

func TestWhyClusterScopedObject(t *testing.T) {
	xr := custom("storage.example.com/v1", "XStorage", "", "shop", "Ready=False Creating: waiting for the bucket", "Synced=True ReconcileSuccess: ")
	bucket := ownedBy(custom("storage.example.com/v1", "Bucket", "", "shop-assets", "Ready=Unknown Creating: "), xr)
	c, _ := fakeClients(t, nil, xr, bucket)

	d, err := WhyObject(context.Background(), c, objectOf(xr, xstorages), WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if d.Namespace != "" || d.Healthy {
		t.Errorf("WhyObject(xstorage/shop) = namespace %q, healthy %v, want cluster-scoped and not healthy", d.Namespace, d.Healthy)
	}
	var reasons []string
	for _, f := range d.Findings {
		reasons = append(reasons, string(f.Severity)+" "+f.Objects[0]+" "+f.Reason+" | "+f.Next)
	}
	want := []string{
		"error xstorage/shop Creating | see its status and events: `tug describe xstorages.storage.example.com/shop`",
		"warning bucket/shop-assets Creating | see why: `tug why buckets.storage.example.com/shop-assets`",
	}
	if strings.Join(reasons, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings =\n  %s\nwant\n  %s", strings.Join(reasons, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestAssess(t *testing.T) {
	pvcs := schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	widgets := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	tests := []struct {
		name       string
		obj        *unstructured.Unstructured
		resource   schema.GroupVersionResource
		wantHealth Health
		wantStatus string
		wantBusy   bool
	}{
		{"ready", custom("example.com/v1", "Widget", "web", "w", "Ready=True Succeeded: Applied revision main@sha1:abc"),
			widgets, HealthReady, "Succeeded: Applied revision main@sha1:abc", false},
		{"not ready", custom("example.com/v1", "Widget", "web", "w", "Ready=False Broken: it broke"),
			widgets, HealthNotReady, "Broken: it broke", false},
		{"stalled beats an unknown ready", custom("example.com/v1", "Widget", "web", "w", "Ready=Unknown Progressing: working", "Stalled=True Timeout: gave up"),
			widgets, HealthNotReady, "Timeout: gave up", false},
		{"reconciling", custom("example.com/v1", "Widget", "web", "w", "Ready=Unknown Progressing: working", "Reconciling=True Progressing: working"),
			widgets, HealthUnknown, "Progressing: working", true},
		{"a node under no pressure", custom("v1", "Node", "", "n1", "MemoryPressure=False KubeletHasSufficientMemory: ok", "NetworkUnavailable=False RouteCreated: ok", "Ready=True KubeletReady: kubelet is posting ready status"),
			widgets, HealthReady, "KubeletReady: kubelet is posting ready status", false},
		{"a node without a network", custom("v1", "Node", "", "n1", "NetworkUnavailable=True NoRouteCreated: no route", "Ready=True KubeletReady: ok"),
			widgets, HealthNotReady, "NoRouteCreated: no route", false},
		{"a part not ready, without Ready", custom("cluster.x-k8s.io/v1beta1", "Cluster", "web", "c", "ControlPlaneReady=False WaitingForControlPlane: "),
			widgets, HealthNotReady, "WaitingForControlPlane", false},
		{"established", custom("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets.example.com", "NamesAccepted=True NoConflicts: no conflicts", "Established=True InitialNamesAccepted: the initial names have been accepted"),
			widgets, HealthReady, "InitialNamesAccepted: the initial names have been accepted", false},
		{"conditions tug does not know", custom("example.com/v1", "Widget", "web", "w", "Issuing=True Renewing: renewing"),
			widgets, HealthNone, "Issuing=True", false},
		{"no conditions", custom("example.com/v1", "Widget", "web", "w"),
			widgets, HealthNone, "", false},
		{"a bound claim", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": "Bound"}}},
			pvcs, HealthReady, "Bound", false},
		{"a pending claim", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": "Pending"}}},
			pvcs, HealthNotReady, "Pending", false},
	}
	lagging := custom("example.com/v1", "Widget", "web", "w", "Ready=True Done: done")
	lagging.SetGeneration(2)
	tests = append(tests, struct {
		name       string
		obj        *unstructured.Unstructured
		resource   schema.GroupVersionResource
		wantHealth Health
		wantStatus string
		wantBusy   bool
	}{"a newer generation", lagging, widgets, HealthUnknown, "generation 2 not yet reconciled", true})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assess(resolve.Object{Resource: tt.resource}, tt.obj)
			if a.health != tt.wantHealth || a.status != tt.wantStatus || a.busy != tt.wantBusy {
				t.Errorf("assess() = %s %q busy %v, want %s %q busy %v", a.health, a.status, a.busy, tt.wantHealth, tt.wantStatus, tt.wantBusy)
			}
		})
	}
}

func TestWhyObjectOfAWorkloadIsWhy(t *testing.T) {
	c, _ := fakeClients(t, []runtime.Object{deployStatus(2, 2, 2, 2), whyPod("api-1"), whyPod("api-2")})
	o := resolve.ObjectOf(resolve.Target{Kind: resolve.KindDeployment, Name: "api", Namespace: "web"})
	d, err := WhyObject(context.Background(), c, o, WhyOptions{Now: func() time.Time { return whyNow }})
	if err != nil {
		t.Fatal(err)
	}
	if d.Target != "deploy/api" || d.Rollout == nil || len(d.Pods) != 2 {
		t.Errorf("WhyObject(deploy/api) = %+v, want Why's diagnosis with its rollout and pods", d)
	}
}
