package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/paingha/tugcli/internal/apiinfo"
)

// apiGroupVersion is one group-version an API fake serves, with its
// resources.
type apiGroupVersion struct {
	gv        string
	resources []metav1.APIResource
}

func apiRes(name, kind string, namespaced bool, short ...string) metav1.APIResource {
	return metav1.APIResource{
		Name: name, Kind: kind, Namespaced: namespaced, ShortNames: short, SingularName: strings.ToLower(kind),
		Verbs: metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete"},
	}
}

// discoveryPaths answers legacy discovery for gvs; a group's first
// group-version is its preferred one.
func discoveryPaths(gvs ...apiGroupVersion) map[string]string {
	paths := map[string]string{}
	var core []string
	var groups []metav1.APIGroup
	index := map[string]int{}
	for _, g := range gvs {
		list := metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: g.gv, APIResources: g.resources}
		b, _ := json.Marshal(list)
		group, version, ok := strings.Cut(g.gv, "/")
		if !ok {
			core = append(core, g.gv)
			paths["/api/"+g.gv] = string(b)
			continue
		}
		paths["/apis/"+g.gv] = string(b)
		v := metav1.GroupVersionForDiscovery{GroupVersion: g.gv, Version: version}
		i, seen := index[group]
		if !seen {
			index[group] = len(groups)
			groups = append(groups, metav1.APIGroup{Name: group, PreferredVersion: v})
			i = len(groups) - 1
		}
		groups[i].Versions = append(groups[i].Versions, v)
	}
	b, _ := json.Marshal(metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: core,
		ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{{ClientCIDR: "0.0.0.0/0", ServerAddress: "127.0.0.1"}}})
	paths["/api"] = string(b)
	b, _ = json.Marshal(metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}, Groups: groups})
	paths["/apis"] = string(b)
	return paths
}

// apiFake is a cluster for the api and crd commands.
type apiFake struct {
	gitVersion string
	discovery  map[string]string
	crds       []map[string]any
	// objects are the metadata lists the cluster answers, by path.
	objects map[string][]metav1.PartialObjectMetadata
}

// widgetCRD is a definition of widgets.example.com that serves v1 and a
// deprecated v1beta1, stores storage and records storedVersions.
func widgetCRD(storage string, stored ...string) map[string]any {
	version := func(name string, deprecated bool, schema map[string]any) map[string]any {
		v := map[string]any{"name": name, "served": true, "storage": name == storage, "schema": map[string]any{"openAPIV3Schema": schema}}
		if deprecated {
			v["deprecated"] = true
			v["deprecationWarning"] = "example.com/v1beta1 Widget is deprecated; use example.com/v1"
		}
		return v
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	v1 := version("v1", false, map[string]any{
		"type": "object", "description": "Widget is a thing that does something. It is made of parts.",
		"properties": map[string]any{
			"apiVersion": str(""), "kind": str(""), "metadata": map[string]any{"type": "object"},
			"spec": map[string]any{
				"type": "object", "description": "Spec is what the widget should be.", "required": []any{"size"},
				"properties": map[string]any{
					"size": map[string]any{"type": "string", "description": "Size of the widget.\n\nLarge widgets need more room.",
						"enum": []any{"small", "large"}, "default": "small"},
					"parts": map[string]any{"type": "array", "description": "Parts the widget is made of.",
						"items": map[string]any{"type": "object", "required": []any{"name"},
							"properties": map[string]any{"name": str("Name of the part."), "count": map[string]any{"type": "integer"}}}},
					"labels": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				},
			},
		},
	})
	v1["subresources"] = map[string]any{"status": map[string]any{}}
	v1["additionalPrinterColumns"] = []any{
		map[string]any{"name": "Size", "type": "string", "jsonPath": ".spec.size"},
		map[string]any{"name": "Age", "type": "date", "jsonPath": ".metadata.creationTimestamp", "priority": 1},
	}
	beta := version("v1beta1", true, map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true})
	storedAny := make([]any, len(stored))
	for i, s := range stored {
		storedAny[i] = s
	}
	return map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "widgets.example.com"},
		"spec": map[string]any{
			"group": "example.com", "scope": "Namespaced",
			"names":      map[string]any{"kind": "Widget", "listKind": "WidgetList", "plural": "widgets", "singular": "widget", "shortNames": []any{"wd"}},
			"versions":   []any{v1, beta},
			"conversion": map[string]any{"strategy": "Webhook", "webhook": map[string]any{"conversionReviewVersions": []any{"v1"}, "clientConfig": map[string]any{"service": map[string]any{"namespace": "widgets", "name": "widget-webhook", "path": "/convert"}}}},
		},
		"status": map[string]any{"storedVersions": storedAny, "conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}
}

func certificateCRD() map[string]any {
	return map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{
			"group": "cert-manager.io", "scope": "Namespaced",
			"names":    map[string]any{"kind": "Certificate", "plural": "certificates", "singular": "certificate", "shortNames": []any{"cert"}},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
		},
		"status": map[string]any{"storedVersions": []any{"v1"}},
	}
}

// written is an object's metadata, last written at versions.
func written(name string, versions ...string) metav1.PartialObjectMetadata {
	m := metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "web"}}
	for _, v := range versions {
		m.ManagedFields = append(m.ManagedFields, metav1.ManagedFieldsEntry{Manager: "helm", APIVersion: v})
	}
	return m
}

// oldAPIs is a 1.24 cluster that still serves versions later releases
// removed, with a deprecated widget version that holds objects.
func oldAPIs() apiFake {
	return apiFake{
		gitVersion: "v1.24.17",
		discovery: discoveryPaths(
			apiGroupVersion{"v1", []metav1.APIResource{apiRes("pods", "Pod", true, "po"), apiRes("namespaces", "Namespace", false, "ns")}},
			apiGroupVersion{"apps/v1", []metav1.APIResource{apiRes("deployments", "Deployment", true, "deploy")}},
			apiGroupVersion{"autoscaling/v2", []metav1.APIResource{apiRes("horizontalpodautoscalers", "HorizontalPodAutoscaler", true, "hpa")}},
			apiGroupVersion{"autoscaling/v1", []metav1.APIResource{apiRes("horizontalpodautoscalers", "HorizontalPodAutoscaler", true, "hpa")}},
			apiGroupVersion{"autoscaling/v2beta2", []metav1.APIResource{apiRes("horizontalpodautoscalers", "HorizontalPodAutoscaler", true, "hpa")}},
			apiGroupVersion{"policy/v1", []metav1.APIResource{apiRes("poddisruptionbudgets", "PodDisruptionBudget", true, "pdb")}},
			apiGroupVersion{"policy/v1beta1", []metav1.APIResource{apiRes("poddisruptionbudgets", "PodDisruptionBudget", true, "pdb"), apiRes("podsecuritypolicies", "PodSecurityPolicy", false, "psp")}},
			apiGroupVersion{"example.com/v1", []metav1.APIResource{apiRes("widgets", "Widget", true, "wd")}},
			apiGroupVersion{"example.com/v1beta1", []metav1.APIResource{apiRes("widgets", "Widget", true, "wd")}},
			apiGroupVersion{"apiextensions.k8s.io/v1", []metav1.APIResource{apiRes("customresourcedefinitions", "CustomResourceDefinition", false, "crd")}},
		),
		crds: []map[string]any{widgetCRD("v1", "v1beta1", "v1")},
		objects: map[string][]metav1.PartialObjectMetadata{
			"/apis/autoscaling/v2/horizontalpodautoscalers": {written("api", "autoscaling/v2beta2"), written("web", "autoscaling/v2")},
			"/apis/policy/v1/poddisruptionbudgets":          {written("api", "policy/v1")},
			"/apis/policy/v1beta1/podsecuritypolicies":      {},
			"/apis/example.com/v1/widgets":                  {written("w1", "example.com/v1beta1"), written("w2", "example.com/v1beta1")},
		},
	}
}

// newAPIs is a 1.35 cluster with nothing deprecated and cert-manager.
func newAPIs() apiFake {
	return apiFake{
		gitVersion: "v1.35.1",
		discovery: discoveryPaths(
			apiGroupVersion{"v1", []metav1.APIResource{apiRes("pods", "Pod", true, "po"), apiRes("namespaces", "Namespace", false, "ns")}},
			apiGroupVersion{"apps/v1", []metav1.APIResource{apiRes("deployments", "Deployment", true, "deploy")}},
			apiGroupVersion{"autoscaling/v2", []metav1.APIResource{apiRes("horizontalpodautoscalers", "HorizontalPodAutoscaler", true, "hpa")}},
			apiGroupVersion{"autoscaling/v1", []metav1.APIResource{apiRes("horizontalpodautoscalers", "HorizontalPodAutoscaler", true, "hpa")}},
			apiGroupVersion{"policy/v1", []metav1.APIResource{apiRes("poddisruptionbudgets", "PodDisruptionBudget", true, "pdb")}},
			apiGroupVersion{"cert-manager.io/v1", []metav1.APIResource{apiRes("certificates", "Certificate", true, "cert")}},
			apiGroupVersion{"example.com/v1", []metav1.APIResource{apiRes("widgets", "Widget", true, "wd")}},
			apiGroupVersion{"apiextensions.k8s.io/v1", []metav1.APIResource{apiRes("customresourcedefinitions", "CustomResourceDefinition", false, "crd")}},
		),
		crds: []map[string]any{certificateCRD(), func() map[string]any {
			c := widgetCRD("v1", "v1")
			spec := c["spec"].(map[string]any)
			spec["versions"] = spec["versions"].([]any)[:1]
			return c
		}()},
	}
}

const crdPath = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"

// serveAPIs starts a fake API server for f that, like fakeAPIServer,
// answers only callers with secretToken and counts writes.
func serveAPIs(t *testing.T, f apiFake) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			writes.Add(1)
			writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "the fake API server only reads")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		metadataOnly := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList")
		if body, ok := f.discovery[p]; ok {
			fmt.Fprint(w, body)
			return
		}
		switch {
		case p == "/version":
			fmt.Fprintf(w, `{"major":"1","minor":"%s","gitVersion":%q,"platform":"linux/amd64"}`, strings.Split(f.gitVersion, ".")[1], f.gitVersion)
		case p == crdPath && metadataOnly:
			items := []metav1.PartialObjectMetadata{}
			for _, c := range f.crds {
				items = append(items, metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: c["metadata"].(map[string]any)["name"].(string)}})
			}
			writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": items})
		case p == crdPath:
			writeJSON(t, w, map[string]any{"kind": "CustomResourceDefinitionList", "apiVersion": "apiextensions.k8s.io/v1", "metadata": map[string]any{}, "items": f.crds})
		case strings.HasPrefix(p, crdPath+"/"):
			name := strings.TrimPrefix(p, crdPath+"/")
			for _, c := range f.crds {
				if c["metadata"].(map[string]any)["name"] == name {
					writeJSON(t, w, c)
					return
				}
			}
			writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("customresourcedefinitions %q not found", name))
		case f.objects[p] != nil && metadataOnly:
			writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": f.objects[p]})
		default:
			writeStatus(w, http.StatusNotFound, "NotFound", p+" not served")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

// connectAPIs imports f's cluster as payments-prod and connects to it.
func connectAPIs(t *testing.T, f apiFake) *atomic.Int32 {
	t.Helper()
	isolate(t)
	srv, writes := serveAPIs(t, f)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	return writes
}

func TestAPIResources(t *testing.T) {
	writes := connectAPIs(t, oldAPIs())

	out := mustTug(t, "api", "resources")
	for _, want := range []string{
		"NAME                        SHORTNAMES   GROUP                  VERSIONS        SCOPE        KIND                       CATEGORIES\n",
		"namespaces                  ns           core                   v1              Cluster      Namespace                  -\n",
		"horizontalpodautoscalers    hpa          autoscaling            v2,v1,v2beta2   Namespaced   HorizontalPodAutoscaler    -\n",
		"widgets                     wd           example.com            v1,v1beta1      Namespaced   Widget                     -\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug api resources lacks %q:\n%s", want, out)
		}
	}
	if first := strings.Split(mustTug(t, "api", "resources", "pod"), "\n")[1]; !strings.HasPrefix(first, "pods ") {
		t.Errorf("tug api resources pod lists %q first, want pods", first)
	}
	for _, tt := range []struct{ args, want string }{
		{"api resources --group core -o name", "namespaces\npods\n"},
		{"api resources --scope cluster -o name", "namespaces\ncustomresourcedefinitions.apiextensions.k8s.io\npodsecuritypolicies.policy\n"},
		{"api resources hpa -o name", "horizontalpodautoscalers.autoscaling\n"},
	} {
		if got := mustTug(t, strings.Fields(tt.args)...); got != tt.want {
			t.Errorf("tug %s = %q, want %q", tt.args, got, tt.want)
		}
	}

	var rs []apiinfo.Resource
	if err := json.Unmarshal([]byte(mustTug(t, "api", "resources", "wd", "-o", "json")), &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Version != "v1" || !slices.Contains(rs[0].Verbs, "list") {
		t.Errorf("tug api resources wd -o json = %+v, want widgets with its verbs", rs)
	}

	for _, tt := range []struct{ args, want string }{
		{"api resources gadget", "no resource type matches \"gadget\"; `tug api resources` lists them all"},
		{"api resources --group policy --verb proxy", "no resource type matches group policy and verb proxy"},
		{"api resources --scope global", `unknown scope "global"`},
		{"api resource pods", `unknown command "resource" for "tug api"`},
		{"crd show widget", `unknown command "show" for "tug crd"`},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Errorf("api resources wrote %d times", writes.Load())
	}
}

func TestAPIVersions(t *testing.T) {
	connectAPIs(t, oldAPIs())
	out := mustTug(t, "api", "versions")
	for _, want := range []string{
		"API VERSION               PREFERRED   RESOURCES   REMOVED IN\n",
		"v1                        yes         2           \n",
		"autoscaling/v2            yes         1           \n",
		"autoscaling/v2beta2                   1           1.26\n",
		"policy/v1beta1                        2           1.25\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug api versions lacks %q:\n%s", want, out)
		}
	}
	names := strings.Fields(mustTug(t, "api", "versions", "-o", "name"))
	if names[0] != "v1" || !slices.Contains(names, "example.com/v1beta1") {
		t.Errorf("tug api versions -o name = %v", names)
	}
}

func TestAPIDeprecations(t *testing.T) {
	writes := connectAPIs(t, oldAPIs())
	out := mustTug(t, "api", "deprecations")
	for _, want := range []string{
		"API VERSION           KIND                      REMOVED IN   USE INSTEAD                   OBJECTS   NOTE\n",
		"autoscaling/v2beta2   HorizontalPodAutoscaler   1.26         autoscaling/v2 (since 1.23)   1         \n",
		"example.com/v1beta1   Widget                    -            example.com/v1                2         deprecated by its definition; objects may still be stored at it\n",
		"policy/v1beta1        PodDisruptionBudget       1.25         policy/v1 (since 1.21)        0         \n",
		"policy/v1beta1        PodSecurityPolicy         1.25         -                             0         no replacement API; use Pod Security Admission or an admission webhook\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug api deprecations lacks %q:\n%s", want, out)
		}
	}
	var report apiinfo.DeprecationReport
	if err := json.Unmarshal([]byte(mustTug(t, "api", "deprecations", "-o", "json")), &report); err != nil {
		t.Fatal(err)
	}
	if report.Server.Minor != "1.24" || len(report.Items) != 4 || report.Items[1].CRD != "widgets.example.com" || !report.Items[1].Stored {
		t.Errorf("tug api deprecations -o json = %+v", report)
	}
	if writes.Load() != 0 {
		t.Errorf("api deprecations wrote %d times", writes.Load())
	}

	connectAPIs(t, newAPIs())
	if out := mustTug(t, "api", "deprecations"); !strings.HasPrefix(out, "Nothing deprecated: v1.35.1 serves no API version") {
		t.Errorf("tug api deprecations on a current cluster:\n%s", out)
	}
}

func TestAPIServerVersion(t *testing.T) {
	connectAPIs(t, oldAPIs())
	out := mustTug(t, "api", "server-version")
	if !strings.HasPrefix(out, "v1.24.17: end of life since 2023-07-28; upgrade to 1.") || !strings.Contains(out, " releases behind 1.") {
		t.Errorf("tug api server-version = %q", out)
	}
	var v serverVersionView
	if err := json.Unmarshal([]byte(mustTug(t, "api", "server-version", "-o", "json")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Cluster != "payments-prod" || v.Minor != "1.24" || v.Platform != "linux/amd64" || v.Support.Status != apiinfo.EndOfLife || v.Support.EndOfLife != "2023-07-28" {
		t.Errorf("tug api server-version -o json = %+v", v)
	}
}

func TestAPIDiff(t *testing.T) {
	home := isolate(t)
	old, oldWrites := serveAPIs(t, oldAPIs())
	cur, curWrites := serveAPIs(t, newAPIs())
	twin, _ := serveAPIs(t, newAPIs())
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{
		"shop-staging-old": old, "shop-staging-new": cur, "shop-staging-twin": twin,
	}))

	// differ runs a diff that finds differences, which exits 1 as diff
	// does.
	differ := func(args ...string) string {
		t.Helper()
		r := tug(t, append([]string{"api", "diff"}, args...)...)
		if r.code != 1 || r.stderr != "" {
			t.Fatalf("tug api diff %v: exit %d, stderr %q; want exit 1 and nothing more on stderr\n%s", args, r.code, r.stderr, r.stdout)
		}
		return r.stdout
	}

	// No session is needed.
	out := differ("staging-old", "shop-staging-new")
	for _, want := range []string{
		"shop-staging-old runs v1.24.17, shop-staging-new runs v1.35.1\n\n",
		"RESOURCE                               KIND                      SHOP-STAGING-OLD         SHOP-STAGING-NEW\n",
		"horizontalpodautoscalers.autoscaling   HorizontalPodAutoscaler   v2,v1,v2beta2            v2,v1\n",
		"certificates.cert-manager.io           Certificate               -                        v1 (stores v1)\n",
		"widgets.example.com                    Widget                    v1,v1beta1 (stores v1)   v1 (stores v1)\n",
		"poddisruptionbudgets.policy            PodDisruptionBudget       v1,v1beta1               v1\n",
		"podsecuritypolicies.policy             PodSecurityPolicy         v1beta1                  -\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug api diff lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deployments.apps") {
		t.Errorf("tug api diff shows a resource served alike:\n%s", out)
	}

	var d apiinfo.Diff
	if err := json.Unmarshal([]byte(differ("shop-staging-old", "shop-staging-new", "-o", "json")), &d); err != nil {
		t.Fatal(err)
	}
	if d.A.Cluster != "shop-staging-old" || d.B.Server.Minor != "1.35" || len(d.Resources) != 5 {
		t.Errorf("tug api diff -o json = %+v", d)
	}
	// Clusters that serve the same APIs exit 0.
	if out := mustTug(t, "api", "diff", "shop-staging-new", "shop-staging-twin"); !strings.Contains(out, "They serve the same") {
		t.Errorf("tug api diff of two clusters that serve the same APIs:\n%s", out)
	}
	if out := mustTug(t, "api", "diff", "shop-staging-new", "shop-staging-twin", "-o", "json"); !strings.Contains(out, `"resources": []`) {
		t.Errorf("tug api diff -o json of two clusters that serve the same APIs:\n%s", out)
	}

	for _, tt := range []struct{ args, want string }{
		{"api diff shop-staging-old shop-staging-old", "name two different clusters"},
		{"api diff shop-staging-old nowhere", `no cluster matches "nowhere"`},
		{"api diff shop-staging-old shop-staging-new --cluster-selector env=staging", "tug api diff does not run across clusters"},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if oldWrites.Load() != 0 || curWrites.Load() != 0 {
		t.Error("api diff wrote to a cluster")
	}
	assertNoSessions(t, home)
}

func TestAPIAcrossClusters(t *testing.T) {
	home := isolate(t)
	old, _ := serveAPIs(t, oldAPIs())
	cur, _ := serveAPIs(t, newAPIs())
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{"shop-staging-old": old, "shop-staging-new": cur}))

	out := mustTug(t, "api", "server-version", "--cluster-selector", "env=staging")
	if !strings.Contains(out, "shop-staging-old | v1.24.17: end of life since 2023-07-28") || !strings.Contains(out, "shop-staging-new | v1.35.1: ") {
		t.Errorf("tug api server-version across clusters:\n%s", out)
	}
	out = mustTug(t, "api", "deprecations", "--cluster-selector", "env=staging")
	if !strings.Contains(out, "shop-staging-old | autoscaling/v2beta2") || !strings.Contains(out, "shop-staging-new | Nothing deprecated") {
		t.Errorf("tug api deprecations across clusters:\n%s", out)
	}
	out = mustTug(t, "crd", "explain", "widget", "--cluster-selector", "env=staging")
	if !strings.Contains(out, "shop-staging-old | Stored versions:  v1beta1, v1 (objects may still be stored at v1beta1") ||
		!strings.Contains(out, "shop-staging-new | Stored versions:  v1\n") {
		t.Errorf("tug crd explain across clusters:\n%s", out)
	}
	assertNoSessions(t, home)
}

func TestCRDExplain(t *testing.T) {
	writes := connectAPIs(t, oldAPIs())
	want := `Name:             widgets.example.com
Kind:             Widget (list WidgetList)
Group:            example.com
Scope:            Namespaced
Names:            widgets, widget, wd
Categories:       -
Conversion:       Webhook widgets/widget-webhook:443/convert, ConversionReview v1
Stored versions:  v1beta1, v1 (objects may still be stored at v1beta1; rewrite them before dropping it)
Conditions:       Established=True

VERSION   SERVED   STORAGE   DEPRECATED   SUBRESOURCES
v1        yes      yes       -            status
v1beta1   yes      -         yes          -
deprecated v1beta1: example.com/v1beta1 Widget is deprecated; use example.com/v1

Printer columns of v1:
NAME   TYPE            JSONPATH                      DESCRIPTION
Size   string          .spec.size                    -
Age    date, -o wide   .metadata.creationTimestamp   -
`
	// By short name, kind, the definition's name, and a fuzzy name.
	for _, arg := range []string{"wd", "Widget", "widgets.example.com", "widg"} {
		if got := mustTug(t, "crd", "explain", arg); got != want {
			t.Errorf("tug crd explain %s =\n%s\nwant\n%s", arg, got, want)
		}
	}
	var crd apiinfo.CRD
	if err := json.Unmarshal([]byte(mustTug(t, "crd", "explain", "wd", "-o", "json")), &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Name != "widgets.example.com" || crd.Storage() != "v1" || len(crd.Unmigrated()) != 1 {
		t.Errorf("tug crd explain -o json = %+v", crd)
	}
	for _, tt := range []struct{ args, want string }{
		{"crd explain pods", "pods is not defined by a custom resource definition; `tug api resources pods` shows it"},
		{"crd explain gizmo", `no custom resource definition matches "gizmo"`},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Errorf("crd explain wrote %d times", writes.Load())
	}
}

// trimLines drops the spaces a table pads an empty last column with.
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

func TestCRDSchema(t *testing.T) {
	connectAPIs(t, oldAPIs())
	want := `Widget example.com/v1: Widget is a thing that does something.

FIELD        TYPE                REQUIRED   DESCRIPTION
apiVersion   string
kind         string
metadata     object
spec         object                         Spec is what the widget should be.
  labels     map[string]string
  parts      []object                       Parts the widget is made of.
    count    integer
    name     string              yes        Name of the part.
  size       string              yes        Size of the widget. One of: small, large. Default: small.
`
	if got := trimLines(mustTug(t, "crd", "schema", "widget")); got != want {
		t.Errorf("tug crd schema widget =\n%s\nwant\n%s", got, want)
	}
	want = `spec.parts ([]object) in Widget example.com/v1
  Parts the widget is made of.

FIELD   TYPE      REQUIRED   DESCRIPTION
count   integer
name    string    yes        Name of the part.
`
	if got := trimLines(mustTug(t, "crd", "schema", "wd", "--field", "spec.parts")); got != want {
		t.Errorf("tug crd schema --field spec.parts =\n%s\nwant\n%s", got, want)
	}
	want = `spec.size (string, required) in Widget example.com/v1
  Size of the widget.
  Large widgets need more room.
  One of: small, large
  Default: small
`
	if got := mustTug(t, "crd", "schema", "wd", "--field", "spec.size"); got != want {
		t.Errorf("tug crd schema --field spec.size =\n%s\nwant\n%s", got, want)
	}
	if got := mustTug(t, "crd", "schema", "wd", "--depth", "1"); strings.Contains(got, "labels") || !strings.Contains(got, "spec ") {
		t.Errorf("tug crd schema --depth 1 shows more than the top level:\n%s", got)
	}
	if got := mustTug(t, "crd", "schema", "wd", "--version", "v1beta1"); got != "Widget example.com/v1beta1\nThe schema declares no fields; the version takes any.\n" {
		t.Errorf("tug crd schema --version v1beta1 = %q", got)
	}

	var view schemaView
	if err := json.Unmarshal([]byte(mustTug(t, "crd", "schema", "wd", "--field", "spec", "-o", "json")), &view); err != nil {
		t.Fatal(err)
	}
	if view.APIVersion != "example.com/v1" || len(view.Fields) != 6 || view.Fields[5].Path != "spec.size" || view.Fields[5].Default != "small" {
		t.Errorf("tug crd schema -o json = %+v", view)
	}

	for _, tt := range []struct{ args, want string }{
		{"crd schema wd --version v9", `widgets.example.com has no version "v9"; it has v1 and v1beta1`},
		{"crd schema wd --field spec.sise", `widgets.example.com v1: no field "sise" under spec; it has labels, parts, size`},
		{"crd schema wd --depth -1", "--depth wants a number from 0"},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
}
