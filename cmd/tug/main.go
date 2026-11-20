// Command tug makes Kubernetes clusters easy to reach and work with.
package main

import (
	"os"

	"github.com/paingha/tugcli/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
