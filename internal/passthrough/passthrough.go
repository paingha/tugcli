// Package passthrough runs kubectl for commands tug does not implement, with
// the session's kubeconfig, and decides which kubectl commands only read.
package passthrough

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// kubectl global flags that take a separate value.
var valueFlags = []string{
	"-n", "--namespace", "--context", "--cluster", "--user", "-s", "--server",
	"--kubeconfig", "--token", "--as", "--as-group", "--as-uid", "--request-timeout",
	"--cache-dir", "--certificate-authority", "--client-certificate", "--client-key",
	"--tls-server-name", "-v", "--v", "--vmodule", "--log-file", "--profile", "--profile-output",
	"-o", "--output", "-l", "--selector", "-f", "--filename", "-c", "--container",
}

// readVerbs never change cluster state.
var readVerbs = []string{
	"get", "describe", "logs", "top", "explain", "api-resources", "api-versions",
	"version", "cluster-info", "port-forward", "events", "diff", "wait", "completion",
	"options", "help", "kustomize", "plugin",
}

// readSubcommands are read-only subcommands of otherwise mutating verbs.
var readSubcommands = map[string][]string{
	"auth":    {"can-i", "whoami"},
	"rollout": {"status", "history"},
	"config":  {"view", "get-contexts", "get-clusters", "get-users", "current-context"},
}

// Words returns the positional words of a kubectl command line, skipping
// flags and their values.
func Words(args []string) []string {
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && slices.Contains(valueFlags, a) {
				i++
			}
			continue
		}
		words = append(words, a)
	}
	return words
}

// IsRead reports whether a kubectl command line only reads. Unknown commands,
// including kubectl plugins, are treated as writes.
func IsRead(args []string) bool {
	if isDryRun(args) {
		return true
	}
	words := Words(args)
	if len(words) == 0 {
		return true // bare flags such as --help
	}
	verb := words[0]
	if slices.Contains(readVerbs, verb) {
		return true
	}
	if subs, ok := readSubcommands[verb]; ok && len(words) > 1 {
		return slices.Contains(subs, words[1])
	}
	return false
}

func isDryRun(args []string) bool {
	for _, a := range args {
		if a == "--dry-run=client" || a == "--dry-run=server" || a == "--dry-run" {
			return true
		}
	}
	return false
}

// LookPath finds kubectl, or says how to make it available.
func LookPath(kubectl string) (string, error) {
	path, err := exec.LookPath(kubectl)
	if err != nil {
		return "", fmt.Errorf("kubectl not found (%w); install it or set kubectl in config.yaml", err)
	}
	return path, nil
}

// Command returns the kubectl command to run with the session's kubeconfig.
func Command(kubectl string, args []string, kubeconfig string) (*exec.Cmd, error) {
	path, err := LookPath(kubectl)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Env = os.Environ()
	if kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// stopGrace is how long kubectl has to stop after it is interrupted before
// it is killed.
const stopGrace = 5 * time.Second

// CommandContext returns kubectl with a kubeconfig for a run that ends with
// ctx: cancelling ctx interrupts kubectl, as Ctrl-C would, and kills it if it
// has not stopped within a few seconds. It has no stdin, and the caller sets
// its stdout and stderr.
func CommandContext(ctx context.Context, kubectl string, args []string, kubeconfig string) (*exec.Cmd, error) {
	path, err := LookPath(kubectl)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Cancel = func() error {
		// Windows cannot deliver an interrupt to another process.
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = stopGrace
	return cmd, nil
}

// ExitCode returns the exit code carried by err from running a command, or 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	return 1
}
