package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// fakeResource is how the fake API server lists one resource.
type fakeResource struct{ groupVersion, listKind string }

var fakeResources = map[string]fakeResource{
	"pods":                   {"v1", "PodList"},
	"events":                 {"v1", "EventList"},
	"services":               {"v1", "ServiceList"},
	"configmaps":             {"v1", "ConfigMapList"},
	"secrets":                {"v1", "SecretList"},
	"persistentvolumeclaims": {"v1", "PersistentVolumeClaimList"},
	"deployments":            {"apps/v1", "DeploymentList"},
	"replicasets":            {"apps/v1", "ReplicaSetList"},
	"statefulsets":           {"apps/v1", "StatefulSetList"},
	"daemonsets":             {"apps/v1", "DaemonSetList"},
	"controllerrevisions":    {"apps/v1", "ControllerRevisionList"},
	"jobs":                   {"batch/v1", "JobList"},
}

// resourceOf returns the resource a typed object is served as.
func resourceOf(o runtime.Object) string {
	switch o.(type) {
	case *corev1.Pod:
		return "pods"
	case *corev1.Event:
		return "events"
	case *corev1.Service:
		return "services"
	case *corev1.ConfigMap:
		return "configmaps"
	case *corev1.Secret:
		return "secrets"
	case *corev1.PersistentVolumeClaim:
		return "persistentvolumeclaims"
	case *appsv1.Deployment:
		return "deployments"
	case *appsv1.ReplicaSet:
		return "replicasets"
	case *appsv1.StatefulSet:
		return "statefulsets"
	case *appsv1.DaemonSet:
		return "daemonsets"
	case *appsv1.ControllerRevision:
		return "controllerrevisions"
	case *batchv1.Job:
		return "jobs"
	default:
		panic(fmt.Sprintf("fake API server cannot serve %T", o))
	}
}

// fakeNamespace serves typed objects in one namespace of the fake API
// server. Lists honour label selectors, as the real API server does, so
// several workloads can share the namespace.
type fakeNamespace struct {
	t  *testing.T
	mu sync.Mutex
	// objects are keyed by resource, such as "pods".
	objects map[string][]runtime.Object
}

func newFakeNamespace(t *testing.T, objs ...runtime.Object) *fakeNamespace {
	f := &fakeNamespace{t: t, objects: map[string][]runtime.Object{}}
	for _, o := range objs {
		r := resourceOf(o)
		f.objects[r] = append(f.objects[r], o)
	}
	return f
}

// get returns the named object of a resource, or nil.
func (f *fakeNamespace) get(resource, name string) runtime.Object {
	for _, o := range f.objects[resource] {
		if m, err := meta.Accessor(o); err == nil && m.GetName() == name {
			return o
		}
	}
	return nil
}

// serve answers a request for /api[s]/<gv>/namespaces/<ns>/<resource>[/<name>[/<sub>]],
// split into its parts after the namespace.
func (f *fakeNamespace) serve(w http.ResponseWriter, r *http.Request, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resource := parts[0]
	res, ok := fakeResources[resource]
	if !ok {
		writeStatus(w, http.StatusNotFound, "NotFound", resource+" not served")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		sel, err := labels.Parse(r.URL.Query().Get("labelSelector"))
		if err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		items := []runtime.Object{}
		for _, o := range f.objects[resource] {
			if m, err := meta.Accessor(o); err == nil && sel.Matches(labels.Set(m.GetLabels())) {
				items = append(items, o)
			}
		}
		writeJSON(f.t, w, map[string]any{"kind": res.listKind, "apiVersion": res.groupVersion, "metadata": map[string]any{}, "items": items})
	case len(parts) == 2 && r.Method == http.MethodGet:
		o := f.get(resource, parts[1])
		if o == nil {
			writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", resource, parts[1]))
			return
		}
		writeJSON(f.t, w, o)
	case len(parts) == 3 && resource == "pods" && parts[2] == "log":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "log of %s\n", parts[1])
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method+" "+strings.Join(parts, "/"))
	}
}

// namespacedParts splits a namespaced API path into its namespace and the
// parts after it: "/api/v1/namespaces/shop/pods/x" is "shop", [pods x].
func namespacedParts(path string) (string, []string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "namespaces" {
			return parts[i+1], parts[i+2:], true
		}
	}
	return "", nil, false
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("fake API server: encoding %T: %v", v, err)
	}
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure", "code": code, "reason": reason, "message": msg,
	})
	_, _ = w.Write(b)
}
