//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"runtime"
)

func addSystem(*rules) {}

func check() Status {
	return Status{Missing: "everything: " + runtime.GOOS + " has no sandbox for plugins; they rely on the process boundary and the scrubbed environment"}
}

// wrap is never called, since check reports no sandbox.
func (s *Sandbox) wrap(c *Command, bin string, args []string, _ rules) error {
	c.Path, c.Args = bin, args
	return nil
}

// Exec is only used on Linux.
func Exec([]string) error {
	return errors.New(ExecCommand + " is only used on Linux; plugins on " + runtime.GOOS + " run without a sandbox")
}
