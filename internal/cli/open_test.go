package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/paingha/tugcli/internal/manifest"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

func TestOpen(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(repo, "db", "databases.yaml")
	if err := os.MkdirAll(filepath.Dir(dbFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbFile, []byte(`apiVersion: db.example.com/v1
kind: Database
metadata:
  name: inventory
---
# the orders database
apiVersion: db.example.com/v1
kind: Database
metadata:
  name: orders
  namespace: shop
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without a terminal, the links are printed and no editor runs.
	t.Setenv("VISUAL", "false")
	r := tug(t, "open", "db/orders", "-n", "shop", "--repo", repo)
	if r.code != 0 || r.stdout != dbFile+":7\n" || !strings.Contains(r.stderr, "database/orders, found through a search of "+repo) {
		t.Fatalf("tug open db/orders: %+v", r)
	}
	if out := mustTug(t, "open", "db/inv", "-n", "shop", "--repo", repo, "--print"); out != dbFile+":1\n" {
		t.Errorf("tug open db/inv --print: %q", out)
	}

	// Nothing defines the backup: its live YAML, read-only.
	r = tug(t, "open", "backup/orders-nightly", "-n", "shop", "--repo", repo)
	if r.code != 0 || !strings.Contains(r.stderr, "No file in "+repo+" defines backup/orders-nightly") {
		t.Fatalf("tug open of an object no file defines: %+v", r)
	}
	path, ok := strings.CutSuffix(strings.TrimSpace(r.stdout), ":1")
	if !ok || !strings.HasPrefix(path, filepath.Join(home, "run", "open")+string(filepath.Separator)) {
		t.Fatalf("tug open printed %q, want a file under tug's runtime directory", r.stdout)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Live backup/orders-nightly in shop on payments-prod", "kind: Backup", "name: orders-nightly", "UploadFailed"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("live YAML lacks %q:\n%s", want, b)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o400 {
		t.Errorf("live YAML has mode %v, want 0400", fi.Mode().Perm())
	}

	if r := tug(t, "open", "nothing-like-this", "-n", "shop", "--repo", repo); r.code == 0 || !strings.Contains(r.stderr, "matches") {
		t.Errorf("tug open of nothing: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatalf("tug open wrote %d times to the API server", writes.Load())
	}
}

func TestGitopsPaths(t *testing.T) {
	gv := schema.GroupVersion{Group: "kustomize.toolkit.fluxcd.io", Version: "v1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.Add(gv.WithKind("Kustomization"), meta.RESTScopeNamespace)
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gv.String(), "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"spec":     map[string]any{"path": "./apps/prod"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(),
		map[schema.GroupVersionResource]string{gv.WithResource("kustomizations"): "KustomizationList"}, ks)
	a := &app{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	paths := a.gitopsPaths(ops.Clients{Dynamic: dyn}, mapper)

	ctx := context.Background()
	if got := paths(ctx, manifest.Source{Kind: manifest.FluxKustomization, Namespace: "flux-system", Name: "apps"}); !slices.Equal(got, []string{"./apps/prod"}) {
		t.Errorf("paths of the Kustomization = %v, want ./apps/prod", got)
	}
	// A source tug cannot read gives no paths rather than an error.
	if got := paths(ctx, manifest.Source{Kind: manifest.FluxKustomization, Namespace: "flux-system", Name: "gone"}); got != nil {
		t.Errorf("paths of a missing Kustomization = %v", got)
	}
	if got := paths(ctx, manifest.Source{Kind: manifest.ArgoApplication, Namespace: "argocd", Name: "shop"}); got != nil {
		t.Errorf("paths of an Application on a cluster without Argo CD = %v", got)
	}
}

// tug open reads a Secret as metadata only, so its data never reaches tug.
func TestLiveObjectOfASecret(t *testing.T) {
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	sch := metadatafake.NewTestScheme()
	sch.AddKnownTypeWithName(gvk, &metav1.PartialObjectMetadata{})
	p := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: "shop", Labels: map[string]string{"app": "api"}}}
	p.SetGroupVersionKind(gvk)
	c := ops.Clients{Metadata: metadatafake.NewSimpleMetadataClient(sch, p)} // no dynamic client to read data with

	o := resolve.Object{Resource: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, Kind: "Secret", Name: "api-key", Namespace: "shop"}
	u, err := liveObject(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetKind() != "Secret" || u.GetAPIVersion() != "v1" || u.GetName() != "api-key" || u.GetLabels()["app"] != "api" {
		t.Errorf("liveObject(secret) = %v", u.Object)
	}
	if _, ok := u.Object["data"]; ok {
		t.Errorf("liveObject(secret) has data: %v", u.Object)
	}
}

func TestPruneOpenFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for name, age := range map[string]time.Duration{"old.yaml": 25 * time.Hour, "new.yaml": time.Hour} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("kind: Pod\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.pruneOpenFiles(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "new.yaml" {
		t.Errorf("after pruning, %v are left, want new.yaml", entries)
	}
}
