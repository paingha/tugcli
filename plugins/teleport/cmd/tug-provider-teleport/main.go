// Command tug-provider-teleport is tug's provider plugin for Kubernetes
// clusters behind Teleport.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/teleport"
)

func main() {
	os.Exit(provider.Main(teleport.New()))
}
