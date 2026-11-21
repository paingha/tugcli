package cli

import (
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
)

// clusterInfoPluginScript is a provider plugin whose cluster gets its
// credential from an exec plugin that asks for cluster information.
// SERVER, CA, PLUGIN and INFO are filled in by the test.
const clusterInfoPluginScript = `#!/bin/sh
cat >/dev/null
reply() { printf '{"apiVersion":"provider/v1alpha1","kind":"%s",%s}\n' "$1" "$2"; }
case "$1" in
  list) reply ListResponse '"clusters":[{"id":"m1","name":"member-1","endpoints":{"public":"SERVER"},"caData":"CA","tlsServerName":"example.com"}]' ;;
  credential) reply CredentialResponse '"credential":{"type":"delegate","exec":{"command":"PLUGIN","args":["INFO"],"apiVersion":"client.authentication.k8s.io/v1","interactiveMode":"Never","provideClusterInfo":true,"clusterConfig":{"clusterName":"member-1"}}}' ;;
  *) exit 2 ;;
esac
`

const clusterInfoPluginManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: inv, version: 1.0.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: GOOS, arch: GOARCH, bin: tug-provider-inv}
`

// execInfoRecorder is an exec plugin that writes the KUBERNETES_EXEC_INFO
// it was given to the file named by its argument, and authenticates to
// fakeAPIServer.
const execInfoRecorder = `#!/bin/sh
printf '%s' "$KUBERNETES_EXEC_INFO" > "$1"
echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"` + secretToken + `"}}'
`

// A delegate that asks for cluster information is told about its catalog
// cluster both when kubectl runs `tug auth token` and when tug runs it
// in process.
func TestDelegateGetsClusterInfo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugins")
	}
	isolate(t)
	srv, _ := fakeAPIServer(t)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	dir := t.TempDir()
	recorder := filepath.Join(dir, "secretreader-plugin")
	info := filepath.Join(dir, "exec-info.json")
	script := strings.NewReplacer("SERVER", srv.URL, "CA", b64(ca), "PLUGIN", recorder, "INFO", info).Replace(clusterInfoPluginScript)
	manifest := strings.NewReplacer("GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH).Replace(clusterInfoPluginManifest)
	for name, content := range map[string]string{
		"plugin.yaml":         manifest,
		"tug-provider-inv":    script,
		"secretreader-plugin": execInfoRecorder,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustTug(t, "plugin", "link", dir)
	if out := mustTug(t, "import", "inv", "--account", "a"); !strings.Contains(out, "inv/a: 1 clusters") {
		t.Fatalf("import inv: %s", out)
	}

	want := &clientauthv1.Cluster{
		Server:                   srv.URL,
		TLSServerName:            "example.com",
		CertificateAuthorityData: ca,
	}
	want.Config.Raw = []byte(`{"clusterName":"member-1"}`)
	received := func(how string) {
		t.Helper()
		b, err := os.ReadFile(info)
		if err != nil {
			t.Fatalf("%s: the plugin did not run: %v", how, err)
		}
		if err := os.Remove(info); err != nil {
			t.Fatal(err)
		}
		var ec clientauthv1.ExecCredential
		if err := json.Unmarshal(b, &ec); err != nil {
			t.Fatalf("%s: KUBERNETES_EXEC_INFO = %s: %v", how, b, err)
		}
		if ec.APIVersion != "client.authentication.k8s.io/v1" || ec.Kind != "ExecCredential" || ec.Spec.Interactive {
			t.Fatalf("%s: KUBERNETES_EXEC_INFO = %s, want a v1 ExecCredential, not interactive", how, b)
		}
		if !reflect.DeepEqual(ec.Spec.Cluster, want) {
			t.Fatalf("%s: KUBERNETES_EXEC_INFO = %s, want spec.cluster %+v", how, b, want)
		}
	}

	// kubectl's exec plugin call.
	if ec := mustTug(t, "auth", "token", "--cluster", "member-1"); !strings.Contains(ec, secretToken) {
		t.Fatalf("auth token: %s", ec)
	}
	received("tug auth token")

	// client-go in tug's process runs the plugin for in-process commands.
	newSession(t)
	mustTug(t, "connect", "member-1")
	if out := mustTug(t, "ns"); !strings.Contains(out, "payments") {
		t.Fatalf("ns list: %s", out)
	}
	received("tug ns")
}
