package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/importer"
	"github.com/paingha/tugcli/internal/oidc"
	"github.com/paingha/tugcli/internal/plugin"
	"github.com/paingha/tugcli/pkg/provider"
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
	// tugd rebuilds each cluster's upstream at least once a minute, which
	// reads the new credential from the store.
	fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf(
		"Sessions that go through tugd switch to it within a minute. Other machines keep the old credential until they ask again: tug auth token --cluster %s --refresh", cl.Name)))
	_, err = fmt.Fprintf(a.stdout, "Rotated the credential for %s\n", cl.Name)
	return err
}

func newAuthLoginCmd(a *app) *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login [cluster] [label=value...]",
		Short: "Sign in to the OpenID Connect issuer a cluster trusts",
		Long: `Sign in for a cluster whose provider gives it an OpenID Connect credential,
so its API server accepts your ID token. tug opens the issuer's sign-in page
in your browser and waits for it to come back to http://localhost:8000 (or
18000, or oidc.listenAddresses in config.yaml). With --device, or over SSH,
it shows a code to enter on any device instead, when the issuer offers that.

The refresh token and the ID token are kept in the credential store, and tug
refreshes the ID token from then on. Clusters that share the issuer, client
and scopes share the sign-in. Without a cluster, sign in for this terminal's
cluster. When kubectl needs a token and nobody is signed in, tug also signs
in on its own if stderr is a terminal.`,
		Example: `  tug auth login cks-train
  tug auth login --device   # this terminal's cluster, with a code to enter elsewhere`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cfg, store, err := a.oidcCluster(cmd.Context(), args)
			if err != nil {
				return err
			}
			t, err := a.oidcClient(store, nil).Login(cmd.Context(), cfg, a.signInUI(device))
			if err != nil {
				return fmt.Errorf("signing in for %s: %w", cl.Name, err)
			}
			_, err = fmt.Fprintf(a.stdout, "Signed in to %s as %s; the ID token is valid until %s\n",
				cfg.Issuer, dash(t.Identity), t.Expiry.Local().Format("2006-01-02 15:04 MST"))
			return err
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().BoolVar(&device, "device", false, "show a code to enter on another device instead of opening a browser here")
	return cmd
}

func newAuthLogoutCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout [cluster] [label=value...]",
		Short: "Forget the OpenID Connect sign-in a cluster uses",
		Long: `Remove the refresh token and ID token tug keeps for a cluster's OpenID Connect
issuer. Every cluster that shares the sign-in needs tug auth login again. The
issuer is not told, so revoke the session there too if the token may have
leaked.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg, store, err := a.oidcCluster(cmd.Context(), args)
			if err != nil {
				return err
			}
			had, err := a.oidcClient(store, nil).Logout(cfg)
			if err != nil {
				return err
			}
			msg := "Signed out of %s\n"
			if !had {
				msg = "Not signed in to %s; nothing to forget\n"
			}
			_, err = fmt.Fprintf(a.stdout, msg, cfg.Issuer)
			return err
		},
		ValidArgsFunction: a.completeClusters,
	}
}

// oidcCluster finds the cluster named in args, or this terminal's, and the
// OpenID Connect issuer its provider says to sign in to.
func (a *app) oidcCluster(ctx context.Context, args []string) (*catalog.Cluster, oidc.Config, credentials.Store, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, oidc.Config{}, nil, err
	}
	var cl *catalog.Cluster
	switch {
	case len(args) > 0:
		// A cluster ID, as hints print it, or a name to match.
		if cl, err = cat.Get(args[0]); err != nil || len(args) > 1 {
			cl, err = a.chooseCluster(cat, args)
		}
	default:
		s, sessErr := a.sessions().Current()
		if sessErr != nil || !s.State.Connected() {
			return nil, oidc.Config{}, nil, errors.New("name a cluster, or run `tug connect` first")
		}
		cl, err = cat.Get(s.State.ClusterID)
	}
	if err != nil {
		return nil, oidc.Config{}, nil, err
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return nil, oidc.Config{}, nil, err
	}
	c, err := broker.Credential(ctx, cl.ID, false)
	if err != nil {
		return nil, oidc.Config{}, nil, fmt.Errorf("asking the %s provider how %s signs in: %w", cl.Provider, cl.Name, err)
	}
	if c.Type != provider.CredentialOIDC {
		return nil, oidc.Config{}, nil, fmt.Errorf("%s does not use OpenID Connect: its credential is of type %s, which needs no sign-in", cl.Name, c.Type)
	}
	return cl, oidc.ConfigFor(c), broker.Store, nil
}
