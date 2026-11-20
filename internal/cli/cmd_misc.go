package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/version"
)

func newAuthCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Rotate cluster credentials, and serve them to kubectl and other clients",
	}
	var clusterID string
	var refresh bool
	token := &cobra.Command{
		Use:   "token --cluster <id>",
		Short: "Print an ExecCredential for a cluster (the exec entry in session kubeconfigs)",
		Long: `Print a client.authentication.k8s.io/v1 ExecCredential for a cluster. Every
kubeconfig tug writes calls this, so secrets stay in the keychain and never in a
file. It answers from the cache, or asks the cluster's provider on a miss.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, err := cat.Get(clusterID)
			if err != nil {
				return err
			}
			_, broker, err := a.services(cat)
			if err != nil {
				return err
			}
			ec, err := broker.ExecCredential(cmd.Context(), cl.ID, refresh)
			if err != nil {
				return fmt.Errorf("credentials for %s: %w", cl.Name, err)
			}
			return json.NewEncoder(a.stdout).Encode(ec)
		},
	}
	token.Flags().StringVar(&clusterID, "cluster", "", "catalog cluster ID or name")
	token.Flags().BoolVar(&refresh, "refresh", false, "ignore the cache and ask the provider")
	_ = token.MarkFlagRequired("cluster")
	cmd.AddCommand(token, newProxyTokenCmd(a), newAuthRotateCmd(a))
	return cmd
}

func newVersionCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print tug's version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			v := version.Get()
			if format == render.FormatJSON || format == render.FormatYAML {
				return render.Structured(a.stdout, format, v)
			}
			_, err = fmt.Fprintf(a.stdout, "tug %s (%s, %s, %s)\n", v.Version, dash(shortCommit(v.Commit)), v.GoVersion, v.Platform)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// passthrough runs kubectl with this terminal's session. On a read-only
// session, commands that may write are refused unless the user confirms.
func (a *app) passthrough(_ context.Context, args []string) (int, error) {
	kubeconfigPath := ""
	s, err := a.sessions().Current()
	switch {
	case err == nil && s.State.Connected():
		kubeconfigPath = s.KubeconfigPath()
		if !s.State.Writable(a.now()) && !passthrough.IsRead(args) {
			question := fmt.Sprintf("%s is read-only. Run `kubectl %s` anyway?", s.State.ClusterName, strings.Join(args, " "))
			if !a.confirm(question, false) {
				return 1, fmt.Errorf("refused: %s is read-only and `kubectl %s` may write; run `tug unlock 15m` to allow writes",
					s.State.ClusterName, strings.Join(args, " "))
			}
		}
	case err == nil, errors.Is(err, session.ErrNoSession):
		// Not connected: behave like plain kubectl.
	default:
		return 1, err
	}
	cmd, err := passthrough.Command(a.cfg.Kubectl, args, kubeconfigPath)
	if err != nil {
		return 1, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	if err := cmd.Run(); err != nil {
		code := passthrough.ExitCode(err)
		var ee interface{ ExitCode() int }
		if !errors.As(err, &ee) {
			return code, err
		}
		return code, nil
	}
	return 0, nil
}
