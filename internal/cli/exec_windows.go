//go:build windows

package cli

import (
	"os"
	"os/exec"

	"github.com/paingha/tugcli/internal/passthrough"
)

// replaceProcess runs another program in the foreground and exits with its
// code. Windows cannot replace a running process.
func (a *app) replaceProcess(path string, args, env []string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return &exitError{code: passthrough.ExitCode(err)}
	}
	return nil
}

func defaultShell() string { return "powershell.exe" }

// defaultEditor is the editor tug profile edit runs when neither $VISUAL
// nor $EDITOR names one.
func defaultEditor() string { return "notepad.exe" }
