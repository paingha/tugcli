package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// buildExample compiles examples/tug-provider-example into a plugin directory.
func buildExample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "tug-provider-example")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "../../examples/tug-provider-example")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building example plugin: %v\n%s", err, out)
	}
	manifest, err := os.ReadFile("../../examples/tug-provider-example/plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSDKPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := buildExample(t)
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	prov := NewProvider(p, &Runner{Bin: p.Bin, Name: p.Name()})
	ctx := context.Background()

	d, err := prov.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "example" || !d.Capabilities.Has(provider.CapList|provider.CapCredential) {
		t.Fatalf("describe = %+v", d)
	}

	acct := provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443,ci=https://ci:6443"}}
	clusters, err := prov.List(ctx, provider.ListRequest{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0].Endpoints.Public != "https://127.0.0.1:6443" {
		t.Fatalf("clusters = %+v", clusters)
	}

	_, err = prov.List(ctx, provider.ListRequest{Account: provider.Account{ID: "empty"}})
	var pe *provider.Error
	if provider.CodeOf(err) != provider.InvalidConfig {
		t.Fatalf("want InvalidConfig, got %v", err)
	}
	if pe = provider.AsError(err); pe.Hint == "" {
		t.Fatal("hint lost on the wire")
	}

	// Undeclared secrets are never sent.
	cred, err := prov.Credential(ctx, provider.CredentialRequest{
		Account: acct, ClusterID: "dev",
		Secrets: map[string]string{"api_token": "t0k", "other_plugin_secret": "nope"},
	})
	if err != nil || cred.Token != "t0k" {
		t.Fatalf("credential = %+v, %v", cred, err)
	}
	if got := prov.declared(map[string]string{"api_token": "a", "x": "b"}); len(got) != 1 {
		t.Fatalf("declared = %v", got)
	}

	if _, err := prov.Accounts(ctx); provider.CodeOf(err) != provider.Unimplemented {
		t.Fatalf("accounts: want Unimplemented, got %v", err)
	}
}

const scriptManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: script, version: 1.0.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: %s, arch: %s, bin: tug-provider-script, sha256: %s}
  env: [DECLARED_VAR]
`

// A plugin needs no SDK: this one is a shell script.
const script = `#!/bin/sh
case "$1" in
  list)
    cat >/dev/null
    printf '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"a","name":"a","endpoints":{"public":"https://a"},"labels":{"leaky":"%s","declared":"%s"}}]}\n' "$LEAKY_VAR" "$DECLARED_VAR"
    ;;
  credential)
    echo "fetching credential" >&2
    printf '{"apiVersion":"provider/v1alpha1","kind":"Error","code":"RateLimited","message":"slow down","retryAfterSeconds":7}\n'
    exit 1
    ;;
  hang) sleep 10 ;;
  *) exit 2 ;;
esac
`

func writeScriptPlugin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tug-provider-script")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	m := strings.NewReplacer("%s", "\x00").Replace(scriptManifest)
	for _, v := range []string{runtime.GOOS, runtime.GOARCH, hex.EncodeToString(sum[:])} {
		m = strings.Replace(m, "\x00", v, 1)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScriptPluginScrubsEnvAndMapsErrors(t *testing.T) {
	dir := writeScriptPlugin(t)
	t.Setenv("LEAKY_VAR", "leaked")
	t.Setenv("DECLARED_VAR", "passed")

	p, err := Load(dir, false) // installed: checksum verified
	if err != nil {
		t.Fatal(err)
	}
	prov := NewProvider(p, &Runner{Bin: p.Bin, Name: p.Name(), Env: p.Manifest.Spec.Env})
	clusters, err := prov.List(context.Background(), provider.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if l := clusters[0].Labels; l["leaky"] != "" || l["declared"] != "passed" {
		t.Fatalf("environment not scrubbed: %v", l)
	}

	_, err = prov.Credential(context.Background(), provider.CredentialRequest{ClusterID: "a"})
	pe := provider.AsError(err)
	if pe.Code != provider.RateLimited || pe.RetryAfter() != 7*time.Second {
		t.Fatalf("error = %+v", pe)
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	dir := writeScriptPlugin(t)
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-script"), []byte(script+"# tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, false); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered plugin loaded: %v", err)
	}
	if _, err := Load(dir, true); err != nil {
		t.Fatalf("dev plugin should skip the checksum: %v", err)
	}
}

func TestTimeoutKillsPlugin(t *testing.T) {
	dir := writeScriptPlugin(t)
	r := &Runner{Bin: filepath.Join(dir, "tug-provider-script"), Name: "script", Timeouts: map[string]time.Duration{"hang": 200 * time.Millisecond}}
	start := time.Now()
	_, err := r.Call(context.Background(), "hang", nil)
	if provider.CodeOf(err) != provider.Unavailable {
		t.Fatalf("want Unavailable timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > killGrace+2*time.Second {
		t.Fatalf("plugin not killed promptly: %s", elapsed)
	}
}

func TestResponseSizeCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	bin := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nhead -c 4096 /dev/zero\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, Name: "big", MaxBytes: 1024}
	if _, err := r.Call(context.Background(), "list", nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want size error, got %v", err)
	}
}

func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"bad name", func(m *Manifest) { m.Metadata.Name = "Bad_Name" }, "metadata.name"},
		{"no protocols", func(m *Manifest) { m.Spec.Protocols = []string{"provider/v9"} }, "spec.protocols"},
		{"escaping bin", func(m *Manifest) { m.Spec.Platforms[0].Bin = "../../bin/sh" }, "inside the plugin directory"},
		{"bad env", func(m *Manifest) { m.Spec.Env = []string{"A=B"} }, "spec.env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{
				APIVersion: ManifestAPIVersion, Kind: ManifestKind,
				Metadata: Metadata{Name: "ok", Version: "1"},
				Spec: Spec{
					Protocols: []string{provider.APIVersion},
					Platforms: []Platform{{OS: "linux", Arch: "amd64", Bin: "bin/x"}},
				},
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("base manifest invalid: %v", err)
			}
			tt.mut(m)
			if err := m.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestDiscoverPrecedence(t *testing.T) {
	data := t.TempDir()
	d := NewDirs(data)

	installed := filepath.Join(d.Installed, "script")
	for _, v := range []string{"0.9.0", "1.10.0", "1.2.0"} {
		src := writeScriptPlugin(t)
		dst := filepath.Join(installed, v)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(PathEnv, "")
	plugins, problems := d.Discover()
	if len(problems) != 0 || len(plugins) != 1 {
		t.Fatalf("plugins %v problems %v", plugins, problems)
	}
	if filepath.Base(plugins[0].Dir) != "1.10.0" || plugins[0].Dev {
		t.Fatalf("picked %s (dev=%v), want 1.10.0", plugins[0].Dir, plugins[0].Dev)
	}

	// A linked dev copy wins over the installed one.
	devDir := writeScriptPlugin(t)
	if _, err := d.Link(devDir); err != nil {
		t.Fatal(err)
	}
	plugins, _ = d.Discover()
	if plugins[0].Dir != devDir || !plugins[0].Dev {
		t.Fatalf("link not preferred: %+v", plugins[0])
	}
	if err := d.Unlink("script"); err != nil {
		t.Fatal(err)
	}
}
