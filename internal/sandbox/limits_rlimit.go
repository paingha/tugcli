//go:build linux || darwin

package sandbox

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
)

var limitSupport = func() LimitSupport {
	if runtime.GOOS == "linux" {
		return LimitSupport{CPU: true, Memory: true}
	}
	return LimitSupport{CPU: true, Missing: "memory: " + runtime.GOOS + " does not let a process lower its memory limit"}
}()

// applyLimits sets this process's resource limits, which the program it
// executes next inherits. Soft and hard limits are the same, so the
// program cannot raise them.
func applyLimits(l Limits) error {
	if l.CPU > 0 {
		secs := uint64(max(1, (l.CPU+time.Second-1)/time.Second))
		if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: secs, Max: secs}); err != nil {
			return fmt.Errorf("limiting processor time to %ds: %w", secs, err)
		}
	}
	if l.Memory > 0 && limitSupport.Memory {
		if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: l.Memory, Max: l.Memory}); err != nil {
			return fmt.Errorf("limiting memory to %d MiB: %w", l.Memory>>20, err)
		}
	}
	return nil
}
