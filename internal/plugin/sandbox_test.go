package plugin

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

// sandboxFor returns a sandbox for p, with this test binary as tug.
func sandboxFor(t *testing.T, p *Plugin) *sandbox.Sandbox {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &sandbox.Sandbox{Policy: p.SandboxPolicy(), Self: self, TempRoot: t.TempDir()}
}

// eachSandbox runs f for p without a sandbox and, where the platform has
// one, with it, so that tests show plugins work the same way in both.
func eachSandbox(t *testing.T, p *Plugin, f func(t *testing.T, sb *sandbox.Sandbox)) {
	t.Helper()
	t.Run("unsandboxed", func(t *testing.T) { f(t, nil) })
	t.Run("sandboxed", func(t *testing.T) {
		needSandbox(t)
		f(t, sandboxFor(t, p))
	})
}

// needSandbox skips a test where the platform cannot confine plugins.
func needSandbox(t *testing.T) sandbox.Status {
	t.Helper()
	st := sandbox.Check()
	if !st.Files {
		t.Skipf("the sandbox cannot confine plugins here: %s", st.Missing)
	}
	return st
}

const sandboxProbeManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: probe, version: 0.1.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential, serve]
  platforms:
  - {os: GOOS, arch: GOARCH, bin: bin/tug-provider-probe}
  sandbox:
    read: ["~/declared"]
  network: ["127.0.0.1:PORT"]
`

// buildSandboxProbe compiles testdata/sandbox-probe into a plugin directory
// whose manifest lets it read ~/declared and connect to port.
func buildSandboxProbe(t *testing.T, port int) *Plugin {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "tug-provider-probe")
	if out, err := exec.Command("go", "build", "-o", bin, "./testdata/sandbox-probe").CombinedOutput(); err != nil {
		t.Fatalf("building the probe plugin: %v\n%s", err, out)
	}
	m := strings.NewReplacer("GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH, "PORT", strconv.Itoa(port)).Replace(sandboxProbeManifest)
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// listen accepts and closes TCP connections on a loopback port until the
// test ends.
func listen(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), ln.Addr().(*net.TCPAddr).Port
}

// TestSandboxConfinesPlugins runs a plugin that tries to read a secret in
// the home directory, write outside its temporary directory, connect to a
// port it may not and run a program it did not declare, in one-shot and
// serve mode. The sandbox refuses those and allows the rest.
func TestSandboxConfinesPlugins(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	st := needSandbox(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"secret", "declared"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	allowed, port := listen(t)
	denied, _ := listen(t)
	p := buildSandboxProbe(t, port)
	acct := provider.Account{ID: "probe", Config: map[string]any{
		"secret":   filepath.Join(home, "secret"),
		"declared": filepath.Join(home, "declared"),
		"outside":  outside,
		"allowed":  allowed,
		"denied":   denied,
	}}
	want := map[string]string{
		"readSecret":     "denied",
		"readDeclared":   "ok",
		"readOwn":        "ok",
		"writeTemp":      "ok",
		"writeOutside":   "denied",
		"connectAllowed": "ok",
		"connectDenied":  "denied",
		"exec":           "denied",
	}
	if !st.Network {
		t.Logf("the sandbox does not limit TCP ports here: %s", st.Missing)
		delete(want, "connectDenied")
	}

	for _, serve := range []bool{false, true} {
		t.Run(fmt.Sprintf("serve=%v", serve), func(t *testing.T) {
			r := &Runner{Bin: p.Bin, Name: p.Name(), Serve: serve, Sandbox: sandboxFor(t, p)}
			prov := NewProvider(p, r)
			t.Cleanup(func() { _ = prov.Close() })
			clusters, err := prov.List(context.Background(), provider.ListRequest{Account: acct})
			if err != nil {
				t.Fatal(err)
			}
			got := maps.Clone(clusters[0].Labels)
			if !st.Network {
				delete(got, "connectDenied")
			}
			if !maps.Equal(got, want) {
				t.Errorf("in the sandbox, the probe found %v, want %v", got, want)
			}
			if _, err := os.Stat(filepath.Join(outside, "probe")); !os.IsNotExist(err) {
				t.Errorf("the probe wrote outside its temporary directory: %v", err)
			}
		})
	}

	// Without the sandbox, the same plugin reaches everything.
	r := &Runner{Bin: p.Bin, Name: p.Name()}
	clusters, err := NewProvider(p, r).List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"readSecret", "writeOutside", "connectDenied", "exec"} {
		if got := clusters[0].Labels[label]; got != "ok" {
			t.Errorf("without the sandbox, %s = %q, want ok", label, got)
		}
	}
}

// TestSandboxRefusalHints checks that an error caused by the sandbox tells
// the user how to run the plugin without it.
func TestSandboxRefusalHints(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	needSandbox(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := filepath.Join(home, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := buildSandboxProbe(t, 1)
	r := &Runner{Bin: p.Bin, Name: p.Name(), Sandbox: sandboxFor(t, p)}
	_, err := NewProvider(p, r).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "probe", Config: map[string]any{"mustRead": secret}}})
	pe := provider.AsError(err)
	if pe.Code != provider.Internal || !strings.Contains(pe.Hint, "plugins.sandbox.exclude") || !strings.Contains(pe.Hint, "probe") {
		t.Fatalf("List = %+v, want an Internal error whose hint names plugins.sandbox.exclude", pe)
	}

	// A plugin that dies without answering gets the hint too.
	r.Bin = filepath.Join(filepath.Dir(p.Bin), "missing")
	if _, err := r.Call(context.Background(), provider.OpDescribe, nil); !strings.Contains(provider.AsError(err).Hint, "plugins.sandbox.exclude") {
		t.Fatalf("Call of a plugin that cannot start = %+v, want the sandbox hint", provider.AsError(err))
	}

	// Without the sandbox, the same failure has no such hint.
	r = &Runner{Bin: p.Bin, Name: p.Name()}
	_, err = NewProvider(p, r).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "probe", Config: map[string]any{"mustRead": filepath.Join(home, "missing")}}})
	if pe := provider.AsError(err); pe.Hint != "" {
		t.Fatalf("List without the sandbox = %+v, want no hint", pe)
	}
}
