package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// Secrets planted in the payments namespace of the support bundle's fake
// API server, in every place a bundle must redact. None may be in a bundle.
const (
	bundleEnvSecret     = "e2e-env-secret"
	bundleArgSecret     = "e2e-arg-secret"
	bundleAppliedSecret = "e2e-applied-secret"
	bundleCMSecret      = "e2e-cm-secret"
	bundleCMURLSecret   = "e2e-cm-url-secret"
	bundleSecretData    = "e2e-secret-data"
	bundleEventSecret   = "e2e-event-secret"
	bundleCRSecret      = "e2e-cr-secret"
	bundleLogBearer     = "e2eLogBearer0123456789"
	bundleLogURLSecret  = "e2e-log-url-secret"
	bundleLogJSONSecret = "e2e-log-json-secret"
	bundleLogAWSKey     = "AKIAIOSFODNN7EXAMPLE"
	bundleCustomerID    = "customer-acme-4242"
)

var bundleSecrets = []string{
	bundleEnvSecret, bundleArgSecret, bundleAppliedSecret, bundleCMSecret, bundleCMURLSecret,
	bundleSecretData, base64.StdEncoding.EncodeToString([]byte(bundleSecretData)),
	bundleEventSecret, bundleCRSecret, bundleLogBearer, bundleLogURLSecret, bundleLogJSONSecret,
	bundleLogAWSKey, bundleCustomerID,
}

// paymentsObjects are deploy/api, whose one pod crashes and mounts a
// ConfigMap and a Secret, a Service, an event, and a Database custom
// resource that owns a Secret. Every object has its kind, as the API server
// returns it.
func paymentsObjects() []k8sruntime.Object {
	yes := true
	one := int32(1)
	labels := map[string]string{"app": "api"}
	typeMeta := func(apiVersion, kind string) metav1.TypeMeta {
		return metav1.TypeMeta{APIVersion: apiVersion, Kind: kind}
	}
	spec := corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app", Image: "registry.example.com/api:v3",
		Args: []string{"--listen=:8080", "--api-token=" + bundleArgSecret},
		Env: []corev1.EnvVar{
			{Name: "DATABASE_PASSWORD", Value: bundleEnvSecret},
			{Name: "DATABASE_USER", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "api-creds"}, Key: "user",
			}}},
		},
	}}}
	deploy := &appsv1.Deployment{
		TypeMeta: typeMeta("apps/v1", "Deployment"),
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments", UID: "deploy-api", Generation: 1,
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DATABASE_PASSWORD","value":"` + bundleAppliedSecret + `"}]}]}}}}`,
			}},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1},
	}
	rsLabels := map[string]string{"app": "api", "pod-template-hash": "5c8d"}
	rs := &appsv1.ReplicaSet{
		TypeMeta: typeMeta("apps/v1", "ReplicaSet"),
		ObjectMeta: metav1.ObjectMeta{Name: "api-5c8d", Namespace: "payments", UID: "rs-api", Labels: rsLabels,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "3"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "deploy-api", Controller: &yes}}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: rsLabels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: rsLabels}, Spec: spec}},
		Status: appsv1.ReplicaSetStatus{Replicas: 1},
	}
	podSpec := *spec.DeepCopy()
	podSpec.NodeName = "node-1"
	podSpec.Volumes = []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"}}}},
		{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "api-creds"}}},
	}
	pod := &corev1.Pod{
		TypeMeta: typeMeta("v1", "Pod"),
		ObjectMeta: metav1.ObjectMeta{Name: "api-5c8d-q7w2e", Namespace: "payments", UID: "pod-api", Labels: rsLabels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-5c8d", UID: "rs-api", Controller: &yes}}},
		Spec: podSpec,
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 2,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}},
			}},
		},
	}
	orders := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "db.example.com/v1", "kind": "Database",
		"metadata": map[string]any{"name": "orders", "namespace": "payments", "uid": "db-orders", "generation": int64(1)},
		"spec":     map[string]any{"adminPassword": bundleCRSecret, "engine": "postgres"},
		"status": map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "Serving", "message": "accepting connections",
		}}},
	}}
	return []k8sruntime.Object{
		deploy, rs, pod, orders,
		&corev1.Service{
			TypeMeta:   typeMeta("v1", "Service"),
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments", UID: "svc-api"},
			Spec:       corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 80}}},
		},
		&corev1.ConfigMap{
			TypeMeta:   typeMeta("v1", "ConfigMap"),
			ObjectMeta: metav1.ObjectMeta{Name: "api-config", Namespace: "payments", UID: "cm-api"},
			Data:       map[string]string{"api.key": bundleCMSecret, "DATABASE_URL": "postgres://app:" + bundleCMURLSecret + "@db/app"},
		},
		&corev1.Secret{
			TypeMeta: typeMeta("v1", "Secret"),
			ObjectMeta: metav1.ObjectMeta{Name: "api-creds", Namespace: "payments", UID: "secret-api",
				Annotations: map[string]string{
					"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Secret","data":{"password":"` +
						base64.StdEncoding.EncodeToString([]byte(bundleSecretData)) + `"},"stringData":{"user":"` + bundleSecretData + `"}}`,
				}},
			Data: map[string][]byte{"password": []byte(bundleSecretData), "user": []byte("app")},
		},
		&corev1.Secret{
			TypeMeta: typeMeta("v1", "Secret"),
			ObjectMeta: metav1.ObjectMeta{Name: "orders-creds", Namespace: "payments", UID: "secret-orders",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "db.example.com/v1", Kind: "Database", Name: "orders", UID: "db-orders", Controller: &yes}}},
			Data: map[string][]byte{"password": []byte(bundleSecretData)},
		},
		&corev1.Event{
			TypeMeta:       typeMeta("v1", "Event"),
			ObjectMeta:     metav1.ObjectMeta{Name: "api.backoff", Namespace: "payments", UID: "event-backoff"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: "payments", UID: pod.UID},
			Type:           corev1.EventTypeWarning, Reason: "BackOff", Count: 9,
			Message:       "Back-off restarting failed container app; last error: token=" + bundleEventSecret,
			LastTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
	}
}

// paymentsLog is what the pod's container logged, credentials and all.
func paymentsLog(previous bool) string {
	if previous {
		return "2026-11-21T14:00:00Z using " + bundleLogAWSKey + " for " + bundleCustomerID + "\n" +
			"2026-11-21T14:00:01Z panic: cannot reach the database\n"
	}
	return "2026-11-21T15:29:00Z listening on :8080\n" +
		"2026-11-21T15:29:01Z GET /charges Authorization: Bearer " + bundleLogBearer + "\n" +
		"2026-11-21T15:29:02Z dialling postgres://app:" + bundleLogURLSecret + "@db:5432/app\n" +
		`2026-11-21T15:29:03Z {"level":"debug","password":"` + bundleLogJSONSecret + `"}` + "\n"
}

// fakeBundleServer serves the payments namespace, with what the cluster
// around it needs: discovery that knows namespaces and nodes, the
// metadata API with annotations (which carry kubectl's last-applied copy of a
// Secret), the pod's logs and its node.
func fakeBundleServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	payments := newFakeNamespace(t, paymentsObjects()...)
	discovery := maps.Clone(fakeDiscovery)
	core, _ := json.Marshal(metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "v1",
		APIResources: func() []metav1.APIResource {
			var rs []metav1.APIResource
			for _, r := range []struct {
				name, kind string
				namespaced bool
				short      []string
			}{
				{"pods", "Pod", true, []string{"po"}}, {"services", "Service", true, []string{"svc"}},
				{"configmaps", "ConfigMap", true, []string{"cm"}}, {"secrets", "Secret", true, nil},
				{"persistentvolumeclaims", "PersistentVolumeClaim", true, []string{"pvc"}},
				{"events", "Event", true, []string{"ev"}},
				{"namespaces", "Namespace", false, []string{"ns"}}, {"nodes", "Node", false, []string{"no"}},
			} {
				rs = append(rs, metav1.APIResource{Name: r.name, Kind: r.kind, Namespaced: r.namespaced, ShortNames: r.short,
					SingularName: strings.ToLower(r.kind), Verbs: metav1.Verbs{"get", "list"}})
			}
			return rs
		}(),
	})
	discovery["/api/v1"] = string(core)
	namespace := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: "payments", UID: "ns-payments"}}
	node := &corev1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue, Reason: "KubeletHasDiskPressure"},
		}}}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && !strings.Contains(r.URL.Path, "/selfsubject") {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		asMetadata := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata")
		if ns, parts, ok := namespacedParts(p); ok {
			switch {
			case ns != "payments":
				writeStatus(w, http.StatusNotFound, "NotFound", "namespace "+ns+" not found")
			case len(parts) == 3 && parts[0] == "pods" && parts[2] == "log":
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, paymentsLog(r.URL.Query().Get("previous") == "true"))
			case asMetadata:
				servePartial(t, w, payments, parts)
			default:
				payments.serve(w, r, parts)
			}
			return
		}
		if body, ok := discovery[p]; ok {
			fmt.Fprint(w, body)
			return
		}
		switch p {
		case "/version":
			fmt.Fprint(w, `{"major":"1","minor":"35","gitVersion":"v1.35.1","platform":"linux/amd64"}`)
		case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			fmt.Fprint(w, `{"kind":"SelfSubjectReview","apiVersion":"authentication.k8s.io/v1","status":{"userInfo":{"username":"alice"}}}`)
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			fmt.Fprint(w, `{"kind":"SelfSubjectAccessReview","apiVersion":"authorization.k8s.io/v1","status":{"allowed":true}}`)
		case "/api/v1/namespaces":
			writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{},
				"items": []any{partialOf(namespace)}})
		case "/api/v1/namespaces/payments":
			writeJSON(t, w, namespace)
		case "/api/v1/nodes/node-1":
			writeJSON(t, w, node)
		default:
			writeStatus(w, http.StatusNotFound, "NotFound", p+" not found")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

// servePartial answers the metadata API for the payments namespace, as the
// API server does: names, labels, annotations and owners, never a Secret's
// data.
func servePartial(t *testing.T, w http.ResponseWriter, f *fakeNamespace, parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch len(parts) {
	case 1:
		items := []any{}
		for _, o := range f.objects[parts[0]] {
			items = append(items, partialOf(o))
		}
		writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": items})
	case 2:
		o := f.get(parts[0], parts[1])
		if o == nil {
			writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", parts[0], parts[1]))
			return
		}
		writeJSON(t, w, partialOf(o))
	default:
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", strings.Join(parts, "/"))
	}
}

func partialOf(o k8sruntime.Object) *metav1.PartialObjectMetadata {
	m, _ := meta.Accessor(o)
	p := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"}}
	p.Name, p.Namespace, p.UID, p.Labels, p.Annotations = m.GetName(), m.GetNamespace(), m.GetUID(), m.GetLabels(), m.GetAnnotations()
	p.OwnerReferences, p.CreationTimestamp = m.GetOwnerReferences(), m.GetCreationTimestamp()
	return p
}

// unpack reads a bundle's archive into its files' contents by path, without
// the directory every file is under, which it checks is name.
func unpack(t *testing.T, data []byte, name string) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the bundle is not gzipped: %v", err)
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		rel, ok := strings.CutPrefix(hdr.Name, name+"/")
		if !ok {
			t.Errorf("archive entry %s is not under %s/", hdr.Name, name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(b)
	}
}

// assertNoBundleSecret fails if a planted secret is in any of the files.
func assertNoBundleSecret(t *testing.T, files map[string]string) {
	t.Helper()
	for path, content := range files {
		for _, s := range bundleSecrets {
			if strings.Contains(content, s) {
				t.Errorf("bundle file %s holds the secret %q:\n%s", path, s, content)
			}
		}
	}
}

func connectToPayments(t *testing.T) *atomic.Int32 {
	t.Helper()
	isolate(t)
	srv, writes := fakeBundleServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod", "-n", "payments")
	return writes
}

func TestSupportBundle(t *testing.T) {
	writes := connectToPayments(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "case-1234.tar.gz")

	r := tug(t, "support", "bundle", "deploy/api", "--redact-pattern", `customer-[a-z]+-[0-9]+`, "--output", out)
	if r.code != 0 {
		t.Fatalf("tug support bundle: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r.stdout != out+"\n" {
		t.Errorf("stdout = %q, want the archive's path alone", r.stdout)
	}
	for _, want := range []string{"reading deployment/api and what it owns…", "running tug doctor's checks…", "review the bundle before you share it"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("bundle mode = %o, want 600", fi.Mode().Perm())
		}
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	files := unpack(t, data, "case-1234")
	assertNoBundleSecret(t, files)
	for _, p := range []string{
		"README.md", "manifest.json", "why/deployment-api.json", "tree/deployment-api.json", "events.json",
		"objects/payments/deployments.apps/api.yaml", "objects/payments/replicasets.apps/api-5c8d.yaml",
		"objects/payments/pods/api-5c8d-q7w2e.yaml", "objects/payments/services/api.yaml",
		"objects/payments/configmaps/api-config.yaml", "objects/payments/secrets/api-creds.yaml",
		"logs/payments/api-5c8d-q7w2e/app.log", "logs/payments/api-5c8d-q7w2e/app.previous.log",
		"nodes/node-1.yaml", "cluster/version.json", "cluster/api-resources.json", "doctor.json",
	} {
		if _, ok := files[p]; !ok {
			t.Errorf("bundle lacks %s", p)
		}
	}
	for path, want := range map[string][]string{
		"objects/payments/deployments.apps/api.yaml":    {"name: DATABASE_PASSWORD", "value: <redacted>", "--api-token=<redacted>", "last-applied-configuration: <redacted>"},
		"objects/payments/configmaps/api-config.yaml":   {"api.key: <redacted>", "DATABASE_URL: <redacted>"},
		"objects/payments/secrets/api-creds.yaml":       {"# Metadata only", "name: api-creds", "last-applied-configuration: <redacted>"},
		"logs/payments/api-5c8d-q7w2e/app.log":          {"listening on :8080", "Bearer <redacted>", "postgres://app:<redacted>@db", `"password":"<redacted>"`},
		"logs/payments/api-5c8d-q7w2e/app.previous.log": {"panic: cannot reach the database", "using <redacted> for <redacted>"},
		"events.json":       {`"reason": "BackOff"`, "token=<redacted>"},
		"nodes/node-1.yaml": {"KubeletHasDiskPressure"},
		"README.md":         {"tar -xzf case-1234.tar.gz", "1 pattern given with `--redact-pattern`"},
		"doctor.json":       {`"cluster": "payments-prod"`},
	} {
		for _, w := range want {
			if !strings.Contains(files[path], w) {
				t.Errorf("%s lacks %q:\n%s", path, w, files[path])
			}
		}
	}
	var m struct {
		Target  string `json:"target"`
		Cluster struct {
			Name string `json:"name"`
		} `json:"cluster"`
		Files []struct {
			Path   string `json:"path"`
			Size   int    `json:"size"`
			Source string `json:"source"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Target != "deployment/api" || m.Cluster.Name != "payments-prod" || len(m.Files) != len(files)-1 {
		t.Errorf("manifest: target %q, cluster %q, %d files for %d in the archive", m.Target, m.Cluster.Name, len(m.Files), len(files))
	}
	for _, f := range m.Files {
		if f.Size != len(files[f.Path]) || f.Source == "" {
			t.Errorf("manifest has %s at %d bytes from %q; the archive's is %d bytes", f.Path, f.Size, f.Source, len(files[f.Path]))
		}
	}

	if r := tug(t, "support", "bundle", "deploy/api", "--output", out); r.code == 0 || !strings.Contains(r.stderr, "already exists; remove it, or choose another file with --output") {
		t.Errorf("a second bundle to the same file = %d, stderr %q", r.code, r.stderr)
	}

	preview := filepath.Join(dir, "preview.tar.gz")
	r = tug(t, "support", "bundle", "deploy/api", "--preview", "--output", preview, "--exclude", "secret", "--logs-tail", "0")
	if r.code != 0 {
		t.Fatalf("tug support bundle --preview: exit %d\n%s", r.code, r.stderr)
	}
	if _, err := os.Stat(preview); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--preview wrote %s (%v)", preview, err)
	}
	for _, want := range []string{"FILE", "BYTES", "SOURCE", "objects/payments/deployments.apps/api.yaml", "GET /apis/apps/v1/namespaces/payments/deployments/api"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("--preview lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "secrets/") || strings.Contains(r.stdout, "logs/") {
		t.Errorf("--preview with --exclude secret --logs-tail 0 lists secrets or logs:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "nothing written") {
		t.Errorf("--preview stderr:\n%s", r.stderr)
	}

	for _, tt := range []struct{ args, want string }{
		{"support bundle deploy/api --include widget", `--include "widget": the cluster has no such kind`},
		{"support bundle deploy/api --redact-pattern (", `redact pattern "("`},
		{"support bundle deploy/api --since 0s", "--since must be more than 0"},
		{"support bundle deploy/api --logs-tail -1", "--logs-tail must be 0 or more"},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("support bundle wrote to the cluster %d times", writes.Load())
	}
}

func TestSupportBundleOfANamespaceAndACustomResource(t *testing.T) {
	writes := connectToPayments(t)

	r := tug(t, "support", "bundle", "namespace/payments", "--output", "-", "--redact-pattern", `customer-[a-z]+-[0-9]+`)
	if r.code != 0 {
		t.Fatalf("tug support bundle namespace/payments: exit %d\n%s", r.code, r.stderr)
	}
	name := archiveDir(t, r.stdout)
	if !strings.HasPrefix(name, "tug-bundle-payments-prod-namespace-payments-") {
		t.Errorf("bundle directory = %q, want the default name, after the cluster and the target", name)
	}
	files := unpack(t, []byte(r.stdout), name)
	assertNoBundleSecret(t, files)
	for _, p := range []string{
		"objects/_cluster/namespaces/payments.yaml", "objects/payments/databases.db.example.com/orders.yaml",
		"objects/payments/secrets/orders-creds.yaml", "objects/payments/configmaps/api-config.yaml",
		"why/deployment-api.json", "logs/payments/api-5c8d-q7w2e/app.log",
	} {
		if _, ok := files[p]; !ok {
			t.Errorf("namespace bundle lacks %s", p)
		}
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "orders.tgz")
	mustTug(t, "support", "bundle", "db/orders", "--output", out, "--redact-pattern", `customer-[a-z]+-[0-9]+`)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	files = unpack(t, data, "orders")
	assertNoBundleSecret(t, files)
	if db := files["objects/payments/databases.db.example.com/orders.yaml"]; !strings.Contains(db, "adminPassword: <redacted>") || !strings.Contains(db, "engine: postgres") {
		t.Errorf("Database YAML:\n%s", db)
	}
	for _, p := range []string{"objects/payments/secrets/orders-creds.yaml", "why/database-orders.json", "tree/database-orders.json"} {
		if _, ok := files[p]; !ok {
			t.Errorf("bundle of db/orders lacks %s", p)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("support bundle wrote to the cluster %d times", writes.Load())
	}
}

// archiveDir returns the directory of the first entry of a gzipped tar
// archive.
func archiveDir(t *testing.T, data string) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := tar.NewReader(zr).Next()
	if err != nil {
		t.Fatal(err)
	}
	dir, _, _ := strings.Cut(hdr.Name, "/")
	return dir
}
