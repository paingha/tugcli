package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/version"
)

func newAuthCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Sign in, rotate credentials, and serve them to kubectl and other clients",
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
	cmd.AddCommand(token, newProxyTokenCmd(a), newAuthLoginCmd(a), newAuthLogoutCmd(a), newAuthRotateCmd(a))
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

// passthrough runs kubectl with the session commands run in (see
// app.session). On a read-only session, commands that may write are
// refused unless the user confirms. Those that run are recorded in the
// audit log first, and their exit code after, unless the session goes
// through tugd, which records each of their requests instead.
func (a *app) passthrough(ctx context.Context, args []string) (int, error) {
	kubeconfigPath := ""
	var write *audit.Entry
	s, err := a.session()
	switch {
	case err == nil && s.State.Connected():
		kubeconfigPath = s.KubeconfigPath()
		if passthrough.IsRead(args) {
			break
		}
		entry := audit.Entry{
			Source:    audit.SourceKubectl,
			Cluster:   s.State.ClusterName,
			ClusterID: s.State.ClusterID,
			Session:   s.State.ID,
			Namespace: audit.CommandNamespace(args, s.State.Namespace),
			Intent:    kubectlIntent(args),
			Command:   audit.RedactCommand(args),
		}
		allowed, err := a.allowKubectlWrite(s, args)
		if err != nil {
			entry.Kind, entry.Reason = audit.KindRefused, audit.ReasonReadOnly
			a.note(ctx, entry)
			return 1, err
		}
		if s.State.ProxyURL == "" {
			entry.Kind, entry.ID, entry.Allowed = audit.KindWrite, audit.NewID(), allowed
			write = &entry
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
	if write != nil {
		if err := a.audit().Record(ctx, *write); err != nil {
			return 1, fmt.Errorf("not running `kubectl %s`, since it could not be recorded in the audit log: %w",
				strings.Join(write.Command, " "), err)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		code = passthrough.ExitCode(runErr)
	}
	if write != nil {
		result := *write
		result.Time, result.Kind, result.Allowed, result.Exit = time.Time{}, audit.KindResult, "", &code
		a.note(ctx, result)
	}
	if runErr != nil {
		var ee interface{ ExitCode() int }
		if !errors.As(runErr, &ee) {
			return code, runErr
		}
	}
	return code, nil
}

// kubectlIntent names a kubectl command line by its first word, as in
// "kubectl delete".
func kubectlIntent(args []string) string {
	if words := passthrough.Words(args); len(words) > 0 {
		return "kubectl " + words[0]
	}
	return "kubectl"
}

// allowKubectlWrite says how a kubectl command that may write is allowed on
// session s, as the audit log puts it. On a read-only session that is not
// unlocked it asks first, and refuses when the answer is no.
func (a *app) allowKubectlWrite(s *session.Session, args []string) (string, error) {
	switch {
	case !s.State.ReadOnly:
		return audit.AllowedReadWrite, nil
	case s.State.Writable(a.now()):
		return audit.AllowedUnlocked, nil
	}
	question := fmt.Sprintf("%s is read-only. Run `kubectl %s` anyway?", s.State.ClusterName, strings.Join(args, " "))
	if !a.confirm(question, false) {
		return "", fmt.Errorf("refused: %s is read-only and `kubectl %s` may write; run `tug unlock %s` to allow writes",
			s.State.ClusterName, strings.Join(args, " "), guard.ShortDuration(a.suggestedUnlock(s.State)))
	}
	return audit.AllowedConfirmed, nil
}
