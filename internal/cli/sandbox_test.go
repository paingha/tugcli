package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/conformance"
)

// linkExample builds the example plugin and links it.
func linkExample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "tug-provider-example")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../examples/tug-provider-example").CombinedOutput(); err != nil {
		t.Fatalf("building the example plugin: %v\n%s", err, out)
	}
	manifest, err := os.ReadFile("../../examples/tug-provider-example/plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "plugin", "link", dir)
	return dir
}

func writeTestConfig(t *testing.T, home, yaml string) {
	t.Helper()
	cfg := filepath.Join(home, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPluginTestRunsInTheSandbox runs the conformance suite through
// `tug plugin test` with the plugin in its sandbox, as every other command
// runs it, and without it when config.yaml leaves the plugin out.
func TestPluginTestRunsInTheSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin")
	}
	home := isolate(t)
	linkExample(t)
	t.Setenv("TUG_SECRET_API_TOKEN", "ex_t0k")
	confined := sandbox.Check().Any()

	run := func(wantLog string) {
		t.Helper()
		r := tug(t, "--debug", "plugin", "test", "example", "--set", "clusters=dev=https://127.0.0.1:6443", "-o", "json")
		var report conformance.Report
		if err := json.Unmarshal([]byte(r.stdout), &report); err != nil || report.Failed() || r.code != 0 {
			t.Fatalf("plugin test: exit %d, %v\nstdout: %s\nstderr: %s", r.code, err, r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, wantLog) {
			t.Errorf("plugin test logged no %q:\n%s", wantLog, r.stderr)
		}
	}
	if confined {
		run("plugin runs in the sandbox")
	} else {
		run("plugin runs without a sandbox")
	}

	writeTestConfig(t, home, "plugins:\n  sandbox:\n    exclude: [example]\n")
	run("plugin runs without the sandbox, as plugins.sandbox in config.yaml says")
}

func TestPluginDescribeShowsTheSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin")
	}
	home := isolate(t)
	linkExample(t)
	want := "runs as:       a process in the operating system's sandbox: TCP ports 443"
	if st := sandbox.Check(); !st.Any() {
		want = "runs as:       a process with your user's access: there is no sandbox here"
	}
	if r := tug(t, "plugin", "describe", "example"); r.code != 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("plugin describe should show %q: %+v", want, r)
	}
	writeTestConfig(t, home, "plugins:\n  sandbox:\n    enabled: false\n")
	want = "runs as:       a process with your user's access: plugins.sandbox in config.yaml turns the sandbox off for it"
	if r := tug(t, "plugin", "describe", "example"); r.code != 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("plugin describe with the sandbox off should show %q: %+v", want, r)
	}
}

// TestPluginSandboxExecNeedsAPolicy runs the hidden launcher the way only
// tug should not: by hand, without the policy tug passes it.
func TestPluginSandboxExecNeedsAPolicy(t *testing.T) {
	isolate(t)
	r := tug(t, sandbox.ExecCommand, "/bin/true", "--help")
	if r.code == 0 || !strings.HasPrefix(r.stderr, "tug: ") || r.stdout != "" {
		t.Fatalf("%s without a policy: %+v", sandbox.ExecCommand, r)
	}
	if r := tug(t, sandbox.ExecCommand); r.code == 0 {
		t.Fatalf("%s with nothing to run: %+v", sandbox.ExecCommand, r)
	}
}
