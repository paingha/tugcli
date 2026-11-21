package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/kube"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/version"
)

// daemonStartTimeout bounds how long tug waits for a new tugd to answer.
const daemonStartTimeout = 5 * time.Second

func newDaemonCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run tugd, the background process behind the local API proxy",
		Long: `tugd owns what must outlive a single command:
  - the local API proxy: sessions connected with --proxy send every client
    (kubectl, helm, k9s) through it, so a read-only session refuses writes
    from all of them;
  - tunnels to private clusters (tug tunnel), which it keeps open;
  - named port-forwards (tug pf --name), which it restarts on new pods.

tug starts tugd when a session needs it, and tugd stops after 30 minutes
without a live session or a named port-forward.`,
	}
	cmd.AddCommand(newDaemonRunCmd(a), newDaemonStartCmd(a), newDaemonStopCmd(a), newDaemonStatusCmd(a))
	return cmd
}

func newDaemonRunCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "run",
		Short:  "Run tugd in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srv := daemon.New(daemon.Config{
				RuntimeDir: a.paths.Runtime,
				Sessions:   a.sessions(),
				Upstream:   a.daemonUpstream,
				Forward:    a.daemonForward,
				Audit:      a.audit(),
				Version:    version.String(),
				Now:        a.now,
				Log:        a.log,
			})
			return srv.Run(cmd.Context())
		},
	}
}

// daemonUpstream builds the client config tugd forwards a cluster's traffic
// with: the catalog's endpoint and CA, and the cluster's real credential.
func (a *app) daemonUpstream(ctx context.Context, clusterID string) (*rest.Config, error) {
	cat, err := a.loadCatalog()
	if err != nil {
		return nil, err
	}
	cl, err := cat.Get(clusterID)
	if err != nil {
		return nil, err
	}
	_, broker, err := a.services(cat)
	if err != nil {
		return nil, err
	}
	f := &kube.Factory{Cluster: cl, Broker: broker, CacheDir: a.paths.Cache, UserAgent: "tugd/" + version.String(), Tunnels: a.tunnels()}
	return f.RESTConfig(ctx)
}

func newDaemonStartCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start tugd in the background if it is not running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.ensureDaemon(cmd.Context())
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "tugd is running (pid %d), proxy at %s\n", st.PID, st.ProxyURL)
			return err
		},
	}
}

func newDaemonStopCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop tugd",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := daemon.NewClient(a.paths.Runtime).Shutdown(cmd.Context())
			if errors.Is(err, daemon.ErrNotRunning) {
				_, err = fmt.Fprintln(a.stdout, "tugd is not running")
				return err
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.stdout, "tugd stopped")
			return err
		},
	}
}

func newDaemonStatusCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether tugd is running and what it serves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			st, err := daemon.NewClient(a.paths.Runtime).Status(cmd.Context())
			if errors.Is(err, daemon.ErrNotRunning) {
				if format == render.FormatJSON || format == render.FormatYAML {
					return render.Structured(a.stdout, format, map[string]bool{"running": false})
				}
				_, err = fmt.Fprintln(a.stdout, "tugd is not running; `tug connect --proxy` starts it")
				return err
			}
			if err != nil {
				return err
			}
			if format == render.FormatJSON || format == render.FormatYAML {
				return render.Structured(a.stdout, format, st)
			}
			_, err = fmt.Fprintf(a.stdout, "running:   pid %d, %s, since %s\nproxy:     %s\nsessions:  %d\nlog:       %s\n",
				st.PID, st.Version, st.Started.Local().Format(time.Kitchen), st.ProxyURL, st.Sessions, a.daemonLog())
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

func (a *app) daemonLog() string { return filepath.Join(a.paths.Runtime, "tugd.log") }

// ensureDaemon returns a client for a running tugd, starting one in the
// background when there is none.
func (a *app) ensureDaemon(ctx context.Context) (*daemon.Client, error) {
	c := daemon.NewClient(a.paths.Runtime)
	_, err := c.Status(ctx)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, daemon.ErrNotRunning) {
		return nil, err
	}
	// Private before the log is opened: in a directory another user can
	// write to, tugd.log could be a link to one of this user's files.
	if err := config.EnsurePrivateDir(a.paths.Runtime); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(a.daemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(selfPath(), "daemon", "run")
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting tugd: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(daemonStartTimeout)
	for {
		if _, err = c.Status(ctx); err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tugd did not start within %s; see %s: %w", daemonStartTimeout, a.daemonLog(), err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// newProxyTokenCmd is the exec entry of sessions that go through tugd.
func newProxyTokenCmd(a *app) *cobra.Command {
	var sessionID string
	cmd := &cobra.Command{
		Use:   "proxy-token --session <id>",
		Short: "Print an ExecCredential for a session's tugd proxy (the exec entry of --proxy sessions)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.ensureDaemon(cmd.Context())
			if err != nil {
				return err
			}
			reg, err := c.Register(cmd.Context(), sessionID)
			if err != nil {
				return fmt.Errorf("session %s: %w", sessionID, err)
			}
			// tugd keeps its port and certificate across restarts, but when
			// it could not, the session's kubeconfig is repointed. The client
			// that ran this command loaded the old one and would send the
			// token to an address that no longer answers, so it is told to
			// run again instead.
			if s, err := a.sessions().Open(sessionID); err == nil {
				changed, err := s.SetProxy(reg.ProxyURL, reg.ProxyCA)
				switch {
				case err != nil:
					a.log.Warn("updating the session's proxy address", "err", err)
				case changed:
					return fmt.Errorf("tugd moved to %s and this session's kubeconfig now points there; "+
						"run the command again, and restart long-running clients such as k9s", reg.ProxyURL)
				}
			}
			return json.NewEncoder(a.stdout).Encode(&clientauthv1.ExecCredential{
				TypeMeta: metav1.TypeMeta{APIVersion: clientauthv1.SchemeGroupVersion.String(), Kind: "ExecCredential"},
				Status:   &clientauthv1.ExecCredentialStatus{Token: reg.Token},
			})
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "tug session ID")
	_ = cmd.MarkFlagRequired("session")
	return cmd
}
