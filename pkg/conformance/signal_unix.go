//go:build !windows

package conformance

import (
	"os/exec"
	"syscall"
)

// newGroup starts cmd in its own process group so that killGroup also reaches
// any children it spawns.
func newGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// terminate sends SIGTERM to the plugin process only, as tug does.
func terminate(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }

// killGroup kills the plugin and everything it started.
func killGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
