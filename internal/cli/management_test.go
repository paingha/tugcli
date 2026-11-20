package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// workloadToken is the credential of the clusters a management cluster
// describes.
const workloadToken = "w0rkl0ad-token-from-management-cluster"

// workloadKubeconfig is what a Cluster API control plane provider writes
// to a <cluster>-kubeconfig Secret.
var workloadKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: workload-a
  cluster:
    server: https://workload-a.example.com:6443
    certificate-authority-data: ` + b64([]byte("-----BEGIN CERTIFICATE-----")) + `
users:
- name: workload-a-admin
  user:
    token: ` + workloadToken + `
contexts:
- name: workload-a-admin@workload-a
  context: {cluster: workload-a, user: workload-a-admin}
current-context: workload-a-admin@workload-a
`

// fakeManagementServer is a management cluster serving the objects the
// built-in management cluster providers read, and only to callers
// presenting secretToken.
func fakeManagementServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/cluster.x-k8s.io":
			fmt.Fprint(w, `{"kind":"APIGroup","apiVersion":"v1","name":"cluster.x-k8s.io","versions":[
				{"groupVersion":"cluster.x-k8s.io/v1beta2","version":"v1beta2"},
				{"groupVersion":"cluster.x-k8s.io/v1beta1","version":"v1beta1"}]}`)
		case "/apis/cluster.x-k8s.io/v1beta1/clusters":
			fmt.Fprint(w, `{"kind":"ClusterList","apiVersion":"cluster.x-k8s.io/v1beta1","metadata":{},"items":[
				{"apiVersion":"cluster.x-k8s.io/v1beta1","kind":"Cluster",
				 "metadata":{"name":"workload-a","namespace":"fleet","labels":{"env":"dev"}},
				 "spec":{"controlPlaneEndpoint":{"host":"workload-a.example.com","port":6443}}}]}`)
		case "/api/v1/namespaces/fleet/secrets/workload-a-kubeconfig":
			secret, _ := json.Marshal(map[string]any{
				"kind": "Secret", "apiVersion": "v1", "type": "cluster.x-k8s.io/secret",
				"metadata": map[string]any{"name": "workload-a-kubeconfig", "namespace": "fleet"},
				"data":     map[string][]byte{"value": []byte(workloadKubeconfig)},
			})
			_, _ = w.Write(secret)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// importManagementCluster imports the fake management cluster's kubeconfig,
// whose contexts are payments-prod and payments-staging.
func importManagementCluster(t *testing.T) {
	t.Helper()
	mgmt := fakeManagementServer(t)
	mustTug(t, "import", "kubeconfig", "--account", "mgmt", writeKubeconfig(t, mgmt))
}

func TestImportFromManagementCluster(t *testing.T) {
	home := isolate(t)
	importManagementCluster(t)

	out := mustTug(t, "import", "capi", "--account", "fleet", "--set", "cluster=payments-staging")
	if !strings.Contains(out, "capi/fleet: 1 clusters") || !strings.Contains(out, "1 credentials moved") {
		t.Fatalf("import capi: %s", out)
	}
	if out := mustTug(t, "ls", "-o", "name", "-l", "provider=capi"); strings.TrimSpace(out) != "workload-a" {
		t.Fatalf("ls -l provider=capi: %s", out)
	}
	b, err := os.ReadFile(filepath.Join(home, "data", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), workloadToken) {
		t.Fatal("the catalog contains the workload cluster's token")
	}
	if ec := mustTug(t, "auth", "token", "--cluster", "workload-a", "--refresh"); !strings.Contains(ec, workloadToken) {
		t.Fatalf("auth token for a Cluster API cluster: %s", ec)
	}

	r := tug(t, "import", "capi", "--account", "lost", "--set", "cluster=nowhere")
	if r.code == 0 || !strings.Contains(r.stderr, "nowhere is not in the catalog") || !strings.Contains(r.stderr, "hint: import the management cluster") {
		t.Fatalf("import with an unknown management cluster: %+v", r)
	}
}

// A management cluster whose credential would come from a provider that
// reads it from that same cluster is refused rather than recursing.
func TestManagementConfigRefusesACycle(t *testing.T) {
	isolate(t)
	importManagementCluster(t)
	a := &app{stdin: strings.NewReader(""), in: bufio.NewReader(strings.NewReader("")), stdout: io.Discard, stderr: io.Discard, now: time.Now}
	if err := a.setup(false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg, err := a.managementConfig(ctx, "payments-staging")
	if err != nil || !strings.HasPrefix(cfg.Host, "https://127.0.0.1:") {
		t.Fatalf("managementConfig(payments-staging) = %+v, %v", cfg, err)
	}
	cat, err := a.loadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cat.Get("payments-staging")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.managementConfig(context.WithValue(ctx, managingKey{}, []string{cl.ID}), "payments-staging")
	if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || !strings.Contains(pe.Message, "through itself") {
		t.Fatalf("managementConfig inside its own resolution = %v, want InvalidConfig", err)
	}
}
