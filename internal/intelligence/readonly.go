package intelligence

import (
	"slices"
	"strings"
)

// shellChars are what make a command more than one program with its
// arguments: pipes, lists, redirections and substitutions.
const shellChars = ";&|<>`$\\\n\r"

// kubectlValueFlags are the global flags that take a value as the next
// argument, so that the verb can be found after them.
var kubectlValueFlags = []string{
	"-n", "--namespace", "--context", "--cluster", "--user", "-s", "--server",
	"--kubeconfig", "--as", "--as-group", "--as-uid", "--request-timeout", "-v", "--v",
	"--cache-dir", "--certificate-authority", "--client-certificate", "--client-key",
	"--tls-server-name", "--profile", "--profile-output",
	// tug's own, which run a command on many clusters.
	"--cluster-selector", "--parallel",
}

// credentialFlags pass a credential on the command line, which a suggestion
// must never do.
var credentialFlags = []string{"--token", "--password", "--username", "--client-key"}

// kubectlReadVerbs are the kubectl commands that only read. Some only read
// with one of their subcommands.
var kubectlReadVerbs = map[string][]string{
	"get":           nil,
	"describe":      nil,
	"logs":          nil,
	"top":           nil,
	"explain":       nil,
	"events":        nil,
	"api-resources": nil,
	"api-versions":  nil,
	"version":       nil,
	"cluster-info":  nil,
	"wait":          nil,
	"auth":          {"can-i", "whoami"},
	"rollout":       {"status", "history"},
}

// tugReadCommands are tug's own commands that only read. Anything else tug
// does not have runs as kubectl, so it is judged as a kubectl command.
var tugReadCommands = []string{"why", "tree", "logs", "top", "status", "ls", "doctor", "version", "audit"}

// tugCommands are tug's other commands, which may change something; a
// suggestion of one is refused rather than taken for kubectl's.
var tugCommands = []string{
	"connect", "disconnect", "ns", "unlock", "lock", "shell", "sh", "pf", "restart", "scale",
	"rollback", "import", "sync", "label", "forget", "auth", "pin", "plugin", "index", "tunnel",
	"daemon", "ui", "init", "session", "search",
}

// Runnable reports whether a suggested command is one tug shows at all: a
// kubectl or tug command, which uses no shell syntax, passes no credential,
// holds no placeholder for a redacted value and does not print a Secret's
// data. It may still change something; ReadOnly says whether it only
// reads. When it is not, it says why.
func Runnable(command string) (ok bool, why string) {
	args, why := commandArgs(command)
	if why != "" {
		return false, why
	}
	if printsSecretData(args) {
		return false, "it would print a Secret's data"
	}
	return true, ""
}

// ReadOnly reports whether a suggested command only reads: a Runnable
// kubectl or tug command that cannot change a cluster, tug's state or a
// file. When it does not, it says why.
//
// It accepts only what it knows, so some harmless commands are refused too.
func ReadOnly(command string) (ok bool, why string) {
	if ok, why := Runnable(command); !ok {
		return false, why
	}
	args, _ := commandArgs(command)
	if args[0] == "tug" {
		return tugReadOnly(args[1:])
	}
	return kubectlReadOnly(args[1:])
}

// commandArgs splits a command into its words, or says why it is not one
// tug shows.
func commandArgs(command string) ([]string, string) {
	command = strings.TrimSpace(command)
	switch {
	case command == "":
		return nil, "it is empty"
	case strings.ContainsAny(command, shellChars):
		return nil, "it uses the shell"
	case HasPlaceholder(command):
		return nil, "it holds a placeholder for a redacted value"
	}
	args := strings.Fields(command)
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(credentialFlags, name) {
			return nil, "it passes a credential on the command line"
		}
	}
	if args[0] != "kubectl" && args[0] != "tug" {
		return nil, "tug keeps only kubectl and tug commands"
	}
	return args, ""
}

func tugReadOnly(args []string) (bool, string) {
	verb, rest := verbOf(args)
	switch {
	case verb == "":
		return false, "it names no command"
	case verb == "why" && slices.ContainsFunc(rest, func(a string) bool { return a == "--deep" || strings.HasPrefix(a, "--deep=") }):
		return false, "it would send data to a model provider again"
	case slices.Contains(tugReadCommands, verb):
		return true, ""
	case slices.Contains(tugCommands, verb):
		return false, "tug " + verb + " is not a command that only reads"
	default:
		return kubectlReadOnly(args)
	}
}

func kubectlReadOnly(args []string) (bool, string) {
	verb, rest := verbOf(args)
	if verb == "" {
		return false, "it names no command"
	}
	subs, ok := kubectlReadVerbs[verb]
	if !ok {
		return false, "kubectl " + verb + " is not a command that only reads"
	}
	if subs != nil {
		sub, _ := verbOf(rest)
		if !slices.Contains(subs, sub) {
			return false, strings.TrimSpace("kubectl "+verb+" "+sub) + " is not a command that only reads"
		}
	}
	return true, ""
}

// verbOf returns the first argument that is not a flag or a flag's value,
// and the arguments after it.
func verbOf(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
		if !strings.Contains(a, "=") && slices.Contains(kubectlValueFlags, a) {
			i++ // skip the flag's value
		}
	}
	return "", nil
}

// printsSecretData reports whether a command is a kubectl get, or tug's,
// that asks for Secrets in a form that shows their data: -o yaml, json,
// jsonpath, a template or custom columns, or --raw on a path under secrets.
func printsSecretData(command []string) bool {
	if verb, _ := verbOf(command[1:]); verb != "get" {
		return false
	}
	_, args := verbOf(command[1:])
	secrets, data := false, false
	for i, a := range args {
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case name == "--raw":
			if !hasVal && i+1 < len(args) {
				val = args[i+1]
			}
			if strings.Contains(val, "/secrets") {
				return true
			}
		case name == "-o" || name == "--output" || (strings.HasPrefix(a, "-o") && len(a) > 2):
			switch {
			case strings.HasPrefix(a, "-o") && name != "-o":
				val = strings.TrimPrefix(a, "-o")
			case !hasVal && i+1 < len(args):
				val = args[i+1]
			}
			format, _, _ := strings.Cut(val, "=")
			data = data || !slices.Contains([]string{"wide", "name", ""}, format)
		case !strings.HasPrefix(a, "-"):
			for _, r := range strings.Split(a, ",") {
				r, _, _ = strings.Cut(r, "/")
				if r == "secret" || r == "secrets" || r == "secret.v1" || strings.HasPrefix(r, "secrets.") {
					secrets = true
				}
			}
		}
	}
	return secrets && data
}
