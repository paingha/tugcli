// Command tug-provider-ionos is tug's provider plugin for IONOS Cloud
// Managed Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/ionos"
)

func main() {
	os.Exit(provider.Main(ionos.New()))
}
