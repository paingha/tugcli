package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/fanout"
)

func newRootCmd(a *app) *cobra.Command {
	var debug bool
	root := &cobra.Command{
		Use:   "tug",
		Short: "Reach any Kubernetes cluster you are allowed to, and work there quickly",
		Long: `tug finds clusters across providers, keeps their credentials in the OS keychain,
and points each terminal at one cluster without touching ~/.kube/config.

Commands tug does not have run as kubectl with this terminal's session:
  tug get pods -A

Commands that read run on every cluster with some labels, each line
prefixed with its cluster:
  tug get pods --cluster-selector env=staging
  tug why api --cluster-selector env=staging --cluster-selector region=eu`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("parallel") {
				return fmt.Errorf("%s works only with %s", flagParallel, flagClusterSelector)
			}
			a.command = cmd.CommandPath() // the intent in audit entries
			return a.setup(debug)
		},
	}
	root.PersistentFlags().BoolVar(&debug, "debug", false, "log debugging detail to stderr")
	// execute takes these two out of the command line before cobra sees
	// it, since kubectl commands never reach cobra; they are declared here
	// for help and completion.
	root.PersistentFlags().StringArray("cluster-selector", nil,
		"run a command that reads on every catalog cluster with this label=value, each in its default namespace (repeatable; a cluster must match them all)")
	root.PersistentFlags().Int("parallel", fanout.DefaultParallel, "with --cluster-selector, how many clusters to run on at once")

	root.AddGroup(
		&cobra.Group{ID: "connect", Title: "Connect:"},
		&cobra.Group{ID: "interact", Title: "Work in the cluster:"},
		&cobra.Group{ID: "discover", Title: "Discover:"},
		&cobra.Group{ID: "safety", Title: "Safety:"},
		&cobra.Group{ID: "other", Title: "Other:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("connect",
		newConnectCmd(a), newDisconnectCmd(a), newStatusCmd(a), newNsCmd(a),
		newShellCmd(a), newInitCmd(a), newUICmd(a), newDoctorCmd(a), newTunnelCmd(a), newPinCmd(a), newProfileCmd(a),
	)
	add("interact",
		newLogsCmd(a), newShCmd(a), newPfCmd(a), newRestartCmd(a), newScaleCmd(a), newRollbackCmd(a), newTopCmd(a),
		newWhyCmd(a), newTreeCmd(a), newSupportCmd(a),
	)
	add("discover", newImportCmd(a), newSyncCmd(a), newLsCmd(a), newLabelCmd(a), newForgetCmd(a), newPluginCmd(a), newTeamCmd(a))
	add("safety", newUnlockCmd(a), newLockCmd(a), newAuditCmd(a))
	add("other", newAuthCmd(a), newDaemonCmd(a), newVersionCmd(a))
	root.AddCommand(newSessionCmd(a), newProviderExecCmd(a), newPluginSandboxExecCmd(a))
	root.SetHelpCommandGroupID("other")
	root.SetCompletionCommandGroupID("other")
	return root
}
