package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/plugin"
)

// TestImportHelpNamesRealSettings keeps the --set examples in `tug import
// --help` in step with the settings the first-party plugins declare.
func TestImportHelpNamesRealSettings(t *testing.T) {
	isolate(t)
	help := mustTug(t, "import", "--help")
	examples := regexp.MustCompile(`tug import (\S+) .*--set ([^=\s]+)=`).FindAllStringSubmatch(help, -1)
	if len(examples) == 0 {
		t.Fatalf("no --set example in tug import --help:\n%s", help)
	}
	for _, ex := range examples {
		name, key := ex[1], ex[2]
		m, err := plugin.LoadManifest(filepath.Join("..", "..", "plugins", name))
		if err != nil {
			t.Fatalf("loading the %s manifest: %v", name, err)
		}
		var known []string
		for _, s := range m.Settings() {
			known = append(known, s.Name)
		}
		if !slices.Contains(known, key) {
			t.Errorf("tug import --help sets %s for %s, want one of %v", key, name, known)
		}
	}
}

// TestImportOneCluster checks that import and forget count a single cluster
// and credential in the singular.
func TestImportOneCluster(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	path := writeKubeconfig(t, srv)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	one := strings.Replace(string(b), "- name: payments-staging\n  context: {cluster: c, user: u}\n", "", 1)
	if one == string(b) {
		t.Fatal("the test kubeconfig no longer has a payments-staging context to drop")
	}
	if err := os.WriteFile(path, []byte(one), 0o600); err != nil {
		t.Fatal(err)
	}

	out := mustTug(t, "import", "kubeconfig", path)
	m := regexp.MustCompile(`^✓ (kubeconfig/[^:]+): 1 cluster, 1 new, 1 credential moved to .+\n$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("tug import kubeconfig = %q, want 1 cluster, 1 new, 1 credential", out)
	}
	if out := mustTug(t, "forget", m[1]); out != "Removed "+m[1]+" and 1 cluster\n" {
		t.Fatalf("tug forget %s = %q, want %q", m[1], out, "Removed "+m[1]+" and 1 cluster\n")
	}
}
