package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

// Access is what a plugin may reach when tug runs it on this machine, and
// within which limits, as tug plugin inspect explains it.
type Access struct {
	// Secrets are the secrets tug hands it, and no others.
	Secrets []provider.SecretSpec
	// Env are the variables of tug's environment it gets besides a few
	// basics such as PATH and HOME.
	Env []string
	// Network lists the hosts it says it talks to. A WebAssembly build
	// may reach only these.
	Network []string
	// Read, Write and Sockets are the files, directories and Unix sockets
	// its manifest asks for beyond its own directory.
	Read, Write, Sockets []string
	// Tools are the programs it runs.
	Tools []Tool
	// Ports are the TCP ports it may connect to in the sandbox.
	Ports []uint16
	// Build is the build tug would run here; empty when none can.
	Build Platform
	// WASM is true when that build is WebAssembly, which tug runs in its
	// own sandbox.
	WASM bool
	// Confined is true when the operating system's sandbox would confine
	// a native build, and Sandbox says what it enforces.
	Confined bool
	Sandbox  sandbox.Status
	// Budget is its processor time, memory and timeouts.
	Budget Budget
	// Limits says which limits this machine enforces.
	Limits sandbox.LimitSupport
}

// Tool is a program a plugin runs, and where it is on this machine.
type Tool struct {
	Name string
	// Path is where it was found; empty when it was not.
	Path string
}

// Access works out what p may reach here within caps; confine says
// whether config.yaml leaves its native build in the sandbox.
func (p *Plugin) Access(caps Caps, confine bool) Access {
	m := p.Manifest
	s := m.Spec.Sandbox
	a := Access{
		Secrets: m.Spec.Secrets,
		Env:     m.Spec.Env,
		Network: m.Spec.Network,
		Read:    s.Read,
		Write:   s.Write,
		Sockets: s.Sockets,
		Ports:   append([]uint16{443}, slices.DeleteFunc(m.SandboxPorts(), func(p uint16) bool { return p == 443 })...),
		Budget:  m.Budget(caps),
		Limits:  sandbox.SupportedLimits(),
	}
	for _, t := range s.Tools {
		a.Tools = append(a.Tools, Tool{Name: t, Path: findTool(t)})
	}
	if p.Bin != "" {
		rel, err := filepath.Rel(p.Dir, p.Bin)
		if err == nil {
			for _, b := range m.Spec.Platforms {
				if filepath.FromSlash(b.Bin) == rel && (b.IsWASM() == p.WASM) {
					a.Build = b
					break
				}
			}
		}
		a.WASM = p.WASM
	}
	if !a.WASM {
		a.Sandbox = sandbox.Check()
		a.Confined = confine && a.Sandbox.Any()
	}
	return a
}

// findTool returns where a tool the manifest names is, or "".
func findTool(tool string) string {
	if strings.ContainsAny(tool, `/\`) {
		if rest, ok := strings.CutPrefix(tool, "~/"); ok {
			tool = "$HOME/" + rest
		}
		path := os.ExpandEnv(tool)
		if _, err := os.Stat(path); err == nil {
			return path
		}
		return ""
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		return ""
	}
	return path
}
