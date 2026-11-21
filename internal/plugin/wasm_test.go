package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// Set in the environment of the test binary when it is re-run as the
// process that hosts a WebAssembly plugin, so the conformance suite can test
// a module through a process as `tug provider-exec` serves it.
const (
	wasmHostEnv  = "TUG_TEST_WASM_HOST"  // the plugin directory
	wasmCacheEnv = "TUG_TEST_WASM_CACHE" // the compilation cache
)

// wasmShared holds what the tests share: the modules, built once, and a
// compilation cache, so that each module is compiled once for the run.
var wasmShared struct {
	dir     string
	mu      sync.Mutex
	modules map[string]string
}

func TestMain(m *testing.M) {
	// On Linux, tug starts a sandboxed plugin by running itself as
	// sandbox.ExecCommand, and this test binary stands in for tug.
	if len(os.Args) > 1 && os.Args[1] == sandbox.ExecCommand {
		err := sandbox.Exec(os.Args[2:])
		fmt.Fprintf(os.Stderr, "tug: %v\n", err)
		os.Exit(1)
	}
	if dir := os.Getenv(wasmHostEnv); dir != "" {
		os.Exit(wasmHostMain(dir))
	}
	dir, err := os.MkdirTemp("", "tug-wasm-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	wasmShared.dir = dir
	code := m.Run()
	_ = os.RemoveAll(dir) // best effort; the code is what matters
	os.Exit(code)
}

// wasmHostMain runs the WebAssembly plugin in dir with this process's
// arguments and streams, stopping on SIGTERM, as `tug provider-exec` does.
func wasmHostMain(dir string) int {
	p, err := Load(dir, true, PreferWASM(true))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r := &Runner{Bin: p.Bin, Name: p.Name(), Env: p.Manifest.Spec.Env, WASM: &WASMOptions{
		Network:  p.Manifest.Spec.Network,
		CacheDir: os.Getenv(wasmCacheEnv),
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := r.Exec(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}

func wasmCache() string { return filepath.Join(wasmShared.dir, "cache") }

// wasmTarget is the conformance target for a WebAssembly plugin: this test
// binary, re-run as its host.
func wasmTarget(t *testing.T, p *Plugin) conformance.Target {
	t.Helper()
	// Compile once here, into the cache the host processes share, rather
	// than in the first of them, which has a call's timeout to do it in.
	warm := &Runner{Bin: p.Bin, Name: p.Name(), WASM: &WASMOptions{CacheDir: wasmCache()}}
	if err := warm.Prepare(); err != nil {
		t.Fatal(err)
	}
	_ = warm.Close()
	return conformance.Target{
		Command: os.Args[0],
		Env:     append(os.Environ(), wasmHostEnv+"="+p.Dir, wasmCacheEnv+"="+wasmCache()),
		Module:  p.Bin,
	}
}

// buildWASM compiles a Go plugin for wasip1 once per test run and returns
// the module's path.
func buildWASM(t *testing.T, pkg string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a WebAssembly module")
	}
	wasmShared.mu.Lock()
	defer wasmShared.mu.Unlock()
	if path, ok := wasmShared.modules[pkg]; ok {
		return path
	}
	path := filepath.Join(wasmShared.dir, filepath.Base(pkg)+".wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-o", path, pkg)
	cmd.Env = append(os.Environ(), "GOOS="+WASMOS, "GOARCH="+WASMArch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s for %s/%s: %v\n%s", pkg, WASMOS, WASMArch, err, out)
	}
	if wasmShared.modules == nil {
		wasmShared.modules = map[string]string{}
	}
	wasmShared.modules[pkg] = path
	return path
}

// wasmPluginDir makes a plugin directory holding module at bin/plugin.wasm
// and the given manifest.
func wasmPluginDir(t *testing.T, module, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "plugin.wasm"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// exampleWASMDir is the example plugin with only its WebAssembly build, so
// Load picks that build on every machine.
func exampleWASMDir(t *testing.T) string {
	t.Helper()
	module := buildWASM(t, "../../examples/tug-provider-example")
	manifest, err := os.ReadFile("../../examples/tug-provider-example/plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := &Manifest{}
	if err := yaml.Unmarshal(manifest, m); err != nil {
		t.Fatal(err)
	}
	var wasm Platform
	for _, p := range m.Spec.Platforms {
		if p.IsWASM() {
			wasm = p
		}
	}
	if wasm.Bin == "" {
		t.Fatal("the example manifest lists no WebAssembly build")
	}
	b, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, filepath.FromSlash(wasm.Bin))
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const probeManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: probe, version: 0.1.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential, serve]
  platforms:
  - {os: wasip1, arch: wasm, bin: bin/plugin.wasm}
  env: [PROBE_DECLARED]
  network: [%s]
`

// probeRunner returns a runner for the probe plugin, allowed to reach the
// hosts in network.
func probeRunner(t *testing.T, o WASMOptions, network ...string) *Runner {
	t.Helper()
	dir := wasmPluginDir(t, buildWASM(t, "./testdata/tug-provider-probe"), fmt.Sprintf(probeManifest, strings.Join(network, ", ")))
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.WASM {
		t.Fatalf("Load(%s) picked %s, want the WebAssembly build", dir, p.Bin)
	}
	o.Network = p.Manifest.Spec.Network
	if o.CacheDir == "" {
		o.CacheDir = wasmCache()
	}
	r := &Runner{Bin: p.Bin, Name: p.Name(), Env: p.Manifest.Spec.Env, WASM: &o}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// probe runs list with the account settings and returns the one cluster's
// labels.
func probe(t *testing.T, r *Runner, settings map[string]any) (map[string]string, error) {
	t.Helper()
	p := NewProvider(&Plugin{Manifest: &Manifest{Metadata: Metadata{Name: "probe"}}}, r)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "probe", Config: settings}})
	if err != nil {
		return nil, err
	}
	if len(clusters) != 1 {
		t.Fatalf("probe listed %d clusters, want 1", len(clusters))
	}
	return clusters[0].Labels, nil
}

// TestWASMExampleConforms runs the example plugin, compiled to WebAssembly,
// through the host and the conformance suite.
func TestWASMExampleConforms(t *testing.T) {
	dir := exampleWASMDir(t)
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.WASM {
		t.Fatalf("Load picked %s, want the WebAssembly build", p.Bin)
	}
	r := conformance.Run(context.Background(), wasmTarget(t, p), conformance.Options{
		Name:            "example",
		Account:         demoAccount,
		Secrets:         map[string]string{"api_token": "ex_conformance"},
		DeclaredSecrets: p.Manifest.Spec.Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-50s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the example plugin does not conform as WebAssembly")
	}
	for _, x := range r.Results {
		skippable := x.Name == "accounts is valid" || (x.Name == "SIGTERM stops the plugin promptly" && runtime.GOOS == "windows")
		if x.Status != conformance.Pass && !skippable {
			t.Errorf("%s: %s %s", x.Name, x.Status, x.Detail)
		}
	}
}

func TestWASMExampleInProcess(t *testing.T) {
	dir := exampleWASMDir(t)
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, serve := range []bool{false, true} {
		t.Run(fmt.Sprintf("serve=%v", serve), func(t *testing.T) {
			r := &Runner{Bin: p.Bin, Name: p.Name(), Serve: serve, WASM: &WASMOptions{CacheDir: wasmCache()}}
			prov := NewProvider(p, r)
			t.Cleanup(func() { _ = prov.Close() })
			ctx := context.Background()
			token := map[string]string{"api_token": "ex_t0k"}
			var wg sync.WaitGroup
			errs := make([]error, 6)
			for i := range errs {
				wg.Go(func() {
					clusters, err := prov.List(ctx, provider.ListRequest{Account: demoAccount, Secrets: token})
					if err == nil && (len(clusters) != 1 || clusters[0].ID != "dev") {
						err = fmt.Errorf("list = %+v", clusters)
					}
					errs[i] = err
				})
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			_, err := prov.Credential(ctx, provider.CredentialRequest{Account: demoAccount, ClusterID: "dev", Secrets: map[string]string{"api_token": "nope"}})
			if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Hint, "ex_") {
				t.Fatalf("credential with a bad token = %v (hint %q), want Unauthenticated with a hint", err, pe.Hint)
			}
			if serve {
				inst, _ := r.srv.running()
				if _, ok := inst.(*wasmInstance); !ok {
					t.Fatalf("serve mode runs %T, want a module instance", inst)
				}
				if _, err := prov.Describe(ctx); err != nil {
					t.Fatal(err)
				}
				if again, _ := r.srv.running(); again != inst {
					t.Fatal("serve mode started a second instance for a later call")
				}
			}
			if err := prov.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestWASMNetwork checks tug_http: declared hosts are reached over https,
// anything else is refused, and limits hold.
func TestWASMNetwork(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		switch r.URL.Path {
		case "/hello":
			fmt.Fprint(w, "hello")
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "%s %s", r.Method, b)
		case "/redirect":
			u, _ := url.Parse(srv.URL)
			http.Redirect(w, r, "https://localhost:"+u.Port()+"/hello", http.StatusFound)
		case "/local-redirect":
			http.Redirect(w, r, "/hello", http.StatusFound)
		case "/big":
			chunk := make([]byte, 1<<20)
			for range provider.MaxHostResponseBodyBytes>>20 + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := probeRunner(t, WASMOptions{Transport: srv.Client().Transport}, u.Host)
	if res := conformance.CheckModule(r.Bin); res.Status != conformance.Pass || !strings.Contains(res.Detail, "read_body") {
		t.Fatalf("CheckModule(probe) = %+v, want a pass naming the tug_http functions it imports", res)
	}

	tests := []struct {
		name     string
		settings map[string]any
		status   string
		body     string
		wantErr  string
	}{
		{"declared host", map[string]any{"url": srv.URL + "/hello"}, "200", "hello", ""},
		{"post with a body", map[string]any{"url": srv.URL + "/echo", "post": "ping"}, "200", "POST ping", ""},
		{"redirect on the same host", map[string]any{"url": srv.URL + "/local-redirect"}, "200", "hello", ""},
		{"status passed through", map[string]any{"url": srv.URL + "/missing"}, "404", "404 page not found\n", ""},
		{"undeclared host", map[string]any{"url": "https://localhost:" + u.Port() + "/hello"}, "", "", "localhost:" + u.Port() + " is not in the plugin's spec.network"},
		{"redirect to an undeclared host", map[string]any{"url": srv.URL + "/redirect"}, "", "", "is not in the plugin's spec.network"},
		{"plain http", map[string]any{"url": "http://" + u.Host + "/hello"}, "", "", "is not https"},
		{"another port", map[string]any{"url": "https://" + u.Hostname() + ":1/hello"}, "", "", "is not in the plugin's spec.network"},
		{"body over the limit", map[string]any{"url": srv.URL + "/big"}, "", "", "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, err := probe(t, r, tt.settings)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("list = %v, %v; want an error containing %q", labels, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if labels["status"] != tt.status || labels["body"] != tt.body {
				t.Fatalf("list labels = %v, want status %s and body %q", labels, tt.status, tt.body)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	for _, h := range seen {
		if h.Get("X-Probe") != "1" {
			t.Errorf("the plugin's own header was lost: %v", h)
		}
		for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if v := h.Get(k); v != "" {
				t.Errorf("tug added %s: %q", k, v)
			}
		}
	}
}

// TestWASMTimeoutStopsBusyModule checks that a module which never yields,
// or sleeps, is stopped when its operation's time is up, in one-shot and
// serve mode, and that serve mode then starts a fresh instance.
func TestWASMTimeoutStopsBusyModule(t *testing.T) {
	for _, serve := range []bool{false, true} {
		for _, settings := range []map[string]any{{"spin": "yes"}, {"sleep": "1h"}} {
			t.Run(fmt.Sprintf("serve=%v/%v", serve, settings), func(t *testing.T) {
				r := probeRunner(t, WASMOptions{})
				r.Serve = serve
				r.Timeouts = map[string]time.Duration{provider.OpList: 500 * time.Millisecond}
				if _, err := r.wasmHost(); err != nil { // compile before timing
					t.Fatal(err)
				}
				start := time.Now()
				_, err := probe(t, r, settings)
				if provider.CodeOf(err) != provider.Unavailable || !strings.Contains(err.Error(), "timed out") {
					t.Fatalf("list = %v, want Unavailable: timed out", err)
				}
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Fatalf("the module ran for %s after a 500ms timeout", elapsed)
				}
				// The next call only shows that the runner still works, so
				// it gets the default time: starting a module can take
				// longer than 500ms on a slow machine running -race.
				r.Timeouts = nil
				if labels, err := probe(t, r, map[string]any{"env": "yes"}); err != nil || labels["env"] == "" {
					t.Fatalf("a call after the timeout = %v, %v", labels, err)
				}
			})
		}
	}
}

// TestWASMMemoryLimit checks that a module that needs more memory than its
// limit fails with an error that says so, and that the runner still works.
func TestWASMMemoryLimit(t *testing.T) {
	r := probeRunner(t, WASMOptions{MemoryLimit: 64 << 20})
	_, err := probe(t, r, map[string]any{"allocMiB": "128"})
	pe := provider.AsError(err)
	if pe.Code != provider.Internal || !strings.Contains(pe.Message, "ran out of memory") || !strings.Contains(pe.Message, "64 MiB") {
		t.Fatalf("list allocating 128 MiB under a 64 MiB limit = %v, want Internal: ran out of memory", err)
	}
	if !strings.Contains(pe.Hint, "memoryLimitMiB") {
		t.Fatalf("hint = %q, want one naming the setting", pe.Hint)
	}
	if labels, err := probe(t, r, map[string]any{"allocMiB": "8"}); err != nil || labels["allocated"] != "8" {
		t.Fatalf("list allocating 8 MiB after that = %v, %v", labels, err)
	}

	small := probeRunner(t, WASMOptions{MemoryLimit: 1 << 20})
	if _, err := probe(t, small, map[string]any{}); err == nil || !strings.Contains(err.Error(), "more than its 1 MiB limit") {
		t.Fatalf("a module that starts with more than its limit = %v", err)
	}
}

// TestWASMSandbox checks what a module sees: a private temporary directory
// and no other files, the operation as its argument, and only the
// environment its manifest declares.
func TestWASMSandbox(t *testing.T) {
	t.Setenv("PROBE_DECLARED", "yes")
	t.Setenv("PROBE_UNDECLARED", "leak")
	temp := t.TempDir()
	r := probeRunner(t, WASMOptions{TempDir: temp})
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}

	labels, err := probe(t, r, map[string]any{"files": secret})
	if err != nil {
		t.Fatal(err)
	}
	if labels["tmp"] != "/tmp/probe.txt" || labels["listTmp"] != "probe.txt" {
		t.Errorf("files = %v, want a writable /tmp", labels)
	}
	if !strings.HasPrefix(labels["read"], "error:") {
		t.Errorf("the module read %s from the host: %q", secret, labels["read"])
	}
	// WASI has no directory above the mount: listing it fails, or shows
	// only the mount.
	for _, k := range []string{"root", "parent"} {
		if v := labels[k]; v != "tmp" && !strings.HasPrefix(v, "error:") {
			t.Errorf("%s = %q, want nothing but tmp", k, v)
		}
	}
	if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
		t.Fatalf("the module's temporary directory was left behind: %v, %v", entries, err)
	}

	labels, err = probe(t, r, map[string]any{"env": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Fields(labels["env"])
	for _, want := range []string{"PROBE_DECLARED=yes", "HOME=/tmp", "TMPDIR=/tmp", "TUG_PLUGIN=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("environment %v lacks %s", env, want)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PROBE_UNDECLARED=") || strings.HasPrefix(kv, "PATH=") {
			t.Errorf("environment has %s, which the manifest does not declare", kv)
		}
	}
	if labels["args"] != "tug-provider-probe list" {
		t.Errorf("args = %q, want the program name and the operation", labels["args"])
	}
}

// TestWASMPackageAndInstall packages a plugin whose only build is
// WebAssembly, installs it, and checks its checksum is held to.
func TestWASMPackageAndInstall(t *testing.T) {
	dir := exampleWASMDir(t)
	res, err := Package(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifest.Spec.Platforms) != 1 || !res.Manifest.Spec.Platforms[0].IsWASM() {
		t.Fatalf("packaged %+v, want the WebAssembly build alone", res.Manifest.Spec.Platforms)
	}
	plat := res.Manifest.Spec.Platforms[0]
	want, err := fileSHA256(filepath.Join(dir, filepath.FromSlash(plat.Bin)))
	if err != nil || plat.SHA256 != want {
		t.Fatalf("packaged sha256 %s, want the module's %s (%v)", plat.SHA256, want, err)
	}
	if len(res.Mismatched) != 0 {
		t.Fatalf("mismatched %+v", res.Mismatched)
	}

	d := NewDirs(t.TempDir())
	staged, err := d.Stage(res.Path, res.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Commit(staged, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.WASM || p.Dev {
		t.Fatalf("installed %+v, want a checked WebAssembly plugin", p)
	}
	if err := os.WriteFile(p.Bin, append(wasmHeader, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p.Dir, false); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a changed module loaded: %v", err)
	}
}

// TestPackageRefusesAModuleAsANativeBuild checks that a module listed as a
// native build is left out, as a binary for another platform is.
func TestPackageRefusesAModuleAsANativeBuild(t *testing.T) {
	module := buildWASM(t, "../../examples/tug-provider-example")
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: mixed, version: 1.0.0}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  platforms:\n" +
		"  - {os: linux, arch: amd64, bin: bin/plugin.wasm}\n  - {os: wasip1, arch: wasm, bin: bin/plugin.wasm}\n"
	res, err := Package(wasmPluginDir(t, module, manifest), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Mismatched) != 1 || res.Mismatched[0].OS != "linux" || len(res.Manifest.Spec.Platforms) != 1 {
		t.Fatalf("packaged %+v, mismatched %+v; want only the WebAssembly entry", res.Manifest.Spec.Platforms, res.Mismatched)
	}
}

// tinyModule encodes a WebAssembly module that imports the given functions,
// each () -> (), and defines a memory of minPages pages. With start, it
// exports a _start function that returns at once; it exports its memory as
// "memory".
func tinyModule(minPages uint32, start bool, imports ...[2]string) []byte {
	leb := func(v uint32) []byte {
		var out []byte
		for {
			c := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				out = append(out, c|0x80)
				continue
			}
			return append(out, c)
		}
	}
	name := func(s string) []byte { return append(leb(uint32(len(s))), s...) }
	section := func(id byte, body ...[]byte) []byte {
		b := slices.Concat(body...)
		return slices.Concat([]byte{id}, leb(uint32(len(b))), b)
	}
	var imps [][]byte
	for _, imp := range imports {
		imps = append(imps, name(imp[0]), name(imp[1]), []byte{0x00, 0x00})
	}
	exports := [][]byte{leb(1), name("memory"), {0x02, 0x00}}
	if start {
		exports = [][]byte{leb(2), name("memory"), {0x02, 0x00}, name("_start"), {0x00}, leb(uint32(len(imports)))}
	}
	return slices.Concat(
		wasmHeader,
		section(1, []byte{1, 0x60, 0, 0}), // one type: () -> ()
		section(2, append([][]byte{leb(uint32(len(imports)))}, imps...)...),
		section(3, []byte{1, 0}), // one function, of type 0
		section(5, []byte{1, 0}, leb(minPages)),
		section(7, exports...),
		section(10, []byte{1, 2, 0, 0x0b}), // its body: no locals, end
	)
}

// TestWASMChecksModules checks that tug refuses, before running it, a module
// it could not run as a plugin.
func TestWASMChecksModules(t *testing.T) {
	tests := []struct {
		name   string
		module []byte
		want   string
	}{
		{"runs", tinyModule(1, true), ""},
		{"browser imports", tinyModule(1, true, [2]string{"gojs", "runtime.wasmExit"}), "imports gojs.runtime.wasmExit, which tug does not provide"},
		{"unknown host function", tinyModule(1, true, [2]string{provider.HostModule, "socket"}), "imports tug_http.socket, which tug does not provide"},
		{"no _start", tinyModule(1, false), "no _start function"},
		{"memory over the limit", tinyModule(64, true), "starts with 4 MiB of memory, more than its 2 MiB limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugin.wasm")
			if err := os.WriteFile(path, tt.module, 0o600); err != nil {
				t.Fatal(err)
			}
			r := &Runner{Bin: path, Name: "tiny", WASM: &WASMOptions{MemoryLimit: 2 << 20}}
			defer r.Close()
			err := r.Prepare()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Prepare() = %v, want nil", err)
				}
				// It runs, and exits 0 without writing a response.
				if _, err := r.Call(context.Background(), provider.OpDescribe, nil); err != nil {
					t.Fatalf("describe = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prepare() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}
