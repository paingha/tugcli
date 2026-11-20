// Package browser opens addresses in the user's web browser.
package browser

import (
	"context"
	"fmt"
	"os/exec"
)

// Env names a command that opens addresses instead of the system's
// browser, as xdg-open and many other tools honour it. tug runs it with the
// address as its only argument, which also lets scripts and tests stand in
// for a browser.
const Env = "BROWSER"

// Open starts a browser on url and does not wait for it to close. getenv
// reads Env.
func Open(ctx context.Context, url string, getenv func(string) string) error {
	name, args := command(url)
	if b := getenv(Env); b != "" {
		name, args = b, []string{url}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	// Some openers stay until the browser closes; ctx ending stops them.
	go func() { _ = cmd.Wait() }()
	return nil
}
