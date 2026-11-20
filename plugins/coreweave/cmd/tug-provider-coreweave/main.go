// Command tug-provider-coreweave is tug's provider plugin for CoreWeave
// Kubernetes Service.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/coreweave"
)

func main() {
	os.Exit(provider.Main(coreweave.New()))
}
