//go:build !windows

package plugin

import (
	"os"
	"syscall"
)

// terminate asks a plugin to stop. exec.Cmd sends SIGKILL after WaitDelay.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
