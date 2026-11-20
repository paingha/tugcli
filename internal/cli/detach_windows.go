//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// Process creation flags from the Windows API.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detach starts a command without a console, so it outlives the terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
