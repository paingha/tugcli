// Command tug-provider-tailscale is tug's provider plugin for Kubernetes
// clusters behind the Tailscale Kubernetes operator's API server proxy.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/tailscale"
)

func main() {
	os.Exit(provider.Main(tailscale.New()))
}
