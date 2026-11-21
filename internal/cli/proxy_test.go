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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/paingha/tugcli/internal/audit"
)

// TestProxySessionGuardsEveryClient runs the real tug binary: tug connect
// --proxy starts tugd, and an ordinary client-go client using the session
// kubeconfig (as kubectl or helm would) is held to the read-only guard.
func TestProxySessionGuardsEveryClient(t *testing.T) {
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
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tug %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
		}
		return string(out)
	}
	t.Cleanup(func() { _ = exec.Command(bin, "daemon", "stop").Run() })

	srv, writes := fakeAPIServer(t)
	run("import", "kubeconfig", writeKubeconfig(t, srv))
	for _, line := range strings.Split(run("session", "new", "--shell", "bash", "--pid", fmt.Sprint(os.Getpid())), "\n") {
		if v, ok := strings.CutPrefix(line, "export TUG_SESSION="); ok {
			t.Setenv("TUG_SESSION", strings.Trim(v, "'"))
		}
	}
	if out := run("connect", "payments-prod", "--proxy"); !strings.Contains(out, "read-only") || !strings.Contains(out, "tugd") {
		t.Fatalf("connect --proxy: %s", out)
	}
	var st statusView
	if err := json.Unmarshal([]byte(run("status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.ProxyURL, "https://127.0.0.1:") {
		t.Fatalf("status: %+v", st)
	}
	kc, err := os.ReadFile(st.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(kc, []byte(secretToken)) || !bytes.Contains(kc, []byte("proxy-token")) {
		t.Fatalf("session kubeconfig:\n%s", kc)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", st.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("listing through the proxy: %v", err)
	}

	err = cs.CoreV1().Namespaces().Delete(ctx, "payments", metav1.DeleteOptions{})
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("delete on a read-only session through the proxy: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("the refused delete reached the cluster")
	}

	run("unlock", "5m")
	err = cs.CoreV1().Namespaces().Delete(ctx, "payments", metav1.DeleteOptions{})
	if apierrors.IsForbidden(err) {
		t.Fatalf("delete after unlock was still refused: %v", err)
	}
	if writes.Load() != 1 {
		t.Fatalf("the unlocked delete should reach the cluster once, got %d", writes.Load())
	}
	// tugd recorded it; the refused one is not there.
	var entries []audit.Entry
	if err := json.Unmarshal([]byte(run("audit", "-o", "json")), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Source != audit.SourceProxy || entries[0].Allowed != audit.AllowedUnlocked ||
		entries[0].Method != "DELETE" || entries[0].Path != "/api/v1/namespaces/payments" || entries[0].Cluster != "payments-prod" {
		t.Fatalf("audit after a delete through tugd: %+v", entries)
	}

	// A client that loaded the kubeconfig before tugd moved is told to run
	// again, not handed a token for an address that no longer answers; the
	// kubeconfig is repointed for the next run.
	stale := bytes.ReplaceAll(kc, []byte(st.ProxyURL), []byte("https://127.0.0.1:1"))
	if err := os.WriteFile(st.Kubeconfig, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := os.Getenv("TUG_SESSION")
	var stderr bytes.Buffer
	moved := exec.Command(bin, "auth", "proxy-token", "--session", sessionID)
	moved.Stderr = &stderr
	if out, err := moved.Output(); err == nil || !strings.Contains(stderr.String(), "run the command again") {
		t.Fatalf("proxy-token after tugd moved: %v\n%s%s", err, out, stderr.String())
	}
	if kc2, _ := os.ReadFile(st.Kubeconfig); !bytes.Contains(kc2, []byte(st.ProxyURL+"/s/"+sessionID)) {
		t.Fatalf("kubeconfig not repointed:\n%s", kc2)
	}
	if out := run("auth", "proxy-token", "--session", sessionID); !strings.Contains(out, "ExecCredential") {
		t.Fatalf("proxy-token once repointed: %s", out)
	}

	if out := run("daemon", "status", "-o", "json"); !strings.Contains(out, `"sessions": 1`) {
		t.Fatalf("daemon status: %s", out)
	}
	if out := run("daemon", "stop"); !strings.Contains(out, "stopped") {
		t.Fatalf("daemon stop: %s", out)
	}
}
