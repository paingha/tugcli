// Command tug-provider-exoscale is tug's provider plugin for Exoscale
// Scalable Kubernetes Service.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/exoscale"
)

func main() {
	os.Exit(provider.Main(exoscale.New()))
}
