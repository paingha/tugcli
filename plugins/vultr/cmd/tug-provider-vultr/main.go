// Command tug-provider-vultr is tug's provider plugin for Vultr Kubernetes
// Engine.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/vultr"
)

func main() {
	os.Exit(provider.Main(vultr.New()))
}
