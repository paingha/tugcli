package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/plugin"
)

// Plugins rotate through the plugin host.
var _ importer.Rotator = (*plugin.Provider)(nil)

func newAuthRotateCmd(a *app) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rotate [cluster] [label=value...]",
		Short: "Replace a cluster's credential with a new one, revoking the old",
		Long: `Ask the cluster's provider for a new credential and keep it in place of the
old one, which the provider revokes. Without a cluster, rotate this terminal's
cluster. Only providers with the rotate capability can do this; tug plugin ls
shows which.

Anything still using the old credential loses access, so tug asks first, and
--yes skips the question. A read-only cluster (env=prod) always needs a
terminal to confirm, unless this terminal is connected to it and unlocked.`,
		Example: `  tug auth rotate lke-prod
  tug auth rotate --yes   # this terminal's cluster, without asking`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.rotateCredential(cmd.Context(), args, yes)
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "rotate without asking (read-only clusters still ask)")
	return cmd
}

func (a *app) rotateCredential(ctx context.Context, args []string, yes bool) error {
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	s, sessErr := a.sessions().Current()
	connected := sessErr == nil && s.State.Connected()
	var cl *catalog.Cluster
	switch {
	case len(args) > 0:
		cl, err = a.chooseCluster(cat, args)
	case connected:
		cl, err = cat.Get(s.State.ClusterID)
	default:
		err = errors.New("name a cluster to rotate, or run `tug connect` first")
	}
	if err != nil {
		return err
	}
	svc, _, err := a.services(cat)
	if err != nil {
		return err
	}
	if !svc.CanRotate(cl) {
		return fmt.Errorf("the %s provider cannot rotate credentials; `tug plugin ls` shows which providers can", cl.Provider)
	}

	readOnly := guard.ReadOnlyFor(cl.EffectiveLabels(), a.cfg.Guard.ReadOnlyLabels)
	unlocked := connected && s.State.ClusterID == cl.ID && s.State.Writable(a.now())
	question := fmt.Sprintf("Rotate the credential for %s? Anything still using the old one loses access.", cl.Name)
	switch {
	case readOnly && !unlocked:
		if !a.interactive() {
			return fmt.Errorf("%s is read-only, so rotating its credential needs a terminal to confirm; or run `tug connect %s` and `tug unlock 15m` first", cl.Name, cl.Name)
		}
		if !a.confirm(cl.Name+" is read-only. "+question, false) {
			return errors.New("not rotated")
		}
	case !yes:
		if !a.interactive() {
			return errors.New("rotating revokes the current credential; pass --yes to rotate without a terminal")
		}
		if !a.confirm(question, false) {
			return errors.New("not rotated")
		}
	}

	fmt.Fprintf(a.stderr, "Rotating the credential for %s through %s...\n", cl.Name, cl.Provider)
	if err := svc.Rotate(ctx, cat, cl.ID); err != nil {
		return fmt.Errorf("rotating the credential for %s: %w", cl.Name, err)
	}
	fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf(
		"Other machines keep the old credential until they ask again: tug auth token --cluster %s --refresh", cl.Name)))
	_, err = fmt.Fprintf(a.stdout, "Rotated the credential for %s\n", cl.Name)
	return err
}
