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
	if r.code != 0 || !strings.Contains(r.stdout, "Installed acme 0.1.0") || !strings.Contains(r.stderr, "secrets:       api_token") {
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
