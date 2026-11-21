// Command tug-provider-scaleway is tug's provider plugin for Scaleway
// Kubernetes Kapsule and Kosmos.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/scaleway"
)

func main() {
	os.Exit(provider.Main(scaleway.New()))
}
