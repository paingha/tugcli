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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// workloadToken is the credential of the clusters a management cluster
// describes.
const workloadToken = "w0rkl0ad-token-from-management-cluster"

// argoToken is the bearer token in an Argo CD cluster Secret.
const argoToken = "4rgo-token-from-a-cluster-secret"

// argoClusterSecret is a cluster Secret as Argo CD's declarative setup
// describes it.
var argoClusterSecret, _ = json.Marshal(map[string]any{
	"kind": "Secret", "apiVersion": "v1", "type": "Opaque",
	"metadata": map[string]any{"name": "cluster-edge-1", "namespace": "argocd",
		"labels": map[string]string{"argocd.argoproj.io/secret-type": "cluster", "env": "dev"}},
	"data": map[string][]byte{
		"name":   []byte("edge-1"),
		"server": []byte("https://edge-1.example.com"),
		"config": []byte(`{"bearerToken":"` + argoToken + `","tlsClientConfig":{"insecure":false,"caData":"` + b64([]byte("-----BEGIN CERTIFICATE-----")) + `"}}`),
	},
})

// memberProfile is a ClusterProfile on a cluster inventory hub, offering
// one access provider. Its exec extension names the cluster, as the
// cluster inventory's secretreader plugin expects.
const memberProfile = `{"apiVersion":"multicluster.x-k8s.io/v1alpha2","kind":"ClusterProfile",
	"metadata":{"name":"member-1","namespace":"fleet","labels":{"env":"dev"}},
	"spec":{"displayName":"member-1","clusterManager":{"name":"fleet-manager"}},
	"status":{"version":{"kubernetes":"v1.34.1"},"accessProviders":[
		{"name":"token-plugin","cluster":{"server":"https://member-1.example.com","extensions":[
			{"name":"client.authentication.k8s.io/exec","extension":{"clusterName":"member-1"}}]}}]}}`

// writeProvidersFile writes a ClusterProfile providers file that maps the
// token-plugin access provider to command, which asks for cluster
// information.
func writeProvidersFile(t *testing.T, command string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"providers": []any{map[string]any{
		"name": "token-plugin",
		"execConfig": map[string]any{
			"apiVersion": "client.authentication.k8s.io/v1", "command": command, "interactiveMode": "Never",
			"provideClusterInfo": true,
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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
		case "/api/v1/namespaces/argocd/secrets":
			if r.URL.Query().Get("labelSelector") != "argocd.argoproj.io/secret-type=cluster" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[%s]}`, argoClusterSecret)
		case "/api/v1/namespaces/argocd/secrets/cluster-edge-1":
			_, _ = w.Write(argoClusterSecret)
		case "/apis/multicluster.x-k8s.io":
			fmt.Fprint(w, `{"kind":"APIGroup","apiVersion":"v1","name":"multicluster.x-k8s.io","versions":[
				{"groupVersion":"multicluster.x-k8s.io/v1alpha2","version":"v1alpha2"},
				{"groupVersion":"multicluster.x-k8s.io/v1alpha1","version":"v1alpha1"}]}`)
		case "/apis/multicluster.x-k8s.io/v1alpha2/clusterprofiles":
			fmt.Fprintf(w, `{"kind":"ClusterProfileList","apiVersion":"multicluster.x-k8s.io/v1alpha2","metadata":{},"items":[%s]}`, memberProfile)
		case "/apis/multicluster.x-k8s.io/v1alpha2/namespaces/fleet/clusterprofiles/member-1":
			fmt.Fprint(w, memberProfile)
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
	if !strings.Contains(out, "capi/fleet: 1 cluster") || !strings.Contains(out, "1 credential moved") {
		t.Fatalf("import capi: %s", out)
	}
	if out := mustTug(t, "ls", "-o", "name", "-l", "provider=capi"); strings.TrimSpace(out) != "workload-a" {
		t.Fatalf("ls -l provider=capi: %s", out)
	}
	if ec := mustTug(t, "auth", "token", "--cluster", "workload-a", "--refresh"); !strings.Contains(ec, workloadToken) {
		t.Fatalf("auth token for a Cluster API cluster: %s", ec)
	}

	out = mustTug(t, "import", "argocd", "--account", "gitops", "--set", "cluster=payments-staging")
	if !strings.Contains(out, "argocd/gitops: 1 cluster") || !strings.Contains(out, "1 credential moved") {
		t.Fatalf("import argocd: %s", out)
	}
	if out := mustTug(t, "ls", "-o", "name", "-l", "provider=argocd"); strings.TrimSpace(out) != "edge-1" {
		t.Fatalf("ls -l provider=argocd: %s", out)
	}
	if ec := mustTug(t, "auth", "token", "--cluster", "edge-1", "--refresh"); !strings.Contains(ec, argoToken) {
		t.Fatalf("auth token for an Argo CD cluster: %s", ec)
	}

	plugin := "tug-test-token-plugin"
	if runtime.GOOS != "windows" {
		plugin = filepath.Join(t.TempDir(), "token-plugin")
		// Like the secretreader plugin, it answers only when it is told
		// which cluster it is for.
		script := `#!/bin/sh
case "$KUBERNETES_EXEC_INFO" in
*'"server":"https://member-1.example.com"'*'"config":{"clusterName":"member-1"}'*)
	echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"from-the-plugin"}}' ;;
*)
	echo "no cluster information in KUBERNETES_EXEC_INFO: $KUBERNETES_EXEC_INFO" >&2; exit 1 ;;
esac
`
		if err := os.WriteFile(plugin, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	out = mustTug(t, "import", "clusterprofile", "--account", "inventory", "--set", "cluster=payments-staging",
		"--set", "providerFile="+writeProvidersFile(t, plugin))
	if !strings.Contains(out, "clusterprofile/inventory: 1 cluster") || !strings.Contains(out, "1 credential moved") {
		t.Fatalf("import clusterprofile: %s", out)
	}
	if out := mustTug(t, "ls", "-o", "name", "-l", "provider=clusterprofile"); strings.TrimSpace(out) != "member-1" {
		t.Fatalf("ls -l provider=clusterprofile: %s", out)
	}
	if runtime.GOOS != "windows" {
		if ec := mustTug(t, "auth", "token", "--cluster", "member-1"); !strings.Contains(ec, "from-the-plugin") {
			t.Fatalf("auth token through the locally configured plugin: %s", ec)
		}
	}

	b, err := os.ReadFile(filepath.Join(home, "data", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{workloadToken, argoToken} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("the catalog contains %s", secret)
		}
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
