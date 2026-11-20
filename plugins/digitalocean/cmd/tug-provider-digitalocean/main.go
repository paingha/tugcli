// Command tug-provider-digitalocean is tug's provider plugin for
// DigitalOcean Kubernetes.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/digitalocean"
)

func main() {
	os.Exit(provider.Main(digitalocean.New()))
}
