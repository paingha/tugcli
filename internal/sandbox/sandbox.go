// Package sandbox confines provider plugins with the operating system's own
// sandbox, so that a plugin reaches its own directory, a private temporary
// directory, the system files every program needs, what its manifest
// declares, and TCP port 443, and nothing else.
//
// On Linux, tug runs itself as the hidden command ExecCommand. That process
// restricts itself with Landlock (files, and TCP ports from ABI 4) and a
// seccomp filter that refuses dangerous system calls, and then executes the
// plugin, which inherits the restrictions. On macOS, tug runs the plugin
// under sandbox-exec with a profile generated from the policy. Windows has
// no sandbox for plugins; they rely on the process boundary and the
// scrubbed environment alone.
//
// Where the sandbox is missing (Windows, a Linux kernel without Landlock, a
// Mac without sandbox-exec), plugins run without it and the logger says so
// at debug level.
package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/paingha/tugcli/internal/config"
)

// ExecCommand is the hidden tug command that confines its own process and
// then runs a plugin in its place. tug starts plugins through it on Linux.
const ExecCommand = "plugin-sandbox-exec"

// defaultPorts are the TCP ports every plugin may connect to: HTTPS, and DNS
// for resolvers that fall back to TCP.
var defaultPorts = []uint16{443, 53}

// Policy is what one plugin may reach beyond its own directory and the
// system files every program needs.
//
// Paths may start with ~/ for the home directory, or with an environment
// variable such as $TELEPORT_HOME; both are expanded with the plugin's own
// environment, and a path whose variables are unset there is left out.
type Policy struct {
	// Name is the plugin's name, for logs and the temporary directory.
	Name string
	// Dir is the plugin directory, which the plugin may read and load code
	// from.
	Dir string
	// Tools are programs the plugin runs: names looked up on the plugin's
	// PATH, or paths.
	Tools []string
	// Read lists files and directories the plugin may read.
	Read []string
	// Write lists files and directories it may also write.
	Write []string
	// Sockets lists Unix sockets it may connect to.
	Sockets []string
	// Ports lists TCP ports it may connect to besides 443.
	Ports []uint16
}

// Sandbox starts the processes of one plugin confined by its policy.
type Sandbox struct {
	Policy Policy
	// Self is tug's own executable, which runs ExecCommand on Linux.
	Self string
	// TempRoot holds the private temporary directories, such as a directory
	// in tug's runtime directory; empty means the system's temporary
	// directory.
	TempRoot string
	// Log receives, at debug level, what the sandbox does not enforce.
	Log *slog.Logger
}

// Command is a plugin process ready to start: Path run with Args and Env.
type Command struct {
	Path string
	Args []string
	Env  []string
	// Confined is true when the operating system's sandbox applies.
	Confined bool

	tempDir string
}

// Cleanup removes the plugin's private temporary directory. Call it once
// every process started from the command has exited.
func (c *Command) Cleanup() {
	if c.tempDir != "" {
		_ = os.RemoveAll(c.tempDir)
	}
}

// Status says what the sandbox enforces on this machine.
type Status struct {
	// Files is true when plugins reach only the files their policy allows.
	Files bool
	// Network is true when plugins connect only to the TCP ports their
	// policy allows.
	Network bool
	// Syscalls is true when a seccomp filter refuses dangerous system calls.
	Syscalls bool
	// Mechanism names what enforces the sandbox, such as "Landlock ABI 6".
	Mechanism string
	// Missing says what is not enforced and why; empty when nothing is
	// missing.
	Missing string
}

// Any reports whether the sandbox enforces anything at all.
func (s Status) Any() bool { return s.Files || s.Network || s.Syscalls }

var (
	checkOnce sync.Once
	checked   Status
)

// Check reports what the sandbox enforces on this machine. It probes once
// and remembers the answer.
func Check() Status {
	checkOnce.Do(func() { checked = check() })
	return checked
}

// Command prepares to run bin with args and the environment env in the
// sandbox: it makes the plugin's private temporary directory, points TMPDIR
// at it, and returns the command that applies the policy and runs bin.
// Where the sandbox is missing, the command runs bin as it is.
func (s *Sandbox) Command(bin string, args, env []string) (*Command, error) {
	st := Check()
	if !st.Any() {
		s.logger().Debug("plugin runs without a sandbox", "plugin", s.Policy.Name, "reason", st.Missing)
		return &Command{Path: bin, Args: args, Env: env}, nil
	}
	tmp, err := s.makeTempDir()
	if err != nil {
		return nil, err
	}
	c := &Command{Env: setEnv(env, "TMPDIR", tmp), Confined: true, tempDir: tmp}
	r := resolve(s.Policy, bin, c.Env, tmp)
	if err := s.wrap(c, bin, args, r); err != nil {
		c.Cleanup()
		return nil, err
	}
	attrs := []any{"plugin", s.Policy.Name, "enforced", st.Mechanism, "ports", r.Ports}
	if st.Missing != "" {
		attrs = append(attrs, "missing", st.Missing)
	}
	s.logger().Debug("plugin runs in the sandbox", attrs...)
	return c, nil
}

func (s *Sandbox) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// makeTempDir creates the plugin's private temporary directory, which only
// the user can enter. TempRoot and the directory holding it must be private
// too, since tug's runtime directory may be in a shared one such as /tmp.
func (s *Sandbox) makeTempDir() (string, error) {
	root := s.TempRoot
	if root == "" {
		root = os.TempDir()
	} else {
		for _, dir := range []string{filepath.Dir(root), root} {
			if err := config.EnsurePrivateDir(dir); err != nil {
				return "", fmt.Errorf("making the temporary directory for plugin %s: %w", s.Policy.Name, err)
			}
		}
	}
	dir, err := os.MkdirTemp(root, "tug-plugin-"+safeName(s.Policy.Name)+"-")
	if err != nil {
		return "", fmt.Errorf("making the temporary directory for plugin %s: %w", s.Policy.Name, err)
	}
	// Sandbox rules match real paths, as on macOS, where /var is a link.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir, nil
}

// safeName keeps the letters, digits and dashes of a plugin name.
func safeName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, name)
}

// setEnv returns env with name set to value, replacing any earlier value.
func setEnv(env []string, name, value string) []string {
	return append(withoutEnv(env, name), name+"="+value)
}

// withoutEnv returns a copy of env without name.
func withoutEnv(env []string, name string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
}
