package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// monitorsGV is the group version of the kind that installLater installs:
// the Prometheus Operator's ServiceMonitor.
const monitorsGV = "monitoring.coreos.com/v1"

// serviceMonitor returns a ServiceMonitor, owned by owner when it is given.
func serviceMonitor(ns, name string, owner *unstructured.Unstructured) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": monitorsGV, "kind": "ServiceMonitor",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "servicemonitor-" + name},
		"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": name}}},
	}}
	if owner != nil {
		u.SetOwnerReferences(ownerRefs(owner))
	}
	return u
}

// installLater returns a server that serves what base serves and, once the
// flag it returns is set, monitoring.coreos.com/v1 as well, with monitors, as
// a cluster does once the ServiceMonitor CRD has been installed. Before
// then, the group is not in discovery.
func installLater(t *testing.T, base *httptest.Server, monitors ...*unstructured.Unstructured) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	inner := base.Config.Handler
	var installed atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if installed.Load() && r.Method == http.MethodGet && r.Header.Get("Authorization") == "Bearer "+secretToken && serveMonitors(t, w, r, monitors) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &installed
}

// serveMonitors answers discovery with monitoring.coreos.com/v1 in it, and
// the reads of ServiceMonitors, whole or as the metadata client asks for
// them. It reports whether it answered.
func serveMonitors(t *testing.T, w http.ResponseWriter, r *http.Request, monitors []*unstructured.Unstructured) bool {
	p := r.URL.Path
	if p == "/apis" {
		var groups metav1.APIGroupList
		if err := json.Unmarshal([]byte(fakeDiscovery["/apis"]), &groups); err != nil {
			t.Error(err)
		}
		v := metav1.GroupVersionForDiscovery{GroupVersion: monitorsGV, Version: "v1"}
		groups.Groups = append(groups.Groups, metav1.APIGroup{Name: "monitoring.coreos.com", Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v})
		writeJSON(t, w, groups)
		return true
	}
	rest, ok := strings.CutPrefix(p, "/apis/"+monitorsGV)
	if !ok {
		return false
	}
	if rest == "" {
		writeJSON(t, w, metav1.APIResourceList{
			TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: monitorsGV,
			APIResources: []metav1.APIResource{{Name: "servicemonitors", SingularName: "servicemonitor", Kind: "ServiceMonitor",
				Namespaced: true, ShortNames: []string{"smon"}, Verbs: metav1.Verbs{"get", "list", "watch"}}},
		})
		return true
	}
	ns, parts, ok := namespacedParts(p)
	if !ok || parts[0] != "servicemonitors" {
		writeStatus(w, http.StatusNotFound, "NotFound", p+" not served")
		return true
	}
	var items []*unstructured.Unstructured
	for _, u := range monitors {
		if u.GetNamespace() == ns && (len(parts) == 1 || u.GetName() == parts[1]) {
			items = append(items, u)
		}
	}
	asMetadata := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata")
	switch {
	case len(parts) == 1 && asMetadata:
		list := []any{}
		for _, u := range items {
			list = append(list, partialOf(u))
		}
		writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": list})
	case len(parts) == 1:
		list := []any{}
		for _, u := range items {
			list = append(list, u.Object)
		}
		writeJSON(t, w, map[string]any{"kind": "ServiceMonitorList", "apiVersion": monitorsGV, "metadata": map[string]any{}, "items": list})
	case len(parts) == 2 && len(items) == 1 && asMetadata:
		writeJSON(t, w, partialOf(items[0]))
	case len(parts) == 2 && len(items) == 1:
		writeJSON(t, w, items[0].Object)
	default:
		writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("servicemonitors %q not found", strings.Join(parts[1:], "/")))
	}
	return true
}

// TestTreeFindsKindsInstalledLater installs a CRD in the fake cluster after a
// first tug tree has filled the discovery cache, which stays fresh for hours,
// with an object of it that database/orders owns: the next tree must show it.
// tug why, watch and reconcile find what an object owns the same way.
func TestTreeFindsKindsInstalledLater(t *testing.T) {
	isolate(t)
	base, writes := fakeAPIServer(t)
	orders := shopDatabase("Database", "orders", "False", "BackupFailing", "the latest backup failed")
	srv, installed := installLater(t, base, serviceMonitor("shop", "orders", orders))
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	if out := mustTug(t, "tree", "db/orders", "-n", "shop"); strings.Contains(out, "servicemonitor/orders") {
		t.Fatalf("tug tree db/orders before the install shows a ServiceMonitor:\n%s", out)
	}
	installed.Store(true)
	out := mustTug(t, "tree", "db/orders", "-n", "shop")
	for _, want := range []string{"── backup/orders-nightly ", "── secret/orders-credentials ", "── servicemonitor/orders "} {
		if !strings.Contains(out, want) {
			t.Errorf("tug tree db/orders after the install lacks %q:\n%s", want, out)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("tree wrote to the cluster %d times", writes.Load())
	}
}

// TestSupportBundleFindsKindsInstalledLater installs a CRD in the fake
// cluster after a first bundle of the namespace has filled the discovery
// cache: the next bundle must hold the objects of the new kind.
func TestSupportBundleFindsKindsInstalledLater(t *testing.T) {
	isolate(t)
	base, writes := fakeBundleServer(t)
	srv, installed := installLater(t, base, serviceMonitor("payments", "api", nil))
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod", "-n", "payments")

	const monitor = "objects/payments/servicemonitors.monitoring.coreos.com/api.yaml"
	bundle := func() map[string]string {
		t.Helper()
		r := tug(t, "support", "bundle", "namespace/payments", "--output", "-")
		if r.code != 0 {
			t.Fatalf("tug support bundle namespace/payments: exit %d\n%s", r.code, r.stderr)
		}
		return unpack(t, []byte(r.stdout), archiveDir(t, r.stdout))
	}
	if _, ok := bundle()[monitor]; ok {
		t.Fatalf("namespace bundle before the install holds %s", monitor)
	}
	installed.Store(true)
	files := bundle()
	if m := files[monitor]; !strings.Contains(m, "kind: ServiceMonitor") {
		t.Errorf("namespace bundle after the install lacks the ServiceMonitor in %s; it has %d files", monitor, len(files))
	}
	if api := files["cluster/api-resources.json"]; !strings.Contains(api, `"resource": "servicemonitors"`) {
		t.Errorf("cluster/api-resources.json after the install lacks servicemonitors")
	}
	if writes.Load() != 0 {
		t.Fatalf("support bundle wrote to the cluster %d times", writes.Load())
	}
}
