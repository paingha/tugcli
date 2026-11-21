package sandbox

import (
	"fmt"
	"strings"
)

// profile renders rules as a macOS sandbox profile, in the Scheme-like
// language sandbox-exec reads. It denies everything by default, takes the
// rules every process needs from Apple's base profile, system.sb, and then
// allows what the rules name. The profile is plain text, so its tests run
// on every platform.
func profile(name string, r rules) string {
	var b strings.Builder
	fmt.Fprintf(&b, ";; tug's sandbox for the plugin %s\n", strings.Map(printable, name))
	b.WriteString(`(version 1)
(deny default)
(import "system.sb")
(system-network)
(allow process-fork)
(allow signal (target same-sandbox))
(allow file-read-metadata file-test-existence)
`)
	allow(&b, "process-exec", filters("literal", r.Programs), filters("subpath", r.ProgramDirs))
	allow(&b, "file-read* file-map-executable", filters("literal", r.Programs), filters("subpath", r.ProgramDirs), filters("subpath", r.Libraries))
	allow(&b, "file-read*", filters("subpath", r.Read))
	allow(&b, "file-read* file-write*", filters("subpath", r.Write), filters("prefix", r.WritePrefixes))
	allow(&b, "network-outbound", filters("literal", r.Sockets))
	// The sandbox cannot match host names, so it allows ports on any host,
	// as Landlock does on Linux. DNS over UDP is for resolvers that do not
	// use mDNSResponder.
	ports := []string{`(remote udp "*:53")`}
	for _, p := range r.Ports {
		ports = append(ports, fmt.Sprintf(`(remote tcp "*:%d")`, p))
	}
	allow(&b, "network-outbound", ports)
	return b.String()
}

// allow writes one allow rule with the filters given, or nothing when there
// are none.
func allow(b *strings.Builder, ops string, groups ...[]string) {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	if len(all) == 0 {
		return
	}
	fmt.Fprintf(b, "(allow %s\n  %s)\n", ops, strings.Join(all, "\n  "))
}

// filters wraps each path in a filter such as (subpath "/usr/lib").
func filters(kind string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, fmt.Sprintf("(%s %s)", kind, quote(p)))
	}
	return out
}

// quote writes s as a profile string literal.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(strings.Map(printable, s)) + `"`
}

// printable drops control characters, which have no place in a path and
// would break a profile line.
func printable(r rune) rune {
	if r < ' ' || r == 0x7f {
		return -1
	}
	return r
}
