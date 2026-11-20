// Command tug-provider-rancher is tug's provider plugin for clusters that
// Rancher manages.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/rancher"
)

func main() {
	os.Exit(provider.Main(rancher.New()))
}
