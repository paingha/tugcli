package local

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

var certExpiry = time.Date(2027, 11, 20, 0, 0, 0, 0, time.UTC)

// clientCert returns a PEM certificate and key, base64-encoded as kubeconfig
// *-data fields are.
func clientCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes-admin"}, NotBefore: time.Now(), NotAfter: certExpiry}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(typ string, b []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
	}
	return enc("CERTIFICATE", der), enc("EC PRIVATE KEY", keyDER)
}

// kubeconfigFor writes a single-context kubeconfig as kind or k3d print it.
func kubeconfigFor(t *testing.T, context, server string) string {
	t.Helper()
	cert, key := clientCert(t)
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    certificate-authority-data: %[3]s
users:
- name: %[1]s
  user:
    client-certificate-data: %[3]s
    client-key-data: %[4]s
contexts:
- name: %[1]s
  context: {cluster: %[1]s, user: %[1]s}
current-context: %[1]s
`, context, server, cert, key)
}

// fakeRunner answers commands from a table keyed by the full command line.
func fakeRunner(outputs map[string]string, errs map[string]error) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		if err, ok := errs[line]; ok {
			return nil, err
		}
		out, ok := outputs[line]
		if !ok {
			return nil, fmt.Errorf("unexpected command %q", line)
		}
		return []byte(out), nil
	}
}

func noTools(string) (string, error) { return "", errors.New("not found") }

func newTestProvider(t *testing.T, run Runner) *Provider {
	t.Helper()
	return New(WithRunner(run), WithLookPath(noTools), WithHome(t.TempDir()), WithGetenv(func(string) string { return "" }))
}

func TestListKind(t *testing.T) {
	run := fakeRunner(map[string]string{
		"kind get clusters":              "ci\ndev\n",
		"kind get kubeconfig --name dev": kubeconfigFor(t, "kind-dev", "https://127.0.0.1:52001"),
		"kind get kubeconfig --name ci":  kubeconfigFor(t, "kind-ci", "https://127.0.0.1:52002"),
	}, nil)
	p := newTestProvider(t, run)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "kind"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0].ID != "kind-ci" || clusters[1].ID != "kind-dev" {
		t.Fatalf("clusters = %+v", clusters)
	}
	dev := clusters[1]
	if dev.Endpoints.Public != "https://127.0.0.1:52001" || len(dev.CAData) == 0 {
		t.Fatalf("kind-dev: %+v", dev)
	}
	for k, v := range map[string]string{"local": "true", "distro": "kind", "env": "dev"} {
		if dev.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, dev.Labels[k], v)
		}
	}
	c := dev.Credential
	if c == nil || c.Type != provider.CredentialClientCert || c.ExpiresAt == nil || !c.ExpiresAt.Equal(certExpiry) {
		t.Fatalf("kind's client certificate should be inline with its expiry: %+v", c)
	}

	cred, err := p.Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "kind"}, ClusterID: "kind-dev"})
	if err != nil || cred.Type != provider.CredentialClientCert {
		t.Fatalf("credential: %+v %v", cred, err)
	}
}

func TestKindWithoutDocker(t *testing.T) {
	run := fakeRunner(nil, map[string]error{
		"kind get clusters": errors.New("kind get clusters: failed to list clusters: command \"docker ps\" failed"),
	})
	_, err := newTestProvider(t, run).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "kind"}})
	// Unavailable, not an empty list: the catalog keeps kind's clusters.
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || !strings.Contains(pe.Hint, "Docker") {
		t.Fatalf("got %+v", pe)
	}
}

func TestListK3d(t *testing.T) {
	run := fakeRunner(map[string]string{
		"k3d cluster list -o json": `[{"name":"dev","serversCount":1}]`,
		"k3d kubeconfig get dev":   kubeconfigFor(t, "k3d-dev", "https://0.0.0.0:6550"),
	}, nil)
	clusters, err := newTestProvider(t, run).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "k3d", Config: map[string]any{ConfigDistro: "k3d"}}})
	if err != nil || len(clusters) != 1 || clusters[0].ID != "k3d-dev" || clusters[0].Labels["distro"] != "k3d" {
		t.Fatalf("k3d: %+v %v", clusters, err)
	}
}

const minikubeCluster = `- name: dev-mk
  cluster:
    server: https://127.0.0.1:52011
    certificate-authority-data: %[1]s
    extensions:
    - name: cluster_info
      extension:
        last-update: Fri, 20 Nov 2026 10:00:00 UTC
        provider: minikube.sigs.k8s.io
        version: v1.34.0
`

// writeHome lays out ~/.kube/config with local and remote contexts, and
// OrbStack's own file, which repeats the orbstack context.
func writeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	cert, key := clientCert(t)
	clusters := fmt.Sprintf(minikubeCluster, cert)
	var users, contexts strings.Builder
	for _, c := range []struct{ name, server string }{
		{"docker-desktop", "https://kubernetes.docker.internal:6443"},
		{"orbstack", "https://127.0.0.1:26443"},
		{"kind-old", "https://127.0.0.1:52001"},
		{"prod-eu", "https://prod.example.com"},
	} {
		clusters += fmt.Sprintf("- name: %s\n  cluster:\n    server: %s\n    certificate-authority-data: %s\n", c.name, c.server, cert)
	}
	for _, name := range []string{"dev-mk", "docker-desktop", "orbstack", "kind-old", "prod-eu"} {
		fmt.Fprintf(&users, "- name: %s\n  user:\n    client-certificate-data: %s\n    client-key-data: %s\n", name, cert, key)
		fmt.Fprintf(&contexts, "- name: %s\n  context: {cluster: %s, user: %s}\n", name, name, name)
	}
	config := "apiVersion: v1\nkind: Config\nclusters:\n" + clusters + "users:\n" + users.String() + "contexts:\n" + contexts.String()
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".kube", "config"), config)
	write(filepath.Join(home, ".orbstack", "k8s", "config.yml"), kubeconfigFor(t, "orbstack", "https://127.0.0.1:26443"))
	return home
}

func TestListFromKubeconfigFiles(t *testing.T) {
	home := writeHome(t)
	p := New(WithRunner(fakeRunner(nil, nil)), WithLookPath(noTools), WithHome(home), WithGetenv(func(string) string { return "" }))
	tests := []struct {
		distro string
		want   []string
	}{
		{"docker-desktop", []string{"docker-desktop"}},
		{"orbstack", []string{"orbstack"}}, // in two files, listed once
		{"minikube", []string{"dev-mk"}},   // found by minikube's extension, not its name
		{"rancher-desktop", nil},
	}
	for _, tt := range tests {
		t.Run(tt.distro, func(t *testing.T) {
			clusters, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: tt.distro}})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, c := range clusters {
				ids = append(ids, c.ID)
				if c.Labels["distro"] != tt.distro || c.Labels["env"] != "dev" {
					t.Errorf("%s labels = %v", c.ID, c.Labels)
				}
			}
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("listed %v, want %v", ids, tt.want)
			}
		})
	}

	cred, err := p.Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "minikube"}, ClusterID: "dev-mk"})
	if err != nil || cred.Type != provider.CredentialClientCert {
		t.Fatalf("minikube credential: %+v %v", cred, err)
	}
	_, err = p.Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "minikube"}, ClusterID: "gone"})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || !strings.Contains(pe.Hint, "start the cluster") {
		t.Fatalf("missing context: %+v", pe)
	}
}

// writeFile writes body to path, creating its directory.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listIDs lists an account's clusters and returns their IDs, comma-separated.
func listIDs(t *testing.T, p *Provider, account string) string {
	t.Helper()
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: account}})
	if err != nil {
		t.Fatalf("listing %s: %v", account, err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	return strings.Join(ids, ",")
}

// envOf returns a getenv function that answers from env.
func envOf(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// The kubeconfig provider leaves local contexts out of every file it finds,
// so this provider has to look in the same files.
func TestListFromEveryKubeconfigFile(t *testing.T) {
	home := writeHome(t)
	writeFile(t, filepath.Join(home, ".kube", "work.yaml"), kubeconfigFor(t, "rancher-desktop", "https://127.0.0.1:6443"))
	// Not a kubeconfig: kind's own cluster config, which people keep in ~/.kube.
	writeFile(t, filepath.Join(home, ".kube", "kind-cluster.yaml"), "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n")
	other := filepath.Join(t.TempDir(), "other.yaml")
	writeFile(t, other, kubeconfigFor(t, "prod-us", "https://prod-us.example.com"))
	// $KUBECONFIG names another file, but ~/.kube/config is still read.
	p := New(WithRunner(fakeRunner(nil, nil)), WithLookPath(noTools), WithHome(home), WithGetenv(envOf(map[string]string{"KUBECONFIG": other})))

	if got := listIDs(t, p, "rancher-desktop"); got != "rancher-desktop" {
		t.Errorf("rancher-desktop from ~/.kube/work.yaml: %q", got)
	}
	if got := listIDs(t, p, "minikube"); got != "dev-mk" {
		t.Errorf("minikube from ~/.kube/config: %q", got)
	}
	accts, err := p.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(accts, func(a provider.Account) bool { return a.ID == "rancher-desktop" }) {
		t.Errorf("no rancher-desktop account in %+v", accts)
	}
}

// Inside a tug session, $KUBECONFIG names the session's own file, so the
// files from before the session are read instead.
func TestListInsideSession(t *testing.T) {
	home := writeHome(t)
	sessions := filepath.Join(t.TempDir(), "run")
	sessionFile := filepath.Join(sessions, "abc123", "kubeconfig")
	// As tug writes it: the cluster's name, and a user that runs tug.
	writeFile(t, sessionFile, `apiVersion: v1
kind: Config
clusters:
- {name: docker-desktop, cluster: {server: "https://kubernetes.docker.internal:6443"}}
users:
- name: docker-desktop
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: tug, args: [auth, token, --cluster, docker-desktop]}
contexts:
- {name: docker-desktop, context: {cluster: docker-desktop, user: docker-desktop}}
current-context: docker-desktop
`)
	orig := filepath.Join(t.TempDir(), "rancher.yaml")
	writeFile(t, orig, kubeconfigFor(t, "rancher-desktop", "https://127.0.0.1:6443"))

	tests := []struct {
		name, orig, distro, want string
	}{
		{"saved KUBECONFIG", orig, "rancher-desktop", "rancher-desktop"},
		{"no saved KUBECONFIG", "", "docker-desktop", "docker-desktop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"KUBECONFIG": sessionFile, "TUG_ORIG_KUBECONFIG": tt.orig}
			p := New(WithRunner(fakeRunner(nil, nil)), WithLookPath(noTools), WithHome(home), WithGetenv(envOf(env)), WithSkipDirs(sessions))
			if got := listIDs(t, p, tt.distro); got != tt.want {
				t.Fatalf("listed %q, want %q", got, tt.want)
			}
			cred, err := p.Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: tt.distro}, ClusterID: tt.want})
			if err != nil || cred.Type != provider.CredentialClientCert {
				t.Fatalf("credential: %+v %v", cred, err)
			}
		})
	}
}

// A file without the context, read first, must not stop the search.
func TestCredentialFromLaterFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".kube", "config"), kubeconfigFor(t, "prod-eu", "https://prod.example.com"))
	writeFile(t, filepath.Join(home, ".orbstack", "k8s", "config.yml"), kubeconfigFor(t, "orbstack", "https://127.0.0.1:26443"))
	p := New(WithRunner(fakeRunner(nil, nil)), WithLookPath(noTools), WithHome(home), WithGetenv(envOf(nil)))

	cred, err := p.Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "orbstack"}, ClusterID: "orbstack"})
	if err != nil || cred.Type != provider.CredentialClientCert {
		t.Fatalf("orbstack credential: %+v %v", cred, err)
	}
}

func TestAccounts(t *testing.T) {
	home := writeHome(t)
	lookPath := func(name string) (string, error) {
		if name == "kind" {
			return "/opt/homebrew/bin/kind", nil
		}
		return "", errors.New("not found")
	}
	p := New(WithRunner(fakeRunner(nil, nil)), WithLookPath(lookPath), WithHome(home), WithGetenv(func(string) string { return "" }))
	accts, err := p.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range accts {
		ids = append(ids, a.ID)
		if a.ConfigString(ConfigDistro) != a.ID {
			t.Errorf("%s config = %v", a.ID, a.Config)
		}
	}
	// kind from its CLI; the rest from contexts. No k3d CLI, no rancher context.
	if got, want := strings.Join(ids, ","), "docker-desktop,kind,minikube,orbstack"; got != want {
		t.Fatalf("accounts = %s, want %s", got, want)
	}
}

func TestUnknownDistro(t *testing.T) {
	_, err := newTestProvider(t, fakeRunner(nil, nil)).List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "microk8s"}})
	if provider.CodeOf(err) != provider.InvalidConfig {
		t.Fatalf("got %v", err)
	}
}
