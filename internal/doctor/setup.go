package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/catalog"
)

// SessionInfo describes the terminal's tug session.
type SessionInfo struct {
	// ID is $TUG_SESSION; "" when the shell hook is not installed.
	ID string
	// Err is the error loading the session, if any.
	Err error
	// Cluster and ReadOnly describe a connected session.
	Cluster  string
	ReadOnly bool
	// Kubeconfig is the session kubeconfig; Env is $KUBECONFIG.
	Kubeconfig string
	Env        string
	// Shell names the shell for the fix, for example "zsh".
	Shell string
}

func checkSession(s SessionInfo) Result {
	const name = "shell session"
	shell := s.Shell
	if shell == "" {
		shell = "zsh"
	}
	switch {
	case s.ID == "":
		return setup(name, Warn, "this terminal has no tug session, so kubectl does not follow tug connect",
			fmt.Sprintf("add `eval \"$(tug init %s)\"` to your shell's startup file, or run `tug shell`", shell))
	case s.Err != nil:
		return setup(name, Warn, s.Err.Error(), "open a new terminal so the shell hook starts a fresh session")
	case s.Cluster == "":
		return setup(name, OK, "session "+s.ID+", not connected", "")
	case s.Env != s.Kubeconfig:
		return setup(name, Warn, "connected to "+s.Cluster+", but KUBECONFIG points elsewhere, so kubectl and helm do not follow it",
			"run any tug command so the hook resets KUBECONFIG, or open a new terminal")
	}
	mode := "read-write"
	if s.ReadOnly {
		mode = "read-only"
	}
	return setup(name, OK, fmt.Sprintf("session %s, connected to %s (%s)", s.ID, s.Cluster, mode), "")
}

// StoreInfo describes the credential store.
type StoreInfo struct {
	Name string
	Err  error
}

func checkStore(s StoreInfo) Result {
	const name = "credential store"
	if s.Err != nil {
		return setup(name, Fail, s.Err.Error(),
			"use the OS keychain, or set TUG_KEYRING_BACKEND=file with TUG_KEYRING_PASSPHRASE for an encrypted file")
	}
	return setup(name, OK, s.Name, "")
}

// KubectlInfo locates kubectl.
type KubectlInfo struct {
	// Path is the kubectl found on PATH; "" when there is none.
	Path string
	// Version returns the client's git version, for example "v1.35.1".
	Version func(ctx context.Context) (string, error)
}

func checkKubectl(ctx context.Context, k KubectlInfo) (Result, string) {
	const name = "kubectl"
	if k.Path == "" {
		return setup(name, Warn, "kubectl is not on PATH; tug's own commands work, but commands it passes to kubectl do not",
			"install kubectl from https://kubernetes.io/docs/tasks/tools/"), ""
	}
	if k.Version == nil {
		return setup(name, OK, k.Path, ""), ""
	}
	v, err := k.Version(ctx)
	if err != nil {
		return setup(name, Warn, fmt.Sprintf("%s: reading its version: %v", k.Path, err), ""), ""
	}
	return setup(name, OK, v+" at "+k.Path, ""), v
}

// KubectlVersion runs `kubectl version --client` and returns the client's
// git version.
func KubectlVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version", "--client", "-o", "json").Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		line, _, _ := strings.Cut(strings.TrimSpace(string(exitErr.Stderr)), "\n")
		return "", errors.New(line)
	}
	if err != nil {
		return "", err
	}
	var v struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("decoding kubectl version: %w", err)
	}
	return v.ClientVersion.GitVersion, nil
}

// KubercInfo describes kubectl's preferences file, whose credential plugin
// policy (Kubernetes 1.35+) can stop kubectl running `tug auth token`.
type KubercInfo struct {
	// Path of the kuberc file; Disabled when KUBERC=off or KUBECTL_KUBERC=false.
	Path     string
	Disabled bool
	// Explicit is true when $KUBERC names the file. kubectl refuses to run
	// when an explicit kuberc cannot be read.
	Explicit bool
	// Data is the file's contents; ReadErr is set when it could not be read.
	// A missing file has neither.
	Data    []byte
	ReadErr error
	// TugCommand is the command session kubeconfigs run for credentials.
	TugCommand string
	// LookPath resolves bare command names; defaults to exec.LookPath.
	LookPath func(string) (string, error)
}

type kuberc struct {
	APIVersion       string `json:"apiVersion"`
	Kind             string `json:"kind"`
	CredentialPolicy string `json:"credentialPluginPolicy"`
	Allowlist        []struct {
		Command string `json:"command"`
		Name    string `json:"name"` // the 1.35 spelling, deprecated in 1.36
	} `json:"credentialPluginAllowlist"`
}

func checkKuberc(k KubercInfo) Result {
	const name = "kubectl credential plugins"
	switch {
	case k.Disabled:
		return setup(name, OK, "kuberc is turned off", "")
	case k.ReadErr != nil && k.Explicit:
		return setup(name, Fail, fmt.Sprintf("KUBERC names %s, which cannot be read (%v), so kubectl refuses to run", k.Path, k.ReadErr),
			"create the file, or unset KUBERC")
	case k.ReadErr != nil:
		return setup(name, Warn, fmt.Sprintf("reading %s: %v", k.Path, k.ReadErr), "")
	case len(k.Data) == 0:
		return setup(name, OK, "no kuberc, so kubectl runs any credential plugin", "")
	}
	var rc kuberc
	if err := yaml.Unmarshal(k.Data, &rc); err != nil {
		return setup(name, Warn, fmt.Sprintf("%s is not valid YAML: %v", k.Path, err), "")
	}
	fix := fmt.Sprintf("add `- command: %s` under credentialPluginAllowlist in %s", k.TugCommand, k.Path)
	switch rc.CredentialPolicy {
	case "", "AllowAll":
		return setup(name, OK, "kuberc allows every credential plugin", "")
	case "DenyAll":
		return setup(name, Fail, "kuberc denies every credential plugin, so kubectl cannot run `tug auth token`",
			fmt.Sprintf("set credentialPluginPolicy: Allowlist in %s, then %s", k.Path, strings.TrimPrefix(fix, "add ")))
	case "Allowlist":
		lookPath := k.LookPath
		if lookPath == nil {
			lookPath = exec.LookPath
		}
		for _, e := range rc.Allowlist {
			entry := e.Command
			if entry == "" {
				entry = e.Name
			}
			if pluginAllowed(entry, k.TugCommand, lookPath) {
				return setup(name, OK, "kuberc allows "+entry, "")
			}
		}
		return setup(name, Fail, "kuberc's credential plugin allowlist does not include tug, so kubectl cannot get credentials", fix)
	default:
		return setup(name, Warn, fmt.Sprintf("kuberc has an unknown credentialPluginPolicy %q", rc.CredentialPolicy), "")
	}
}

// pluginAllowed applies kubectl's allowlist rule: an entry matches when it is
// the same string as the plugin's command, or when both resolve to the same
// full path. Symlinks are not followed.
func pluginAllowed(entry, command string, lookPath func(string) (string, error)) bool {
	if entry == "" {
		return false
	}
	if entry == command {
		return true
	}
	resolve := func(s string) string {
		if filepath.IsAbs(s) {
			return filepath.Clean(s)
		}
		p, err := lookPath(s)
		if err != nil {
			return ""
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	a, b := resolve(entry), resolve(command)
	return a != "" && a == b
}

func checkCatalog(c *catalog.Catalog) []Result {
	const name = "catalog"
	if len(c.Clusters) == 0 {
		return []Result{setup(name, Warn, "no clusters imported", "run `tug import`")}
	}
	stale := 0
	for _, cl := range c.Clusters {
		if cl.Stale {
			stale++
		}
	}
	detail := fmt.Sprintf("%d clusters from %d sources", len(c.Clusters), len(c.Sources))
	out := []Result{setup(name, OK, detail, "")}
	if stale > 0 {
		out[0] = setup(name, Warn, fmt.Sprintf("%s; %d no longer listed by their provider", detail, stale),
			"`tug ls` marks them stale; `tug forget <provider>/<account>` removes a source you no longer use")
	}
	for _, s := range c.Sources {
		if s.LastError != "" {
			out = append(out, setup("source "+s.Key(), Warn, "last sync failed: "+s.LastError, "fix the cause, then run `tug sync`"))
		}
	}
	return out
}
