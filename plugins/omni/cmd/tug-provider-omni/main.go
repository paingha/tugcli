// Command tug-provider-omni is tug's provider plugin for the Talos Linux
// clusters that Sidero Omni manages.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/omni"
)

func main() {
	os.Exit(provider.Main(omni.New()))
}
