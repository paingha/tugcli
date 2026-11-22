package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/paingha/tugcli/internal/gitops"
)

// fakeGitOpsResources are the Flux and Argo CD resources the fake API server
// serves, by group and version.
var fakeGitOpsResources = map[string][]metav1.APIResource{
	"source.toolkit.fluxcd.io/v1": {
		{Name: "gitrepositories", Kind: "GitRepository", ShortNames: []string{"gitrepo"}},
		{Name: "ocirepositories", Kind: "OCIRepository", ShortNames: []string{"ocirepo"}},
		{Name: "helmrepositories", Kind: "HelmRepository", ShortNames: []string{"helmrepo"}},
		{Name: "helmcharts", Kind: "HelmChart", ShortNames: []string{"hc"}},
		{Name: "buckets", Kind: "Bucket"},
	},
	"kustomize.toolkit.fluxcd.io/v1": {{Name: "kustomizations", Kind: "Kustomization", ShortNames: []string{"ks"}}},
	"helm.toolkit.fluxcd.io/v2":      {{Name: "helmreleases", Kind: "HelmRelease", ShortNames: []string{"hr"}}},
	"argoproj.io/v1alpha1":           {{Name: "applications", Kind: "Application", ShortNames: []string{"app", "apps"}}},
}

// gitopsMatch reports whether s matches a regular expression.
func gitopsMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }

// ago is a time some while before now, as the API server writes it.
func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func gitopsObject(apiVersion, kind, ns, name string, spec, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": strings.ToLower(kind) + "-" + name, "generation": int64(1)},
		"spec":     spec, "status": status,
	}}
}

func fluxReady(status, reason, msg string) []any {
	return []any{map[string]any{
		"type": "Ready", "status": status, "reason": reason, "message": msg, "observedGeneration": int64(1), "lastTransitionTime": ago(15 * time.Minute),
	}}
}

const (
	fleetRevision = "main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
	oldRevision   = "main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"
)

// gitopsObjects are what Flux in flux-system and Argo CD in argocd report:
// a Kustomization rejected by a webhook, one that waits on it, one whose
// inventory holds the shop namespace's pricing deployment, which lacks a
// Secret, and its failing Database, and an Application that is out of sync
// and degraded. They are keyed by group, version and resource.
func gitopsObjects() map[string][]*unstructured.Unstructured {
	source := map[string]any{"kind": "GitRepository", "name": "fleet"}
	ks := func(name string, spec map[string]any, status map[string]any) *unstructured.Unstructured {
		spec["interval"], spec["path"], spec["prune"], spec["sourceRef"] = "10m0s", "./"+name, true, source
		status["observedGeneration"] = int64(1)
		return gitopsObject("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", name, spec, status)
	}
	return map[string][]*unstructured.Unstructured{
		"source.toolkit.fluxcd.io/v1/gitrepositories": {
			gitopsObject("source.toolkit.fluxcd.io/v1", "GitRepository", "flux-system", "fleet",
				map[string]any{"interval": "1m0s", "url": "ssh://git@github.com/example/fleet"},
				map[string]any{
					"observedGeneration": int64(1),
					"artifact":           map[string]any{"revision": fleetRevision, "lastUpdateTime": ago(12 * time.Minute)},
					"conditions":         fluxReady("True", "Succeeded", "stored artifact for revision '"+fleetRevision+"'"),
				}),
		},
		"kustomize.toolkit.fluxcd.io/v1/kustomizations": {
			ks("infra", map[string]any{}, map[string]any{
				"lastAppliedRevision": oldRevision, "lastAttemptedRevision": fleetRevision,
				"conditions": fluxReady("False", "ReconciliationFailed",
					`ClusterIssuer/letsencrypt dry-run failed (Forbidden): admission webhook "validate.kyverno.svc-fail" denied the request:`+
						"\n\nrequire-team-label:\n  check-team: 'validation error: label team is required.'"),
			}),
			ks("apps", map[string]any{"dependsOn": []any{map[string]any{"name": "infra"}}, "targetNamespace": "shop"}, map[string]any{
				"lastAppliedRevision": oldRevision, "lastAttemptedRevision": oldRevision,
				"conditions": fluxReady("False", "DependencyNotReady", "dependency 'flux-system/infra' is not ready"),
			}),
			ks("tenants", map[string]any{"dependsOn": []any{map[string]any{"name": "apps"}, map[string]any{"name": "crds", "namespace": "platform"}}},
				map[string]any{"conditions": fluxReady("False", "DependencyNotReady", "dependency 'flux-system/apps' is not ready")}),
			ks("storefront", map[string]any{}, map[string]any{
				"lastAppliedRevision": fleetRevision, "lastAttemptedRevision": fleetRevision,
				"conditions": fluxReady("True", "ReconciliationSucceeded", "Applied revision: "+fleetRevision),
				"inventory": map[string]any{"entries": []any{
					map[string]any{"id": "shop_checkout_apps_Deployment", "v": "v1"},
					map[string]any{"id": "shop_pricing_apps_Deployment", "v": "v1"},
					map[string]any{"id": "shop_orders_db.example.com_Database", "v": "v1"},
					map[string]any{"id": "shop_checkout-config__ConfigMap", "v": "v1"},
					map[string]any{"id": "shop_checkout-tls__Secret", "v": "v1"},
				}},
			}),
		},
		"argoproj.io/v1alpha1/applications": {
			gitopsObject("argoproj.io/v1alpha1", "Application", "argocd", "guestbook",
				map[string]any{
					"destination": map[string]any{"namespace": "shop", "server": "https://kubernetes.default.svc"},
					"project":     "default",
					"source":      map[string]any{"repoURL": "https://github.com/example/apps.git", "path": "guestbook", "targetRevision": "HEAD"},
				},
				map[string]any{
					"health":       map[string]any{"status": "Degraded"},
					"reconciledAt": ago(11 * time.Minute),
					"history":      []any{map[string]any{"id": int64(3), "revision": "3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a", "deployedAt": ago(time.Hour)}},
					"sync":         map[string]any{"status": "OutOfSync", "revision": "3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"},
					"resources": []any{
						map[string]any{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "shop", "name": "pricing", "status": "OutOfSync",
							"health": map[string]any{"status": "Degraded", "message": `Deployment "pricing" exceeded its progress deadline`}},
						map[string]any{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "shop", "name": "checkout", "status": "Synced",
							"health": map[string]any{"status": "Healthy"}},
						map[string]any{"version": "v1", "kind": "ConfigMap", "namespace": "shop", "name": "guestbook-ui", "status": "OutOfSync", "requiresPruning": true},
					},
				}),
		},
	}
}

// gitopsAPIServer is fakeAPIServer with Flux and Argo CD: it serves their
// discovery and the objects of gitopsObjects, and passes everything else to
// fakeAPIServer.
func gitopsAPIServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	base, writes := fakeAPIServer(t)
	inner := base.Config.Handler
	objects := gitopsObjects()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") == "Bearer "+secretToken && serveGitOps(t, w, r, objects, writes):
		case strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata;"):
			gitopsServePartial(t, w, r, inner)
		default:
			inner.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, writes
}

// gitopsServePartial answers a request for one object's metadata as the API
// server does, with nothing but its metadata, which fakeAPIServer does not.
func gitopsServePartial(t *testing.T, w http.ResponseWriter, r *http.Request, inner http.Handler) {
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var p metav1.PartialObjectMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Errorf("fake API server: decoding %s: %v", r.URL.Path, err)
	}
	p.TypeMeta = metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"}
	writeJSON(t, w, p)
}

// serveGitOps answers a request about Flux or Argo CD, reporting whether it
// was one.
func serveGitOps(t *testing.T, w http.ResponseWriter, r *http.Request, objects map[string][]*unstructured.Unstructured, writes *atomic.Int32) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.URL.Path == "/apis" {
		var groups metav1.APIGroupList
		if err := json.Unmarshal([]byte(fakeDiscovery["/apis"]), &groups); err != nil {
			t.Fatal(err)
		}
		for gv := range fakeGitOpsResources {
			group, version, _ := strings.Cut(gv, "/")
			v := metav1.GroupVersionForDiscovery{GroupVersion: gv, Version: version}
			groups.Groups = append(groups.Groups, metav1.APIGroup{Name: group, Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v})
		}
		writeJSON(t, w, groups)
		return true
	}
	if len(parts) < 3 || parts[0] != "apis" {
		return false
	}
	gv := parts[1] + "/" + parts[2]
	resources, ok := fakeGitOpsResources[gv]
	if !ok {
		return false
	}
	if r.Method != http.MethodGet {
		writes.Add(1)
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method+" "+r.URL.Path)
		return true
	}
	rest := parts[3:]
	if len(rest) == 0 {
		rs := slices.Clone(resources)
		for i := range rs {
			rs[i].Namespaced, rs[i].Verbs, rs[i].SingularName = true, metav1.Verbs{"get", "list", "watch"}, strings.ToLower(rs[i].Kind)
		}
		writeJSON(t, w, metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: gv, APIResources: rs})
		return true
	}
	ns := ""
	if rest[0] == "namespaces" && len(rest) >= 3 {
		ns, rest = rest[1], rest[2:]
	}
	var items []*unstructured.Unstructured
	for _, u := range objects[gv+"/"+rest[0]] {
		if ns == "" || u.GetNamespace() == ns {
			items = append(items, u)
		}
	}
	switch {
	case len(rest) == 1 && strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList"):
		partial := []metav1.PartialObjectMetadata{}
		for _, u := range items {
			p := metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "PartialObjectMetadata"}}
			p.Name, p.Namespace, p.UID = u.GetName(), u.GetNamespace(), u.GetUID()
			partial = append(partial, p)
		}
		writeJSON(t, w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": partial})
	case len(rest) == 1:
		list := []any{}
		for _, u := range items {
			list = append(list, u.Object)
		}
		kind := ""
		for _, res := range resources {
			if res.Name == rest[0] {
				kind = res.Kind
			}
		}
		writeJSON(t, w, map[string]any{"kind": kind + "List", "apiVersion": gv, "metadata": map[string]any{}, "items": list})
	case len(rest) == 2:
		for _, u := range items {
			if u.GetName() == rest[1] {
				writeJSON(t, w, u.Object)
				return true
			}
		}
		writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("%s %q not found", rest[0], rest[1]))
	default:
		writeStatus(w, http.StatusNotFound, "NotFound", r.URL.Path+" not served")
	}
	return true
}

func TestGitOps(t *testing.T) {
	isolate(t)
	srv, writes := gitopsAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	contains := func(t *testing.T, what, out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s lacks %q:\n%s", what, w, out)
			}
		}
	}
	failing := func(t *testing.T, args ...string) string {
		t.Helper()
		r := tug(t, append([]string{"gitops"}, args...)...)
		if r.code != 1 {
			t.Fatalf("tug gitops %v: exit %d, want 1\n%s%s", args, r.code, r.stdout, r.stderr)
		}
		return r.stdout
	}

	t.Run("status", func(t *testing.T) {
		out := mustTug(t, "gitops", "status", "-n", "shop")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 5 || !strings.HasPrefix(lines[0], "NAME ") {
			t.Fatalf("tug gitops status -n shop: want a header and the source, the two Kustomizations deploying into shop and the Application\n%s", out)
		}
		for i, want := range []string{
			`^gitrepository/fleet\s+flux-system\s+True\s+Succeeded\s+False\s+main@sha1:1a2b3c4\s+-\s+12m\s+stored artifact for revision 'main@sha1:1a2b3c4'$`,
			`^kustomization/apps\s+flux-system\s+False\s+DependencyNotReady\s+False\s+main@sha1:1a2b3c4\s+main@sha1:9f8e7d6\s+15m\s+dependency 'flux-system/infra' is not ready$`,
			`^kustomization/storefront\s+flux-system\s+True\s+ReconciliationSucceeded\s+False\s+main@sha1:1a2b3c4\s+main@sha1:1a2b3c4\s+15m\s+Applied revision: main@sha1:1a2b3c4$`,
			`^application/guestbook\s+argocd\s+False\s+OutOfSync, Degraded\s+-\s+3f2c1a0\s+3f2c1a0\s+11m\s+-$`,
		} {
			if !gitopsMatch(want, lines[i+1]) {
				t.Errorf("line %d of tug gitops status = %q, want it to match %s", i+1, lines[i+1], want)
			}
		}

		var r gitops.Report
		if err := json.Unmarshal([]byte(mustTug(t, "gitops", "status", "-A", "-o", "json")), &r); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(r.Tools, []gitops.Tool{gitops.Flux, gitops.ArgoCD}) || len(r.Objects) != 6 || r.Namespace != "" {
			t.Errorf("tug gitops status -A -o json = tools %v, %d objects, namespace %q; want flux and argocd, 6, every namespace", r.Tools, len(r.Objects), r.Namespace)
		}
		names := mustTug(t, "gitops", "status", "-A", "-o", "name")
		contains(t, "tug gitops status -o name", names, "kustomizations.kustomize.toolkit.fluxcd.io/infra\n", "applications.argoproj.io/guestbook\n")

		if r := tug(t, "gitops", "status", "-n", "nothing"); r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "No Flux or Argo CD objects concern namespace nothing") {
			t.Errorf("tug gitops status -n nothing = %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		}
	})

	t.Run("explain", func(t *testing.T) {
		contains(t, "tug gitops explain apps", failing(t, "explain", "apps"),
			"kustomization/apps in flux-system is not ready: something it depends on is not ready\n",
			"source gitrepository/fleet at main@sha1:1a2b3c4, last applied main@sha1:9f8e7d6, reported 15m ago\n",
			"error    kustomization/apps, kustomization/infra: DependencyNotReady\n",
			"         next: see why:\n               tug gitops explain kustomization/infra -n flux-system\n")
		contains(t, "tug gitops explain ks/infra", failing(t, "explain", "ks/infra"),
			"is not ready: the cluster's admission control rejects what it applies\n",
			"label team is required.",
			"change the manifest so that the policy named above allows it")

		out := failing(t, "explain", "storefront", "-n", "flux-system")
		contains(t, "tug gitops explain storefront", out,
			"kustomization/storefront in flux-system is not ready: what it manages is not healthy\n",
			"error    pod/pricing-9f2a-m4k7p, container main: CreateContainerConfigError\n",
			"error    database/orders: BackupFailing\n",
			"Manages 5 objects, 2 of them not healthy:\n",
			"deploy/pricing    shop        not-ready   -      0/1 ready\n")
		if strings.Contains(out, "not a real key") {
			t.Errorf("tug gitops explain showed a Secret's data:\n%s", out)
		}

		var x gitops.Explanation
		if err := json.Unmarshal([]byte(failing(t, "explain", "app/guestbook", "-o", "json")), &x); err != nil {
			t.Fatal(err)
		}
		if x.Healthy || x.Stage != gitops.StageHealth || x.Object.Name != "guestbook" || len(x.Managed) != 3 {
			t.Errorf("tug gitops explain app/guestbook -o json = healthy %v, stage %s, managed %d", x.Healthy, x.Stage, len(x.Managed))
		}
	})

	t.Run("drift", func(t *testing.T) {
		out := failing(t, "drift", "app/guestbook")
		contains(t, "tug gitops drift app/guestbook", out,
			"application/guestbook in argocd has 2 objects out of sync\n",
			"Argo CD last compared it with its source 11m ago.\n")
		if !gitopsMatch(`(?m)^deploy/pricing\s+shop\s+OutOfSync\s+Degraded\s+-$`, out) ||
			!gitopsMatch(`(?m)^cm/guestbook-ui\s+shop\s+OutOfSync\s+-\s+no longer in its source; a sync with pruning deletes it$`, out) {
			t.Errorf("tug gitops drift app/guestbook lists the wrong objects:\n%s", out)
		}
		out = mustTug(t, "gitops", "drift", "ks/apps")
		contains(t, "tug gitops drift ks/apps", out, "kustomization/apps in flux-system has no drift report\n", "flux diff kustomization apps -n flux-system --path ./apps")
	})

	t.Run("dependencies", func(t *testing.T) {
		out := mustTug(t, "gitops", "dependencies", "apps")
		contains(t, "tug gitops dependencies apps", out,
			"Depends on:\nkustomization/apps       ✗ DependencyNotReady: dependency 'flux-system/infra' is not ready\n└── kustomization/infra  ✗ ReconciliationFailed:",
			"Required by:\nkustomization/apps         ✗ DependencyNotReady: dependency 'flux-system/infra' is not ready\n└── kustomization/tenants  ✗ DependencyNotReady")
		out = mustTug(t, "gitops", "deps", "infra")
		contains(t, "tug gitops deps infra", out, "Depends on:\nnothing\n", "Required by:\nkustomization/infra            ✗",
			"└── kustomization/apps         ✗ DependencyNotReady", "    └── kustomization/tenants  ✗ DependencyNotReady")
		out = mustTug(t, "gitops", "deps", "tenants")
		contains(t, "tug gitops deps tenants", out, "└── kustomization/crds in platform  ✗ not in the cluster\n")
	})

	for _, tt := range []struct{ args, want string }{
		{"gitops explain nothing", `no Kustomization, HelmRelease or Application matches "nothing"; list them with ` + "`tug gitops status -A`"},
		{"gitops explain apps -n shop", `no Kustomization, HelmRelease or Application in namespace shop matches "apps"`},
		{"gitops drift gitrepo/fleet", `"gitrepo" is not a Kustomization, HelmRelease or Application`},
		{"gitops explain apps -o name", "explain supports -o table, json or yaml"},
		{"gitops status -n shop -A", "none of the others can be"},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("gitops wrote to the cluster %d times", writes.Load())
	}
}

func TestGitOpsWithoutFluxOrArgoCD(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	if r := tug(t, "gitops", "status"); r.code != 0 || !strings.Contains(r.stderr, "Neither Flux nor Argo CD is installed in this cluster.") {
		t.Errorf("tug gitops status = %d, stderr %q", r.code, r.stderr)
	}
	if out := mustTug(t, "gitops", "status", "-o", "json"); !strings.Contains(out, `"objects": []`) {
		t.Errorf("tug gitops status -o json = %s, want an empty list of objects", out)
	}
	if r := tug(t, "gitops", "explain", "apps"); r.code == 0 || !strings.Contains(r.stderr, "the cluster serves neither Flux nor Argo CD") {
		t.Errorf("tug gitops explain apps = %d, stderr %q", r.code, r.stderr)
	}
}

// TestGitOpsFindsToolsInstalledLater installs Flux and Argo CD in the fake
// cluster after a first tug gitops status has filled the discovery cache,
// which stays fresh for hours: the next command must find them all the same.
func TestGitOpsFindsToolsInstalledLater(t *testing.T) {
	isolate(t)
	base, writes := fakeAPIServer(t)
	inner := base.Config.Handler
	objects := gitopsObjects()
	var installed atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if installed.Load() && r.Header.Get("Authorization") == "Bearer "+secretToken && serveGitOps(t, w, r, objects, writes) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	if r := tug(t, "gitops", "status", "-A"); r.code != 0 || !strings.Contains(r.stderr, "Neither Flux nor Argo CD is installed") {
		t.Fatalf("tug gitops status -A before the install = %d, stderr %q", r.code, r.stderr)
	}
	installed.Store(true)
	names := mustTug(t, "gitops", "status", "-A", "-o", "name")
	for _, want := range []string{"kustomizations.kustomize.toolkit.fluxcd.io/infra\n", "applications.argoproj.io/guestbook\n"} {
		if !strings.Contains(names, want) {
			t.Errorf("tug gitops status -A -o name after the install lacks %q:\n%s", want, names)
		}
	}
}

func TestGitOpsRevisions(t *testing.T) {
	a := &app{now: time.Now}
	for _, tt := range []struct {
		o    gitops.Object
		want string
	}{
		{gitops.Object{Tool: gitops.Flux, Namespace: "flux-system", Source: &gitops.Ref{Kind: "GitRepository", Name: "fleet", Namespace: "flux-system"},
			Revision: fleetRevision, Applied: oldRevision}, "source gitrepository/fleet at main@sha1:1a2b3c4, last applied main@sha1:9f8e7d6"},
		{gitops.Object{Tool: gitops.Flux, Namespace: "shop", Source: &gitops.Ref{Kind: "HelmChart", Name: "shop-podinfo", Namespace: "flux-system"},
			Revision: "6.7.1", Applied: "6.7.0"}, "source helmchart/shop-podinfo in flux-system at 6.7.1, last applied 6.7.0"},
		// An Application whose source has no repoURL, such as one whose
		// sources Argo CD has not resolved yet.
		{gitops.Object{Tool: gitops.ArgoCD, Revision: "3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"}, "revision 3f2c1a0"},
		{gitops.Object{Tool: gitops.ArgoCD}, ""},
	} {
		if got := a.gitopsRevisions(&tt.o); got != tt.want {
			t.Errorf("gitopsRevisions(%+v) = %q, want %q", tt.o, got, tt.want)
		}
	}
}
