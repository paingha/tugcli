package cli

import (
	"encoding/json"
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
	return packagePluginWith(t, name, version, "")
}

// packagePluginWith packages a shell script plugin whose manifest's spec
// also has the given lines.
func packagePluginWith(t *testing.T, name, version, spec string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: " + name + ", version: " + version + ", publisher: tests}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  capabilities: [list, credential]\n  platforms:\n" +
		"  - {os: " + runtime.GOOS + ", arch: " + runtime.GOARCH + ", bin: tug-provider-" + name + "}\n" + spec
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

func TestPluginRevocation(t *testing.T) {
	s := newPluginSite(t, "", "1.0.0", "2.0.0")
	mustTug(t, "plugin", "install", "idx@1.0.0", "--yes")
	if r := tug(t, "plugin", "ls"); r.code != 0 || strings.Contains(r.stderr, "revoked") {
		t.Fatalf("plugin ls before any revocation: %+v", r)
	}

	// The publisher revokes 1.0.0, from its archive.
	out := mustTug(t, "plugin", "index", "revoke", s.index, s.archives["1.0.0"], "--reason", "sends tokens to a third party")
	if !strings.Contains(out, "Revoked idx 1.0.0 (sends tokens to a third party)") {
		t.Fatalf("index revoke: %s", out)
	}
	s.sign(t)

	r := tug(t, "plugin", "ls")
	if r.code != 0 || !strings.Contains(r.stderr, "revoked: index test has revoked idx 1.0.0: sends tokens to a third party; remove it with tug plugin remove idx") {
		t.Fatalf("plugin ls after the revocation: %+v", r)
	}
	if r := tug(t, "plugin", "install", "idx@1.0.0", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "index test has revoked idx 1.0.0: sends tokens to a third party; not installed") {
		t.Fatalf("install of a revoked version by name: %+v", r)
	}
	if r := tug(t, "plugin", "install", s.archives["1.0.0"], "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "index test has revoked this archive, idx 1.0.0") {
		t.Fatalf("install of a revoked archive from a file: %+v", r)
	}
	// The latest version that is not revoked is the one installed by name.
	if r := tug(t, "plugin", "install", "idx", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "Installed idx 2.0.0") {
		t.Fatalf("install by name with one version revoked: %+v", r)
	}

	// With every version revoked, there is nothing to install or find.
	if out := mustTug(t, "plugin", "index", "revoke", s.index, s.archives["2.0.0"]); !strings.Contains(out, "Revoked idx 2.0.0") {
		t.Fatalf("index revoke: %s", out)
	}
	s.sign(t)
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "revoked every version of idx") {
		t.Fatalf("install by name with every version revoked: %+v", r)
	}
	if out := mustTug(t, "plugin", "search"); strings.Contains(out, "idx") {
		t.Fatalf("plugin search lists a plugin with every version revoked: %s", out)
	}
}

func TestPluginIndexKeyRotation(t *testing.T) {
	s := newPluginSite(t, "", "1.0.0")
	next := strings.TrimSpace(mustTug(t, "plugin", "index", "keygen", "--name", "next"))

	// The publisher hands over to the new key and signs with it.
	if out := mustTug(t, "plugin", "index", "rotate", s.index, "--to", "next"); !strings.Contains(out, "hands over from "+s.pub+" to "+next) {
		t.Fatalf("index rotate: %s", out)
	}
	if r := tug(t, "plugin", "index", "sign", s.index); r.code == 0 || !strings.Contains(r.stderr, "superseded") {
		t.Fatalf("index sign with the replaced key: %+v", r)
	}
	mustTug(t, "plugin", "index", "sign", s.index, "--name", "next")
	if out := mustTug(t, "plugin", "index", "verify", s.index, "--public-key", s.pub); !strings.Contains(out, "signed by "+next+", which that key handed over to,") {
		t.Fatalf("index verify with the old key: %s", out)
	}
	if err := os.RemoveAll(filepath.Join(s.home, "cache", "plugin-index")); err != nil {
		t.Fatal(err)
	}

	// A user whose config.yaml names the old key follows the handover and
	// is told to update it.
	r := tug(t, "plugin", "search")
	if r.code != 0 || !strings.Contains(r.stdout, "idx") || !strings.Contains(r.stderr, "index test has handed over from the key in config.yaml to "+next) {
		t.Fatalf("plugin search after a key rotation: %+v", r)
	}

	// Someone with the old key cannot then serve an index of their own:
	// tug has moved to the new key.
	forged := filepath.Join(t.TempDir(), "index.yaml")
	if err := os.Rename(s.index, forged); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "plugin", "index", "add", s.index, s.archives["1.0.0"], "--url", s.srv.URL+"/"+filepath.Base(s.archives["1.0.0"]), "--description", "trust me")
	mustTug(t, "plugin", "index", "sign", s.index)
	if err := os.RemoveAll(filepath.Join(s.home, "cache", "plugin-index")); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "plugin", "search"); strings.Contains(r.stdout, "idx") || !strings.Contains(r.stderr, "index test: the index signature does not match its key") {
		t.Fatalf("plugin search of an index signed with the replaced key: %+v", r)
	}
}

func TestPluginInspect(t *testing.T) {
	s := newPluginSite(t, "  limits: {cpuSeconds: 30}\n", "1.0.0")
	archive := packagePluginWith(t, "idx", "2.0.0", `  secrets: [{name: token}, {name: org, optional: true}]
  env: [IDX_PROFILE]
  network: [api.idx.example]
  sandbox: {tools: [sh, idxctl-not-installed], read: ["~/.idx"], ports: [8443]}
  limits: {cpuSeconds: 120, memoryMiB: 256, timeouts: {list: 2m}}
`)
	published := filepath.Join(s.site, filepath.Base(archive))
	if err := os.Rename(archive, published); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "plugin", "index", "add", s.index, published, "--url", s.srv.URL+"/"+filepath.Base(published))
	s.sign(t)

	out := mustTug(t, "plugin", "inspect", published)
	for _, want := range []string{
		"idx 2.0.0",
		"listed in index test",
		"signature:     none: index test names no Sigstore signature for it",
		"revoked:       no",
		"secrets:       token, org (optional)",
		"environment:   IDX_PROFILE",
		"network hosts: api.idx.example",
		"reads:         ~/.idx",
		"idxctl-not-installed (not found here)",
		"30s of CPU time and 256 MiB of memory per process",
		"list 2m0s",
		"it asks for more: cpuSeconds 120, capped at 30",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plugin inspect of an archive lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.home, "data", "plugins", "idx")); err == nil {
		t.Fatal("plugin inspect installed the plugin")
	}

	var r inspection
	if err := json.Unmarshal([]byte(mustTug(t, "plugin", "inspect", published, "-o", "json")), &r); err != nil {
		t.Fatal(err)
	}
	if r.Source.Kind != "archive" || r.Source.Index != "test" || r.Limits.CPUSeconds != 30 || len(r.Secrets) != 2 || r.Runs.As != "native" || r.Revoked != nil {
		t.Fatalf("plugin inspect -o json = %+v", r)
	}

	// From the index, by name@version, and installed.
	if out := mustTug(t, "plugin", "inspect", "idx@1.0.0"); !strings.Contains(out, "idx 1.0.0") || !strings.Contains(out, "source:        index test, "+s.srv.URL) {
		t.Fatalf("plugin inspect from the index: %s", out)
	}
	mustTug(t, "plugin", "install", "idx@1.0.0", "--yes")
	out = mustTug(t, "plugin", "inspect", "idx")
	if !strings.Contains(out, "source:        installed in ") || !strings.Contains(out, "listed in index test") || !strings.Contains(out, "installed from "+s.srv.URL) {
		t.Fatalf("plugin inspect of an installed plugin: %s", out)
	}

	// Revoked once installed, and from the index while pinned elsewhere.
	mustTug(t, "plugin", "index", "revoke", s.index, s.archives["1.0.0"], "--reason", "leaks tokens")
	s.sign(t)
	if out := mustTug(t, "plugin", "inspect", "idx"); !strings.Contains(out, "revoked:       REVOKED by index test: leaks tokens") {
		t.Fatalf("plugin inspect of a revoked installed plugin: %s", out)
	}
	s.config(t, "  pins: {idx: \"2.0.0\"}\n")
	out = mustTug(t, "plugin", "inspect", "idx@1.0.0")
	if !strings.Contains(out, "revoked:       REVOKED by index test: leaks tokens") || !strings.Contains(out, "pinned:        2.0.0 in config.yaml, so tug plugin install refuses 1.0.0") {
		t.Fatalf("plugin inspect of a revoked version pinned to another: %s", out)
	}
	// With the pinned version not installed, the name means the pin.
	if out := mustTug(t, "plugin", "inspect", "idx"); !strings.Contains(out, "idx 2.0.0") || !strings.Contains(out, "pinned:        2.0.0 in config.yaml\n") {
		t.Fatalf("plugin inspect of a plugin pinned to a version that is not installed: %s", out)
	}

	if r := tug(t, "plugin", "inspect", "kubeconfig"); r.code == 0 || !strings.Contains(r.stderr, "built into tug") {
		t.Fatalf("plugin inspect of a built-in: %+v", r)
	}
	if r := tug(t, "plugin", "inspect", "nope"); r.code == 0 || !strings.Contains(r.stderr, "no plugin nope in the plugin indexes") {
		t.Fatalf("plugin inspect of an unknown plugin: %+v", r)
	}
}

func TestPluginInspectSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	s := newSignedIndex(t, false)
	s.add(t, s.signedBy(releaseWorkflow)...)
	if out := mustTug(t, "plugin", "inspect", "idx@1.0.0"); !strings.Contains(out, "signed by:     "+releaseWorkflow) || !strings.Contains(out, "(checked now)") {
		t.Fatalf("plugin inspect of a signed plugin: %s", out)
	}
	mustTug(t, "plugin", "install", "idx", "--yes")
	if out := mustTug(t, "plugin", "inspect", "idx"); !strings.Contains(out, "signed by:     "+releaseWorkflow+" (checked when it was installed from ") {
		t.Fatalf("plugin inspect of an installed signed plugin: %s", out)
	}

	// A signature by someone else shows as invalid.
	s.add(t, s.signedBy(strings.Replace(releaseWorkflow, "tests/", "mallory/", 1))...)
	if out := mustTug(t, "plugin", "inspect", "idx@1.0.0"); !strings.Contains(out, "signature:     INVALID") {
		t.Fatalf("plugin inspect with the wrong signer: %s", out)
	}
}
