package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/oidc/oidctest"
)

const oidcPluginManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: idp, version: 1.0.0}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: GOOS, arch: GOARCH, bin: tug-provider-idp}
`

// oidcPluginScript lists one cluster whose credential is a sign-in to
// ISSUER.
const oidcPluginScript = `#!/bin/sh
cat >/dev/null
case "$1" in
  list) printf '{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"o1","name":"oidc-dev","endpoints":{"public":"https://127.0.0.1:1"}}]}\n' ;;
  credential) printf '{"apiVersion":"provider/v1alpha1","kind":"CredentialResponse","credential":{"type":"oidc","issuer":"ISSUER","clientID":"CLIENT","scopes":["groups"]}}\n' ;;
  *) exit 2 ;;
esac
`

func TestAuthLoginServesIDTokens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin and browser")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl stands in for the browser")
	}
	home := isolate(t)
	t.Setenv("SSH_CONNECTION", "") // over SSH tug would prefer a device code
	iss := oidctest.NewIssuer(t)

	dir := t.TempDir()
	files := map[string]string{
		"plugin.yaml":      strings.NewReplacer("GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH).Replace(oidcPluginManifest),
		"tug-provider-idp": strings.NewReplacer("ISSUER", iss.URL, "CLIENT", oidctest.ClientID).Replace(oidcPluginScript),
		"browser":          "#!/bin/sh\nexec " + curl + " -fsSL -o /dev/null \"$1\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BROWSER", filepath.Join(dir, "browser"))
	cfg := filepath.Join(home, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("oidc:\n  listenAddresses: [\"127.0.0.1:0\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustTug(t, "plugin", "link", dir)
	mustTug(t, "import", "idp", "--account", "a")

	// Without a terminal, kubectl's call cannot sign in; it says how.
	r := tug(t, "auth", "token", "--cluster", "oidc-dev")
	if r.code == 0 || !strings.Contains(r.stderr, "not signed in to "+iss.URL) || !strings.Contains(r.stderr, "tug auth login idp/a/o1") {
		t.Fatalf("auth token before signing in: %+v", r)
	}

	r = tug(t, "auth", "login", "oidc-dev")
	if r.code != 0 || !strings.Contains(r.stdout, "Signed in to "+iss.URL+" as "+oidctest.Email) || !strings.Contains(r.stderr, "/authorize?") {
		t.Fatalf("auth login: %+v", r)
	}

	var ec struct {
		Status struct {
			Token               string `json:"token"`
			ExpirationTimestamp string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(mustTug(t, "auth", "token", "--cluster", "oidc-dev")), &ec); err != nil {
		t.Fatal(err)
	}
	if strings.Count(ec.Status.Token, ".") != 2 || ec.Status.ExpirationTimestamp == "" {
		t.Fatalf("auth token after signing in: %+v; want the ID token and its expiry", ec.Status)
	}
	// The ID token is kept only in the (encrypted) credential store.
	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), ec.Status.Token) {
			t.Errorf("%s holds the ID token", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if out := mustTug(t, "auth", "logout", "oidc-dev"); !strings.Contains(out, "Signed out of "+iss.URL) {
		t.Fatalf("auth logout: %s", out)
	}
	if out := mustTug(t, "auth", "logout", "oidc-dev"); !strings.Contains(out, "Not signed in") {
		t.Fatalf("second auth logout: %s", out)
	}
	if logins, _ := iss.Counts(); logins != 1 {
		t.Fatalf("%d sign-ins, want 1", logins)
	}
}

func TestAuthLoginRefusesOtherCredentials(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	if r := tug(t, "auth", "login", "prod"); r.code == 0 || !strings.Contains(r.stderr, "does not use OpenID Connect") {
		t.Fatalf("auth login for a token cluster: %+v", r)
	}
	if r := tug(t, "auth", "logout"); r.code == 0 || !strings.Contains(r.stderr, "name a cluster") {
		t.Fatalf("auth logout with no cluster and no session: %+v", r)
	}
}
