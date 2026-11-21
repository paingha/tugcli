// Command tug-provider-ibm is tug's provider plugin for IBM Cloud
// Kubernetes Service and Red Hat OpenShift on IBM Cloud.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/ibm"
)

func main() {
	os.Exit(provider.Main(ibm.New()))
}
