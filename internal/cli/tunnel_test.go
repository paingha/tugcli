package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/paingha/tugcli/internal/tunnel/tunneltest"
)

func TestTunnelCommands(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	kubeconfigPath := writeKubeconfig(t, srv)
	mustTug(t, "import", "kubeconfig", kubeconfigPath)
	spec, jump := tunneltest.SSH(t, true)
	api := strings.TrimPrefix(srv.URL, "https://")

	if r := tug(t, "tunnel", "set", "payments-staging", "http://user:secret@proxy.example.com:3128"); r.code == 0 || !strings.Contains(r.stderr, "leave the password out") {
		t.Fatalf("a tunnel with a password: %+v", r)
	}
	if out := mustTug(t, "tunnel", "set", "payments-staging", spec); !strings.Contains(out, "payments-staging goes through ssh://tester@") {
		t.Fatalf("tunnel set: %s", out)
	}
	// Syncs keep the tunnel.
	mustTug(t, "sync")
	if out := mustTug(t, "tunnel", "ls", "-o", "name"); strings.TrimSpace(out) != "payments-staging" {
		t.Fatalf("tunnel ls after sync: %q", out)
	}

	if out := mustTug(t, "tunnel", "test", "payments-staging"); !strings.Contains(out, "Reached "+api+" through ssh://") {
		t.Fatalf("tunnel test: %s", out)
	}

	// tug's own commands go through the tunnel: doctor reaches the API
	// server by way of the jump host instead of dialling it.
	dialsBefore := len(jump.Dials())
	r := tug(t, "doctor", "payments-staging")
	if !strings.Contains(r.stdout, "reached "+api+" through ssh://") || !strings.Contains(r.stdout, "API server") {
		t.Fatalf("doctor through the tunnel: %+v", r)
	}
	if dials := jump.Dials(); len(dials) <= dialsBefore+1 {
		t.Fatalf("the jump host opened %v; want the tunnel check and the API checks", dials)
	}

	if out := mustTug(t, "tunnel", "rm", "payments-staging"); !strings.Contains(out, "Removed the tunnel ssh://") {
		t.Fatalf("tunnel rm: %s", out)
	}
	if r := tug(t, "tunnel", "rm", "payments-staging"); r.code == 0 || !strings.Contains(r.stderr, "has no tunnel") {
		t.Fatalf("second tunnel rm: %+v", r)
	}
}

// tunnelPluginScript lists one cluster that its provider reaches through a
// SOCKS proxy.
const tunnelPluginScript = `#!/bin/sh
cat >/dev/null
case "$1" in
  list) printf '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"e1","name":"edge","endpoints":{"public":"https://10.0.0.1:6443"},"tunnel":"socks5://127.0.0.1:1080"}]}\n' ;;
  credential) printf '{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":"token","token":"t"}}\n' ;;
  *) exit 2 ;;
esac
`

func TestProviderTunnels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	isolate(t)
	dir := t.TempDir()
	manifest := strings.NewReplacer("idp", "edge", "GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH).Replace(oidcPluginManifest)
	for name, body := range map[string]string{"plugin.yaml": manifest, "tug-provider-edge": tunnelPluginScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustTug(t, "plugin", "link", dir)
	mustTug(t, "import", "edge", "--account", "a")

	type row struct{ Cluster, Tunnel, Source string }
	ls := func() []row {
		t.Helper()
		var rows []row
		if err := json.Unmarshal([]byte(mustTug(t, "tunnel", "ls", "-o", "json")), &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := ls(); len(rows) != 1 || rows[0] != (row{"edge", "socks5://127.0.0.1:1080", "provider"}) {
		t.Fatalf("tunnel ls after import = %+v, want the provider's tunnel", rows)
	}
	if r := tug(t, "tunnel", "rm", "edge"); r.code == 0 || !strings.Contains(r.stderr, "which comes from edge; tug tunnel set replaces it with yours") {
		t.Fatalf("tunnel rm of the provider's tunnel: %+v", r)
	}

	// The user's tunnel wins, and syncs keep it.
	mustTug(t, "tunnel", "set", "edge", "http://proxy.corp:3128")
	mustTug(t, "sync")
	if rows := ls(); len(rows) != 1 || rows[0] != (row{"edge", "http://proxy.corp:3128", "user"}) {
		t.Fatalf("tunnel ls after tunnel set and sync = %+v, want the user's tunnel", rows)
	}
	r := tug(t, "tunnel", "rm", "edge")
	if r.code != 0 || !strings.Contains(r.stderr, "edge goes through socks5://127.0.0.1:1080 again, from edge") {
		t.Fatalf("tunnel rm of the user's tunnel: %+v", r)
	}
	if rows := ls(); len(rows) != 1 || rows[0].Source != "provider" {
		t.Fatalf("tunnel ls after tunnel rm = %+v, want the provider's tunnel back", rows)
	}
}

// TestTunnelThroughTugd runs the real tug binary: connecting to a
// tunnelled cluster routes the session through tugd, and an ordinary
// client-go client using the session kubeconfig reaches the API server
// through tugd and the jump host.
func TestTunnelThroughTugd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds tug and starts tugd")
	}
	isolate(t)
	bin := filepath.Join(t.TempDir(), "tug")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/tug").CombinedOutput(); err != nil {
		t.Fatalf("building tug: %v\n%s", err, out)
	}
	run := func(args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tug %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
		}
		return string(out), stderr.String()
	}
	t.Cleanup(func() { _ = exec.Command(bin, "daemon", "stop").Run() })

	srv, _ := fakeAPIServer(t)
	run("import", "kubeconfig", writeKubeconfig(t, srv))
	spec, jump := tunneltest.SSH(t, true)
	run("tunnel", "set", "payments-staging", spec)
	out, _ := run("session", "new", "--shell", "bash", "--pid", fmt.Sprint(os.Getpid()))
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "export TUG_SESSION="); ok {
			t.Setenv("TUG_SESSION", strings.Trim(v, "'"))
		}
	}
	if _, stderr := run("connect", "payments-staging"); !strings.Contains(stderr, "goes through tugd") {
		t.Fatalf("connect to a tunnelled cluster: %s", stderr)
	}
	var st statusView
	status, _ := run("status", "-o", "json")
	if err := json.Unmarshal([]byte(status), &st); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", st.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Fatalf("listing through tugd and the tunnel: %v", err)
	}
	api := strings.TrimPrefix(srv.URL, "https://")
	if dials := jump.Dials(); len(dials) == 0 || dials[len(dials)-1] != api {
		t.Fatalf("the jump host opened %v; want the API server %s", dials, api)
	}
}

func TestPfLsAndStopWithoutTugd(t *testing.T) {
	isolate(t)
	if out := mustTug(t, "pf", "ls", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("pf ls without tugd: %q", out)
	}
	if r := tug(t, "pf", "stop"); r.code == 0 || !strings.Contains(r.stderr, "--all") {
		t.Fatalf("pf stop with nothing named: %+v", r)
	}
	mustTug(t, "pf", "stop", "--all") // nothing to stop is not an error
}
