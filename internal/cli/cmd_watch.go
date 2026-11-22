package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

type watchOptions struct {
	namespace string
	output    string
	events    bool
	until     string
	timeout   time.Duration
}

func newWatchCmd(a *app) *cobra.Command {
	var o watchOptions
	cmd := &cobra.Command{
		Use:         "watch <target>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Follow a rollout, or any object's reconciliation, as it happens",
		Long: `Follow a workload's rollout, or any object's reconciliation, as it happens: one
line for each meaningful change to the object and to everything it owns, as
tree finds it. A line says when a condition changes its status or reason, when
the spec gets a new generation and when the controller catches up with it, how
replica counts move, when a pod's status changes, a container restarts or no
node can take a pod, and when an object appears, becomes ready or not ready,
terminates or is deleted. With --events, new Warning events about any of it
are shown too.

Each line is marked as tree marks objects: ready (✓), not ready (✗), unknown
(?) or no health of its own (·), with ! for a warning event and - for a
deleted object. watch begins with the object's state and whatever it owns that
is not ready, then runs until Ctrl-C.

With --until ready, watch exits 0 once the object is ready: a workload rolled
out, a job complete, a pod ready, any other object ready by its conditions or,
when it reports no health of its own, by what it owns. It exits 1 when that
cannot happen as things are, as when a job has failed or a deployment has
exceeded its progress deadline, and when --timeout passes first. --until
deleted waits for the object to be gone.

-o json writes one JSON object per change. watch only reads, and never reads a
Secret's data.`,
		Example: `  tug watch api                              # deploy/api-server, its ReplicaSets and pods
  tug watch deploy/api --until ready --timeout 5m
  tug watch certificate/web-tls --events
  tug watch job/migrate --until ready -o json
  tug watch pod/api-7d9f-x2x4z --until deleted`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format != render.FormatTable && format != render.FormatJSON {
				return errors.New("watch supports -o table or json")
			}
			until, err := parseUntil(o.until)
			if err != nil {
				return err
			}
			if o.timeout > 0 && until == ops.UntilStopped {
				return errors.New("--timeout needs --until")
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, o.namespace)
			if err != nil {
				return err
			}
			clients, mapper, err := a.objectClients(env)
			if err != nil {
				return err
			}
			obj, err := a.resolveObject(ctx, env, clients, mapper, args[0])
			if err != nil {
				return err
			}

			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if o.timeout > 0 {
				wctx, cancel = context.WithTimeout(wctx, o.timeout)
				defer cancel()
			}
			// A change that cannot be written, as when the reader has gone,
			// ends the watch.
			var werr error
			write := func(ch ops.Change) error { return a.printChange(ch, obj.Namespace) }
			if format == render.FormatJSON {
				enc := json.NewEncoder(a.stdout)
				write = func(ch ops.Change) error { return enc.Encode(ch) }
			}
			sink := ops.WatchSink{
				Change: func(ch ops.Change) {
					if werr == nil {
						if werr = write(ch); werr != nil {
							cancel()
						}
					}
				},
				Error: func(err error) {
					fmt.Fprintf(a.stderr, "%s %v\n", a.colors().Yellow("warning:"), err)
				},
			}
			err = ops.Watch(wctx, clients, obj, ops.WatchOptions{Events: o.events, Until: until, Now: a.now}, sink)
			switch {
			case werr != nil:
				return werr
			case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("timed out after %s waiting for %s to be %s", o.timeout, obj, until)
			case errors.Is(err, context.Canceled):
				return nil // Ctrl-C
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.StringVarP(&o.output, "output", "o", "", "output format: table, or json for one object per change")
	f.BoolVar(&o.events, "events", false, "also show new Warning events about the object and what it owns")
	f.StringVar(&o.until, "until", "", "exit once the object is ready or deleted (default: watch until Ctrl-C)")
	f.DurationVar(&o.timeout, "timeout", 0, "with --until, give up and exit 1 after this long, such as 5m (default: no limit)")
	return cmd
}

func parseUntil(s string) (ops.Until, error) {
	switch u := ops.Until(strings.ToLower(s)); u {
	case ops.UntilStopped, ops.UntilReady, ops.UntilDeleted:
		return u, nil
	default:
		return "", fmt.Errorf("--until wants ready or deleted, got %q", s)
	}
}

// printChange writes one change as a line: the time, a health marker, what
// changed, and how. An object in another namespace than the watched one's,
// beneath a cluster-scoped object, has its namespace after its name.
func (a *app) printChange(ch ops.Change, namespace string) error {
	c := a.colors()
	marker := healthMarker(c, ch.Health)
	switch ch.Type {
	case ops.ChangeEvent:
		marker = c.Yellow("!")
	case ops.ChangeDeleted:
		marker = c.Dim("-")
	}
	object := ch.Object
	if ch.Root {
		object = c.Bold(object)
	}
	if ch.Namespace != "" && ch.Namespace != namespace {
		object += c.Dim(" (" + ch.Namespace + ")")
	}
	_, err := fmt.Fprintf(a.stdout, "%s %s %s  %s\n", c.Dim(ch.Time.Format(time.TimeOnly)), marker, object, ch.Message)
	return err
}

// healthMarker marks a health as tree does.
func healthMarker(c render.Colors, h ops.Health) string {
	switch h {
	case ops.HealthReady:
		return c.Green("✓")
	case ops.HealthNotReady:
		return c.Red("✗")
	case ops.HealthNone:
		return c.Dim("·")
	default:
		return c.Yellow("?")
	}
}
