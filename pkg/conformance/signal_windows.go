//go:build windows

package conformance

import "os/exec"

func newGroup(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) error { return cmd.Process.Kill() }

func killGroup(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
