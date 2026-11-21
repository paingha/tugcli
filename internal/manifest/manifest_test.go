package manifest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestScanDocs(t *testing.T) {
	src := `# the API
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api # the main one
  namespace: "shop"
  labels:
    name: not-this
spec:
  template:
    metadata:
      name: nor-this
---

apiVersion: v1
kind: Service
metadata:
    labels: {app: api}
    name: 'api'
--- # a document of nothing
...
{{- if .Values.worker.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "web.fullname" . }}-worker
{{- end }}
`
	got := scanDocs([]byte(src))
	want := []doc{
		{Line: 2, APIVersion: "apps/v1", Kind: "Deployment", Name: "api", Namespace: "shop"},
		{Line: 15, APIVersion: "v1", Kind: "Service", Name: "api"},
		{Line: 23, APIVersion: "apps/v1", Kind: "Deployment", Name: `{{ include "web.fullname" . }}-worker`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("scanDocs =\n%+v\nwant\n%+v", got, want)
	}
}

func TestSources(t *testing.T) {
	tests := []struct {
		name string
		o    Object
		want []Source
	}{
		{"flux kustomization", Object{Labels: map[string]string{
			"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system",
		}}, []Source{{FluxKustomization, "flux-system", "apps"}}},
		{"flux helm release", Object{Labels: map[string]string{
			"helm.toolkit.fluxcd.io/name": "web", "helm.toolkit.fluxcd.io/namespace": "shop",
		}, Annotations: map[string]string{"meta.helm.sh/release-name": "web"}}, []Source{{FluxHelmRelease, "shop", "web"}}},
		{"argo tracking id", Object{Annotations: map[string]string{
			"argocd.argoproj.io/tracking-id": "shop-api:apps/Deployment:shop/api",
		}}, []Source{{ArgoApplication, "argocd", "shop-api"}}},
		{"argo app in another namespace", Object{Annotations: map[string]string{
			"argocd.argoproj.io/tracking-id": "team-a_shop-api:apps/Deployment:shop/api",
		}}, []Source{{ArgoApplication, "team-a", "shop-api"}}},
		{"argo instance label", Object{Labels: map[string]string{"app.kubernetes.io/instance": "shop-api"}},
			[]Source{{ArgoApplication, "argocd", "shop-api"}}},
		// Helm sets the same label on what it installs.
		{"helm instance label", Object{Labels: map[string]string{"app.kubernetes.io/instance": "web"},
			Annotations: map[string]string{"meta.helm.sh/release-name": "web"}}, nil},
		{"nothing", Object{}, nil},
	}
	for _, tt := range tests {
		if got := Sources(tt.o); !slices.Equal(got, tt.want) {
			t.Errorf("Sources(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if s := (Source{ArgoApplication, "argocd", "shop-api"}).String(); s != "Argo CD Application argocd/shop-api" {
		t.Errorf("String() = %q", s)
	}
}

func TestSourcePaths(t *testing.T) {
	tests := []struct {
		kind string
		obj  map[string]any
		want []string
	}{
		{FluxKustomization, map[string]any{"spec": map[string]any{"path": "./apps/prod"}}, []string{"./apps/prod"}},
		{FluxKustomization, map[string]any{"spec": map[string]any{}}, []string{"./"}},
		{FluxHelmRelease, map[string]any{"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": "./charts/web"}}}}, []string{"./charts/web"}},
		{FluxHelmRelease, map[string]any{"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": "podinfo"}}}}, nil},
		{ArgoApplication, map[string]any{"spec": map[string]any{
			"source":  map[string]any{"path": "apps/prod"},
			"sources": []any{map[string]any{"path": "apps/prod"}, map[string]any{"path": "charts/web"}, map[string]any{"chart": "redis"}},
		}}, []string{"apps/prod", "charts/web"}},
	}
	for _, tt := range tests {
		if got := SourcePaths(tt.kind, tt.obj); !slices.Equal(got, tt.want) {
			t.Errorf("SourcePaths(%s, %v) = %v, want %v", tt.kind, tt.obj, got, tt.want)
		}
	}
}

// repo writes a repository of manifests and returns its root.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"apps/base/api.yaml": `apiVersion: v1
kind: Service
metadata:
  name: api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
`,
		"apps/base/kustomization.yaml": "resources: [api.yaml]\n",
		"apps/prod/api-patch.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
`,
		"apps/staging/api.yml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: staging
`,
		"charts/web/Chart.yaml": "apiVersion: v2\nname: web\nversion: 1.2.0+build.1\n",
		"charts/web/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "web.fullname" . }}
`,
		"charts/web/templates/service.yaml": `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "web.fullname" . }}
{{- end }}
`,
		"node_modules/pkg/api.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n",
		".hidden/api.yaml":          "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n",
		"other/extensions.yaml":     "apiVersion: example.com/v1\nkind: Deployment\nmetadata:\n  name: api\n",
	}
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFind(t *testing.T) {
	root := repo(t)
	at := func(rel string, line int) string {
		return filepath.Join(root, filepath.FromSlash(rel)) + ":" + strconv.Itoa(line)
	}
	deploy := func(ns string) Object {
		return Object{APIVersion: "apps/v1", Kind: "Deployment", Name: "api", Namespace: ns}
	}
	paths := map[Source][]string{
		{FluxKustomization, "flux-system", "apps-staging"}: {"./apps/staging"},
		{ArgoApplication, "argocd", "web"}:                 {"charts/web"},
		{ArgoApplication, "argocd", "escape"}:              {"../../etc"},
	}
	f := &Finder{Root: root, SourcePaths: func(_ context.Context, s Source) []string { return paths[s] }}

	with := func(o Object, labels, annotations map[string]string) Object {
		o.Labels, o.Annotations = labels, annotations
		return o
	}
	tests := []struct {
		name string
		o    Object
		want []string
		via  string
	}{
		{"a search: its namespace first, no other namespace, nothing hidden", deploy("prod"),
			[]string{at("apps/prod/api-patch.yaml", 1), at("apps/base/api.yaml", 6)}, "a search of"},
		{"a flux kustomization's path", with(deploy("staging"), map[string]string{
			"kustomize.toolkit.fluxcd.io/name": "apps-staging", "kustomize.toolkit.fluxcd.io/namespace": "flux-system",
		}, nil), []string{at("apps/staging/api.yml", 1)}, "Flux Kustomization flux-system/apps-staging"},
		{"kustomize's origin annotation, through a name prefix", with(Object{APIVersion: "apps/v1", Kind: "Deployment", Name: "prod-api", Namespace: "prod"},
			nil, map[string]string{"config.kubernetes.io/origin": "path: ../base/api.yaml\nrepo: https://example.com/shop.git\n"}),
			[]string{at("apps/base/api.yaml", 6)}, "kustomize's origin annotation"},
		{"a helm chart by its label", with(Object{APIVersion: "v1", Kind: "Service", Name: "web", Namespace: "shop"},
			map[string]string{"helm.sh/chart": "web-1.2.0_build.1"}, map[string]string{"meta.helm.sh/release-name": "web"}),
			[]string{at("charts/web/templates/service.yaml", 2)}, "Helm chart web of release web"},
		{"an argo application on a chart", with(Object{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "shop"},
			map[string]string{"app.kubernetes.io/instance": "web"}, nil),
			[]string{at("charts/web/templates/deployment.yaml", 1)}, "Helm chart web of Argo CD Application argocd/web"},
		{"a source path outside the repository is ignored", with(deploy("staging"), map[string]string{"argocd.argoproj.io/instance": "escape"}, nil),
			[]string{at("apps/staging/api.yml", 1), at("apps/base/api.yaml", 6)}, "a search of"},
		{"nothing", Object{APIVersion: "v1", Kind: "ConfigMap", Name: "api", Namespace: "prod"}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locs, err := f.Find(context.Background(), tt.o)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(locs))
			for i, l := range locs {
				got[i] = l.String()
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Find = %v, want %v", got, tt.want)
			}
			if len(locs) > 0 && !strings.HasPrefix(locs[0].Via, tt.via) {
				t.Errorf("Find found it via %q, want %q", locs[0].Via, tt.via)
			}
		})
	}
}

func TestFindRoot(t *testing.T) {
	root := repo(t)
	deep := filepath.Join(root, "apps", "prod")
	if got := FindRoot(deep); got != root {
		t.Errorf("FindRoot(%s) = %s, want %s", deep, got, root)
	}
	plain := t.TempDir()
	if got := FindRoot(plain); got != plain {
		t.Errorf("FindRoot outside a repository = %s, want %s", got, plain)
	}
}
