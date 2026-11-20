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
	// The management cluster providers read from a cluster in the catalog.
	importManagementCluster(t)

	tests := []struct {
		name    string
		account provider.Account
	}{
		{"kubeconfig", provider.Account{ID: "downloaded", Config: map[string]any{"path": kubeconfigPath}}},
		{"capi", provider.Account{ID: "fleet", Config: map[string]any{"cluster": "payments-staging"}}},
		{"argocd", provider.Account{ID: "gitops", Config: map[string]any{"cluster": "payments-staging"}}},
		{"clusterprofile", provider.Account{ID: "inventory", Config: map[string]any{
			"cluster": "payments-staging", "providerFile": writeProvidersFile(t, "tug-test-token-plugin"),
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := conformance.Run(context.Background(), conformance.Target{
				Command: bin,
				Args:    []string{"provider-exec", tt.name},
				Env:     os.Environ(),
			}, conformance.Options{Name: tt.name, Account: tt.account})
			for _, x := range r.Results {
				t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
			}
			if r.Failed() {
				t.Fatalf("the %s provider does not conform", tt.name)
			}
			for _, x := range r.Results {
				if x.Name == "credential is valid and unexpired" && x.Status != conformance.Pass {
					t.Fatalf("%s: %s %s", x.Name, x.Status, x.Detail)
				}
			}
		})
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

const rotatePluginScript = `#!/bin/sh
cat >/dev/null
reply() { printf '{"apiVersion":"provider/v1alpha1","kind":"%s",%s}\n' "$1" "$2"; }
case "$1" in
  list) reply ListResponse '"clusters":[{"id":"r1","name":"rot-dev","endpoints":{"public":"https://127.0.0.1:1"}},{"id":"r2","name":"rot-prod","endpoints":{"public":"https://127.0.0.1:2"},"labels":{"env":"prod"}}]' ;;
  credential) reply CredentialResponse '"credential":{"type":"token","token":"old-token"}' ;;
  rotate) reply CredentialResponse '"credential":{"type":"token","token":"new-token"}' ;;
  *) exit 2 ;;
esac
`

const rotatePluginManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: rot, version: 1.0.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential, rotate]
  platforms:
  - {os: GOOS, arch: GOARCH, bin: tug-provider-rot}
`

func TestAuthRotate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	isolate(t)
	dir := t.TempDir()
	manifest := strings.NewReplacer("GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH).Replace(rotatePluginManifest)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-rot"), []byte(rotatePluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "plugin", "link", dir)
	mustTug(t, "import", "rot", "--account", "a")
	token := func(cluster string) string {
		t.Helper()
		return mustTug(t, "auth", "token", "--cluster", cluster)
	}
	if ec := token("rot-dev"); !strings.Contains(ec, `"token":"old-token"`) {
		t.Fatalf("token before rotating: %s", ec)
	}

	// Without a terminal, rotating needs --yes.
	if r := tug(t, "auth", "rotate", "rot-dev"); r.code == 0 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("rotate without --yes: %+v", r)
	}
	r := tug(t, "auth", "rotate", "rot-dev", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Rotated the credential for rot-dev") {
		t.Fatalf("rotate --yes: %+v", r)
	}
	// The cache now holds the rotated token; the credential operation would
	// still answer old-token.
	if ec := token("rot-dev"); !strings.Contains(ec, `"token":"new-token"`) {
		t.Fatalf("token after rotating: %s", ec)
	}

	// A read-only cluster is not rotated without a terminal, even with --yes...
	if r := tug(t, "auth", "rotate", "rot-prod", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "rot-prod is read-only") {
		t.Fatalf("rotate a read-only cluster: %+v", r)
	}
	if ec := token("rot-prod"); !strings.Contains(ec, `"token":"old-token"`) {
		t.Fatalf("read-only cluster rotated anyway: %s", ec)
	}
	// ...unless this terminal is connected to it and unlocked.
	newSession(t)
	mustTug(t, "connect", "rot-prod")
	mustTug(t, "unlock", "5m")
	if out := mustTug(t, "auth", "rotate", "--yes"); !strings.Contains(out, "Rotated the credential for rot-prod") {
		t.Fatalf("rotate the unlocked session's cluster: %s", out)
	}
	if ec := token("rot-prod"); !strings.Contains(ec, `"token":"new-token"`) {
		t.Fatalf("token after rotating the unlocked cluster: %s", ec)
	}
}

func TestAuthRotateUnsupportedProvider(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	if r := tug(t, "auth", "rotate", "prod", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "cannot rotate credentials") {
		t.Fatalf("rotate through the kubeconfig provider: %+v", r)
	}
	if r := tug(t, "auth", "rotate", "--yes"); r.code == 0 || !strings.Contains(r.stderr, "name a cluster") {
		t.Fatalf("rotate with no cluster and no session: %+v", r)
	}
}
