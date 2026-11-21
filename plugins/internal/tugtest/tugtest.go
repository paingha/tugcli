// Package tugtest runs a first-party plugin's conformance suite through the
// tug binary, as `tug plugin test`, so that the plugin is tested the way tug
// runs it: with the scrubbed environment and inside tug's sandbox, under the
// plugin's own manifest. It talks to tug only as a program, so the plugins
// keep to the public SDK.
package tugtest

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// tugPackage is built when TUG_BIN does not name a tug to use.
const tugPackage = "github.com/paingha/tugcli/cmd/tug"

// Plugin is what ConformInSandbox tests.
type Plugin struct {
	// Tug is the tug executable to run; empty means Build's.
	Tug string
	// Name is the provider name.
	Name string
	// Manifest is the path of the plugin's plugin.yaml.
	Manifest string
	// Bin is the plugin executable, already built for this platform.
	Bin string
	// Settings are the account settings, passed as --set. A setting that is
	// a URL with a port, such as a fake API's, also has its port allowed in
	// the sandbox.
	Settings map[string]any
	// Secrets are the account's secrets, passed as TUG_SECRET_<NAME>.
	Secrets map[string]string
	// Env names more variables of the test's environment for tug to pass to
	// the plugin, such as those a fake tool reads, by adding them to
	// spec.env.
	Env []string
}

// ConformInSandbox runs `tug plugin test` against p, which runs the
// conformance suite with the plugin in tug's sandbox where the platform has
// one, and fails t when a check fails. The rest of the test's environment,
// such as PATH with fake tools on it, reaches the plugin as tug passes it.
func ConformInSandbox(t *testing.T, p Plugin) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Log("Windows has no sandbox for plugins; the direct run is enough")
		return
	}
	tug := p.Tug
	if tug == "" {
		tug = Build(t)
	}
	dir := pluginDir(t, p)

	cmd := exec.Command(tug, "--debug", "plugin", "test", p.Name, "-o", "json")
	for _, k := range slices.Sorted(maps.Keys(p.Settings)) {
		cmd.Args = append(cmd.Args, "--set", fmt.Sprintf("%s=%v", k, p.Settings[k]))
	}
	cmd.Env = append(os.Environ(),
		"TUG_HOME="+t.TempDir(),
		"TUG_KEYRING_BACKEND=file",
		"TUG_KEYRING_PASSPHRASE=tugtest",
		"TUG_PLUGIN_PATH="+dir,
		"TUG_SESSION=",
		"NO_COLOR=1",
	)
	for name, v := range p.Secrets {
		cmd.Env = append(cmd.Env, "TUG_SECRET_"+strings.ToUpper(name)+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	var report struct {
		Results []struct {
			Name, Status, Detail string
		}
	}
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("tug plugin test %s: %v, and no report: %v\nstderr:\n%s", p.Name, runErr, err, stderr.String())
	}
	// Without a listing, most of what the plugin does in the sandbox would
	// go untested, so the settings must be enough for list to work.
	failed, listed := false, false
	for _, x := range report.Results {
		t.Logf("in tug: %-4s %-40s %s", x.Status, x.Name, x.Detail)
		failed = failed || x.Status == "fail"
		listed = listed || (x.Name == "list returns valid clusters" && x.Status == "pass")
	}
	if !listed {
		failed = true
		t.Errorf("in tug, list did not return clusters; give ConformInSandbox the settings it needs")
	}
	switch {
	case strings.Contains(stderr.String(), "plugin runs in the sandbox"):
		t.Log("in tug: the plugin ran in tug's sandbox")
	case strings.Contains(stderr.String(), "plugin runs without a sandbox"):
		t.Log("in tug: this platform has no sandbox, so the plugin ran without one")
	}
	if failed || runErr != nil {
		t.Fatalf("the %s plugin does not conform when tug runs it (%v); tug's log:\n%s", p.Name, runErr, stderr.String())
	}
}

// Build returns TUG_BIN, or builds tug from this module. A test that points
// HOME elsewhere must call it first, since go finds its caches through HOME.
func Build(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "" // ConformInSandbox has nothing to add there
	}
	if bin := os.Getenv("TUG_BIN"); bin != "" {
		return bin
	}
	if err := exec.Command("go", "list", tugPackage).Run(); err != nil {
		t.Skipf("no tug to run the plugin with: set TUG_BIN, or test from tug's own module (%v)", err)
	}
	bin := filepath.Join(t.TempDir(), "tug")
	if out, err := exec.Command("go", "build", "-o", bin, tugPackage).CombinedOutput(); err != nil {
		t.Fatalf("building tug: %v\n%s", err, out)
	}
	return bin
}

// pluginDir lays out a plugin directory for TUG_PLUGIN_PATH: the manifest,
// with the ports of URL settings added to spec.network and p.Env to
// spec.env, and the executable where the manifest says it is for this
// platform.
func pluginDir(t *testing.T, p Plugin) string {
	t.Helper()
	raw, err := os.ReadFile(p.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reading %s: %v", p.Manifest, err)
	}
	spec, _ := m["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("%s has no spec", p.Manifest)
	}
	network, _ := spec["network"].([]any)
	for _, v := range p.Settings {
		if u, err := url.Parse(fmt.Sprint(v)); err == nil && u.Port() != "" {
			network = append(network, u.Host)
		}
	}
	spec["network"] = network
	env, _ := spec["env"].([]any)
	for _, name := range p.Env {
		env = append(env, name)
	}
	spec["env"] = env

	var bin string
	platforms, _ := spec["platforms"].([]any)
	for _, entry := range platforms {
		if pl, ok := entry.(map[string]any); ok && pl["os"] == runtime.GOOS && pl["arch"] == runtime.GOARCH {
			bin, _ = pl["bin"].(string)
		}
	}
	if bin == "" {
		t.Fatalf("%s has no build for %s/%s", p.Manifest, runtime.GOOS, runtime.GOARCH)
	}

	dir := t.TempDir()
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(p.Bin)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.FromSlash(bin))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, exe, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
