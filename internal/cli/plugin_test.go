package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// TestBuiltinProvidersConform runs the conformance suite against built-in
// providers through `tug provider-exec`, the same process interface plugins
// use.
func TestBuiltinProvidersConform(t *testing.T) {
	if testing.Short() {
		t.Skip("builds tug")
	}
	isolate(t)
	bin := filepath.Join(t.TempDir(), "tug")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/tug").CombinedOutput(); err != nil {
		t.Fatalf("building tug: %v\n%s", err, out)
	}
	srv, _ := fakeAPIServer(t)
	kubeconfigPath := writeKubeconfig(t, srv)

	r := conformance.Run(context.Background(), conformance.Target{
		Command: bin,
		Args:    []string{"provider-exec", "kubeconfig"},
		Env:     os.Environ(),
	}, conformance.Options{
		Name:    "kubeconfig",
		Account: provider.Account{ID: "downloaded", Config: map[string]any{"path": kubeconfigPath}},
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the kubeconfig provider does not conform")
	}
}

func TestPluginLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script plugin")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	isolate(t)
	work := t.TempDir()
	dir := filepath.Join(work, "tug-provider-acme")

	out := mustTug(t, "plugin", "new", "acme", "--lang", "python", "--dir", dir)
	if !strings.Contains(out, "tug-provider-acme") {
		t.Fatalf("plugin new: %s", out)
	}

	dist := filepath.Join(work, "dist")
	out = mustTug(t, "plugin", "package", dir, "-o", dist)
	archive := strings.Fields(out)[0]
	if !strings.HasSuffix(archive, "tug-provider-acme_0.1.0.tar.gz") {
		t.Fatalf("plugin package: %s", out)
	}

	// Refusals before anything is installed.
	if r := tug(t, "plugin", "install", archive); r.code == 0 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("install without a terminal or --yes: %+v", r)
	}
	if r := tug(t, "plugin", "install", "https://example.com/p.tar.gz"); r.code == 0 || !strings.Contains(r.stderr, "--sha256") {
		t.Fatalf("URL without --sha256: %+v", r)
	}
	if r := tug(t, "plugin", "install", "coreweave"); r.code == 0 || !strings.Contains(r.stderr, "index") {
		t.Fatalf("install by name: %+v", r)
	}

	r := tug(t, "plugin", "install", archive, "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Installed acme 0.1.0") || !strings.Contains(r.stderr, "secrets:       api_token") ||
		!strings.Contains(r.stderr, "settings:      clusters: ") {
		t.Fatalf("install: %+v", r)
	}
	if out := mustTug(t, "plugin", "ls"); !strings.Contains(out, "acme") || !strings.Contains(out, "plugin") {
		t.Fatalf("plugin ls: %s", out)
	}

	t.Setenv("TUG_SECRET_API_TOKEN", "demo_test")
	var report conformance.Report
	out = mustTug(t, "plugin", "test", "acme", "--set", "clusters=dev=https://127.0.0.1:6443", "-o", "json")
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.Failed() {
		t.Fatalf("plugin test: %v\n%s", err, out)
	}

	out = mustTug(t, "import", "acme", "--account", "demo", "--set", "clusters=dev=https://127.0.0.1:6443")
	if !strings.Contains(out, "acme/demo: 1 clusters") {
		t.Fatalf("import through the installed plugin: %s", out)
	}
	if out := mustTug(t, "ls", "-o", "name"); strings.TrimSpace(out) != "dev" {
		t.Fatalf("ls: %s", out)
	}
	ec := mustTug(t, "auth", "token", "--cluster", "dev")
	if !strings.Contains(ec, `"token":"demo_test"`) {
		t.Fatalf("auth token through the plugin: %s", ec)
	}

	mustTug(t, "plugin", "remove", "acme")
	if out := mustTug(t, "plugin", "ls"); strings.Contains(out, "acme") {
		t.Fatalf("still listed after remove: %s", out)
	}
}

func TestProviderExecServesBuiltins(t *testing.T) {
	isolate(t)
	out := mustTug(t, "provider-exec", "kubeconfig", "describe")
	d, err := provider.DecodeDescribe([]byte(out))
	if err != nil || d.Name != "kubeconfig" || !d.Capabilities.Has(provider.CapAccounts) {
		t.Fatalf("describe = %+v, %v", d, err)
	}
	if r := tug(t, "provider-exec", "kubeconfig", "bogus"); r.code != provider.ExitUsage {
		t.Fatalf("unknown op: %+v", r)
	}
}

const slowPluginScript = `#!/bin/sh
cat >/dev/null
case "$1" in
  describe)
    sleep 1
    printf '{"apiVersion":"provider/v1alpha1","kind":"DescribeResponse","name":"slow","version":"1.0.0","description":"Clusters that take their time","protocols":["provider/v1alpha1"],"capabilities":["list","credential"]}\n'
    ;;
  list) printf '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[]}\n' ;;
  *) exit 2 ;;
esac
`

const slowPluginManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: slow, version: 1.0.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: GOOS, arch: GOARCH, bin: tug-provider-slow}
  hosts: [api.slow.example]
  accountConfig:
    type: object
    properties:
      zone: {type: string, description: "only clusters in this zone"}
`

// TestPluginHostFollowsConfigAndManifest covers what the plugin host takes
// from config.yaml and plugin.yaml beyond running the plugin: timeouts for
// every operation, warnings about fields and settings it does not know, and
// saying what unlink removed.
func TestPluginHostFollowsConfigAndManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	home := isolate(t)
	dir := t.TempDir()
	manifest := strings.NewReplacer("GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH).Replace(slowPluginManifest)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-slow"), []byte(slowPluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	setDescribeTimeout := func(d string) {
		t.Helper()
		cfg := filepath.Join(home, "config", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte("plugins:\n  timeouts:\n    describe: "+d+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r := tug(t, "plugin", "link", dir)
	if r.code != 0 || !strings.Contains(r.stderr, "does not know, which it ignores: spec.hosts") {
		t.Fatalf("link should warn about spec.hosts: %+v", r)
	}

	// describe takes a second; a configured timeout shorter than that wins
	// over the 30s default.
	setDescribeTimeout("200ms")
	if r := tug(t, "plugin", "describe", "slow"); r.code == 0 || !strings.Contains(r.stderr, "timed out after 200ms") {
		t.Fatalf("describe with a 200ms timeout: %+v", r)
	}
	setDescribeTimeout("10s")
	r = tug(t, "plugin", "describe", "slow")
	if r.code != 0 || !strings.Contains(r.stderr, "settings:      zone: only clusters in this zone") ||
		!strings.Contains(r.stderr, "description:   Clusters that take their time") {
		t.Fatalf("describe should show the description and account settings: %+v", r)
	}

	r = tug(t, "import", "slow", "--account", "a", "--set", "zone=x", "--set", "zoen=y")
	if r.code != 0 || !strings.Contains(r.stderr, "slow does not describe the setting zoen; it knows zone") {
		t.Fatalf("import should warn about --set zoen: %+v", r)
	}

	if out := mustTug(t, "plugin", "unlink", "slow"); !strings.Contains(out, "Unlinked slow ("+dir+")") {
		t.Fatalf("unlink should say what it removed: %q", out)
	}
}
