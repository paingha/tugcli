//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// sandboxExec runs a program under a sandbox profile. Apple has marked it
// deprecated since macOS 10.12 but still ships it and uses its profile
// language for its own services; tug runs plugins without a sandbox, and
// says so in the debug log, when it is missing or does not work.
const sandboxExec = "/usr/bin/sandbox-exec"

// Libraries outside the system that programs installed with Homebrew load,
// and the Homebrew CA bundles.
var darwinLibraries = []string{
	"/opt/homebrew/Cellar", "/opt/homebrew/opt", "/opt/homebrew/lib", "/opt/homebrew/Frameworks",
	"/usr/local/Cellar", "/usr/local/opt", "/usr/local/lib", "/usr/local/Frameworks",
}

// Files every program may read beyond what Apple's system.sb allows: name
// resolution, CA certificates and the time zone. None holds secrets.
var darwinReads = []string{
	"/private/etc/hosts", "/private/etc/resolv.conf", "/private/var/run/resolv.conf",
	"/private/etc/ssl", "/private/etc/localtime",
	"/opt/homebrew/etc/ca-certificates", "/opt/homebrew/etc/openssl@3",
	"/usr/local/etc/ca-certificates", "/usr/local/etc/openssl@3",
}

// mDNSResponder answers DNS for libSystem's resolver, which Go programs use.
const mDNSResponder = "/private/var/run/mDNSResponder"

func addSystem(r *rules) {
	r.Libraries = append(r.Libraries, darwinLibraries...)
	r.Read = append(r.Read, darwinReads...)
	r.Sockets = append(r.Sockets, mDNSResponder)
	// /bin/sh runs the shell that /private/var/select/sh names.
	if slices.Contains(r.Programs, "/bin/sh") {
		r.Read = append(r.Read, "/private/var/select/sh")
		shell := "/bin/bash"
		if target, err := filepath.EvalSymlinks("/private/var/select/sh"); err == nil {
			shell = target
		}
		r.Programs = append(r.Programs, shell)
	}
	// A program inside a framework, such as Homebrew's or python.org's
	// python3 in Python.framework/Versions/3.x/bin, loads code from the
	// rest of the framework and runs again as the app bundled in its
	// Resources so that macOS treats it as an application.
	for _, p := range slices.Clone(r.Programs) {
		version, launchers := frameworkOf(p)
		if version == "" {
			continue
		}
		r.Libraries = append(r.Libraries, version)
		r.ProgramDirs = append(r.ProgramDirs, launchers...)
	}
	// Programs in /usr/bin such as python3 and git are shims that run the
	// real one from the developer tools that xcode-select chose, after
	// caching where it is in the user's temporary directory.
	if slices.ContainsFunc(r.Programs, func(p string) bool { return strings.HasPrefix(p, "/usr/bin/") }) {
		dev := developerDir()
		// Xcode's tools load frameworks from the rest of the app bundle.
		if contents, ok := strings.CutSuffix(dev, "/Developer"); ok && strings.HasSuffix(contents, ".app/Contents") {
			dev = contents
		}
		r.ProgramDirs = append(r.ProgramDirs, dev)
		r.Read = append(r.Read, "/private/var/db/xcode_select_link", "/private/var/select/developer_dir")
		if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
			r.WritePrefixes = append(r.WritePrefixes, filepath.Join(tmp, "xcrun_db"))
		}
	}
}

// frameworkOf returns, for a program inside a framework version such as
// .../Python.framework/Versions/3.15/bin/python3.15, the version's directory
// and the directories of the app launchers in its Resources, such as
// .../Versions/3.15/Resources/Python.app/Contents/MacOS. It returns "" for
// any other program.
func frameworkOf(program string) (version string, launchers []string) {
	const marker = ".framework/Versions/"
	i := strings.Index(program, marker)
	if i < 0 {
		return "", nil
	}
	rest := program[i+len(marker):]
	v, _, ok := strings.Cut(rest, "/")
	if !ok || v == "" {
		return "", nil
	}
	version = program[:i+len(marker)+len(v)]
	launchers, _ = filepath.Glob(filepath.Join(version, "Resources", "*.app", "Contents", "MacOS"))
	return version, launchers
}

// developerDir returns the developer tools directory xcode-select chose.
func developerDir() string {
	for _, link := range []string{"/private/var/db/xcode_select_link", "/private/var/select/developer_dir"} {
		if dir, err := filepath.EvalSymlinks(link); err == nil {
			return dir
		}
	}
	return "/Library/Developer/CommandLineTools"
}

func check() Status {
	if _, err := os.Stat(sandboxExec); err != nil {
		return Status{Missing: "everything: " + sandboxExec + " is missing"}
	}
	// It fails inside another sandbox, which may not nest one.
	if err := exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").Run(); err != nil {
		return Status{Missing: "everything: " + sandboxExec + " does not work here: " + err.Error()}
	}
	return Status{Files: true, Network: true, Mechanism: "sandbox-exec", Missing: "system calls: only Linux filters them"}
}

// wrap runs bin under sandbox-exec with a profile made from the rules.
func (s *Sandbox) wrap(c *Command, bin string, args []string, r rules) error {
	c.Path = sandboxExec
	c.Args = append([]string{"-p", profile(s.Policy.Name, r), bin}, args...)
	return nil
}

// Exec is only used on Linux; on macOS, sandbox-exec starts plugins.
func Exec([]string) error {
	return errors.New(ExecCommand + " is only used on Linux; on macOS, tug starts plugins with sandbox-exec")
}
