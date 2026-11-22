package cli

import (
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/intelligence"
)

// TestDeepKnowsEveryTugCommand holds --deep's command check to tug's own
// commands: every one, by name and alias, is either known to only read
// (with the subcommands that do) or known to do more. One the check did not
// know would be judged as the kubectl command of the same name.
func TestDeepKnowsEveryTugCommand(t *testing.T) {
	isolate(t)
	root := newRootCmd(&app{})
	for _, c := range root.Commands() {
		if c.Hidden {
			continue
		}
		for _, name := range append([]string{c.Name()}, c.Aliases...) {
			ok, why := intelligence.ReadOnly("tug " + name + " x")
			if !ok && strings.HasPrefix(why, "kubectl ") {
				t.Errorf("--deep's command check does not know tug %s, and judged it as kubectl: %s", name, why)
			}
		}
	}
}
