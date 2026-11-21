package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

type whyOptions struct {
	namespace string
	output    string
	deep      bool
	yes       bool
	provider  string
	model     string
}

func newWhyCmd(a *app) *cobra.Command {
	var o whyOptions
	cmd := &cobra.Command{
		Use:         "why <target>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explain why a workload, pod or any other object is not ready, and what to do next",
		Long: `Explain why a deployment, statefulset, daemonset, job or pod is not ready: its
rollout; its pods' phases and conditions; containers that crash (with the last
exit code), cannot pull their image, run out of memory or miss a ConfigMap or
Secret; failing readiness and liveness probes; pods no node can take, and why;
volume claims stuck in Pending; and recent Warning events. Each finding has a
severity and a next step.

Any other kind works too, named as kubectl names it: a custom resource such as
certificate/web-tls or kustomization.kustomize.toolkit.fluxcd.io/apps, a
cronjob or a node. why reads its status conditions, checks that its controller
has caught up with its latest change, and looks at everything it owns through
ownerReferences, with the diagnosis above for each workload it owns, and at
recent Warning events about all of it.

why only reads, and never reads a Secret's data. It exits 1 when the target is
not healthy, as doctor does when a check fails, so scripts can wait on it.

--deep, once turned on in config.yaml (intelligence.enabled), also asks a
model for an explanation: Claude, OpenAI, Mistral or any OpenAI-compatible
endpoint. tug collects bounded evidence (the diagnosis, what the target owns,
recent events and the last lines of failing containers' logs), replaces
secrets in it with placeholders, and shows it, with the estimated cost, before
asking to send it. The answer separates facts, each citing the evidence, from
hypotheses, suggests next steps that only read, and lists any change apart, as
needing your approval; tug runs nothing it suggests. See tug intelligence for
keys, prices and usage.`,
		Example: `  tug why api
  tug why sts/db -n data
  tug why job/migrate -o json
  tug why certificate/web-tls          # a custom resource and what it owns
  tug why cronjob/nightly-backup       # judged by its newest job
  tug why billing --deep               # show the evidence and cost, ask, then explain
  tug why billing --deep --provider openai --model gpt-6.2-sol`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("why supports -o table, json or yaml")
			}
			deep, err := a.prepareDeep(o)
			if err != nil {
				return err
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
			d, err := ops.WhyObject(ctx, clients, obj, ops.WhyOptions{
				Now:               a.now,
				ExplicitNamespace: env.namespace != env.session.State.Namespace,
			})
			if err != nil {
				return err
			}
			switch {
			case deep != nil:
				err = a.whyDeep(ctx, env, clients, obj, d, format, deep)
			case format != render.FormatTable:
				err = render.Structured(a.stdout, format, d)
			default:
				err = a.printWhy(d)
			}
			if err == nil && !d.Healthy {
				return &exitError{code: 1}
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().BoolVar(&o.deep, "deep", false, "also ask a model to explain the evidence, after showing it and asking (needs intelligence.enabled in config.yaml)")
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "with --deep, send the evidence without asking")
	cmd.Flags().StringVar(&o.provider, "provider", "", "with --deep, the provider to ask: anthropic, openai, mistral or custom (default: intelligence.provider)")
	cmd.Flags().StringVar(&o.model, "model", "", "with --deep, the model to ask (default: intelligence.model, or the provider's default)")
	return cmd
}

func (a *app) printWhy(d *ops.Diagnosis) error {
	c := a.colors()
	w := a.stdout
	state := c.Red("not ready")
	if d.Healthy {
		state = c.Green("healthy")
	}
	where := ""
	if d.Namespace != "" {
		where = " in " + d.Namespace
	}
	fmt.Fprintf(w, "%s%s is %s: %s\n", c.Bold(d.Target), where, state, d.Summary)

	if len(d.Conditions) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(d.Conditions))
		for _, cond := range d.Conditions {
			since := "-"
			if !cond.LastTransition.IsZero() {
				since = duration.HumanDuration(a.now().Sub(cond.LastTransition))
			}
			rows = append(rows, []string{cond.Type, cond.Status, cmp.Or(cond.Reason, "-"), since})
		}
		if err := render.Table(w, []string{"CONDITION", "STATUS", "REASON", "SINCE"}, rows); err != nil {
			return err
		}
	}

	if len(d.Pods) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(d.Pods))
		for _, p := range d.Pods {
			node := p.Node
			if node == "" {
				node = "<none>"
			}
			rows = append(rows, []string{
				p.Name, p.Status, fmt.Sprintf("%d/%d", p.ReadyContainers, p.Containers),
				strconv.Itoa(int(p.Restarts)), node, duration.HumanDuration(a.now().Sub(p.Created)),
			})
		}
		if err := render.Table(w, []string{"POD", "STATUS", "READY", "RESTARTS", "NODE", "AGE"}, rows); err != nil {
			return err
		}
	}

	width := render.Width(w)
	for _, f := range d.Findings {
		fmt.Fprintln(w)
		printFinding(w, c, width, f)
	}
	if d.Healthy && len(d.Findings) == 0 {
		fmt.Fprintf(w, "\n%s\n", c.Green("Nothing wrong found."))
	}

	if len(d.Events) > 0 {
		fmt.Fprintf(w, "\n%s\n", c.Bold("Recent warning events:"))
		rows := make([][]string, 0, len(d.Events))
		for _, e := range d.Events {
			seen := duration.HumanDuration(a.now().Sub(e.LastSeen))
			if e.Count > 1 {
				seen += fmt.Sprintf(" (x%d)", e.Count)
			}
			rows = append(rows, []string{seen, e.Object, e.Reason, e.Message})
		}
		return render.Table(w, []string{"LAST SEEN", "OBJECT", "REASON", "MESSAGE"}, rows)
	}
	return nil
}

// findingIndent lines a finding's message and next step up under the text
// after its severity.
const findingIndent = "         "

// printFinding writes one finding: its severity, subject and reason, then
// its message and next step beneath. On a terminal, the text wraps to the
// window under its own indent. A command that ends the next step gets a line
// of its own, without the backticks it has in JSON and YAML, so that it is
// never broken across lines and copies straight into a shell.
func printFinding(w io.Writer, c render.Colors, width int, f ops.Finding) {
	subject := objectList(f.Objects)
	if f.Container != "" {
		subject += ", container " + f.Container
	}
	fmt.Fprintf(w, "%s %s: %s\n", severity(c, f.Severity), subject, c.Bold(f.Reason))
	writeIndented(w, findingIndent, findingIndent, f.Message, width)
	if f.Next == "" {
		return
	}
	text, cmd := trailingCommand(f.Next)
	label := findingIndent + c.Dim("next:") + " "
	under := findingIndent + "      " // under the text after "next: "
	if text == "" {
		fmt.Fprintf(w, "%s%s\n", label, c.Cyan(cmd))
		return
	}
	writeIndented(w, label, under, inlineCommands(c, text), width)
	if cmd != "" {
		fmt.Fprintf(w, "%s%s\n", under, c.Cyan(cmd))
	}
}

// writeIndented writes s after first, wrapped to width with later lines
// after rest. Both prefixes are the same width once colour is left out.
func writeIndented(w io.Writer, first, rest, s string, width int) {
	if width > 0 {
		width -= len(rest)
		width = max(width, 30) // a very narrow window gets long lines instead
	}
	for i, line := range render.Wrap(s, width) {
		prefix := rest
		if i == 0 {
			prefix = first
		}
		fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
}

// trailingCommand splits a next step that ends with a command in backticks,
// such as "read its logs: `tug logs pod/x`", into its text and the command.
// A next step that does not end with one is all text.
func trailingCommand(next string) (text, cmd string) {
	if !strings.HasSuffix(next, "`") {
		return next, ""
	}
	open := strings.LastIndexByte(next[:len(next)-1], '`')
	if open < 0 {
		return next, ""
	}
	return strings.TrimSpace(next[:open]), next[open+1 : len(next)-1]
}

// inlineCommands shows the commands in backticks inside a sentence in the
// command colour, dropping the backticks, which then only get in the way.
// Without colour, the backticks stay to mark where each command starts and
// ends.
func inlineCommands(c render.Colors, s string) string {
	if !c.Enabled {
		return s
	}
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 {
		return s // an unmatched backtick: leave the text as it is
	}
	for i := 1; i < len(parts); i += 2 {
		parts[i] = c.Cyan(parts[i])
	}
	return strings.Join(parts, "")
}

// severity pads and colours a finding's severity for the column it starts.
func severity(c render.Colors, s ops.Severity) string {
	label := fmt.Sprintf("%-8s", s)
	switch s {
	case ops.SeverityError:
		return c.Red(label)
	case ops.SeverityWarning:
		return c.Yellow(label)
	default:
		return c.Dim(label)
	}
}

// objectList names up to three objects and counts the rest.
func objectList(objects []string) string {
	const show = 3
	if len(objects) <= show {
		return strings.Join(objects, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(objects[:show], ", "), len(objects)-show)
}
