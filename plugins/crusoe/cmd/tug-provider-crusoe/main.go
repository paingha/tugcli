// Command tug-provider-crusoe is tug's provider plugin for Crusoe Managed
// Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/crusoe"
)

func main() {
	os.Exit(provider.Main(crusoe.New()))
}
