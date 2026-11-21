// Command tug-provider-oracle is tug's provider plugin for Oracle
// Kubernetes Engine (OKE).
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/oracle"
)

func main() {
	os.Exit(provider.Main(oracle.New()))
}
