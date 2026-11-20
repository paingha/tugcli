package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

var demoAccount = provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443"}}

func conform(t *testing.T, target conformance.Target, name string) {
	t.Helper()
	r := conformance.Run(context.Background(), target, conformance.Options{
		Name:            name,
		Account:         demoAccount,
		Secrets:         map[string]string{"api_token": "demo_test"},
		DeclaredSecrets: []provider.SecretSpec{{Name: "api_token"}},
	})
	if r.Failed() {
		for _, x := range r.Results {
			t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
		}
		t.Fatal("scaffolded plugin does not conform")
	}
}

func TestScaffoldGoBuildsTestsAndConforms(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "tug-provider-my-cloud")
	files, err := Scaffold(ScaffoldOptions{Name: "my-cloud", Lang: LangGo, Dir: dir, SDKReplace: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("wrote %v", files)
	}
	release, _ := os.ReadFile(filepath.Join(dir, ".github", "workflows", "release.yml"))
	if !strings.Contains(string(release), "${{ github.token }}") {
		t.Fatal("GitHub expressions were mangled by templating")
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("vet", "./...")
	run("test", "./...")
	bin := filepath.Join(dir, "bin", runtime.GOOS+"_"+runtime.GOARCH, "tug-provider-my-cloud")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	run("build", "-o", bin, ".")

	p, err := Load(dir, true)
	if err != nil {
		t.Fatalf("scaffolded manifest does not load: %v", err)
	}
	if p.Bin != bin {
		t.Fatalf("manifest points at %s, built %s", p.Bin, bin)
	}
	conform(t, conformance.Target{Command: bin}, "my-cloud")

	// The scaffold packages and installs.
	res, err := Package(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifest.Spec.Platforms) != 1 || len(res.Missing) != 4 {
		t.Fatalf("packaged %d platforms, %d missing", len(res.Manifest.Spec.Platforms), len(res.Missing))
	}
	staged, err := NewDirs(t.TempDir()).Stage(res.Path, res.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	staged.Discard()
}

func TestScaffoldPythonConforms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script plugin")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	dir := filepath.Join(t.TempDir(), "tug-provider-pycloud")
	if _, err := Scaffold(ScaffoldOptions{Name: "pycloud", Lang: LangPython, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	conform(t, conformance.Target{Command: p.Bin}, "pycloud")

	cmd := exec.Command("python3", "-m", "unittest", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated python tests: %v\n%s", err, out)
	}
}

func TestScaffoldRefusesNonEmptyDirAndBadNames(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600)
	if _, err := Scaffold(ScaffoldOptions{Name: "ok", Dir: dir}); err == nil {
		t.Fatal("scaffolded into a non-empty directory")
	}
	if _, err := Scaffold(ScaffoldOptions{Name: "Bad Name", Dir: t.TempDir()}); err == nil {
		t.Fatal("accepted an invalid name")
	}
}

func TestGoIdent(t *testing.T) {
	for in, want := range map[string]string{"coreweave": "coreweave", "my-cloud": "myCloud", "9cloud": "p9cloud", "a-b-c": "aBC"} {
		if got := goIdent(in); got != want {
			t.Errorf("goIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScaffoldGoFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tug-provider-acme")
	if _, err := Scaffold(ScaffoldOptions{Name: "acme", Dir: dir, SDKVersion: "v0.4.0"}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tests := []struct {
		file, want, why string
	}{
		{"go.mod", "require github.com/paingha/tugcli v0.4.0", "a release of tug pins its SDK"},
		{"Makefile", "bin/$(HOST)/$(BIN)$(EXT)", "Windows builds need .exe to match plugin.yaml"},
		{filepath.Join(".github", "workflows", "release.yml"), "does not match plugin.yaml", "the tag must match the manifest version"},
		{filepath.Join(".github", "workflows", "release.yml"), "${{ secrets.TUGCLI_TOKEN }}", "the SDK can be fetched while its repository is private"},
	}
	for _, tt := range tests {
		if !strings.Contains(read(tt.file), tt.want) {
			t.Errorf("%s lacks %q (%s)", tt.file, tt.want, tt.why)
		}
	}

	bare := filepath.Join(t.TempDir(), "tug-provider-bare")
	if _, err := Scaffold(ScaffoldOptions{Name: "bare", Dir: bare}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(bare, "go.mod")); strings.Contains(string(b), "require") {
		t.Errorf("a development build should leave the SDK version to go mod tidy:\n%s", b)
	}
}
