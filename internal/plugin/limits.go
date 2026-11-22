package plugin

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

// DefaultMaxTimeout is the longest call a manifest may ask for when
// plugins.limits.timeout in config.yaml does not say.
const DefaultMaxTimeout = 5 * time.Minute

// Limits is spec.limits: what a plugin says it needs, which config.yaml
// may cap. Each process the plugin runs as, and each program it starts,
// gets the CPU and memory limits on its own.
type Limits struct {
	// CPUSeconds is the processor time each plugin process may use. For a
	// plugin in serve mode, one process answers many calls, so it is the
	// time for all of them.
	CPUSeconds int `json:"cpuSeconds,omitempty"`
	// MemoryMiB is the memory each plugin process may map, enforced on
	// Linux, or that its WebAssembly build may use.
	MemoryMiB int `json:"memoryMiB,omitempty"`
	// Timeouts replaces tug's timeout for an operation, such as
	// {list: 2m} for a provider whose API is slow to list.
	Timeouts map[string]config.Duration `json:"timeouts,omitempty"`
}

// timedOps are the operations a timeout can be set for.
var timedOps = []string{provider.OpDescribe, provider.OpAccounts, provider.OpList, provider.OpCredential, provider.OpRotate, provider.OpCheckAccess}

func (l Limits) validate() []error {
	var errs []error
	if l.CPUSeconds < 0 {
		errs = append(errs, fmt.Errorf("spec.limits.cpuSeconds %d must be positive", l.CPUSeconds))
	}
	if l.MemoryMiB < 0 {
		errs = append(errs, fmt.Errorf("spec.limits.memoryMiB %d must be positive", l.MemoryMiB))
	}
	for _, op := range slices.Sorted(maps.Keys(l.Timeouts)) {
		switch {
		case !slices.Contains(timedOps, op):
			errs = append(errs, fmt.Errorf("spec.limits.timeouts.%s is not an operation; use %s", op, strings.Join(timedOps, ", ")))
		case l.Timeouts[op].Duration <= 0:
			errs = append(errs, fmt.Errorf("spec.limits.timeouts.%s must be a positive duration such as 30s", op))
		}
	}
	return errs
}

// Caps are what config.yaml allows any plugin, whatever its manifest asks.
type Caps struct {
	// CPU and Memory are plugins.limits; zero leaves them to the manifest.
	CPU    time.Duration
	Memory uint64
	// Timeout is plugins.limits.timeout, which caps every call; zero caps
	// only what manifests ask for, at DefaultMaxTimeout.
	Timeout time.Duration
	// Timeouts are plugins.timeouts: the timeout for an operation, for
	// every plugin, which wins over what a manifest asks.
	Timeouts map[string]time.Duration
	// WASMMemory is plugins.wasm.memoryLimitMiB in bytes; zero means
	// DefaultWASMMemoryLimit.
	WASMMemory uint64
}

// Budget is what one plugin gets: its manifest's limits within the caps.
type Budget struct {
	// Process limits each plugin process.
	Process sandbox.Limits
	// WASMMemory caps the plugin's WebAssembly build.
	WASMMemory uint64
	// Timeouts holds the timeout for every operation.
	Timeouts map[string]time.Duration
	// Capped says what the manifest asked for that the caps cut, such as
	// "cpuSeconds 600, capped at 60".
	Capped []string
}

// Budget works out what the plugin gets within c.
func (m *Manifest) Budget(c Caps) Budget {
	l := m.Spec.Limits
	b := Budget{Timeouts: map[string]time.Duration{}}

	b.Process.CPU = time.Duration(l.CPUSeconds) * time.Second
	if c.CPU > 0 && (b.Process.CPU == 0 || b.Process.CPU > c.CPU) {
		if b.Process.CPU > 0 {
			b.Capped = append(b.Capped, fmt.Sprintf("cpuSeconds %d, capped at %d", l.CPUSeconds, int(c.CPU/time.Second)))
		}
		b.Process.CPU = c.CPU
	}
	asked := uint64(l.MemoryMiB) << 20
	b.Process.Memory = asked
	if c.Memory > 0 && (asked == 0 || asked > c.Memory) {
		if asked > 0 {
			b.Capped = append(b.Capped, fmt.Sprintf("memoryMiB %d, capped at %d", l.MemoryMiB, c.Memory>>20))
		}
		b.Process.Memory = c.Memory
	}
	b.WASMMemory = c.WASMMemory
	if b.WASMMemory == 0 {
		b.WASMMemory = DefaultWASMMemoryLimit
	}
	if m.HasWASM() && asked > b.WASMMemory {
		b.Capped = append(b.Capped, fmt.Sprintf("memoryMiB %d for WebAssembly, capped at %d", l.MemoryMiB, b.WASMMemory>>20))
	}
	if asked > 0 {
		b.WASMMemory = min(b.WASMMemory, asked)
	}
	if c.Memory > 0 {
		b.WASMMemory = min(b.WASMMemory, c.Memory)
	}

	maxAsk := c.Timeout
	if maxAsk <= 0 {
		maxAsk = DefaultMaxTimeout
	}
	for _, op := range timedOps {
		d := defaultTimeout(op)
		if t := l.Timeouts[op].Duration; t > 0 {
			if t > maxAsk {
				b.Capped = append(b.Capped, fmt.Sprintf("timeouts.%s %s, capped at %s", op, t, maxAsk))
				t = maxAsk
			}
			d = t
		}
		if t := c.Timeouts[op]; t > 0 {
			d = t
		}
		if c.Timeout > 0 {
			d = min(d, c.Timeout)
		}
		b.Timeouts[op] = d
	}
	return b
}

// HasWASM reports whether the manifest lists a WebAssembly build.
func (m *Manifest) HasWASM() bool { return slices.ContainsFunc(m.Spec.Platforms, Platform.IsWASM) }

// defaultTimeout is tug's own timeout for op.
func defaultTimeout(op string) time.Duration {
	switch op {
	case provider.OpList:
		return DefaultListTimeout
	case provider.OpCredential:
		return DefaultCredentialTimeout
	case provider.OpRotate:
		return DefaultRotateTimeout
	default:
		return DefaultTimeout
	}
}

// limitHint tells the user what the plugin's limits were, for a plugin
// that stopped without answering while it had some.
func (r *Runner) limitHint() string {
	var parts []string
	if r.Limits.CPU > 0 {
		parts = append(parts, fmt.Sprintf("CPU time %s", r.Limits.CPU))
	}
	if r.Limits.Memory > 0 && sandbox.SupportedLimits().Memory {
		parts = append(parts, fmt.Sprintf("memory %d MiB", r.Limits.Memory>>20))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("%s may have used up its limits (%s); spec.limits in its plugin.yaml asks for them, and plugins.limits in config.yaml caps them",
		r.Name, strings.Join(parts, ", "))
}

// withLimitHint adds the limit hint to pe when the runner has limits.
func (r *Runner) withLimitHint(pe *provider.Error) *provider.Error {
	h := r.limitHint()
	switch {
	case h == "":
		return pe
	case pe.Hint == "":
		return pe.WithHint(h)
	default:
		return pe.WithHint(pe.Hint + "; or " + h)
	}
}
