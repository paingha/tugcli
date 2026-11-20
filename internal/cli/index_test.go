package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPluginIndex builds an index the way a publisher would, serves it, and
// installs from it by name, as a user would.
func TestPluginIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	home := isolate(t)
	t.Setenv(IndexKeyEnv, "")

	// A plugin, packaged.
	dir := t.TempDir()
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: idx, version: 1.0.0, publisher: tests}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  capabilities: [list, credential]\n  platforms:\n" +
		"  - {os: " + runtime.GOOS + ", arch: " + runtime.GOARCH + ", bin: tug-provider-idx}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-idx"), []byte(rotatePluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	archive := strings.Fields(mustTug(t, "plugin", "package", dir, "-o", dist))[0]

	// The publisher's side: a key, the index entry, the signature.
	pub := strings.TrimSpace(mustTug(t, "plugin", "index", "keygen"))
	site := t.TempDir()
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(site)))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := os.Link(archive, filepath.Join(site, filepath.Base(archive))); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(site, "index.yaml")
	if out := mustTug(t, "plugin", "index", "add", index, archive, "--url", srv.URL+"/"+filepath.Base(archive), "--description", "Test clusters"); !strings.Contains(out, "Added idx 1.0.0") {
		t.Fatalf("index add: %s", out)
	}
	mustTug(t, "plugin", "index", "sign", index)
	if out := mustTug(t, "plugin", "index", "verify", index, "--public-key", pub); !strings.Contains(out, "lists 1 plugins") {
		t.Fatalf("index verify: %s", out)
	}

	// The user's side.
	if r := tug(t, "plugin", "install", "idx"); r.code == 0 || !strings.Contains(r.stderr, "no plugin index is set up") {
		t.Fatalf("install by name without an index: %+v", r)
	}
	cfg := filepath.Join(home, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := "plugins:\n  indexes:\n  - {name: test, url: " + srv.URL + "/index.yaml, publicKey: " + pub + "}\n"
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustTug(t, "plugin", "search", "test clusters"); !strings.Contains(out, "idx") || !strings.Contains(out, "1.0.0") {
		t.Fatalf("plugin search: %s", out)
	}
	r := tug(t, "plugin", "install", "idx", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Installed idx 1.0.0") || !strings.Contains(r.stderr, "from index test") {
		t.Fatalf("install by name: %+v", r)
	}
	if r := tug(t, "plugin", "install", "idx@9.9.9", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "no version") {
		t.Fatalf("install of a version the index lacks: %+v", r)
	}

	// A changed index no longer matches its signature, even once cached.
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte(strings.Replace(string(b), "Test clusters", "Trust me", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "cache", "plugin-index")); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "does not match its key") {
		t.Fatalf("install from a changed index: %+v", r)
	}
}
