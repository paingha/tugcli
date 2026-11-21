// Command fetchroot writes the public-good Sigstore trusted root, fetched
// through TUF from the root of trust built into sigstore-go, to the file
// named by its argument. go generate runs it to refresh the copy built into
// tug; review the diff, since it changes whom tug trusts.
package main

import (
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/tuf"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fetchroot <trusted_root.json>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "fetchroot: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	c, err := tuf.New(tuf.DefaultOptions().WithDisableLocalCache())
	if err != nil {
		return fmt.Errorf("updating from %s: %w", tuf.DefaultMirror, err)
	}
	b, err := c.GetTarget("trusted_root.json")
	if err != nil {
		return fmt.Errorf("fetching trusted_root.json: %w", err)
	}
	// The trusted root is public and is committed to the repository.
	return os.WriteFile(out, b, 0o644)
}
