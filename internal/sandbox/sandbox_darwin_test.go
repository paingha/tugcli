//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeFramework lays out a framework whose program, in bin, runs again as
// the app in its Resources, as Homebrew's and python.org's Python do. It
// returns the program and the launcher's directory.
func fakeFramework(t *testing.T) (program, launchers string) {
	t.Helper()
	version := filepath.Join(realPath(t.TempDir()), "Fake.framework", "Versions", "3.15")
	launchers = filepath.Join(version, "Resources", "Fake.app", "Contents", "MacOS")
	program = filepath.Join(version, "bin", "fake3.15")
	for path, script := range map[string]string{
		program:                          "#!/bin/sh\nexec " + filepath.Join(launchers, "Fake") + " \"$@\"\n",
		filepath.Join(launchers, "Fake"): "#!/bin/sh\necho launched \"$@\"\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return program, launchers
}

func TestFrameworkOf(t *testing.T) {
	program, launchers := fakeFramework(t)
	version, got := frameworkOf(program)
	if want := filepath.Dir(filepath.Dir(program)); version != want || !slices.Equal(got, []string{launchers}) {
		t.Errorf("frameworkOf(%s) = %s, %v, want %s, [%s]", program, version, got, want, launchers)
	}
	for _, p := range []string{"/opt/homebrew/bin/tsh", "/usr/bin/python3", "/x/Bad.framework/Versions/"} {
		if version, got := frameworkOf(p); version != "" || got != nil {
			t.Errorf("frameworkOf(%s) = %q, %v, want nothing", p, version, got)
		}
	}
}

// TestSandboxRunsAFrameworkLauncher runs the fake framework's program in the
// sandbox, which must let it run again as its app, or Homebrew's python3
// fails to start with "posix_spawn: ... Python.app/Contents/MacOS/Python".
func TestSandboxRunsAFrameworkLauncher(t *testing.T) {
	if !Check().Any() {
		t.Skipf("no sandbox here: %s", Check().Missing)
	}
	program, _ := fakeFramework(t)
	s := &Sandbox{Policy: Policy{Name: "fake", Dir: filepath.Dir(program)}, TempRoot: t.TempDir()}
	c, err := s.Command(program, []string{"list"}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Cleanup()
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = s.Policy.Dir // the sandbox may not let it read the test's directory
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "launched list" {
		t.Fatalf("running %s in the sandbox = %v\nstdout: %s\nstderr: %s", program, err, out, stderr.String())
	}
}
