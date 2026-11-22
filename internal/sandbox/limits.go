package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// limitsEnv carries a plugin's limits from tug to ExecCommand, which
// removes it before it runs the plugin.
const limitsEnv = "TUG_PLUGIN_LIMITS"

// Limits caps what each plugin process may use. A zero field is no limit.
// The limits hold for the life of the process, which for a plugin in serve
// mode spans many calls, and are inherited by the programs it runs, each
// of which gets the same limits of its own.
type Limits struct {
	// CPU is the processor time the process may use. On Linux the kernel
	// kills a process that uses it all. On macOS it sends SIGXCPU, which
	// ends a process that does not catch it, and plugins built on tug's
	// SDK stop on it too.
	CPU time.Duration `json:"cpu,omitempty"`
	// Memory is the data memory, in bytes, the process may map: its heap,
	// in effect. Only Linux enforces it; macOS does not let a process
	// lower it.
	Memory uint64 `json:"memory,omitempty"`
}

// IsZero reports whether l limits nothing.
func (l Limits) IsZero() bool { return l.CPU <= 0 && l.Memory == 0 }

// LimitSupport says which limits this machine enforces.
type LimitSupport struct {
	CPU, Memory bool
	// Missing says what is not enforced and why; empty when nothing is
	// missing.
	Missing string
}

// SupportedLimits reports which limits plugins get on this machine.
func SupportedLimits() LimitSupport { return limitSupport }

// Limit makes c apply l to the process it starts, through tug's own
// ExecCommand, which self names. A command that already starts through it,
// as a sandboxed plugin does on Linux, only gains the limits. It does
// nothing for a zero l, or where limits are not supported.
func (c *Command) Limit(self string, l Limits) error {
	if l.IsZero() || (!limitSupport.CPU && !limitSupport.Memory) {
		return nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if len(c.Args) == 0 || c.Path != self || c.Args[0] != ExecCommand {
		if self == "" {
			return fmt.Errorf("limiting the plugin: tug does not know its own executable")
		}
		c.Args = append([]string{ExecCommand, c.Path}, c.Args...)
		c.Path = self
	}
	c.Env = setEnv(c.Env, limitsEnv, string(b))
	return nil
}

// takeLimits reads and removes the limits tug passed in the environment.
func takeLimits() (Limits, bool, error) {
	raw, ok := os.LookupEnv(limitsEnv)
	if !ok {
		return Limits{}, false, nil
	}
	var l Limits
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return Limits{}, false, fmt.Errorf("reading the plugin's limits: %w", err)
	}
	return l, true, nil
}
