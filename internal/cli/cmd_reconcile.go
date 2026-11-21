package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
)

func newReconcileCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Diagnose the controllers that reconcile objects, and what they did",
		Long: `Diagnose how controllers reconcile objects: whether a controller runs and
leads, which controller reconciles an object and on what evidence, what
happened to an object and what it owns, and the events about them.

Kubernetes does not record which controller acted on which object, so some of
what tug says is its reading of indirect evidence. Each finding is labelled
observed, when tug read it from the API as it is, or inferred, when it rests on
evidence that could mislead, such as a field manager that is named after a
deployment. Kubernetes keeps no history of reconciles either: trace pieces one
together from timestamps and from events, which the API server keeps for an
hour by default.

Every reconcile command only reads, and none reads a Secret's data.`,
	}
	cmd.AddCommand(newReconcileStatusCmd(a), newReconcileExplainCmd(a), newReconcileTraceCmd(a), newReconcileEventsCmd(a))
	return cmd
}

type reconcileOptions struct {
	namespace string
	output    string
}

func (o *reconcileOptions) flags(cmd *cobra.Command, namespaceHelp string) {
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", namespaceHelp)
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
}

// format parses -o for the reconcile commands, which have no name output.
func (o *reconcileOptions) format(command string) (render.Format, error) {
	f, err := render.ParseFormat(o.output)
	if err != nil {
		return "", err
	}
	if f == render.FormatName {
		return "", fmt.Errorf("reconcile %s supports -o table, json or yaml", command)
	}
	return f, nil
}

func newReconcileStatusCmd(a *app) *cobra.Command {
	var o reconcileOptions
	cmd := &cobra.Command{
		Use:         "status <controller>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show a controller's health, leader election, restarts and the kinds it reconciles",
		Long: `Show the state of a controller: its deployment, statefulset or daemonset (or a
static pod, as kube-controller-manager often is), with the diagnosis tug why
gives; which of its pods holds its leader-election Lease, and how long ago it
renewed it; its containers' restarts; and the kinds it reconciles, as far as its
permissions tell: the kinds whose status its service account may update.

Without -n, a controller that is not in the session's namespace is looked for
in every namespace, since controllers usually run in their own. It exits 1 when
the controller is not healthy or has no leader.`,
		Example: `  tug reconcile status cert-manager                      # wherever it runs
  tug reconcile status deploy/kustomize-controller -n flux-system
  tug reconcile status pod/kube-controller-manager-cp-1 -n kube-system -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := o.format("status")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, o.namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveController(ctx, env, args[0], o.namespace != "")
			if err != nil {
				return err
			}
			clients, _, err := a.objectClients(env)
			if err != nil {
				return err
			}
			st, err := ops.ControllerStatusOf(ctx, clients, t, ops.WhyOptions{
				Now: a.now, ExplicitNamespace: t.Namespace != env.session.State.Namespace,
			})
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				err = render.Structured(a.stdout, format, st)
			} else {
				err = a.printControllerStatus(st)
			}
			if err == nil && !st.Healthy {
				return &exitError{code: 1}
			}
			return err
		},
	}
	o.flags(cmd, "namespace of the controller (default: the session's, then every namespace)")
	return cmd
}

// resolveController turns an argument into the workload or pod of a
// controller. Without -n, controllers in every namespace are candidates
// too, since controllers usually run in their own namespace; a match in
// the session's namespace wins a tie.
func (a *app) resolveController(ctx context.Context, env *opsEnv, arg string, namespaced bool) (resolve.Target, error) {
	q, err := resolve.ParseQuery(arg)
	if err != nil {
		return resolve.Target{}, err
	}
	if err := q.RequireWorkload(); err != nil {
		return resolve.Target{}, err
	}
	candidates, err := resolve.Workloads(ctx, env.cs, env.namespace, q.Kind)
	if err != nil {
		return resolve.Target{}, fmt.Errorf("listing workloads in %s: %w", env.namespace, err)
	}
	for i := range candidates {
		candidates[i].Namespace = env.namespace
	}
	where := "namespace " + env.namespace
	if !namespaced {
		all, err := ops.ControllerTargets(ctx, env.cs)
		switch {
		case err != nil:
			a.log.Debug("not looking for the controller in other namespaces", "err", err)
		default:
			where = "any namespace"
			for _, t := range all {
				if t.Namespace != env.namespace {
					candidates = append(candidates, t)
				}
			}
		}
	}
	matches := resolve.MatchTargets(q, candidates)
	var local []resolve.Target
	for _, m := range matches {
		if m.Namespace == env.namespace {
			local = append(local, m)
		}
	}
	if len(local) > 0 {
		matches = local
	}
	switch len(matches) {
	case 0:
		hint := ""
		if namespaced {
			hint = "; leave out -n to look in every namespace"
		}
		return resolve.Target{}, fmt.Errorf("nothing in %s matches %q%s", where, arg, hint)
	case 1:
		return matches[0], nil
	}
	labels := make([]string, len(matches))
	for i, m := range matches {
		labels[i] = m.String() + " in " + m.Namespace
	}
	i, err := a.pick("Matches for "+arg+":", labels)
	if err != nil {
		return resolve.Target{}, err
	}
	return matches[i], nil
}

func (a *app) printControllerStatus(st *ops.ControllerStatus) error {
	c := a.colors()
	w := a.stdout
	state := c.Red("not healthy")
	if st.Healthy {
		state = c.Green("healthy")
	}
	fmt.Fprintf(w, "%s in %s is %s: %s\n", c.Bold(st.Target), st.Namespace, state, st.Summary)
	fmt.Fprintf(w, "service account %s\n", st.ServiceAccount)

	if len(st.Pods) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(st.Pods))
		for _, p := range st.Pods {
			leader := ""
			if p.Name == st.Leader {
				leader = " (leader)"
			}
			rows = append(rows, []string{
				p.Name + leader, p.Status, fmt.Sprintf("%d/%d", p.ReadyContainers, p.Containers),
				strconv.Itoa(int(p.Restarts)), cmp.Or(p.Node, "<none>"), a.age(p.Created),
			})
		}
		if err := render.Table(w, []string{"POD", "STATUS", "READY", "RESTARTS", "NODE", "AGE"}, rows); err != nil {
			return err
		}
	}

	if len(st.Leases) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(st.Leases))
		for _, l := range st.Leases {
			renewed := "-"
			if !l.Renewed.IsZero() {
				renewed = a.age(l.Renewed) + " ago"
				if l.Expired {
					renewed += ", expired"
				}
			}
			duration := "-"
			if l.DurationSeconds > 0 {
				duration = fmt.Sprintf("%ds", l.DurationSeconds)
			}
			rows = append(rows, []string{"lease/" + l.Name, cmp.Or(l.Holder, "<none>"), renewed, duration,
				strconv.Itoa(int(l.Transitions)), string(l.Basis)})
		}
		if err := render.Table(w, []string{"LEASE", "HOLDER", "RENEWED", "DURATION", "CHANGES", "BASIS"}, rows); err != nil {
			return err
		}
	}

	if len(st.Restarts) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(st.Restarts))
		for _, r := range st.Restarts {
			exit, ended := "-", "-"
			if r.LastReason != "" || r.LastExitCode != 0 {
				exit = fmt.Sprintf("%d (%s)", r.LastExitCode, cmp.Or(r.LastReason, "exited"))
			}
			if !r.LastFinished.IsZero() {
				ended = a.age(r.LastFinished) + " ago"
			}
			rows = append(rows, []string{r.Pod, r.Container, strconv.Itoa(int(r.Count)), exit, ended})
		}
		if err := render.Table(w, []string{"POD", "CONTAINER", "RESTARTS", "LAST EXIT", "ENDED"}, rows); err != nil {
			return err
		}
	}

	if len(st.Reconciles) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Bold("Reconciles, as its permissions tell (inferred):"))
		rows := make([][]string, 0, len(st.Reconciles))
		for _, k := range st.Reconciles {
			rows = append(rows, []string{cmp.Or(k.Kind, "-"), k.Resource, k.Evidence})
		}
		if err := render.Table(w, []string{"KIND", "RESOURCE", "EVIDENCE"}, rows); err != nil {
			return err
		}
	}

	a.printClaims(st.Findings, st.Healthy)
	return a.printEvents(st.Events)
}

func newReconcileExplainCmd(a *app) *cobra.Command {
	var o reconcileOptions
	cmd := &cobra.Command{
		Use:         "explain <resource>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explain how an object is reconciled: by which controller, and why it is stuck",
		Long: `Explain how any object is reconciled. explain starts from tug why's diagnosis of
the object (its conditions, whether its controller has caught up with its latest
generation, what it owns, recent warning events), then looks for the controller
that reconciles it: the field managers that wrote its status (managedFields),
who reported events about it, the service accounts whose roles let them update
its status, its ownerReferences, and the operators that usually serve its API
group. For the likeliest controller it finds a workload for, it checks that the
workload runs and holds its leader Lease, and says what that means for the
object.

Each finding and each piece of evidence is labelled observed or inferred. It
exits 1 when the object is not healthy, as why does.`,
		Example: `  tug reconcile explain certificate/web-tls
  tug reconcile explain kustomization.kustomize.toolkit.fluxcd.io/apps -n flux-system
  tug reconcile explain db/orders -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := o.format("explain")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			env, clients, obj, err := a.reconcileObject(ctx, o.namespace, args[0])
			if err != nil {
				return err
			}
			ex, err := ops.Explain(ctx, clients, obj, ops.WhyOptions{
				Now: a.now, ExplicitNamespace: env.namespace != env.session.State.Namespace,
			})
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				err = render.Structured(a.stdout, format, ex)
			} else {
				err = a.printExplanation(ex)
			}
			if err == nil && !ex.Healthy {
				return &exitError{code: 1}
			}
			return err
		},
	}
	o.flags(cmd, "namespace (default: the session's)")
	return cmd
}

// reconcileObject resolves the object a reconcile command is about.
func (a *app) reconcileObject(ctx context.Context, namespace, arg string) (*opsEnv, ops.Clients, resolve.Object, error) {
	env, err := a.opsEnv(ctx, namespace)
	if err != nil {
		return nil, ops.Clients{}, resolve.Object{}, err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return nil, ops.Clients{}, resolve.Object{}, err
	}
	o, err := a.resolveObject(ctx, env, clients, mapper, arg)
	return env, clients, o, err
}

// generationLine says whether an object's controller has caught up with
// its latest generation, or "" when the object does not say.
func generationLine(gen, seen int64) string {
	switch {
	case gen == 0 || seen == 0:
		return ""
	case seen < gen:
		return fmt.Sprintf("generation %d, last reconciled at %d: its controller has not caught up", gen, seen)
	default:
		return fmt.Sprintf("generation %d, reconciled", gen)
	}
}

func (a *app) printExplanation(ex *ops.Explanation) error {
	c := a.colors()
	w := a.stdout
	state := c.Red("not ready")
	if ex.Healthy {
		state = c.Green("healthy")
	}
	where := ""
	if ex.Namespace != "" {
		where = " in " + ex.Namespace
	}
	fmt.Fprintf(w, "%s%s is %s: %s\n", c.Bold(ex.Target), where, state, ex.Summary)
	if line := generationLine(ex.Generation, ex.ObservedGeneration); line != "" {
		fmt.Fprintln(w, line)
	}
	for _, owner := range ex.Owners {
		role := ""
		if owner.Controller {
			role = " (its controller)"
		}
		fmt.Fprintf(w, "owned by %s%s\n", owner.Object, role)
	}

	if len(ex.Conditions) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(ex.Conditions))
		for _, cond := range ex.Conditions {
			since := "-"
			if !cond.LastTransition.IsZero() {
				since = a.age(cond.LastTransition)
			}
			rows = append(rows, []string{cond.Type, cond.Status, cmp.Or(cond.Reason, "-"), since})
		}
		if err := render.Table(w, []string{"CONDITION", "STATUS", "REASON", "SINCE"}, rows); err != nil {
			return err
		}
	}

	if len(ex.Controllers) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Bold("Controllers that may reconcile it, likeliest first:"))
		for _, cand := range ex.Controllers {
			name := cand.Name
			if cand.Workload != "" {
				name = cand.Workload + " in " + cand.Namespace
			}
			state := ""
			if cand.Healthy != nil {
				state = " is " + c.Red("not healthy")
				if *cand.Healthy {
					state = " is " + c.Green("healthy")
				}
				state += ": " + cand.Summary
			}
			fmt.Fprintf(w, "  %s%s %s\n", c.Bold(name), state, c.Dim("("+string(cand.Basis)+")"))
			for _, e := range cand.Evidence {
				fmt.Fprintf(w, "    %s %s\n", basisLabel(c, e.Basis), e.Text)
			}
		}
	}

	if len(ex.Managers) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(ex.Managers))
		for _, m := range ex.Managers {
			wrote := cmp.Or(strings.Join(m.Fields, ", "), "-")
			if m.Subresource != "" && !slices.Contains(m.Fields, m.Subresource) {
				wrote += " (through " + m.Subresource + ")"
			}
			last := "-"
			if !m.Time.IsZero() {
				last = a.age(m.Time) + " ago"
			}
			rows = append(rows, []string{m.Manager, m.Operation, wrote, last})
		}
		if err := render.Table(w, []string{"FIELD MANAGER", "OPERATION", "WROTE", "LAST WRITE"}, rows); err != nil {
			return err
		}
	}

	a.printClaims(ex.Findings, ex.Healthy)
	return a.printEvents(ex.Events)
}

func newReconcileTraceCmd(a *app) *cobra.Command {
	var (
		o          reconcileOptions
		logs       bool
		controller string
		since      time.Duration
	)
	cmd := &cobra.Command{
		Use:         "trace <resource>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Show a timeline of what happened to an object and what it owns",
		Long: `Show a timeline of an object, oldest first: when it was created or marked for
deletion, when each field manager last wrote it, when each status condition
last changed, when what it owns was created or marked for deletion, when the
containers of pods it owns started and exited, the events about all of it and,
with --logs, the lines its controller logged that mention its name or UID.

` + strings.Join(render.Wrap(ops.NoReconcileHistory, 80), "\n") + `

Each entry says where it comes from. A log line that has the object's UID is
observed; one that has only its name is marked inferred, since another object
may share the name. The controller whose logs are read is the one named with
--controller or, without it, the likeliest that tug reconcile explain finds.`,
		Example: `  tug reconcile trace kustomization.kustomize.toolkit.fluxcd.io/apps -n flux-system
  tug reconcile trace certificate/web-tls --logs
  tug reconcile trace db/orders --controller deploy/db-operator --since 2h
  tug reconcile trace deploy/api -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := o.format("trace")
			if err != nil {
				return err
			}
			if since <= 0 {
				return errors.New("--since wants a positive duration, such as 30m")
			}
			ctx := cmd.Context()
			env, clients, obj, err := a.reconcileObject(ctx, o.namespace, args[0])
			if err != nil {
				return err
			}
			opts := ops.TraceOptions{Logs: logs || controller != "", Since: since}
			if controller != "" {
				t, err := a.resolveController(ctx, env, controller, false)
				if err != nil {
					return fmt.Errorf("finding the controller: %w", err)
				}
				opts.Controller = &t
			}
			tl, err := ops.Trace(ctx, clients, obj, opts)
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, tl)
			}
			return a.printTimeline(tl)
		},
	}
	o.flags(cmd, "namespace (default: the session's)")
	cmd.Flags().BoolVar(&logs, "logs", false, "add the lines the controller logged that mention the object")
	cmd.Flags().StringVar(&controller, "controller", "", "the controller whose logs to read, such as deploy/db-operator (implies --logs; default: the likeliest)")
	cmd.Flags().DurationVar(&since, "since", time.Hour, "how far back to read the controller's logs")
	return cmd
}

func (a *app) printTimeline(tl *ops.Timeline) error {
	c := a.colors()
	w := a.stdout
	head := c.Bold(tl.Target)
	if tl.Namespace != "" {
		head += " in " + tl.Namespace
	}
	if line := generationLine(tl.Generation, tl.ObservedGeneration); line != "" {
		head += ": " + line
	}
	fmt.Fprintln(w, head)
	if tl.Controller != "" {
		how := "as named"
		if tl.ControllerBasis == ops.BasisInferred {
			how = "the likeliest controller (inferred)"
		}
		fmt.Fprintf(w, "logs from %s in %s, %s\n", tl.Controller, tl.ControllerNamespace, how)
	}
	fmt.Fprintln(w)
	rows := make([][]string, 0, len(tl.Entries))
	for _, e := range tl.Entries {
		what := e.Message
		if e.Source == ops.SourceEvent {
			what = e.Type + " " + e.Reason + ": " + e.Message
			if e.Count > 1 {
				what += fmt.Sprintf(" (x%d", e.Count)
				if !e.Until.IsZero() {
					what += ", last " + a.age(e.Until) + " ago"
				}
				what += ")"
			}
		}
		if e.Basis == ops.BasisInferred {
			what = c.Dim("(inferred)") + " " + what
		}
		rows = append(rows, []string{a.age(e.Time), string(e.Source), e.Object, what})
	}
	if err := render.Table(w, []string{"AGE", "SOURCE", "OBJECT", "WHAT"}, rows); err != nil {
		return err
	}
	for _, n := range tl.Notes {
		writeIndented(a.stderr, a.colors().Dim("note:")+" ", "      ", n, render.Width(a.stderr))
	}
	return nil
}

type reconcileEventsOptions struct {
	reconcileOptions
	follow bool
}

func newReconcileEventsCmd(a *app) *cobra.Command {
	var o reconcileEventsOptions
	cmd := &cobra.Command{
		Use:         "events <resource>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "List the events about an object and everything it owns, or follow them",
		Long: `List the events about an object and everything it owns through ownerReferences,
oldest first, with who reported each. With -f, keep following new events,
including those about objects it comes to own, such as the pods of a new
rollout, until Ctrl-C. While following, each line starts with the time the
event last happened.

The API server keeps events for an hour by default, so older ones are gone.`,
		Example: `  tug reconcile events db/orders
  tug reconcile events deploy/api -f
  tug reconcile events certificate/web-tls -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := o.format("events")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			_, clients, obj, err := a.reconcileObject(ctx, o.namespace, args[0])
			if err != nil {
				return err
			}
			feed, err := ops.NewEventFeed(ctx, clients, obj)
			if err != nil {
				return err
			}
			if len(feed.Unlisted) > 0 {
				fmt.Fprintf(a.stderr, "%s could not list %s, so events about anything of those kinds it owns are missing\n",
					a.colors().Yellow("warning:"), strings.Join(feed.Unlisted, ", "))
			}
			if !o.follow {
				if format != render.FormatTable {
					return render.Structured(a.stdout, format, feed.Records)
				}
				return a.printEventRecords(feed.Records)
			}
			send := func(r ops.EventRecord) {
				if err := a.writeEventRecord(format, r); err != nil {
					a.log.Debug("writing an event", "err", err)
				}
			}
			for _, r := range feed.Records {
				send(r)
			}
			err = feed.Follow(ctx, send)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	o.flags(cmd, "namespace (default: the session's)")
	cmd.Flags().BoolVarP(&o.follow, "follow", "f", false, "keep following new events until Ctrl-C")
	return cmd
}

func (a *app) printEventRecords(records []ops.EventRecord) error {
	if len(records) == 0 {
		fmt.Fprintln(a.stderr, "No events; the API server keeps them for an hour by default.")
		return nil
	}
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		seen := a.age(r.LastSeen)
		if r.Count > 1 {
			seen += fmt.Sprintf(" (x%d)", r.Count)
		}
		rows = append(rows, []string{seen, r.Type, r.Object, r.Reason, cmp.Or(r.Reporter, "-"), r.Message})
	}
	return render.Table(a.stdout, []string{"LAST SEEN", "TYPE", "OBJECT", "REASON", "FROM", "MESSAGE"}, rows)
}

// writeEventRecord writes one followed event: a line starting with the time
// it last happened, or a JSON or YAML document.
func (a *app) writeEventRecord(format render.Format, r ops.EventRecord) error {
	switch format {
	case render.FormatJSON:
		return render.Structured(a.stdout, format, r)
	case render.FormatYAML:
		if _, err := fmt.Fprintln(a.stdout, "---"); err != nil {
			return err
		}
		return render.Structured(a.stdout, format, r)
	}
	c := a.colors()
	typ := fmt.Sprintf("%-7s", r.Type)
	if r.Type == "Warning" {
		typ = c.Yellow(typ)
	}
	count := ""
	if r.Count > 1 {
		count = fmt.Sprintf(" (x%d)", r.Count)
	}
	_, err := fmt.Fprintf(a.stdout, "%s  %s  %s  %s: %s%s\n",
		c.Dim(r.LastSeen.Local().Format(time.TimeOnly)), typ, r.Object, c.Bold(r.Reason), r.Message, count)
	return err
}

// age is how long ago t was, as kubectl writes ages.
func (a *app) age(t time.Time) string { return duration.HumanDuration(a.now().Sub(t)) }

// printClaims writes findings with how tug knows each, or that nothing is
// wrong.
func (a *app) printClaims(cs []ops.Claim, healthy bool) {
	c := a.colors()
	width := render.Width(a.stdout)
	for _, cl := range cs {
		fmt.Fprintln(a.stdout)
		printClaim(a.stdout, c, width, cl)
	}
	if healthy && len(cs) == 0 {
		fmt.Fprintf(a.stdout, "\n%s\n", c.Green("Nothing wrong found."))
	}
}

// printClaim writes a finding as printFinding does, with its basis after
// its reason.
func printClaim(w io.Writer, c render.Colors, width int, cl ops.Claim) {
	f := cl.Finding
	subject := objectList(f.Objects)
	if f.Container != "" {
		subject += ", container " + f.Container
	}
	fmt.Fprintf(w, "%s %s: %s %s\n", severity(c, f.Severity), subject, c.Bold(f.Reason), c.Dim("("+string(cl.Basis)+")"))
	writeIndented(w, findingIndent, findingIndent, f.Message, width)
	if f.Next == "" {
		return
	}
	text, cmd := trailingCommand(f.Next)
	label := findingIndent + c.Dim("next:") + " "
	under := findingIndent + "      "
	if text == "" {
		fmt.Fprintf(w, "%s%s\n", label, c.Cyan(cmd))
		return
	}
	writeIndented(w, label, under, inlineCommands(c, text), width)
	if cmd != "" {
		fmt.Fprintf(w, "%s%s\n", under, c.Cyan(cmd))
	}
}

// basisLabel pads and colours a basis for the column it starts.
func basisLabel(c render.Colors, b ops.Basis) string {
	label := fmt.Sprintf("%-8s", b)
	if b == ops.BasisInferred {
		return c.Yellow(label)
	}
	return c.Dim(label)
}

// printEvents writes recent Warning events as why does.
func (a *app) printEvents(events []ops.Event) error {
	if len(events) == 0 {
		return nil
	}
	fmt.Fprintf(a.stdout, "\n%s\n", a.colors().Bold("Recent warning events:"))
	rows := make([][]string, 0, len(events))
	for _, e := range events {
		seen := a.age(e.LastSeen)
		if e.Count > 1 {
			seen += fmt.Sprintf(" (x%d)", e.Count)
		}
		rows = append(rows, []string{seen, e.Object, e.Reason, e.Message})
	}
	return render.Table(a.stdout, []string{"LAST SEEN", "OBJECT", "REASON", "MESSAGE"}, rows)
}
