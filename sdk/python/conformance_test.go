// Package python holds the Go tests of the Python provider SDK in this
// directory. They run its scripts with python3, and skip themselves without
// it, under -short, and on Windows.
package python

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

// needPython skips the test unless python3, at the oldest version the SDK
// supports, can run the SDK's scripts, and returns its path.
func needPython(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs python3")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the plugins are scripts, which Windows cannot run directly")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if err := exec.Command(python, "-c", "import sys; sys.exit(sys.version_info < (3, 9))").Run(); err != nil {
		t.Skipf("python3 is older than 3.9, which the SDK needs: %v", err)
	}
	return python
}

// pythonEnv is the test's environment with the SDK in this checkout on the
// import path, so that an installed copy cannot stand in for it.
func pythonEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	sdk, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PYTHONPATH="+sdk, "PYTHONDONTWRITEBYTECODE=1")
	return append(env, extra...)
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestExampleConforms(t *testing.T) {
	needPython(t)
	p, err := plugin.Load(filepath.Join("examples", "tug-provider-pyexample"), true)
	if err != nil {
		t.Fatalf("the example's manifest does not load: %v", err)
	}
	clusters := "dev=https://127.0.0.1:6443"
	r := conformance.Run(context.Background(), conformance.Target{
		Command: p.Bin,
		Env:     pythonEnv(t, "PYEXAMPLE_CLUSTERS="+clusters),
	}, conformance.Options{
		Name:            p.Manifest.Metadata.Name,
		Account:         provider.Account{ID: "demo", Config: map[string]any{"clusters": clusters}},
		Secrets:         map[string]string{"api_token": "ex_test"},
		DeclaredSecrets: p.Manifest.Spec.Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the Python example plugin does not conform")
	}
	// With an account, secrets and the accounts operation, every check but
	// serve has something to test, so a skip means a check did not run.
	for _, x := range r.Results {
		if x.Status == conformance.Skip && x.Name != "serve answers like one-shot mode" {
			t.Errorf("%s was skipped: %s", x.Name, x.Detail)
		}
	}
}
