// Command tug-provider-openshift is tug's provider plugin for the OpenShift
// clusters in Red Hat OpenShift Cluster Manager.
package main

import (
	"os"

	"github.com/paingha/tugcli/pkg/provider"
	"github.com/paingha/tugcli/plugins/openshift"
)

func main() {
	os.Exit(provider.Main(openshift.New()))
}
