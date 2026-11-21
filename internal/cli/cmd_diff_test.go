package cli

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/diff"
)

// apply answers a server-side apply as the API server does for the shapes
// these tests use, and only as a dry run: maps merge field by field, lists
// of named items item by item, other lists whole. A field the apply sets to
// a new value while another manager owns it is a conflict, answered with
// 409 unless forced; forced, the applier takes the field over. Like
// admission and defaulting on a real cluster, a Deployment gets a label for
// the applier, as a mutating webhook would add, and a default
// revisionHistoryLimit that nobody owns.
func (f *fakeNamespace) apply(w http.ResponseWriter, r *http.Request, resource, name string) {
	q := r.URL.Query()
	if q.Get("dryRun") != "All" {
		writeStatus(w, http.StatusBadRequest, "BadRequest", "the fake API server only dry-runs applies")
		return
	}
	manager := q.Get("fieldManager")
	var applied map[string]any
	body, err := io.ReadAll(r.Body)
	if err == nil {
		err = yaml.Unmarshal(body, &applied)
	}
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	live := map[string]any{}
	if o := f.get(resource, name); o != nil {
		b, _ := json.Marshal(o)
		_ = json.Unmarshal(b, &live)
	}
	var entries []map[string]any
	if md, ok := live["metadata"].(map[string]any); ok {
		for _, e := range asList(md["managedFields"]) {
			if m, ok := e.(map[string]any); ok && !(m["manager"] == manager && m["operation"] == "Apply") {
				entries = append(entries, m)
			}
		}
	}
	var leaves [][]string
	var causes []metav1.StatusCause
	var taken [][]string
	merged, _ := ssaMerge(live, len(live) > 0, applied, nil, func(path []string, before any, inLive bool, value any) {
		if !managedPath(path) {
			return
		}
		leaves = append(leaves, path)
		if !inLive || jsonSame(before, value) {
			return
		}
		for _, e := range entries {
			if fieldsHave(e["fieldsV1"], path) {
				sub := ""
				if s, _ := e["subresource"].(string); s != "" {
					sub = fmt.Sprintf(" with subresource %q", s)
				}
				causes = append(causes, metav1.StatusCause{
					Type:    metav1.CauseTypeFieldManagerConflict,
					Message: fmt.Sprintf("conflict with %q%s using %s", e["manager"], sub, e["apiVersion"]),
					Field:   ssaField(path),
				})
				taken = append(taken, path)
			}
		}
	}).(map[string]any)
	if len(causes) > 0 && q.Get("force") != "true" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		writeJSON(f.t, w, metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure, Code: http.StatusConflict, Reason: metav1.StatusReasonConflict,
			Message: fmt.Sprintf("Apply failed with %d conflicts", len(causes)),
			Details: &metav1.StatusDetails{Causes: causes},
		})
		return
	}
	md, _ := merged["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		merged["metadata"] = md
	}
	md["namespace"] = "shop"
	if resource == "deployments" {
		labels, _ := md["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			md["labels"] = labels
		}
		labels["policy.example.com/scanned"] = "true"
		leaves = append(leaves, []string{"f:metadata", "f:labels", "f:policy.example.com/scanned"})
		if spec, ok := merged["spec"].(map[string]any); ok && spec["revisionHistoryLimit"] == nil {
			spec["revisionHistoryLimit"] = 10
		}
	}
	kept := []any{}
	for _, e := range entries {
		set, _ := e["fieldsV1"].(map[string]any)
		for _, p := range taken {
			fieldsDelete(set, p)
		}
		if len(set) > 0 {
			kept = append(kept, e)
		}
	}
	md["managedFields"] = append(kept, map[string]any{
		"manager": manager, "operation": "Apply", "apiVersion": applied["apiVersion"],
		"time": "2026-11-21T10:00:00Z", "fieldsType": "FieldsV1", "fieldsV1": fieldsOf(leaves),
	})
	writeJSON(f.t, w, merged)
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// managedPath reports whether server-side apply records a field: not the
// apiVersion, the kind, or the name and namespace that say which object it
// is.
func managedPath(path []string) bool {
	switch strings.Join(path, ".") {
	case "f:apiVersion", "f:kind", "f:metadata.f:name", "f:metadata.f:namespace":
		return false
	}
	return true
}

// ssaMerge merges applied into live, calling leaf with the FieldsV1 path,
// the live value and the applied value of every field the apply sets.
func ssaMerge(live any, inLive bool, applied any, path []string, leaf func(path []string, live any, inLive bool, applied any)) any {
	at := func(step string) []string { return append(slices.Clip(path), step) }
	switch av := applied.(type) {
	case map[string]any:
		lm, _ := live.(map[string]any)
		out := maps.Clone(lm)
		if out == nil {
			out = map[string]any{}
		}
		for k, v := range av {
			lv, ok := lm[k]
			out[k] = ssaMerge(lv, ok, v, at("f:"+k), leaf)
		}
		return out
	case []any:
		ll, _ := live.([]any)
		an, aok := fakeItemNames(av)
		ln, lok := fakeItemNames(ll)
		if aok && lok {
			out := slices.Clone(ll)
			for i, it := range av {
				step := fmt.Sprintf(`k:{"name":%q}`, an[i])
				if j := slices.Index(ln, an[i]); j >= 0 {
					out[j] = ssaMerge(ll[j], true, it, at(step), leaf)
				} else {
					out = append(out, ssaMerge(nil, false, it, at(step), leaf))
				}
			}
			return out
		}
	}
	leaf(path, live, inLive, applied)
	return applied
}

func fakeItemNames(list []any) ([]string, bool) {
	var names []string
	for _, it := range list {
		m, ok := it.(map[string]any)
		name, isString := m["name"].(string)
		if !ok || !isString {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}

func jsonSame(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// fieldsHave reports whether a FieldsV1 set holds path, or a field above it
// whole.
func fieldsHave(set any, path []string) bool {
	node, _ := set.(map[string]any)
	for _, step := range path {
		next, ok := node[step].(map[string]any)
		if !ok {
			return false
		}
		if len(next) == 0 {
			return true
		}
		node = next
	}
	return true
}

// fieldsDelete takes path out of a FieldsV1 set, with any node left empty,
// and reports whether node is empty after.
func fieldsDelete(node map[string]any, path []string) bool {
	if len(path) == 0 || node == nil {
		return len(node) == 0
	}
	child, ok := node[path[0]].(map[string]any)
	if !ok {
		return len(node) == 0
	}
	if len(path) == 1 || fieldsDelete(child, path[1:]) {
		delete(node, path[0])
	}
	if len(node) == 1 && node["."] != nil {
		delete(node, ".")
	}
	return len(node) == 0
}

// fieldsOf builds a FieldsV1 set from the paths of the fields applied.
func fieldsOf(paths [][]string) map[string]any {
	set := map[string]any{}
	for _, p := range paths {
		node := set
		for _, step := range p {
			next, ok := node[step].(map[string]any)
			if !ok {
				next = map[string]any{}
				node[step] = next
			}
			if strings.HasPrefix(step, "k:") {
				next["."] = map[string]any{}
			}
			node = next
		}
	}
	return set
}

// ssaField writes a FieldsV1 path as conflict messages do.
func ssaField(path []string) string {
	var b strings.Builder
	for _, step := range path {
		switch {
		case strings.HasPrefix(step, "f:"):
			b.WriteByte('.')
			b.WriteString(step[2:])
		case strings.HasPrefix(step, "k:"):
			var keys map[string]any
			_ = json.Unmarshal([]byte(step[2:]), &keys)
			v, _ := json.Marshal(keys["name"])
			fmt.Fprintf(&b, "[name=%s]", v)
		}
	}
	return b.String()
}

// diffCluster is one fake cluster for tug diff and tug drift.
type diffCluster struct {
	gitVersion string
	// objects are served in the shop namespace.
	objects string
	// crds are served as CustomResourceDefinitions, and with them the
	// db.example.com API group; without any, the cluster does not serve
	// that group.
	crds []string
}

// diffRequests is what a fake cluster saw.
type diffRequests struct {
	writes atomic.Int32
	mu     sync.Mutex
	// secretReads are requests for Secrets that would carry their data.
	secretReads []string
	// applies are the query strings of server-side applies.
	applies []string
}

func (d *diffRequests) note(r *http.Request) {
	dryRun := r.URL.Query().Get("dryRun") == "All"
	if r.Method != http.MethodGet && !dryRun && !strings.Contains(r.URL.Path, "/selfsubject") {
		d.writes.Add(1)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(r.URL.Path, "/secrets") && !strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata") {
		d.secretReads = append(d.secretReads, r.Method+" "+r.URL.Path)
	}
	if r.Method == http.MethodPatch {
		d.applies = append(d.applies, r.URL.RawQuery)
	}
}

// check fails the test if tug wrote anything or read a Secret's data.
func (d *diffRequests) check(t *testing.T, cluster string) {
	t.Helper()
	if n := d.writes.Load(); n != 0 {
		t.Errorf("%s: %d writes, want none", cluster, n)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.secretReads) > 0 {
		t.Errorf("%s: Secrets read with their data: %q", cluster, d.secretReads)
	}
}

// objectsOf parses YAML documents into the objects a fake cluster serves.
func objectsOf(t *testing.T, docs string) []runtime.Object {
	t.Helper()
	var out []runtime.Object
	for _, doc := range strings.Split(docs, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("fixture: %v\n%s", err, doc)
		}
		if len(m) > 0 {
			out = append(out, &unstructured.Unstructured{Object: m})
		}
	}
	return out
}

func diffAPIServer(t *testing.T, c diffCluster) (*httptest.Server, *diffRequests) {
	t.Helper()
	reqs := &diffRequests{}
	shop := newFakeNamespace(t, objectsOf(t, c.objects)...)
	disc := maps.Clone(fakeDiscovery)
	if len(c.crds) == 0 {
		var groups metav1.APIGroupList
		if err := json.Unmarshal([]byte(disc["/apis"]), &groups); err != nil {
			t.Fatal(err)
		}
		groups.Groups = slices.DeleteFunc(groups.Groups, func(g metav1.APIGroup) bool { return g.Name == "db.example.com" })
		b, _ := json.Marshal(groups)
		disc["/apis"] = string(b)
		delete(disc, "/apis/db.example.com/v1")
	}
	crds := []any{}
	for _, name := range c.crds {
		crds = append(crds, map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}}},
		})
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reqs.note(r)
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		if ns, parts, ok := namespacedParts(p); ok && ns == "shop" {
			shop.serve(w, r, parts)
			return
		}
		if body, ok := disc[p]; ok {
			fmt.Fprint(w, body)
			return
		}
		switch p {
		case "/version":
			fmt.Fprintf(w, `{"major":"1","minor":"35","gitVersion":%q}`, c.gitVersion)
		case "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
			writeJSON(t, w, map[string]any{"kind": "CustomResourceDefinitionList", "apiVersion": "apiextensions.k8s.io/v1", "metadata": map[string]any{}, "items": crds})
		default:
			writeStatus(w, http.StatusNotFound, "NotFound", p+" not served")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

// The storefront on prod: applied with kubectl --server-side, scaled by an
// autoscaler through the scale subresource and then hand-edited; a
// ConfigMap applied with kubectl, the CA bundle Kubernetes writes in every
// namespace, a Secret and a Service whose cluster IP the API server
// allocated.
const prodShop = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: storefront
  namespace: shop
  uid: storefront-prod
  resourceVersion: "48211"
  generation: 7
  labels:
    app: storefront
  annotations:
    deployment.kubernetes.io/revision: "7"
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: apps/v1
    time: "2026-11-20T10:00:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:app: {}
      f:spec:
        f:selector: {}
        f:template:
          f:metadata:
            f:labels:
              f:app: {}
          f:spec:
            f:containers:
              k:{"name":"web"}:
                .: {}
                f:image: {}
                f:name: {}
  - manager: kube-controller-manager
    operation: Update
    apiVersion: apps/v1
    subresource: scale
    time: "2026-11-21T08:00:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:replicas: {}
  - manager: kube-controller-manager
    operation: Update
    apiVersion: apps/v1
    time: "2026-11-21T08:00:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:deployment.kubernetes.io/revision: {}
  - manager: kubectl-edit
    operation: Update
    apiVersion: apps/v1
    time: "2026-11-21T07:30:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:template:
          f:spec:
            f:containers:
              k:{"name":"web"}:
                f:env:
                  .: {}
                  k:{"name":"LOG_LEVEL"}:
                    .: {}
                    f:name: {}
                    f:value: {}
spec:
  replicas: 5
  selector:
    matchLabels:
      app: storefront
  template:
    metadata:
      labels:
        app: storefront
    spec:
      containers:
      - name: web
        image: registry.example.com/storefront:1.4.2
        imagePullPolicy: IfNotPresent
        env:
        - name: LOG_LEVEL
          value: debug
status:
  replicas: 5
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: flags
  namespace: shop
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:checkout: {}
data:
  checkout: "on"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kube-root-ca.crt
  namespace: shop
  managedFields:
  - manager: kube-controller-manager
    operation: Update
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:ca.crt: {}
data:
  ca.crt: prod-ca
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: prod-only
  namespace: shop
data:
  a: b
---
apiVersion: v1
kind: Secret
metadata:
  name: api-key
  namespace: shop
  labels:
    rotated: 2026-11
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:password: {}
      f:metadata:
        f:labels:
          f:rotated: {}
data:
  password: aHVudGVyMg==
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:ports:
          k:{"port":80,"protocol":"TCP"}:
            .: {}
            f:port: {}
            f:protocol: {}
spec:
  clusterIP: 10.0.0.10
  clusterIPs: [10.0.0.10]
  ports:
  - port: 80
    protocol: TCP
`

// The same on staging: a newer image, its own autoscaler's replicas, no
// hand edits, another CA, another Secret and another cluster IP.
const stagingShop = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: storefront
  namespace: shop
  uid: storefront-staging
  generation: 3
  labels:
    app: storefront
  annotations:
    deployment.kubernetes.io/revision: "3"
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: apps/v1
    time: "2026-11-21T09:00:00Z"
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:app: {}
      f:spec:
        f:selector: {}
        f:template:
          f:metadata:
            f:labels:
              f:app: {}
          f:spec:
            f:containers:
              k:{"name":"web"}:
                .: {}
                f:image: {}
                f:name: {}
  - manager: kube-controller-manager
    operation: Update
    apiVersion: apps/v1
    subresource: scale
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:replicas: {}
  - manager: kube-controller-manager
    operation: Update
    apiVersion: apps/v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:deployment.kubernetes.io/revision: {}
spec:
  replicas: 2
  selector:
    matchLabels:
      app: storefront
  template:
    metadata:
      labels:
        app: storefront
    spec:
      containers:
      - name: web
        image: registry.example.com/storefront:1.5.0
        imagePullPolicy: IfNotPresent
status:
  replicas: 2
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: flags
  namespace: shop
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:checkout: {}
data:
  checkout: "off"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kube-root-ca.crt
  namespace: shop
  managedFields:
  - manager: kube-controller-manager
    operation: Update
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:ca.crt: {}
data:
  ca.crt: staging-ca
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: staging-only
  namespace: shop
data:
  a: b
---
apiVersion: v1
kind: Secret
metadata:
  name: api-key
  namespace: shop
  labels:
    rotated: 2026-10
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:password: {}
      f:metadata:
        f:labels:
          f:rotated: {}
data:
  password: c3RhZ2luZw==
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  managedFields:
  - manager: kubectl
    operation: Apply
    apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:ports:
          k:{"port":80,"protocol":"TCP"}:
            .: {}
            f:port: {}
            f:protocol: {}
spec:
  clusterIP: 10.1.0.10
  clusterIPs: [10.1.0.10]
  ports:
  - port: 80
    protocol: TCP
`

// diffClusters imports prod, staging and dr, a second name for prod's
// server, connects this session to prod and returns what each server saw.
func diffClusters(t *testing.T) (prod, staging *diffRequests) {
	t.Helper()
	isolate(t)
	prodSrv, prod := diffAPIServer(t, diffCluster{gitVersion: "v1.35.1", objects: prodShop, crds: []string{"backups.db.example.com", "databases.db.example.com"}})
	stagingSrv, staging := diffAPIServer(t, diffCluster{gitVersion: "v1.34.2", objects: stagingShop})
	ca := func(s *httptest.Server) string {
		return b64(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	}
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: %s, certificate-authority-data: %s}
- name: staging
  cluster: {server: %s, certificate-authority-data: %s}
users:
- name: u
  user: {token: %s}
contexts:
- name: payments-prod
  context: {cluster: prod, user: u}
- name: payments-staging
  context: {cluster: staging, user: u}
- name: payments-dr
  context: {cluster: prod, user: u}
current-context: payments-prod
`, prodSrv.URL, ca(prodSrv), stagingSrv.URL, ca(stagingSrv), secretToken)
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "import", "kubeconfig", path)
	newSession(t)
	if out := mustTug(t, "connect", "payments-prod"); !strings.Contains(out, "read-only") {
		t.Fatalf("payments-prod should open read-only, so the tests show dry runs pass the guard: %s", out)
	}
	return prod, staging
}

// storefrontManifest is what a person would apply to prod: a new image and
// three replicas, the flags ConfigMap as it is, a Secret and a new
// ConfigMap.
const storefrontManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: storefront
  labels:
    app: storefront
spec:
  replicas: 3
  selector:
    matchLabels:
      app: storefront
  template:
    metadata:
      labels:
        app: storefront
    spec:
      containers:
      - name: web
        image: registry.example.com/storefront:1.5.0
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: flags
data:
  checkout: "on"
---
apiVersion: v1
kind: Secret
metadata:
  name: api-key
stringData:
  password: hunter3
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: new-flags
data:
  search: "on"
`

func writeManifest(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "storefront.yaml")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiffFile(t *testing.T) {
	prod, _ := diffClusters(t)
	file := writeManifest(t, storefrontManifest)

	r := tug(t, "diff", "-f", file, "-n", "shop", "--field-manager", "kubectl")
	if r.code != 1 {
		t.Fatalf("tug diff -f: exit %d, want 1 for a difference\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"deployment/storefront in shop\n--- payments-prod (live)\n+++ " + file + " (server-side dry run as kubectl)\n@@ ",
		"\n-  replicas: 5\n+  replicas: 3\n",
		"\n+  revisionHistoryLimit: 10\n",
		"\n-        image: registry.example.com/storefront:1.4.2\n+        image: registry.example.com/storefront:1.5.0\n",
		// The hand-edited env stays: the apply does not own it.
		"\n         - name: LOG_LEVEL\n",
		"configmap/new-flags in shop\n--- payments-prod (not there yet)\n",
		"\n+data:\n+  search: \"on\"\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("tug diff -f lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, row := range [][]string{
		{"FIELD", "LIVE OWNERS", "OWNERS AFTER", "NOTE"},
		{"metadata.labels.policy.example.com/scanned", "(not set)", "kubectl", "set by admission"},
		{"spec.replicas", "kube-controller-manager (scale)", "kubectl", "conflicts with kube-controller-manager"},
		{"spec.revisionHistoryLimit", "(not set)", "-", "defaulted by the API server"},
		{`spec.template.spec.containers[name="web"].image`, "kubectl", "kubectl"},
		{"(the whole object)", "(not set)", "kubectl"},
	} {
		if !hasRow(r.stdout, row...) {
			t.Errorf("tug diff -f lacks the row %q:\n%s", row, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "configmap/flags") || strings.Contains(r.stdout, "hunter") || strings.Contains(r.stdout, "aHVudGVy") {
		t.Errorf("tug diff -f shows the unchanged ConfigMap or the Secret:\n%s", r.stdout)
	}
	for _, want := range []string{
		"configmap/flags in shop: no differences between payments-prod (live) and ",
		"warning: skipped secret/api-key from " + file + ": a dry run returns a Secret's data, which tug never reads\n",
		"warning: an apply as kubectl would conflict with the field managers named above; the dry run was forced",
		"note: a dry run shows what admission and defaulting make of an apply; it does not predict what controllers do",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("tug diff -f stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	// A target picks one object from the file; the default manager is tug,
	// which conflicts with kubectl over the image too.
	r = tug(t, "diff", "storefront", "-f", file, "-n", "shop", "-o", "json")
	if r.code != 1 {
		t.Fatalf("tug diff storefront -f -o json: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var results []diff.Result
	if err := json.Unmarshal([]byte(r.stdout), &results); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if len(results) != 1 || results[0].Object != "deployment/storefront" || results[0].Same || results[0].Diff == "" {
		t.Fatalf("tug diff storefront -f -o json = %+v", results)
	}
	var conflicts []string
	for _, c := range results[0].Conflicts {
		conflicts = append(conflicts, c.Manager+" "+c.Field)
	}
	slices.Sort(conflicts)
	if want := []string{"kube-controller-manager spec.replicas", `kubectl spec.template.spec.containers[name="web"].image`}; !slices.Equal(conflicts, want) {
		t.Errorf("conflicts as tug = %q, want %q", conflicts, want)
	}

	// Nothing to change: exit 0 and nothing on stdout, as kubectl diff.
	same := writeManifest(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: flags}\ndata: {checkout: \"on\"}\n")
	if r := tug(t, "diff", "-f", same, "-n", "shop"); r.code != 0 || r.stdout != "" {
		t.Errorf("tug diff -f of an unchanged object: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	prod.mu.Lock()
	applies := slices.Clone(prod.applies)
	prod.mu.Unlock()
	if len(applies) == 0 {
		t.Fatal("no dry-run apply reached the API server")
	}
	for _, q := range applies {
		if !strings.Contains(q, "dryRun=All") || !strings.Contains(q, "fieldManager=") {
			t.Errorf("an apply without dryRun=All and a field manager: %s", q)
		}
	}
	prod.check(t, "payments-prod")
}

// hasRow reports whether out has a table row with these cells in order,
// separated only by the spaces the table pads with.
func hasRow(out string, cells ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Join(strings.Fields(line), " ") == strings.Join(strings.Fields(strings.Join(cells, " ")), " ") {
			return true
		}
	}
	return false
}

func TestDiffExitCodes(t *testing.T) {
	prod, _ := diffClusters(t)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	other := writeManifest(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: other}\n")
	for _, tt := range []struct{ args, want string }{
		{"diff -f " + missing, "missing.yaml"},
		{"diff --no-such-flag -f x", "unknown flag: --no-such-flag"},
		{"diff a b -f x", "accepts at most 1 arg(s)"},
		{"diff deploy/storefront -n shop", "say what to compare with"},
		{"diff -f x --against payments-staging", "say what to compare with"},
		{"diff --managed-fields", "name the object to compare"},
		{"diff deploy/storefront -n shop --against payments-prod", "--against names payments-prod, which this terminal is connected to"},
		{"diff deploy/storefront -n shop --against nowhere", `no cluster matches "nowhere"`},
		{"diff deploy/storefront -n shop -f x --all-fields", "--all-fields works only with --against"},
		{"diff storefront -n shop -f " + other, "has no deployment/storefront in namespace shop"},
		{"diff widget/x -n shop --managed-fields", `the cluster has no resource type "widget"`},
		{"drift", "name the cluster to compare with"},
		{"drift --against payments-staging -n shop -A", "-n and -A cannot be used together"},
		{"drift --against payments-staging -n shop widgets", `the cluster has no resource type "widgets"`},
		{"drift --against payments-staging -o name", "drift supports -o table, json or yaml"},
	} {
		r := tug(t, strings.Fields(tt.args)...)
		if r.code != 2 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s: exit %d, stderr %q; want exit 2 and %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	prod.check(t, "payments-prod")
}

func TestDiffAgainst(t *testing.T) {
	prod, staging := diffClusters(t)

	r := tug(t, "diff", "deploy/storefront", "-n", "shop", "--against", "payments-staging")
	if r.code != 1 {
		t.Fatalf("tug diff --against: exit %d, want 1\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"deployment/storefront in shop\n--- payments-prod\n+++ payments-staging\n@@ ",
		// YAML writes an item's first field on its "- " line.
		"\n-      - env:\n-        - name: LOG_LEVEL\n-          value: debug\n-        image: registry.example.com/storefront:1.4.2\n" +
			"+      - image: registry.example.com/storefront:1.5.0\n         imagePullPolicy: IfNotPresent\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("tug diff --against lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, gone := range []string{"replicas", "revision", "uid", "generation"} {
		if strings.Contains(r.stdout, gone) {
			t.Errorf("tug diff --against shows %s, which differs between clusters by design:\n%s", gone, r.stdout)
		}
	}
	for _, row := range [][]string{
		{"FIELD", "OWNERS ON PAYMENTS-PROD", "OWNERS ON PAYMENTS-STAGING", "NOTE"},
		{`spec.template.spec.containers[name="web"].env`, "kubectl-edit", "(not set)"},
		{`spec.template.spec.containers[name="web"].image`, "kubectl", "kubectl", "conflicts with kubectl"},
	} {
		if !hasRow(r.stdout, row...) {
			t.Errorf("tug diff --against lacks the row %q:\n%s", row, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, "note: left out 2 fields that only the clusters set themselves; --all-fields shows them") {
		t.Errorf("tug diff --against stderr:\n%s", r.stderr)
	}

	r = tug(t, "diff", "deploy/storefront", "-n", "shop", "--against", "payments-staging", "--all-fields")
	if r.code != 1 || !strings.Contains(r.stdout, "\n-  replicas: 5\n+  replicas: 2\n") ||
		!hasRow(r.stdout, "spec.replicas", "kube-controller-manager (scale)", "kube-controller-manager (scale)", "conflicts with kube-controller-manager") {
		t.Errorf("tug diff --against --all-fields: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	// The Service differs only in what each API server allocated.
	if r := tug(t, "diff", "svc/web", "-n", "shop", "--against", "payments-staging"); r.code != 0 || r.stdout != "" ||
		!strings.Contains(r.stderr, "service/web in shop: no differences between payments-prod and payments-staging") {
		t.Errorf("tug diff svc/web --against: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	// An object the other cluster lacks is all removed.
	r = tug(t, "diff", "cm/prod-only", "-n", "shop", "--against", "payments-staging")
	if r.code != 1 || !strings.Contains(r.stdout, "@@ -1,7 +0,0 @@\n-apiVersion: v1\n") ||
		!strings.Contains(r.stderr, "warning: configmap/prod-only is not on payments-staging\n") {
		t.Errorf("tug diff cm/prod-only --against: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	// A Secret is compared by its metadata alone.
	r = tug(t, "diff", "secret/api-key", "-n", "shop", "--against", "payments-staging", "-o", "json")
	if r.code != 1 {
		t.Fatalf("tug diff secret/api-key --against: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var res diff.Result
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path.String() != "metadata.labels.rotated" || strings.Contains(r.stdout, "password") {
		t.Errorf("tug diff secret/api-key --against -o json:\n%s", r.stdout)
	}

	prod.check(t, "payments-prod")
	staging.check(t, "payments-staging")
}

func TestDiffManagedFields(t *testing.T) {
	prod, _ := diffClusters(t)
	out := mustTug(t, "diff", "deploy/storefront", "-n", "shop", "--managed-fields")
	if !strings.HasPrefix(out, "deployment/storefront in shop has 4 field managers\n") {
		t.Errorf("tug diff --managed-fields:\n%s", out)
	}
	for _, row := range [][]string{
		{"MANAGER", "OPERATION", "SUBRESOURCE", "UPDATED", "FIELDS"},
		{"kube-controller-manager", "Update", "scale"},
	} {
		if !slices.ContainsFunc(strings.Split(out, "\n"), func(line string) bool {
			return strings.HasPrefix(strings.Join(strings.Fields(line), " "), strings.Join(row, " "))
		}) {
			t.Errorf("tug diff --managed-fields lacks the row %q:\n%s", row, out)
		}
	}
	for _, want := range []string{
		"\nkubectl (Apply) owns:\n  metadata.labels.app\n  spec.selector\n",
		"\nkubectl-edit (Update) owns:\n  spec.template.spec.containers[name=\"web\"].env[name=\"LOG_LEVEL\"].name\n",
		"\nkube-controller-manager (Update, scale) owns:\n  spec.replicas\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug diff --managed-fields lacks %q:\n%s", want, out)
		}
	}

	var v managedFieldsView
	if err := json.Unmarshal([]byte(mustTug(t, "diff", "secret/api-key", "-n", "shop", "--managed-fields", "-o", "json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Object != "secret/api-key" || len(v.Managers) != 1 || v.Managers[0].Manager != "kubectl" ||
		!slices.Equal(v.Managers[0].Fields, []string{"data.password", "metadata.labels.rotated"}) {
		t.Errorf("tug diff secret/api-key --managed-fields -o json = %+v", v)
	}
	prod.check(t, "payments-prod")
}

func TestDrift(t *testing.T) {
	prod, staging := diffClusters(t)

	r := tug(t, "drift", "--against", "payments-staging", "-n", "shop")
	if r.code != 1 {
		t.Fatalf("tug drift: exit %d, want 1\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"payments-prod (v1.35.1) against payments-staging (v1.34.2), namespace shop\n",
		"\nAPI versions only on payments-prod: db.example.com/v1\n",
		"\nDrift: 4 of 6 objects of deployments.apps, statefulsets.apps, daemonsets.apps, services, configmaps differ or are on one cluster only\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("tug drift lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, row := range [][]string{
		{"CRD", "PAYMENTS-PROD", "PAYMENTS-STAGING"},
		{"backups.db.example.com", "v1", "-"},
		{"databases.db.example.com", "v1", "-"},
		{"KIND", "NAME", "STATE", "FIELDS"},
		{"configmap", "flags", "differs", "data.checkout"},
		{"configmap", "prod-only", "only on payments-prod", "-"},
		{"configmap", "staging-only", "only on payments-staging", "-"},
		{"deployment", "storefront", "differs", `spec.template.spec.containers[name="web"].env, spec.template.spec.containers[name="web"].image`},
	} {
		if !hasRow(r.stdout, row...) {
			t.Errorf("tug drift lacks the row %q:\n%s", row, r.stdout)
		}
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.Contains(line, "kube-root-ca.crt") || strings.HasPrefix(line, "service ") {
			t.Errorf("tug drift shows an object that differs only in what the clusters set: %q", line)
		}
	}
	if !strings.Contains(r.stderr, "hint: `tug diff <kind>/<name> --against payments-staging` shows an object's differences in full") {
		t.Errorf("tug drift stderr:\n%s", r.stderr)
	}

	r = tug(t, "drift", "--against", "payments-staging", "-n", "shop", "secrets", "-o", "json")
	if r.code != 1 {
		t.Fatalf("tug drift secrets -o json: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var d diff.Drift
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatal(err)
	}
	if d.Same || !slices.Equal(d.Resources, []string{"secrets"}) || len(d.Objects) != 1 ||
		d.Objects[0].Name != "api-key" || d.Objects[0].Changes[0].Path.String() != "metadata.labels.rotated" || strings.Contains(r.stdout, "password") {
		t.Errorf("tug drift secrets -o json:\n%s", r.stdout)
	}

	// payments-dr is prod under another name: nothing drifts.
	r = tug(t, "drift", "--against", "payments-dr", "-n", "shop", "deploy", "cm", "secrets", "svc")
	if r.code != 0 || !strings.Contains(r.stdout, "\nNo drift: 6 objects of deployments.apps, configmaps, secrets, services, the CRDs and the API versions match\n") {
		t.Errorf("tug drift --against payments-dr: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	prod.check(t, "payments-prod")
	staging.check(t, "payments-staging")
}
