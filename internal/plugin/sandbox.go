package plugin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

// SandboxPolicy returns what the plugin may reach in tug's sandbox: its own
// directory, plus what spec.sandbox and spec.network declare.
func (p *Plugin) SandboxPolicy() sandbox.Policy {
	s := p.Manifest.Spec.Sandbox
	return sandbox.Policy{
		Name:    p.Name(),
		Dir:     p.Dir,
		Tools:   s.Tools,
		Read:    s.Read,
		Write:   s.Write,
		Sockets: s.Sockets,
		Ports:   p.Manifest.SandboxPorts(),
	}
}

// Command returns how to start the plugin with args: in its sandbox when
// the runner has one, or as it is. Call Cleanup on the result once every
// process started from it has exited.
func (r *Runner) Command(args ...string) (*sandbox.Command, error) {
	if r.WASM != nil {
		return nil, provider.Errorf(provider.Internal, "plugin %s is a WebAssembly module, which runs in tug rather than as a process", r.Name)
	}
	env := r.Environ()
	c := &sandbox.Command{Path: r.Bin, Args: args, Env: env}
	if r.Sandbox != nil {
		var err error
		if c, err = r.Sandbox.Command(r.Bin, args, env); err != nil {
			return nil, provider.Errorf(provider.Internal, "plugin %s: %v", r.Name, err).WithHint(r.sandboxHint())
		}
	}
	self := r.Self
	if self == "" && r.Sandbox != nil {
		self = r.Sandbox.Self
	}
	if err := c.Limit(self, r.Limits); err != nil {
		c.Cleanup()
		return nil, provider.Errorf(provider.Internal, "plugin %s: %v", r.Name, err)
	}
	return c, nil
}

// sandboxed reports whether the runner's plugin processes run in a sandbox
// that enforces something on this machine. A WebAssembly plugin has no
// process to sandbox.
func (r *Runner) sandboxed() bool {
	return r.Sandbox != nil && r.WASM == nil && sandbox.Check().Any()
}

// sandboxHint tells the user how to run a plugin that the sandbox stopped.
func (r *Runner) sandboxHint() string {
	return fmt.Sprintf("tug's sandbox may have stopped %s; to run it without one, add %s to plugins.sandbox.exclude in config.yaml", r.Name, r.Name)
}

// deniedPhrases are how the sandbox's refusals read in error messages:
// Landlock refuses with EACCES, seccomp and macOS with EPERM, and macOS's
// loader names the sandbox.
var deniedPhrases = []string{"permission denied", "operation not permitted", "sandbox"}

// explain adds the sandbox hint to a sandboxed plugin's error when the
// error reads like the sandbox refused something. A hint the plugin gave
// is kept, since the refusal may have come from an API instead.
func (r *Runner) explain(err error) error {
	var pe *provider.Error
	if err == nil || !r.sandboxed() || !errors.As(err, &pe) {
		return err
	}
	msg := strings.ToLower(pe.Message)
	for _, phrase := range deniedPhrases {
		if strings.Contains(msg, phrase) {
			return r.withSandboxHint(pe)
		}
	}
	return err
}

// withSandboxHint adds the sandbox hint to pe when the plugin is sandboxed.
func (r *Runner) withSandboxHint(pe *provider.Error) *provider.Error {
	if !r.sandboxed() {
		return pe
	}
	if pe.Hint == "" {
		return pe.WithHint(r.sandboxHint())
	}
	return pe.WithHint(pe.Hint + "; or " + r.sandboxHint())
}
