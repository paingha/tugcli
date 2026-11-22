package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/rest"
)

// TestPreferredResourcesReadsAfresh checks what PreferredResources finds and
// what it costs with tug's discovery cache. A command that finds discovery
// cached by an earlier one reads it again from the cluster, and so sees a
// group installed since; it does so once, however often it asks; and a
// command that finds no cache reads the cluster only once.
func TestPreferredResourcesReadsAfresh(t *testing.T) {
	var installed atomic.Bool
	var groupLists atomic.Int32 // how often /apis was read: once per read of discovery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := func(name string) metav1.APIGroup {
			v := metav1.GroupVersionForDiscovery{GroupVersion: name + "/v1", Version: "v1"}
			return metav1.APIGroup{Name: name, Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v}
		}
		resources := func(gv, name, kind string) metav1.APIResourceList {
			return metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: gv,
				APIResources: []metav1.APIResource{{Name: name, Kind: kind, Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}}}
		}
		var body any
		switch r.URL.Path {
		case "/api":
			body = metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}}
		case "/apis":
			groupLists.Add(1)
			groups := metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}, Groups: []metav1.APIGroup{group("apps")}}
			if installed.Load() {
				groups.Groups = append(groups.Groups, group("monitoring.coreos.com"))
			}
			body = groups
		case "/api/v1":
			body = resources("v1", "pods", "Pod")
		case "/apis/apps/v1":
			body = resources("apps/v1", "deployments", "Deployment")
		case "/apis/monitoring.coreos.com/v1":
			if !installed.Load() {
				http.NotFound(w, r)
				return
			}
			body = resources("monitoring.coreos.com/v1", "servicemonitors", "ServiceMonitor")
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encoding %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)

	// Each command has a discovery client of its own on the same cache, as
	// kube.Factory.DiscoveryFor makes it.
	cache := t.TempDir()
	command := func() discovery.CachedDiscoveryInterface {
		t.Helper()
		d, err := disk.NewCachedDiscoveryClientForConfig(&rest.Config{Host: srv.URL},
			filepath.Join(cache, "discovery"), filepath.Join(cache, "http"), 6*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	served := func(d discovery.DiscoveryInterface) []string {
		t.Helper()
		lists, failed, err := PreferredResources(context.Background(), d)
		if err != nil || len(failed) > 0 {
			t.Fatalf("PreferredResources = failed %v, error %v", failed, err)
		}
		var out []string
		for _, l := range lists {
			for _, r := range l.APIResources {
				out = append(out, l.GroupVersion+"/"+r.Name)
			}
		}
		slices.Sort(out)
		return out
	}
	check := func(step string, d discovery.DiscoveryInterface, reads int32, want ...string) {
		t.Helper()
		before := groupLists.Load()
		if got := served(d); !slices.Equal(got, want) {
			t.Errorf("%s: PreferredResources = %v, want %v", step, got, want)
		}
		if got := groupLists.Load() - before; got != reads {
			t.Errorf("%s: PreferredResources read discovery from the cluster %d times, want %d", step, got, reads)
		}
	}

	check("first command, no cache", command(), 1, "apps/v1/deployments", "v1/pods")
	installed.Store(true)
	second := command()
	check("next command, after the install", second, 1, "apps/v1/deployments", "monitoring.coreos.com/v1/servicemonitors", "v1/pods")
	check("next command, asking again", second, 0, "apps/v1/deployments", "monitoring.coreos.com/v1/servicemonitors", "v1/pods")
}
