package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAliasCommands(t *testing.T) {
	home := isolate(t)

	if out := mustTug(t, "alias", "set", "wide", "get -o wide"); out != "Alias wide runs tug get -o wide\n" {
		t.Errorf("alias set wide: %q", out)
	}
	mustTug(t, "alias", "set", "errs", "--", "logs", "--since", "10m", "--json")
	r := tug(t, "alias", "set", "errs2", "errs -f")
	if r.code != 0 || !strings.Contains(r.stderr, "not expanded again") {
		t.Errorf("alias set to an alias should warn it is not expanded again: %+v", r)
	}

	for _, name := range []string{"connect", "c", "status", "alias", "help", "completion"} {
		r := tug(t, "alias", "set", name, "get pods")
		if r.code == 0 || !strings.Contains(r.stderr, "one of tug's own commands") {
			t.Errorf("alias set %s: %+v, want a refusal to hide tug's command", name, r)
		}
	}
	if r := tug(t, "alias", "set", "bad", "get 'pods"); r.code == 0 || !strings.Contains(r.stderr, "unterminated") {
		t.Errorf("alias set with an open quote: %+v", r)
	}

	var views []aliasView
	if err := json.Unmarshal([]byte(mustTug(t, "alias", "ls", "-o", "json")), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 || views[0].Name != "errs" || strings.Join(views[0].Expansion, " ") != "logs --since 10m --json" {
		t.Errorf("alias ls -o json: %+v", views)
	}
	if out := mustTug(t, "alias", "ls"); !strings.Contains(out, "wide    tug get -o wide") {
		t.Errorf("alias ls:\n%s", out)
	}

	if out := mustTug(t, "alias", "rm", "errs2", "wide"); out != "Removed alias errs2\nRemoved alias wide\n" {
		t.Errorf("alias rm: %q", out)
	}
	if r := tug(t, "alias", "rm", "wide"); r.code == 0 || !strings.Contains(r.stderr, "no such alias") {
		t.Errorf("alias rm of a removed alias: %+v", r)
	}
	if got := strings.TrimSpace(mustTug(t, "alias", "ls", "-o", "name")); got != "errs" {
		t.Errorf("alias ls -o name after rm = %q", got)
	}
	assertMode600(t, filepath.Join(home, "config", "aliases.yaml"))
}

// An alias runs the command it stands for through the same guard, and one
// that may write says what it runs; one that only reads stays quiet.
func TestAliasExpansionKeepsTheGuard(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod") // env=prod: read-only

	mustTug(t, "alias", "set", "lg", "logs --json")
	mustTug(t, "alias", "set", "rs", "restart")
	mustTug(t, "alias", "set", "nuke", "delete pod api-0")

	r := tug(t, "lg", "api")
	if r.code != 0 || !strings.Contains(r.stdout, `"msg": "listening"`) {
		t.Fatalf("tug lg api: %+v", r)
	}
	if strings.Contains(r.stderr, "alias") {
		t.Errorf("an alias that only reads should not announce itself: %q", r.stderr)
	}

	r = tug(t, "rs", "api")
	if r.code == 0 || !strings.Contains(r.stderr, "read-only") {
		t.Fatalf("tug rs api on a read-only session: %+v", r)
	}
	if !strings.Contains(r.stderr, "alias rs: tug restart api\n") {
		t.Errorf("an alias that writes should say what it runs:\n%s", r.stderr)
	}
	r = tug(t, "nuke")
	if r.code == 0 || !strings.Contains(r.stderr, "read-only") || !strings.Contains(r.stderr, "alias nuke: tug delete pod api-0\n") {
		t.Errorf("tug nuke (kubectl delete) on a read-only session: %+v", r)
	}
	// A run across clusters refuses the write before anything runs.
	r = tug(t, "rs", "api", "--cluster-selector", "env=staging")
	if r.code == 0 || !strings.Contains(r.stderr, "does not run across clusters") {
		t.Errorf("tug rs api --cluster-selector: %+v", r)
	}
	if writes.Load() != 0 {
		t.Fatalf("%d writes reached the API server through aliases", writes.Load())
	}

	mustTug(t, "unlock", "5m")
	r = tug(t, "rs", "api")
	if r.code != 0 || !strings.Contains(r.stdout, "Restarted deploy/api-server") || !strings.Contains(r.stderr, "alias rs:") {
		t.Fatalf("tug rs api after unlock: %+v", r)
	}
	if writes.Load() != 1 {
		t.Fatalf("want 1 write after unlock, got %d", writes.Load())
	}
}

// aliases.yaml written by hand cannot hide tug's commands either, and a
// file that cannot be read costs only the aliases.
func TestAliasNeverShadowsTugCommands(t *testing.T) {
	home := isolate(t)
	path := filepath.Join(home, "config", "aliases.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("aliases:\n  version: [get, pods]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustTug(t, "version"); strings.Contains(out, "pods") || !strings.Contains(out, "tug") {
		t.Errorf("tug version with a version alias: %q", out)
	}
	if out := mustTug(t, "alias", "ls"); !strings.Contains(out, "hidden by tug's own version command") {
		t.Errorf("alias ls should mark the hidden alias:\n%s", out)
	}

	if err := os.WriteFile(path, []byte("not: [valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustTug(t, "version"); !strings.Contains(out, "tug") {
		t.Errorf("tug version with a broken aliases.yaml: %q", out)
	}
	r := tug(t, "wide")
	if !strings.Contains(r.stderr, "ignoring aliases") {
		t.Errorf("a broken aliases.yaml should be reported when an alias may be meant: %+v", r)
	}
}

func assertMode600(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("%s has mode %v, want 0600", path, fi.Mode().Perm())
	}
}
