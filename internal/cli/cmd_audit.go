package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/render"
)

type auditOptions struct {
	cluster string
	since   time.Duration
	output  string
}

func newAuditCmd(a *app) *cobra.Command {
	var o auditOptions
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "List the writes tug let through to clusters, newest first",
		Long: `List the writes tug let through, newest first: those of tug's own commands,
of kubectl run by tug on a session, and of every client (kubectl, helm, k9s)
that goes through tugd's proxy. Each says when, on which cluster and from
which session, and whether the session was read-write or unlocked, or the
write was confirmed.

The log stays on this machine, in JSON lines under tug's data directory, and
is rotated as it grows. It never holds request bodies, tokens, or command-line
values that could be secrets.`,
		Example: `  tug audit --since 24h
  tug audit --cluster payments-prod -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("audit supports -o table, json or yaml")
			}
			if o.since < 0 {
				return fmt.Errorf("--since must be a positive duration such as 24h, got %s", o.since)
			}
			f := audit.Filter{Cluster: o.cluster}
			if o.since > 0 {
				f.Since = a.now().Add(-o.since)
			}
			entries, err := a.audit().Entries(cmd.Context(), f)
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, entries)
			}
			if len(entries) == 0 {
				_, err := fmt.Fprintln(a.stdout, "No writes recorded.")
				return err
			}
			c := a.colors()
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				allowed := e.Allowed
				if allowed != audit.AllowedReadWrite {
					allowed = c.Yellow(allowed)
				}
				rows = append(rows, []string{
					e.Time.Local().Format(time.DateTime), e.Cluster, dash(e.Session), auditSource(e), allowed, auditAction(e),
				})
			}
			return render.Table(a.stdout, []string{"TIME", "CLUSTER", "SESSION", "SOURCE", "ALLOWED", "WRITE"}, rows)
		},
	}
	cmd.Flags().StringVar(&o.cluster, "cluster", "", "only writes to this cluster (name or ID)")
	cmd.Flags().DurationVar(&o.since, "since", 0, "only writes newer than this, e.g. 24h (default: all)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// auditSource names what made a write, with the client's name for one
// through tugd, as in "tugd (Helm)".
func auditSource(e audit.Entry) string {
	if e.Source != audit.SourceProxy || e.Client == "" {
		return e.Source
	}
	name, _, _ := strings.Cut(e.Client, "/")
	return fmt.Sprintf("%s (%s)", e.Source, name)
}

// auditAction says what a write was, as "kubectl delete pod api-0" or
// "DELETE pods/api-0 -n web".
func auditAction(e audit.Entry) string {
	if len(e.Command) > 0 {
		return "kubectl " + strings.Join(e.Command, " ")
	}
	action := guard.Describe(e.Method, &url.URL{Path: e.Path})
	if ns := namespaceOf(e.Path); ns != "" {
		action += " -n " + ns
	}
	return action
}

// namespaceOf returns the namespace an API path is in, or "" for a
// cluster-wide one such as a namespace itself.
func namespaceOf(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "namespaces" && ((parts[0] == "api" && i == 2) || (parts[0] == "apis" && i == 3)) {
			return parts[i+1]
		}
	}
	return ""
}
