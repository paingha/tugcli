// Package editor runs the user's own editor, named by $VISUAL or $EDITOR,
// on a file. It knows how common editors take a line to start at, and opens
// the file at its top in any other.
package editor

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrNoEditor is returned when neither $VISUAL nor $EDITOR names one.
var ErrNoEditor = errors.New("no editor; set $VISUAL or $EDITOR")

// Editors by how they take a line: before the file as +line, after it as
// file:line, or with --goto file:line.
var (
	plusLine  = []string{"vi", "vim", "nvim", "gvim", "mvim", "view", "nano", "pico", "emacs", "emacsclient", "micro", "kak", "joe", "mg", "ne", "jed"}
	colonLine = []string{"hx", "helix", "subl", "sublime_text", "zed", "mate"}
	gotoLine  = []string{"code", "code-insiders", "codium", "cursor", "windsurf"}
)

// Args returns the command line that opens path at line with an editor
// given as $VISUAL or $EDITOR would give it: a command and perhaps flags,
// such as "code --wait". A line below 1 opens the file at its top.
func Args(editor, path string, line int) []string {
	words := strings.Fields(editor)
	if len(words) == 0 {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(words[0])), ".exe")
	l := strconv.Itoa(line)
	switch {
	case line < 1:
		return append(words, path)
	case slices.Contains(plusLine, name):
		return append(words, "+"+l, path)
	case slices.Contains(colonLine, name):
		return append(words, path+":"+l)
	case slices.Contains(gotoLine, name):
		return append(words, "--goto", path+":"+l)
	default:
		return append(words, path)
	}
}

// Command returns the command that opens path at line in the user's
// editor, $VISUAL before $EDITOR, as getenv reads them. The caller connects
// its streams to the terminal.
func Command(ctx context.Context, getenv func(string) string, path string, line int) (*exec.Cmd, error) {
	ed := strings.TrimSpace(getenv("VISUAL"))
	if ed == "" {
		ed = strings.TrimSpace(getenv("EDITOR"))
	}
	if ed == "" {
		return nil, ErrNoEditor
	}
	args := Args(ed, path, line)
	return exec.CommandContext(ctx, args[0], args[1:]...), nil
}
