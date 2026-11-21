// Command tug-provider-ovhcloud is tug's provider plugin for OVHcloud
// Managed Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/ovhcloud"
)

func main() {
	os.Exit(provider.Main(ovhcloud.New()))
}
