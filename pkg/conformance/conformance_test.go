package conformance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

func buildExample(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-example")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../examples/tug-provider-example").CombinedOutput(); err != nil {
		t.Fatalf("building example: %v\n%s", err, out)
	}
	return bin
}

func results(r Report) map[string]Result {
	m := map[string]Result{}
	for _, x := range r.Results {
		m[x.Name] = x
	}
	return m
}

func TestExamplePluginConforms(t *testing.T) {
	bin := buildExample(t)
	r := Run(context.Background(), Target{Command: bin}, Options{
		Name:            "example",
		Account:         provider.Account{ID: "demo", Config: map[string]any{"clusters": "dev=https://127.0.0.1:6443,ci=https://ci.example:6443"}},
		Secrets:         map[string]string{"api_token": "ex_conformance"},
		DeclaredSecrets: []provider.SecretSpec{{Name: "api_token"}},
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("example plugin should conform")
	}
	got := results(r)
	for _, name := range []string{"describe is valid", "list returns valid clusters", "credential is valid and unexpired", "invalid secrets give Unauthenticated", "stdout carries only one JSON object"} {
		if got[name].Status != Pass {
			t.Errorf("%s: %s", name, got[name].Status)
		}
	}
}

func TestWithoutAccountListIsSkipped(t *testing.T) {
	bin := buildExample(t)
	r := Run(context.Background(), Target{Command: bin}, Options{Name: "example"})
	if r.Failed() {
		for _, x := range r.Results {
			t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
		}
		t.Fatal("missing account should skip, not fail")
	}
	if got := results(r)["list returns valid clusters"]; got.Status != Skip {
		t.Fatalf("list: %+v", got)
	}
}

// shellPlugin writes a script plugin for checks the SDK makes hard to break.
func shellPlugin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	path := filepath.Join(t.TempDir(), "tug-provider-bad")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodDescribe = `describe) echo '{"apiVersion":"provider/v1alpha1","kind":"DescribeResponse","name":"bad","protocols":["provider/v1alpha1"],"capabilities":["list","credential"]}' ;;`
const unknown = `*) echo '{"apiVersion":"provider/v1alpha1","kind":"Error","code":"InvalidConfig","message":"nope"}'; exit 2 ;;`

func TestDetectsNonConformingPlugins(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		opts     Options
		failing  string
		contains string
	}{
		{
			name: "log line on stdout",
			script: `case "$1" in
  ` + goodDescribe + `
  list) read -r _; echo "connecting to api..."; echo '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[]}' ;;
  ` + unknown + `
esac`,
			opts:     Options{Name: "bad", Account: provider.Account{ID: "a"}},
			failing:  "list returns valid clusters",
			contains: "not JSON",
		},
		{
			name: "accepts any secret",
			script: `case "$1" in
  ` + goodDescribe + `
  list) cat >/dev/null; echo '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[]}' ;;
  ` + unknown + `
esac`,
			opts:     Options{Name: "bad", DeclaredSecrets: []provider.SecretSpec{{Name: "token"}}},
			failing:  "invalid secrets give Unauthenticated",
			contains: "exited 0",
		},
		{
			name: "ignores SIGTERM",
			script: `trap '' TERM
case "$1" in
  ` + goodDescribe + `
  list) cat >/dev/null; sleep 3 ;;
  ` + unknown + `
esac`,
			opts:     Options{Name: "bad"},
			failing:  "SIGTERM stops the plugin promptly",
			contains: "still running",
		},
		{
			name: "describe names another provider",
			script: `case "$1" in
  ` + goodDescribe + `
  ` + unknown + `
esac`,
			opts:     Options{Name: "other"},
			failing:  "describe is valid",
			contains: "manifest names",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := shellPlugin(t, tt.script)
			r := Run(context.Background(), Target{Command: bin}, tt.opts)
			got := results(r)[tt.failing]
			if got.Status != Fail || !strings.Contains(got.Detail, tt.contains) {
				for _, x := range r.Results {
					t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
				}
				t.Fatalf("%q = %+v, want fail mentioning %q", tt.failing, got, tt.contains)
			}
		})
	}
}

func TestSingleJSONObject(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{`{"a":1}`, true},
		{"{\"a\":1}\n", true},
		{`{"a":1}{"b":2}`, false},
		{`{"a":1} trailing`, false},
		{`[1,2]`, false},
		{``, false},
		{`log line`, false},
	}
	for _, tt := range tests {
		if err := singleJSONObject([]byte(tt.in)); (err == nil) != tt.ok {
			t.Errorf("singleJSONObject(%q) = %v", tt.in, err)
		}
	}
}
