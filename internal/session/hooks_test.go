package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// hookRunners load a hook in a shell that is not interactive, draw the
// prompt once and print $TUG_PROMPT. TUG_SESSION_PID names the shell, so
// the hook takes the session as its own and makes none; the hook's tug is
// true(1), which ending the session on exit runs.
var hookRunners = map[Shell][]string{
	Zsh: {"zsh", "-f", "-c", `export TUG_SESSION_PID=$$; source "$TUG_TEST_HOOK"; _tug_sync; print -rn -- "$TUG_PROMPT"`},
	Bash: {"bash", "--norc", "--noprofile", "-c",
		`export TUG_SESSION_PID=$$; source "$TUG_TEST_HOOK"; _tug_sync; printf '%s' "$TUG_PROMPT"`},
	Fish: {"fish", "--no-config", "-c", `set -gx TUG_SESSION_PID $fish_pid; source $TUG_TEST_HOOK; _tug_sync; printf '%s' $TUG_PROMPT`},
	PowerShell: {"pwsh", "-NoProfile", "-NonInteractive", "-Command",
		`$env:TUG_SESSION_PID = "$PID"; . $env:TUG_TEST_HOOK; _TugSync; [Console]::Out.Write($global:TugPrompt)`},
}

// promptMarkup matches colour and terminal markup in a drawn prompt: ANSI
// sequences, bash's \001 and \002 around them, and zsh's prompt escapes.
var promptMarkup = regexp.MustCompile(`\x1b\[[0-9;]*m|\x1b\(B|[\x01\x02]|%[BbFfKk](\{[a-z]+\})?`)

func plainPrompt(s string) string {
	return strings.ReplaceAll(promptMarkup.ReplaceAllString(s, ""), "%%", "%")
}

// TestHookPrompts draws the prompt in every installed shell from prompt
// files as tug writes them, including an unlock that ends while no tug
// command runs, which the hook notices with the shell's own clock.
func TestHookPrompts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("true(1) stands in for tug; TestHooksRenderAndParse checks the PowerShell hook here")
	}
	now := time.Now().Unix()
	tests := []struct {
		name, file string
		want       string
		not        []string
	}{
		{"read-write", "normal\tdev-us:web\n0\tdev\t-\t-\n", "⎈ dev-us:web [dev]", []string{"read-only", "PROD"}},
		{"production read-only through tugd", "prod\tprod-eu:pay\n0\tprod\tPROD\tproxy\n", " PROD  ⎈ prod-eu:pay (read-only, proxy)", []string{"[prod]"}},
		{"production read-write", "normal\tprod-eu:pay\n0\tprod\tPROD\t-\n", " PROD  ⎈ prod-eu:pay", []string{"read-only"}},
		{"unlocked", fmt.Sprintf("unlocked\tprod-eu:pay\n%d\tprod\tPROD\t-\n", now+570), "⎈ prod-eu:pay (unlocked 10m)", nil},
		{"an unlock that has ended", fmt.Sprintf("unlocked\tprod-eu:pay\n%d\tprod\tPROD\t-\n", now-5), "⎈ prod-eu:pay (read-only)", []string{"unlocked"}},
		{"a prompt file from before the second line", "unlocked\tdev:web\n", "⎈ dev:web (unlocked)", []string{"[", "-"}},
		{"percent signs", "normal\tdev:50%\n0\t-\t-\t-\n", "⎈ dev:50%", nil},
	}
	for _, sh := range Shells() {
		t.Run(string(sh), func(t *testing.T) {
			argv := hookRunners[sh]
			bin, err := exec.LookPath(argv[0])
			if err != nil {
				t.Skipf("%s not installed", argv[0])
			}
			script, err := Hook(sh, "true")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			hook := filepath.Join(dir, "hook")
			if sh == PowerShell {
				hook += ".ps1"
			}
			sess := filepath.Join(dir, "session")
			if err := os.MkdirAll(sess, 0o700); err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{hook: script, filepath.Join(sess, "kubeconfig"): ""} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					if err := os.WriteFile(filepath.Join(sess, promptFileName), []byte(tt.file), 0o600); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command(bin, argv[1:]...)
					cmd.Dir = dir
					cmd.Env = append(os.Environ(), "TUG_TEST_HOOK="+hook, "TUG_SESSION_DIR="+sess,
						"TUG_SESSION=", "TUG_PIN_DISABLE=1", "TUG_PROMPT_DISABLE=1", "TERM=xterm-256color")
					out, err := cmd.Output()
					if err != nil {
						var stderr string
						if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
							stderr = string(ee.Stderr)
						}
						t.Fatalf("%s: %v\n%s%s", sh, err, out, stderr)
					}
					got := plainPrompt(string(out))
					if !strings.Contains(got, tt.want) {
						t.Fatalf("prompt %q (raw %q), want it to contain %q", got, out, tt.want)
					}
					for _, n := range tt.not {
						if strings.Contains(got, n) {
							t.Fatalf("prompt %q contains %q", got, n)
						}
					}
				})
			}
		})
	}
}
