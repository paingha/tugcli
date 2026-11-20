package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// errNotInteractive is returned when a prompt is needed but there is no
// terminal to ask on.
var errNotInteractive = errors.New("input needed but stdin is not a terminal")

// interactive reports whether we can prompt: stdin and stderr are terminals.
func (a *app) interactive() bool {
	in, ok := a.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return false
	}
	errf, ok := a.stderr.(*os.File)
	return ok && term.IsTerminal(int(errf.Fd()))
}

// readLine prompts on stderr and reads one line.
func (a *app) readLine(prompt string) (string, error) {
	fmt.Fprint(a.stderr, prompt)
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// confirm asks a yes/no question. Without a terminal it returns def.
func (a *app) confirm(question string, def bool) bool {
	if !a.interactive() {
		return def
	}
	hint := " [y/N] "
	if def {
		hint = " [Y/n] "
	}
	answer, err := a.readLine(question + hint)
	if err != nil {
		return false
	}
	switch strings.ToLower(answer) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

// pick asks the user to choose one item and returns its index.
func (a *app) pick(title string, items []string) (int, error) {
	if len(items) == 1 {
		return 0, nil
	}
	if !a.interactive() {
		return 0, fmt.Errorf("%s: %d matches (%s); be more specific", title, len(items), strings.Join(items, ", "))
	}
	fmt.Fprintln(a.stderr, title)
	for i, it := range items {
		fmt.Fprintf(a.stderr, "  %2d) %s\n", i+1, it)
	}
	for {
		answer, err := a.readLine("Choose [1-" + strconv.Itoa(len(items)) + "]: ")
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(answer)
		if err == nil && n >= 1 && n <= len(items) {
			return n - 1, nil
		}
		fmt.Fprintln(a.stderr, "Please enter a number from the list.")
	}
}

// pickMany asks for a comma-separated selection; empty means all.
func (a *app) pickMany(title string, items []string) ([]int, error) {
	if !a.interactive() {
		return nil, errNotInteractive
	}
	fmt.Fprintln(a.stderr, title)
	for i, it := range items {
		fmt.Fprintf(a.stderr, "  %2d) %s\n", i+1, it)
	}
	for {
		answer, err := a.readLine("Import which? [all, or numbers like 1,3; none to cancel]: ")
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(answer) {
		case "", "all", "a":
			all := make([]int, len(items))
			for i := range items {
				all[i] = i
			}
			return all, nil
		case "none", "n", "q":
			return nil, nil
		}
		var picked []int
		ok := true
		for _, f := range strings.Split(answer, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 || n > len(items) {
				ok = false
				break
			}
			picked = append(picked, n-1)
		}
		if ok {
			return picked, nil
		}
		fmt.Fprintln(a.stderr, "Please enter numbers from the list, separated by commas.")
	}
}

// readSecret prompts for a secret without echoing it.
func (a *app) readSecret(prompt string) (string, error) {
	in, ok := a.stdin.(*os.File)
	if !ok || !a.interactive() {
		return "", errNotInteractive
	}
	fmt.Fprint(a.stderr, prompt+": ")
	b, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(a.stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
