package sandbox

import (
	"strings"
	"testing"
)

func TestProfile(t *testing.T) {
	r := rules{
		Programs:      []string{"/plugins/acme/tug-provider-acme", "/opt/homebrew/bin/tsh"},
		ProgramDirs:   []string{"/Library/Developer/CommandLineTools"},
		Libraries:     []string{"/plugins/acme"},
		Read:          []string{`/Users/ada/odd "name"\dir`},
		Write:         []string{"/private/tmp/tug-plugin-acme-1"},
		WritePrefixes: []string{"/private/var/folders/x/T/xcrun_db"},
		Sockets:       []string{"/private/var/run/mDNSResponder"},
		Ports:         []uint16{53, 443, 3080},
	}
	got := profile("acme\n(allow default)", r)
	for _, want := range []string{
		"(deny default)",
		`(import "system.sb")`,
		"(allow process-exec\n  (literal \"/plugins/acme/tug-provider-acme\")\n  (literal \"/opt/homebrew/bin/tsh\")\n  (subpath \"/Library/Developer/CommandLineTools\"))",
		`(subpath "/plugins/acme"))`,
		`(allow file-read*` + "\n" + `  (subpath "/Users/ada/odd \"name\"\\dir"))`,
		"(allow file-read* file-write*\n  (subpath \"/private/tmp/tug-plugin-acme-1\")\n  (prefix \"/private/var/folders/x/T/xcrun_db\"))",
		"(allow network-outbound\n  (literal \"/private/var/run/mDNSResponder\"))",
		`(remote tcp "*:443")`,
		`(remote tcp "*:3080")`,
		`(remote udp "*:53")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile lacks %s; it is:\n%s", want, got)
		}
	}
	// The name goes in a comment, which must not end early.
	if strings.Contains(got, "\n(allow default)") {
		t.Errorf("a newline in the plugin name escaped the comment:\n%s", got)
	}
}

func TestProfileLeavesOutEmptyRules(t *testing.T) {
	got := profile("acme", rules{Programs: []string{"/p/tug-provider-acme"}})
	if strings.Contains(got, "(allow file-read* file-write*") || strings.Contains(got, "(allow file-read*\n") {
		t.Errorf("profile has a rule with nothing in it:\n%s", got)
	}
	if strings.Count(got, "(allow network-outbound") != 1 {
		t.Errorf("profile should allow only DNS over UDP on the network:\n%s", got)
	}
}
