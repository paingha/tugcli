package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const secretToken = "s3cr3t-token-from-kubeconfig"

const apiServerDeployment = `{"kind":"Deployment","apiVersion":"apps/v1",
	"metadata":{"name":"api-server"},
	"spec":{"selector":{"matchLabels":{"app":"api-server"}},"template":{"spec":{"containers":[{"name":"main"}]}}}}`

// fakeAPIServer serves just enough of the Kubernetes API for tug's commands,
// and only to callers presenting secretToken.
func fakeAPIServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","apiVersion":"v1","items":[
				{"metadata":{"name":"default"}},{"metadata":{"name":"payments"}},{"metadata":{"name":"kube-system"}}]}`)
		case strings.HasSuffix(p, "/deployments") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","items":[`+apiServerDeployment+`]}`)
		case strings.HasSuffix(p, "/deployments/api-server") && r.Method == http.MethodPatch:
			fmt.Fprint(w, apiServerDeployment)
		case strings.HasSuffix(p, "/statefulsets"):
			fmt.Fprint(w, `{"kind":"StatefulSetList","apiVersion":"apps/v1","items":[]}`)
		case strings.HasSuffix(p, "/daemonsets"):
			fmt.Fprint(w, `{"kind":"DaemonSetList","apiVersion":"apps/v1","items":[]}`)
		case strings.HasSuffix(p, "/pods"):
			fmt.Fprint(w, `{"kind":"PodList","apiVersion":"v1","items":[{
				"metadata":{"name":"api-server-7d9f-x2x4z","labels":{"app":"api-server"},
				  "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"api-server-7d9f","uid":"rs","controller":true}]},
				"spec":{"containers":[{"name":"main"}]},
				"status":{"phase":"Running"}}]}`)
		case strings.HasSuffix(p, "/pods/api-server-7d9f-x2x4z/log"):
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "{\"level\":\"info\",\"msg\":\"listening\"}\nready\n")
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func writeKubeconfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: u
  user:
    token: %s
contexts:
- name: payments-prod
  context: {cluster: c, user: u}
- name: payments-staging
  context: {cluster: c, user: u}
current-context: payments-prod
`, srv.URL, b64(ca), secretToken)
	path := filepath.Join(t.TempDir(), "downloaded-kubeconfig.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func b64(b []byte) string {
	out, _ := json.Marshal(b) // encoding/json base64-encodes []byte
	return strings.Trim(string(out), `"`)
}

// isolate points tug at a temporary home with the encrypted file keyring.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TUG_HOME", home)
	t.Setenv("TUG_KEYRING_BACKEND", "file")
	t.Setenv("TUG_KEYRING_PASSPHRASE", "test-passphrase")
	t.Setenv("TUG_SESSION", "")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("NO_COLOR", "1")
	return home
}

type result struct {
	code           int
	stdout, stderr string
}

func tug(t *testing.T, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func mustTug(t *testing.T, args ...string) string {
	t.Helper()
	r := tug(t, args...)
	if r.code != 0 {
		t.Fatalf("tug %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// newSession runs the hook's `session new` and exports the session.
func newSession(t *testing.T) string {
	t.Helper()
	out := mustTug(t, "session", "new", "--shell", "bash", "--pid", fmt.Sprint(os.Getpid()))
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "export TUG_SESSION="); ok {
			id := strings.Trim(v, "'")
			t.Setenv("TUG_SESSION", id)
			return id
		}
	}
	t.Fatalf("no TUG_SESSION in %q", out)
	return ""
}

func TestConnectFlow(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	kubeconfigPath := writeKubeconfig(t, srv)

	out := mustTug(t, "import", "kubeconfig", kubeconfigPath)
	if !strings.Contains(out, "2 clusters") || !strings.Contains(out, "2 credentials moved") {
		t.Fatalf("import output: %s", out)
	}
	assertNoSecretUnder(t, filepath.Join(home, "data", "catalog.yaml"))

	// Without a session, connect explains how to get one.
	if r := tug(t, "connect", "prod"); r.code == 0 || !strings.Contains(r.stderr, "tug init") {
		t.Fatalf("connect without session: %+v", r)
	}

	newSession(t)
	out = mustTug(t, "connect", "prod")
	if !strings.Contains(out, "payments-prod") || !strings.Contains(out, "read-only") {
		t.Fatalf("connect output: %s", out)
	}

	var st statusView
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Cluster != "payments-prod" || !st.ReadOnly || st.Writable || st.Kubeconfig == "" {
		t.Fatalf("status: %+v", st)
	}
	assertNoSecretUnder(t, st.Kubeconfig)

	// kubectl's exec plugin call.
	var ec struct {
		APIVersion string `json:"apiVersion"`
		Status     struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(mustTug(t, "auth", "token", "--cluster", "payments-prod")), &ec); err != nil {
		t.Fatal(err)
	}
	if ec.Status.Token != secretToken || ec.APIVersion != "client.authentication.k8s.io/v1" {
		t.Fatalf("ExecCredential: %+v", ec)
	}

	// The token keeps working after the downloaded file is deleted.
	if err := os.Remove(kubeconfigPath); err != nil {
		t.Fatal(err)
	}

	// Namespaces come from the API server through the in-process client.
	out = mustTug(t, "ns")
	if !strings.Contains(out, "* default") || !strings.Contains(out, "  payments") {
		t.Fatalf("ns list: %s", out)
	}
	mustTug(t, "ns", "pay")
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil || st.Namespace != "payments" {
		t.Fatalf("namespace after switch: %+v %v", st, err)
	}

	// Writes through kubectl are refused on a read-only session.
	r := tug(t, "delete", "pod", "api-0")
	if r.code == 0 || !strings.Contains(r.stderr, "read-only") || !strings.Contains(r.stderr, "tug unlock") {
		t.Fatalf("delete on read-only: %+v", r)
	}

	if runtime.GOOS != "windows" {
		truePath, err := exec.LookPath("true")
		if err == nil {
			t.Setenv("TUG_KUBECTL", truePath)
			mustTug(t, "unlock", "5m")
			mustTug(t, "delete", "pod", "api-0") // reaches "kubectl" (true) now
			mustTug(t, "lock")
			if r := tug(t, "delete", "pod", "api-0"); r.code == 0 {
				t.Fatal("lock did not restore read-only")
			}
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("API server saw %d writes", writes.Load())
	}

	// Labels and listing.
	var clusters []clusterView
	if err := json.Unmarshal([]byte(mustTug(t, "ls", "-o", "json", "-l", "env=staging")), &clusters); err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Name != "payments-staging" {
		t.Fatalf("ls -l env=staging: %+v", clusters)
	}

	// Sync keeps clusters (the file is gone) and reports the problem.
	r = tug(t, "sync")
	if r.code == 0 || !strings.Contains(r.stderr, "no longer exists") {
		t.Fatalf("sync after file removal: %+v", r)
	}
	if out := mustTug(t, "ls", "-o", "name"); !strings.Contains(out, "payments-prod") {
		t.Fatalf("clusters lost after failed sync: %s", out)
	}

	mustTug(t, "disconnect")
	if out := mustTug(t, "status"); !strings.Contains(out, "Not connected") {
		t.Fatalf("status after disconnect: %s", out)
	}

	mustTug(t, "forget", "kubeconfig/downloaded-kubeconfig")
	if out := mustTug(t, "ls", "-o", "name"); strings.TrimSpace(out) != "" {
		t.Fatalf("clusters left after forget: %s", out)
	}
	if r := tug(t, "auth", "token", "--cluster", "payments-prod"); r.code == 0 {
		t.Fatal("credential survived forget")
	}
}

func TestReadWriteFlagAndAmbiguity(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)

	r := tug(t, "connect", "payments")
	if r.code == 0 || !strings.Contains(r.stderr, "2 matches") {
		t.Fatalf("ambiguous target without a terminal should fail: %+v", r)
	}
	if out := mustTug(t, "connect", "payments-prod", "--read-write"); !strings.Contains(out, "read-write") {
		t.Fatalf("--read-write: %s", out)
	}
	if out := mustTug(t, "connect", "env=staging"); !strings.Contains(out, "payments-staging") {
		t.Fatalf("label selection: %s", out)
	}
}

func TestDailyCommands(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod") // env=prod: read-only

	// "api" resolves to deploy/api-server and streams its pod with a prefix.
	out := mustTug(t, "logs", "api", "--json")
	if !strings.Contains(out, "api-server-7d9f-x2x4z/main") || !strings.Contains(out, `"msg": "listening"`) || !strings.Contains(out, "ready") {
		t.Fatalf("logs output:\n%s", out)
	}

	// Writes are refused on a read-only session without a terminal to ask on.
	r := tug(t, "restart", "api")
	if r.code == 0 || !strings.Contains(r.stderr, "read-only") {
		t.Fatalf("restart on read-only: %+v", r)
	}
	if r := tug(t, "scale", "api", "3"); r.code == 0 {
		t.Fatal("scale allowed on a read-only session")
	}
	if writes.Load() != 0 {
		t.Fatalf("%d writes reached the API server", writes.Load())
	}

	mustTug(t, "unlock", "5m")
	if out := mustTug(t, "restart", "api"); !strings.Contains(out, "Restarted deploy/api-server") {
		t.Fatalf("restart: %s", out)
	}
	if writes.Load() != 1 {
		t.Fatalf("want 1 write after unlock, got %d", writes.Load())
	}

	if r := tug(t, "logs", "nothing-like-this"); r.code == 0 || !strings.Contains(r.stderr, "matches") {
		t.Fatalf("unknown target: %+v", r)
	}
	if r := tug(t, "sh", "api", "ls"); r.code == 0 || !strings.Contains(r.stderr, "after --") {
		t.Fatalf("sh without --: %+v", r)
	}
}

func TestInitPrintsHooks(t *testing.T) {
	isolate(t)
	for _, sh := range []string{"zsh", "bash", "fish", "powershell"} {
		if out := mustTug(t, "init", sh); !strings.Contains(out, "session new") {
			t.Errorf("init %s: %s", sh, out)
		}
	}
	if r := tug(t, "init", "tcsh"); r.code == 0 {
		t.Error("unsupported shell accepted")
	}
}

func TestIsPassthrough(t *testing.T) {
	root := newRootCmd(&app{})
	tests := []struct {
		args string
		want bool
	}{
		{"get pods", true},
		{"-n kube-system get pods", true},
		{"connect prod", false},
		{"c prod", false},
		{"--help", false},
		{"help", false},
		{"", false},
		{"__complete connect ''", false},
	}
	for _, tt := range tests {
		if got := isPassthrough(root, strings.Fields(tt.args)); got != tt.want {
			t.Errorf("isPassthrough(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func assertNoSecretUnder(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(secretToken)) {
		t.Fatalf("%s contains the token", path)
	}
}
