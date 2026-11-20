// Command tug-provider-nebius is tug's provider plugin for Nebius AI Cloud
// Managed Service for Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/nebius"
)

func main() {
	os.Exit(provider.Main(nebius.New()))
}
