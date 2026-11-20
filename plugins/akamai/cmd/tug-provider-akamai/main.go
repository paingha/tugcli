// Command tug-provider-akamai is tug's provider plugin for Akamai (Linode)
// Kubernetes Engine.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/akamai"
)

func main() {
	os.Exit(provider.Main(akamai.New()))
}
