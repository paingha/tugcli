package cli

import (
	"github.com/spf13/cobra"
)

func newRootCmd(a *app) *cobra.Command {
	var debug bool
	root := &cobra.Command{
		Use:   "tug",
		Short: "Reach any Kubernetes cluster you are allowed to, and work there quickly",
		Long: `tug finds clusters across providers, keeps their credentials in the OS keychain,
and points each terminal at one cluster without touching ~/.kube/config.

Commands tug does not have run as kubectl with this terminal's session:
  tug get pods -A`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.setup(debug)
		},
	}
	root.PersistentFlags().BoolVar(&debug, "debug", false, "log debugging detail to stderr")

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
		newShellCmd(a), newInitCmd(a), newUICmd(a), newDoctorCmd(a),
	)
	add("interact",
		newLogsCmd(a), newShCmd(a), newPfCmd(a), newRestartCmd(a), newScaleCmd(a), newRollbackCmd(a), newTopCmd(a),
		newWhyCmd(a), newTreeCmd(a),
	)
	add("discover", newImportCmd(a), newSyncCmd(a), newLsCmd(a), newLabelCmd(a), newForgetCmd(a), newPluginCmd(a))
	add("safety", newUnlockCmd(a), newLockCmd(a))
	add("other", newAuthCmd(a), newDaemonCmd(a), newVersionCmd(a))
	root.AddCommand(newSessionCmd(a), newProviderExecCmd(a))
	root.SetHelpCommandGroupID("other")
	root.SetCompletionCommandGroupID("other")
	return root
}
