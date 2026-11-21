//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// policyEnv carries the resolved rules from tug to ExecCommand, which
// removes it before it runs the plugin.
const policyEnv = "TUG_SANDBOX_POLICY"

// Directories of shared libraries, which the dynamic linker must be able to
// execute and the programs it loads must be able to read.
var linuxLibraries = []string{
	"/lib", "/lib32", "/lib64", "/libx32",
	"/usr/lib", "/usr/lib32", "/usr/lib64", "/usr/libx32", "/usr/local/lib",
	"/nix/store", "/gnu/store",
}

// Files every program may read: the rest of /usr, name resolution, CA
// certificates, time zones, users and groups, the dynamic linker's cache,
// and what the Go runtime reads about the machine. None holds secrets.
var linuxReads = []string{
	"/usr",
	"/etc/resolv.conf", "/etc/hosts", "/etc/host.conf", "/etc/nsswitch.conf", "/etc/gai.conf",
	"/etc/services", "/etc/protocols",
	"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/ca-certificates.conf", "/etc/crypto-policies",
	"/etc/localtime", "/etc/timezone",
	"/etc/passwd", "/etc/group",
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/mime.types", "/etc/os-release",
	"/proc/self",
	"/sys/devices/system/cpu", "/sys/fs/cgroup", "/sys/kernel/mm/transparent_hugepage",
	"/dev/random", "/dev/urandom", "/dev/zero",
}

// linuxWrites are devices every program may write.
var linuxWrites = []string{"/dev/null"}

func addSystem(r *rules) {
	r.Libraries = append(r.Libraries, linuxLibraries...)
	r.Read = append(r.Read, linuxReads...)
	// systemd-resolved and NetworkManager replace the file /etc/resolv.conf
	// links to, and a rule on the old file would not cover the new one.
	if target, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && target != "/etc/resolv.conf" {
		r.Read = append(r.Read, filepath.Dir(target))
	}
	r.Write = append(r.Write, linuxWrites...)
}

func check() Status {
	abi := landlockABI()
	_, seccompOK := seccompTable()
	if seccompOK {
		// EINVAL means a kernel built without seccomp.
		_, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
		seccompOK = err == nil
	}
	st := Status{Files: abi >= 1, Network: abi >= 4, Syscalls: seccompOK}
	var mech, missing []string
	switch {
	case abi >= 1:
		mech = append(mech, fmt.Sprintf("Landlock ABI %d", abi))
		if abi < 4 {
			missing = append(missing, fmt.Sprintf("network: Landlock ABI %d has no network rules (Linux 6.7 or later has them)", abi))
		}
	default:
		missing = append(missing, "files and network: Landlock is unavailable (it needs Linux 5.13 or later with Landlock turned on)")
	}
	if seccompOK {
		mech = append(mech, "seccomp")
	} else {
		missing = append(missing, "system calls: seccomp is unavailable on this kernel or architecture")
	}
	st.Mechanism = strings.Join(mech, " and ")
	st.Missing = strings.Join(missing, "; ")
	return st
}

// wrap runs bin through tug's own ExecCommand, which applies the rules to
// its process and then becomes the plugin.
func (s *Sandbox) wrap(c *Command, bin string, args []string, r rules) error {
	if s.Self == "" {
		return fmt.Errorf("starting plugin %s in the sandbox: tug does not know its own executable", s.Policy.Name)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding the sandbox policy for plugin %s: %w", s.Policy.Name, err)
	}
	c.Path = s.Self
	c.Args = append([]string{ExecCommand, bin}, args...)
	c.Env = setEnv(c.Env, policyEnv, string(b))
	return nil
}

// Exec confines this process with the rules tug passed in the environment
// and then executes the plugin named by args[0] with args, which inherits
// the restrictions. It is ExecCommand's whole job, and returns only when it
// fails.
func Exec(args []string) error {
	if len(args) == 0 {
		return errors.New("no plugin to run; tug runs this command itself when it starts a plugin")
	}
	raw, ok := os.LookupEnv(policyEnv)
	if !ok {
		return fmt.Errorf("%s is not set; tug sets it when it starts a plugin in the sandbox", policyEnv)
	}
	var r rules
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("reading the sandbox policy: %w", err)
	}
	env := withoutEnv(os.Environ(), policyEnv)

	// Landlock and seccomp restrict the thread that asks, and this thread
	// becomes the plugin when it executes it.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	if err := applyLandlock(r); err != nil {
		return fmt.Errorf("restricting files and network with Landlock: %w", err)
	}
	if err := applySeccomp(len(r.Sockets) > 0); err != nil {
		return fmt.Errorf("installing the seccomp filter: %w", err)
	}
	err := unix.Exec(args[0], args, env)
	if errors.Is(err, unix.EACCES) {
		return fmt.Errorf("running %s: %w; the sandbox allows only the plugin, its interpreter and its tools", args[0], err)
	}
	return fmt.Errorf("running %s: %w", args[0], err)
}
