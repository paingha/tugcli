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
