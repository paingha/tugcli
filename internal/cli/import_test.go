package cli

import (
	"path/filepath"
	"regexp"
	"slices"
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
