//go:build !windows

package cli

import "syscall"

// replaceProcess replaces tug with another program, keeping the PID, so the
// program owns the terminal and its exit code is tug's.
func (a *app) replaceProcess(path string, args, env []string) error {
	return syscall.Exec(path, append([]string{path}, args...), env)
}

func defaultShell() string { return "/bin/sh" }

// defaultEditor is the editor tug profile edit runs when neither $VISUAL
// nor $EDITOR names one.
func defaultEditor() string { return "vi" }
