// Command tug-provider-civo is tug's provider plugin for Civo Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/civo"
)

func main() {
	os.Exit(provider.Main(civo.New()))
}
