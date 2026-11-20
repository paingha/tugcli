package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/session"
)

func newInitCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "init <zsh|bash|fish|powershell>",
		Short: "Print the shell hook that gives each terminal its own session",
		Long: `Print the shell hook. Add it to your shell's startup file:

  zsh         eval "$(tug init zsh)"                        # ~/.zshrc
  bash        eval "$(tug init bash)"                       # ~/.bashrc
  fish        tug init fish | source                        # config.fish
  powershell  Invoke-Expression (& tug init powershell | Out-String)

The hook exports KUBECONFIG for this terminal only and adds the cluster to
your prompt. Set TUG_PROMPT_DISABLE=1 to leave the prompt alone.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"zsh", "bash", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			sh, err := session.ParseShell(args[0])
			if err != nil {
				return err
			}
			script, err := session.Hook(sh, selfPath())
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(a.stdout, script)
			return err
		},
	}
}

// newSessionCmd holds the plumbing the shell hook calls.
func newSessionCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "session",
		Short:  "Session plumbing used by the shell hook",
		Hidden: true,
	}

	var shellName, parent string
	var pid int
	newCmd := &cobra.Command{
		Use:   "new",
		Short: "Create a session and print the variables to export",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sh, err := session.ParseShell(shellName)
			if err != nil {
				return err
			}
			mgr := a.sessions()
			if _, err := mgr.Sweep(a.cfg.Sessions.MaxAge.Duration); err != nil {
				a.log.Debug("sweeping sessions", "err", err)
			}
			s, err := mgr.Create(pid, parent)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(a.stdout, session.ExportScript(sh, sessionEnv(s, mgr, pid)))
			return err
		},
	}
	newCmd.Flags().StringVar(&shellName, "shell", "zsh", "shell syntax to print")
	newCmd.Flags().IntVar(&pid, "pid", os.Getppid(), "process ID of the shell that owns the session")
	newCmd.Flags().StringVar(&parent, "parent", "", "session to copy the connection from")

	var id string
	endCmd := &cobra.Command{
		Use:   "end",
		Short: "Remove a session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id == "" {
				id = os.Getenv(session.EnvSession)
			}
			if id == "" {
				return session.ErrNoSession
			}
			return a.sessions().Remove(id)
		},
	}
	endCmd.Flags().StringVar(&id, "id", "", "session ID (default $TUG_SESSION)")

	cmd.AddCommand(newCmd, endCmd, newSessionPinCmd(a))
	return cmd
}

// sessionEnv returns the variables that bind a shell to a session, in the
// order the hook exports them.
func sessionEnv(s *session.Session, mgr *session.Manager, pid int) [][2]string {
	orig := os.Getenv("KUBECONFIG")
	if within(orig, mgr.Root()) {
		// Started from a tug session; keep what that session would restore.
		orig = os.Getenv(session.EnvOrigKubeconfig)
	}
	return [][2]string{
		{session.EnvSession, s.State.ID},
		{session.EnvSessionPID, strconv.Itoa(pid)},
		{session.EnvSessionDir, s.Dir()},
		{session.EnvOrigKubeconfig, orig},
	}
}

// within reports whether path is inside dir.
func within(path, dir string) bool {
	if path == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// selfPath returns the absolute path that shell hooks and session kubeconfigs
// use to run tug. When `tug` on PATH is this binary, the PATH entry is used,
// since package managers keep it stable across upgrades.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "tug"
	}
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		realExe = exe
	}
	if onPath, err := exec.LookPath("tug"); err == nil {
		if abs, err := filepath.Abs(onPath); err == nil {
			if realOnPath, err := filepath.EvalSymlinks(abs); err == nil && realOnPath == realExe {
				return abs
			}
		}
	}
	return exe
}
