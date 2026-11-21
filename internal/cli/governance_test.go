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

// pluginSite is a signed plugin index, served over HTTP, that lists the
// idx plugin at several versions, and a config.yaml that trusts it.
type pluginSite struct {
	home, site, index, pub string
	srv                    *httptest.Server
	// archives holds each version's packaged archive.
	archives map[string]string
}

// newPluginSite packages idx at each version, publishes them in a signed
// index and points config.yaml at it, with extra settings under plugins.
func newPluginSite(t *testing.T, extra string, versions ...string) *pluginSite {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	s := &pluginSite{home: isolate(t), site: t.TempDir(), archives: map[string]string{}}
	t.Setenv(IndexKeyEnv, "")
	s.srv = httptest.NewServer(http.FileServer(http.Dir(s.site)))
	t.Cleanup(s.srv.Close)
	s.index = filepath.Join(s.site, "index.yaml")
	s.pub = strings.TrimSpace(mustTug(t, "plugin", "index", "keygen"))
	for _, v := range versions {
		archive := packagePlugin(t, "idx", v)
		published := filepath.Join(s.site, filepath.Base(archive))
		if err := os.Rename(archive, published); err != nil {
			t.Fatal(err)
		}
		s.archives[v] = published
		mustTug(t, "plugin", "index", "add", s.index, published, "--url", s.srv.URL+"/"+filepath.Base(archive))
	}
	s.sign(t)
	s.config(t, extra)
	return s
}

// sign signs the index and drops tug's cached copy of it.
func (s *pluginSite) sign(t *testing.T) {
	t.Helper()
	mustTug(t, "plugin", "index", "sign", s.index)
	if err := os.RemoveAll(filepath.Join(s.home, "cache", "plugin-index")); err != nil {
		t.Fatal(err)
	}
}

// config writes config.yaml: the index, then extra settings under plugins.
func (s *pluginSite) config(t *testing.T, extra string) {
	t.Helper()
	conf := "plugins:\n  indexes:\n  - {name: test, url: " + s.srv.URL + "/index.yaml, publicKey: " + s.pub + "}\n" + extra
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.configPath(), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *pluginSite) configPath() string { return filepath.Join(s.home, "config", "config.yaml") }

// packagePlugin packages a shell script plugin with the given name and
// version, and returns the archive's path.
func packagePlugin(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: " + name + ", version: " + version + ", publisher: tests}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  capabilities: [list, credential]\n  platforms:\n" +
		"  - {os: " + runtime.GOOS + ", arch: " + runtime.GOARCH + ", bin: tug-provider-" + name + "}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-"+name), []byte(rotatePluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return strings.Fields(mustTug(t, "plugin", "package", dir, "-o", t.TempDir()))[0]
}

// pluginRow returns the row of tug plugin ls for name, split into columns.
func pluginRow(t *testing.T, name string) []string {
	t.Helper()
	for _, line := range strings.Split(mustTug(t, "plugin", "ls"), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return f
		}
	}
	return nil
}

func TestPluginPins(t *testing.T) {
	s := newPluginSite(t, "", "1.0.0", "2.0.0")

	r := tug(t, "plugin", "install", "idx@1.0.0", "--pin", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Installed idx 1.0.0") || !strings.Contains(r.stdout, "Pinned idx to 1.0.0 in config.yaml") {
		t.Fatalf("install --pin: %+v", r)
	}
	conf, err := os.ReadFile(s.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "pins:\n    idx: \"1.0.0\"") || !strings.Contains(string(conf), s.pub) {
		t.Fatalf("config.yaml after --pin:\n%s", conf)
	}
	if row := pluginRow(t, "idx"); len(row) < 3 || row[1] != "1.0.0" || row[2] != "1.0.0" {
		t.Fatalf("plugin ls row for idx = %v, want version and pin 1.0.0", row)
	}

	// Without a version, a pinned plugin installs at its pin; any other
	// version is refused, by name or from an archive.
	if r := tug(t, "plugin", "install", "idx", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "idx is pinned to 1.0.0") || !strings.Contains(r.stderr, "1.0.0 is already installed") {
		t.Fatalf("install of a pinned plugin by name: %+v", r)
	}
	if r := tug(t, "plugin", "install", "idx@2.0.0", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "pinned to another version") || !strings.Contains(r.stderr, "--pin") {
		t.Fatalf("install of another version by name: %+v", r)
	}
	if r := tug(t, "plugin", "install", s.archives["2.0.0"], "--yes"); r.code == 0 || !strings.Contains(r.stderr, "pins it to 1.0.0, and this is 2.0.0") {
		t.Fatalf("install of another version from an archive: %+v", r)
	}

	// --pin moves the pin.
	r = tug(t, "plugin", "install", "idx@2.0.0", "--pin", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Pinned idx to 2.0.0 in config.yaml, which pinned it to 1.0.0") {
		t.Fatalf("install --pin of another version: %+v", r)
	}

	// With both installed, tug runs the pinned version, not the highest.
	mustTug(t, "plugin", "install", "idx@1.0.0", "--pin", "--yes", "--force")
	if row := pluginRow(t, "idx"); len(row) < 3 || row[1] != "1.0.0" {
		t.Fatalf("plugin ls row for idx = %v, want the pinned 1.0.0 running", row)
	}

	// A pin to a version that is not installed is reported.
	s.config(t, "  pins: {idx: \"3.0.0\"}\n")
	r = tug(t, "plugin", "ls")
	if r.code != 0 || !strings.Contains(r.stderr, "idx is pinned to 3.0.0 in config.yaml, which is not installed (installed: 2.0.0, 1.0.0)") {
		t.Fatalf("plugin ls with a pin to a missing version: %+v", r)
	}
}
