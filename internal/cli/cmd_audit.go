package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
)

type auditOptions struct {
	cluster   string
	session   string
	namespace string
	events    []string
	since     time.Duration
	output    string
}

func newAuditCmd(a *app) *cobra.Command {
	var o auditOptions
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "List what tug let through to clusters and what it refused, newest first",
		Long: `List what tug recorded, newest first:
  write           a write tug let through: of tug's own commands, of kubectl run
                  by tug, or of any client (kubectl, helm, k9s) through tugd
  refused         a write the read-only guard refused
  session-start   a terminal connecting to a cluster
  session-end     a terminal leaving it: disconnecting, connecting elsewhere,
                  or closing
  unlock          tug unlock, and until when
  lock            tug lock, ending an unlock early
  unlock-expired  an unlock running out

A write says how it was allowed (read-write, unlocked or confirmed), the
command it came from as far as tug can tell, the request tug or tugd saw,
and how it was answered: the HTTP status, or kubectl's exit code.

The log stays on this machine, in JSON lines under tug's data directory, and
is rotated as it grows. It never holds request bodies, query strings, tokens,
or command-line values that could be secrets. It is a record for you, written
with your own permissions, not evidence that it was not changed; the
cluster's own audit log is that.`,
		Example: `  tug audit --since 24h
  tug audit --cluster payments-prod --event write --event refused
  tug audit --session "$TUG_SESSION" -o json`,
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
			for _, ev := range o.events {
				if !slices.Contains(audit.Kinds, ev) {
					return fmt.Errorf("unknown --event %q; use one of %s", ev, strings.Join(audit.Kinds, ", "))
				}
			}
			f := audit.Filter{Cluster: o.cluster, Session: o.session, Namespace: o.namespace, Kinds: o.events}
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
				_, err := fmt.Fprintln(a.stdout, "Nothing recorded.")
				return err
			}
			c := a.colors()
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []string{
					e.Time.Local().Format(time.DateTime), e.Cluster, dash(e.Session), auditSource(e),
					auditEvent(c, e), dash(auditDetail(e)), auditResult(c, e),
				})
			}
			return render.Table(a.stdout, []string{"TIME", "CLUSTER", "SESSION", "SOURCE", "EVENT", "DETAIL", "RESULT"}, rows)
		},
	}
	cmd.Flags().StringVar(&o.cluster, "cluster", "", "only entries about this cluster (name or ID)")
	cmd.Flags().StringVar(&o.session, "session", "", "only entries from this session, such as $TUG_SESSION")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "only entries in this namespace")
	cmd.Flags().StringArrayVar(&o.events, "event", nil, "only entries of this kind, such as write or refused (repeatable; default: all)")
	cmd.Flags().DurationVar(&o.since, "since", 0, "only entries newer than this, e.g. 24h (default: all)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// auditSource names what made an entry, with the client's name for one
// through tugd, as in "tugd (Helm)".
func auditSource(e audit.Entry) string {
	if e.Source != audit.SourceProxy || e.Client == "" {
		return e.Source
	}
	name, _, _ := strings.Cut(e.Client, "/")
	return fmt.Sprintf("%s (%s)", e.Source, name)
}

// auditEvent is an entry's kind, with how a write was allowed when the
// session was read-only, as in "write (unlocked)".
func auditEvent(c render.Colors, e audit.Entry) string {
	switch {
	case e.Kind == audit.KindRefused:
		return c.Red(e.Kind)
	case e.Kind == audit.KindWrite && e.Allowed != "" && e.Allowed != audit.AllowedReadWrite:
		return c.Yellow(fmt.Sprintf("%s (%s)", e.Kind, e.Allowed))
	case e.Kind == audit.KindUnlock:
		return c.Yellow(e.Kind)
	}
	return e.Kind
}

// auditDetail says what an entry was about: the request of a write, as
// "DELETE pods/api-0 -n web (kubectl delete)", the session of a session
// entry, or when an unlock ends.
func auditDetail(e audit.Entry) string {
	switch e.Kind {
	case audit.KindWrite, audit.KindRefused:
		return auditAction(e)
	case audit.KindSessionStart:
		mode := "read-write"
		if e.ReadOnly {
			mode = "read-only"
		}
		detail := "namespace " + e.Namespace + ", " + mode
		if e.Proxy {
			detail += ", through tugd"
		}
		return detail
	case audit.KindSessionEnd:
		return "namespace " + e.Namespace
	case audit.KindUnlock:
		return "until " + e.Until.Local().Format(time.DateTime)
	case audit.KindLock:
		if !e.Until.IsZero() {
			return "was until " + e.Until.Local().Format(time.DateTime)
		}
	}
	return ""
}

// auditAction says what a write was, as "kubectl delete pod api-0" or
// "DELETE pods/api-0 -n web", with the command it came from when known.
func auditAction(e audit.Entry) string {
	switch {
	case len(e.Command) > 0:
		return "kubectl " + strings.Join(e.Command, " ")
	case e.Method == "" && e.Path == "":
		return e.Intent // refused before it made a request
	}
	action := guard.Describe(e.Method, &url.URL{Path: e.Path})
	if e.Namespace != "" {
		action += " -n " + e.Namespace
	}
	if e.Intent != "" {
		action += " (" + e.Intent + ")"
	}
	return action
}

// auditResult says how a write was answered or why it was refused.
func auditResult(c render.Colors, e audit.Entry) string {
	switch {
	case e.Kind == audit.KindRefused:
		return c.Red(dash(e.Reason))
	case e.Kind != audit.KindWrite:
		return "-"
	case e.Exit != nil && *e.Exit != 0:
		return c.Red("exit " + strconv.Itoa(*e.Exit))
	case e.Exit != nil:
		return "exit 0"
	case e.Status >= 400:
		return c.Red(strconv.Itoa(e.Status))
	case e.Status != 0:
		return strconv.Itoa(e.Status)
	}
	return dash(e.Reason) // no answer, or none recorded
}

// sessionEntry is an audit entry of a kind about session st, from the
// command running.
func (a *app) sessionEntry(kind string, st session.State) audit.Entry {
	return audit.Entry{
		Kind:      kind,
		Source:    audit.SourceTug,
		Cluster:   st.ClusterName,
		ClusterID: st.ClusterID,
		Session:   st.ID,
		Namespace: st.Namespace,
		ReadOnly:  st.ReadOnly,
		Proxy:     st.ProxyURL != "",
		Intent:    a.command,
	}
}

// noteSession records a session starting or ending on the cluster in st,
// when it has one.
func (a *app) noteSession(ctx context.Context, kind string, st session.State) {
	if st.Connected() {
		a.note(ctx, a.sessionEntry(kind, st))
	}
}

// noteRefused records that the running command was refused on read-only
// session st before it sent anything to namespace ns.
func (a *app) noteRefused(st session.State, ns string) {
	e := a.sessionEntry(audit.KindRefused, st)
	e.ReadOnly, e.Proxy, e.Namespace, e.Reason = false, false, ns, audit.ReasonReadOnly
	a.note(context.Background(), e)
}

// noteTimeout bounds how long recording an entry that changes nothing
// waits for the audit log.
const noteTimeout = 5 * time.Second

// note records an entry that cannot change what happens, such as a session
// starting, a refusal or an outcome, logging a failure to record it. A
// cancelled command still has it recorded.
func (a *app) note(ctx context.Context, e audit.Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noteTimeout)
	defer cancel()
	if err := a.audit().Record(ctx, e); err != nil {
		a.log.Warn("recording in the audit log", "kind", e.Kind, "err", err)
	}
}

// endExpiredUnlock records, once, that session s's unlock has run out, if
// it has. tugd does the same for proxied sessions; whichever sees it first
// records it.
func (a *app) endExpiredUnlock(s *session.Session) {
	st := s.State
	now := a.now()
	if a.log == nil || !st.ReadOnly || st.UnlockedUntil.IsZero() || now.Before(st.UnlockedUntil) {
		return
	}
	_, err := s.EndExpiredUnlock(now, func(until time.Time) error {
		e := a.sessionEntry(audit.KindUnlockExpired, st)
		e.Time, e.Until, e.Intent = until, until, ""
		ctx, cancel := context.WithTimeout(context.Background(), noteTimeout)
		defer cancel()
		return a.audit().Record(ctx, e)
	})
	if err != nil {
		a.log.Warn("recording an unlock that ran out", "session", st.ID, "err", err)
	}
}
