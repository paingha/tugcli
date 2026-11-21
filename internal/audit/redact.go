package audit

import (
	"slices"
	"strings"
)

// Redacted stands for a value RedactCommand leaves out.
const Redacted = "***"

// keptFlags take a value that says where or how a command acts and is never
// a secret, so RedactCommand keeps it.
var keptFlags = []string{
	"-n", "--namespace", "--context", "--cluster", "-l", "--selector", "--field-selector",
	"-f", "--filename", "-k", "--kustomize", "-c", "--container", "-o", "--output",
	"--replicas", "--to-revision", "--grace-period", "--timeout", "--cascade", "--dry-run",
	"--type", "--image", "--port", "--target-port", "--subresource", "--field-manager",
}

// boolFlags take no value, so the word after one is not a value.
var boolFlags = []string{
	"-A", "--all-namespaces", "--all", "--force", "--now", "--wait", "-R", "--recursive",
	"--overwrite", "--ignore-not-found", "-w", "--watch", "--server-side", "--force-conflicts",
	"--prune", "--local", "-i", "--stdin", "-t", "--tty", "-q", "--quiet", "--rm",
}

// writeVerbs are the kubectl commands whose other words name what they act
// on. The words of anything else, such as a kubectl plugin, are not known,
// so only its name is kept.
var writeVerbs = []string{
	"create", "apply", "delete", "replace", "patch", "edit", "label", "annotate", "scale",
	"autoscale", "rollout", "set", "expose", "run", "cordon", "uncordon", "drain", "taint",
	"exec", "attach", "cp", "debug", "certificate", "auth", "config", "proxy",
}

// RedactCommand returns a kubectl command line with every value that could
// be a secret replaced by Redacted: flag values other than keptFlags',
// the value of each key=value word (as in kubectl set env), the word after a
// flag that may have taken it as its value, everything after "--" (the
// command an exec runs) and, for kubectl config, which edits credentials,
// everything after the subcommand. Only the command's name is kept for a
// command kubectl does not have.
func RedactCommand(args []string) []string {
	out := make([]string, 0, len(args))
	var words int // positional words so far
	verb := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			out = append(out, arg)
			if i+1 < len(args) {
				out = append(out, Redacted)
			}
			return out
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			flag, takesNext := redactFlag(arg)
			out = append(out, flag)
			if takesNext != nil && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out = append(out, takesNext(args[i]))
			}
		default:
			words++
			switch {
			case words == 1:
				verb = arg
				out = append(out, arg)
			case !slices.Contains(writeVerbs, verb), verb == "config" && words > 2:
				out = append(out, Redacted)
			default:
				out = append(out, redactWord(arg))
			}
		}
	}
	return out
}

// redactFlag returns a flag as it is recorded and, when the next word may
// be its value, how that word is recorded.
func redactFlag(arg string) (string, func(string) string) {
	name, _, hasValue := strings.Cut(arg, "=")
	keep := func(s string) string { return s }
	hide := func(string) string { return Redacted }
	switch {
	case hasValue && (slices.Contains(keptFlags, name) || slices.Contains(boolFlags, name)):
		return arg, nil
	case hasValue:
		return name + "=" + Redacted, nil
	case slices.Contains(keptFlags, name):
		return arg, keep
	case slices.Contains(boolFlags, name):
		return arg, nil
	case !strings.HasPrefix(name, "--") && len(name) > 2:
		return redactShortFlags(name)
	default:
		// A flag tug does not know may take the next word as its value.
		return name, hide
	}
}

// redactShortFlags handles a single dash with several letters: bool flags
// run together, as in -it, or a short flag with its value attached, as in
// -nweb or -p'{"data":...}'.
func redactShortFlags(arg string) (string, func(string) string) {
	letters := arg[1:]
	if !strings.ContainsFunc(letters, func(r rune) bool { return !slices.Contains(boolFlags, "-"+string(r)) }) {
		return arg, nil
	}
	if slices.Contains(keptFlags, arg[:2]) {
		return arg, nil
	}
	return arg[:2] + Redacted, nil
}

// redactWord keeps a word unless it is key=value, whose value it hides.
func redactWord(w string) string {
	if k, _, ok := strings.Cut(w, "="); ok {
		return k + "=" + Redacted
	}
	return w
}

// CommandNamespace returns the namespace a kubectl command line acts in:
// the value of its -n or --namespace flag, "" when it acts in every
// namespace (-A), or def, the session's namespace, when it names none.
func CommandNamespace(args []string, def string) string {
	ns := def
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return ns
		case arg == "-A", arg == "--all-namespaces":
			ns = ""
		case arg == "-n", arg == "--namespace":
			if i+1 < len(args) {
				i++
				ns = args[i]
			}
		case strings.HasPrefix(arg, "--namespace="):
			ns = strings.TrimPrefix(arg, "--namespace=")
		case strings.HasPrefix(arg, "-n") && !strings.HasPrefix(arg, "--"):
			ns = strings.TrimPrefix(strings.TrimPrefix(arg, "-n"), "=")
		}
	}
	return ns
}
