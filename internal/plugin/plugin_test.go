package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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
	token := map[string]string{"api_token": "ex_t0k"}
	clusters, err := prov.List(ctx, provider.ListRequest{Account: acct, Secrets: token})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0].Endpoints.Public != "https://127.0.0.1:6443" {
		t.Fatalf("clusters = %+v", clusters)
	}

	_, err = prov.List(ctx, provider.ListRequest{Account: provider.Account{ID: "empty"}, Secrets: token})
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
		Secrets: map[string]string{"api_token": "ex_t0k", "other_plugin_secret": "nope"},
	})
	if err != nil || cred.Token != "ex_t0k" {
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
  capabilities: [list, credential, checkAccess]
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
  checkAccess)
    ip=$(sed -n 's/.*"egressIP":"\([^"]*\)".*/\1/p')
    printf '{"apiVersion":"provider/v1alpha1","kind":"CheckAccessResponse","findings":[{"severity":"error","message":"%s is not allowed","fix":"add %s/32"}]}\n' "$ip" "$ip"
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
		{"bad minTugVersion", func(m *Manifest) { m.Spec.MinTugVersion = "soon" }, "spec.minTugVersion"},
		{"wasip1 without wasm", func(m *Manifest) { m.Spec.Platforms[0] = Platform{OS: WASMOS, Arch: "amd64", Bin: "x.wasm"} }, "{os: wasip1, arch: wasm}"},
		{"wasm without wasip1", func(m *Manifest) { m.Spec.Platforms[0] = Platform{OS: "js", Arch: WASMArch, Bin: "x.wasm"} }, "{os: wasip1, arch: wasm}"},
		{"two WebAssembly builds", func(m *Manifest) {
			m.Spec.Platforms = append(m.Spec.Platforms, Platform{OS: WASMOS, Arch: WASMArch, Bin: "a.wasm"}, Platform{OS: WASMOS, Arch: WASMArch, Bin: "b.wasm"})
		}, "more than one WebAssembly build"},
		{"WebAssembly with a described network", func(m *Manifest) {
			m.Spec.Platforms = append(m.Spec.Platforms, Platform{OS: WASMOS, Arch: WASMArch, Bin: "a.wasm"})
			m.Spec.Network = []string{"the server in the account's url setting"}
		}, "spec.network entry"},
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

// TestPlatformSelection checks which build Load runs: the native one, else
// the WebAssembly one, the WebAssembly one first when preferred, and in each
// case the first that is built.
func TestPlatformSelection(t *testing.T) {
	native := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH, Bin: "bin/native"}
	other := Platform{OS: "plan9", Arch: "386", Bin: "bin/plan9"}
	wasm := Platform{OS: WASMOS, Arch: WASMArch, Bin: "bin/plugin.wasm"}
	tests := []struct {
		name      string
		platforms []Platform
		built     []string
		prefer    bool
		want      string
		wantErr   string
	}{
		{"native", []Platform{native, wasm}, []string{"bin/native", "bin/plugin.wasm"}, false, "bin/native", ""},
		{"WebAssembly preferred", []Platform{native, wasm}, []string{"bin/native", "bin/plugin.wasm"}, true, "bin/plugin.wasm", ""},
		{"no native build for here", []Platform{other, wasm}, []string{"bin/plan9", "bin/plugin.wasm"}, false, "bin/plugin.wasm", ""},
		{"native not built", []Platform{native, wasm}, []string{"bin/plugin.wasm"}, false, "bin/plugin.wasm", ""},
		{"preferred but not built", []Platform{native, wasm}, []string{"bin/native"}, true, "bin/native", ""},
		{"nothing for here", []Platform{other}, []string{"bin/plan9"}, false, "", "no WebAssembly build"},
		{"nothing built", []Platform{native, wasm}, nil, false, "", "bin/native"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var lines []string
			for _, p := range tt.platforms {
				lines = append(lines, "  - {os: "+p.OS+", arch: "+p.Arch+", bin: "+p.Bin+"}")
			}
			manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: pick, version: 1.0.0}\n" +
				"spec:\n  protocols: [provider/v1alpha1]\n  platforms:\n" + strings.Join(lines, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, b := range tt.built {
				content := []byte("#!/bin/sh\n")
				if strings.HasSuffix(b, ".wasm") {
					content = wasmHeader
				}
				path := filepath.Join(dir, filepath.FromSlash(b))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			p, err := Load(dir, true, PreferWASM(tt.prefer))
			if tt.wantErr != "" {
				// With nothing built, the error is the best build's.
				if tt.built == nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Load = %v, want a missing file", err)
				}
				if err == nil || !strings.Contains(filepath.ToSlash(err.Error()), tt.wantErr) {
					t.Fatalf("Load = %v, %v; want an error containing %q", p, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := filepath.ToSlash(strings.TrimPrefix(p.Bin, dir+string(filepath.Separator))); got != tt.want || p.WASM != strings.HasSuffix(tt.want, ".wasm") {
				t.Fatalf("Load picked %s (WASM %v), want %s", got, p.WASM, tt.want)
			}
		})
	}
}

func TestLoadRefusesAWASMBuildThatIsNot(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: fake, version: 1.0.0}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  platforms:\n  - {os: wasip1, arch: wasm, bin: plugin.wasm}\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, true); err == nil || !strings.Contains(err.Error(), "is not a WebAssembly module") {
		t.Fatalf("Load of a script listed as WebAssembly = %v", err)
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
	if got, err := d.Unlink("script"); err != nil || got != devDir {
		t.Fatalf("Unlink(script) = %q, %v, want %q", got, err, devDir)
	}
	if _, err := d.Unlink("script"); err == nil {
		t.Fatal("Unlink of a plugin that is not linked succeeded")
	}
}

func TestRunnerTimeouts(t *testing.T) {
	r := &Runner{Timeouts: map[string]time.Duration{provider.OpList: time.Minute}}
	tests := []struct {
		op   string
		want time.Duration
	}{
		{provider.OpList, time.Minute}, // configured
		{provider.OpCredential, DefaultCredentialTimeout},
		{provider.OpRotate, DefaultRotateTimeout}, // longer than credential: providers poll after revoking
		{provider.OpDescribe, DefaultTimeout},
	}
	for _, tt := range tests {
		if got := r.timeout(tt.op); got != tt.want {
			t.Errorf("timeout(%s) = %s, want %s", tt.op, got, tt.want)
		}
	}
}

func TestCheckAccess(t *testing.T) {
	dir := writeScriptPlugin(t)
	p, err := Load(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	prov := NewProvider(p, &Runner{Bin: p.Bin, Name: p.Name()})
	findings, err := prov.CheckAccess(context.Background(), provider.AccessRequest{Cluster: provider.Cluster{ID: "a"}, EgressIP: "198.51.100.9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != provider.SeverityError || findings[0].Fix != "add 198.51.100.9/32" {
		t.Fatalf("findings = %+v", findings)
	}

	p.Manifest.Spec.Capabilities &^= provider.CapCheckAccess
	if _, err := prov.CheckAccess(context.Background(), provider.AccessRequest{}); provider.CodeOf(err) != provider.Unimplemented {
		t.Fatalf("a plugin without checkAccess: %v", err)
	}
}

func TestCheckTugVersion(t *testing.T) {
	tests := []struct {
		need, tug string
		ok        bool
	}{
		{"", "v0.1.0", true},
		{"0.5.0", "v0.4.0", false},
		{"v0.5.0", "v0.4.9", false},
		{"0.5.0", "v0.5.0", true},
		{"0.5", "v0.10.0", true},
		{"9.0.0", "dev", true},                     // a development build cannot be compared
		{"9.0.0", "v0.4.0-3-g1a2b3c4-dirty", true}, // nor can a local build between releases
	}
	for _, tt := range tests {
		m := &Manifest{Metadata: Metadata{Name: "acme"}, Spec: Spec{MinTugVersion: tt.need}}
		err := m.CheckTugVersion(tt.tug)
		if (err == nil) != tt.ok {
			t.Errorf("CheckTugVersion(%q) with minTugVersion %q = %v, want ok=%v", tt.tug, tt.need, err, tt.ok)
		}
	}
}

func TestTooNewPluginRefused(t *testing.T) {
	dir := writeScriptPlugin(t)
	path := filepath.Join(dir, ManifestFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "spec:\n", "spec:\n  minTugVersion: 9.0.0\n", 1))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PathEnv, dir)

	d := NewDirs(t.TempDir())
	d.TugVersion = "v0.4.0"
	if plugins, problems := d.Discover(); len(plugins) != 0 || len(problems) != 1 || !strings.Contains(problems[0].Err.Error(), "needs tug v9.0.0") {
		t.Fatalf("Discover() = %v, %v, want the plugin refused", plugins, problems)
	}
	if _, err := d.Link(dir); err == nil {
		t.Fatal("Link of a plugin that needs a newer tug succeeded")
	}

	d.TugVersion = "dev"
	if plugins, problems := d.Discover(); len(plugins) != 1 || len(problems) != 0 {
		t.Fatalf("Discover() on a development build = %v, %v, want the plugin", plugins, problems)
	}
}

func TestManifestUnknownFields(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
colour: blue
metadata: {name: acme, version: 1.0.0, publsher: acme}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: linux, arch: amd64, bin: tug-provider-acme, sha: abc}
  secrets:
  - {name: api_token, promt: "Token"}
  hosts: [api.acme.example]
  accountConfig:
    type: object
    properties:
      region: {type: string, description: "where clusters live"}
      auth: {type: string, enum: [token, oidc]}
`
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"colour", "metadata.publsher", "spec.hosts", "spec.platforms[0].sha", "spec.secrets[0].promt"}
	if !slices.Equal(m.Unknown, want) {
		t.Errorf("Unknown = %v, want %v", m.Unknown, want)
	}

	settings := m.Settings()
	if len(settings) != 2 || settings[0].Name != "auth" || !slices.Equal(settings[0].Enum, []string{"token", "oidc"}) ||
		settings[1].Name != "region" || settings[1].Description != "where clusters live" {
		t.Errorf("Settings() = %+v", settings)
	}
	if got := m.UnknownSettings(map[string]any{"region": "eu", "regoin": "eu", "auth": "oidc"}); !slices.Equal(got, []string{"regoin"}) {
		t.Errorf("UnknownSettings = %v, want [regoin]", got)
	}

	m.Spec.AccountConfig["additionalProperties"] = true
	if got := m.UnknownSettings(map[string]any{"anything": "x"}); len(got) != 0 {
		t.Errorf("UnknownSettings with additionalProperties = %v, want none", got)
	}
	if got := (&Manifest{}).UnknownSettings(map[string]any{"x": "y"}); len(got) != 0 {
		t.Errorf("UnknownSettings without a schema = %v, want none", got)
	}
}

func TestRotateNeedsTheCapability(t *testing.T) {
	dir := writeScriptPlugin(t) // declares list, credential and checkAccess only
	p, err := Load(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	prov := NewProvider(p, &Runner{Bin: p.Bin, Name: p.Name()})
	if _, err := prov.Rotate(context.Background(), provider.CredentialRequest{ClusterID: "a"}); provider.CodeOf(err) != provider.Unimplemented {
		t.Fatalf("Rotate = %v, want Unimplemented without the rotate capability", err)
	}
}

// TestServeModeKeepsOneProcess runs the example plugin in serve mode:
// concurrent calls share one process, a dead process is started again, and
// Close stops it.
func TestServeModeKeepsOneProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := buildExample(t)
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Manifest.Spec.Capabilities.Has(provider.CapServe) {
		t.Fatal("the example manifest should declare serve")
	}
	r := &Runner{Bin: p.Bin, Name: p.Name(), Serve: true}
	prov := NewProvider(p, r)
	t.Cleanup(func() { _ = prov.Close() })
	ctx := context.Background()
	acct := provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443"}}
	token := map[string]string{"api_token": "ex_t0k"}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			if i%2 == 0 {
				_, errs[i] = prov.List(ctx, provider.ListRequest{Account: acct, Secrets: token})
			} else {
				_, errs[i] = prov.Credential(ctx, provider.CredentialRequest{Account: acct, ClusterID: "dev", Secrets: token})
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	inst, done := r.srv.running()
	if _, err := prov.List(ctx, provider.ListRequest{Account: acct, Secrets: map[string]string{"api_token": "bad"}}); provider.CodeOf(err) != provider.Unauthenticated {
		t.Fatalf("a provider error through serve mode = %v, want Unauthenticated", err)
	}
	if again, _ := r.srv.running(); again != inst {
		t.Fatal("serve mode started a second process for a later call")
	}

	// The process dies; the next call starts a new one.
	_ = inst.kill()
	<-done
	if _, err := prov.Describe(ctx); err != nil {
		t.Fatalf("describe after the process died: %v", err)
	}
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Describe(ctx); err == nil {
		t.Fatal("a call after Close succeeded")
	}
}
