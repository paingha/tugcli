package cli

import (
	"errors"
	"fmt"
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
}

func newWhyCmd(a *app) *cobra.Command {
	var o whyOptions
	cmd := &cobra.Command{
		Use:         "why <target>",
		Annotations: acrossClusters(acrossInProcess),
		Short:       "Explain why a workload or pod is not ready, and what to do next",
		Long: `Explain why a deployment, statefulset, daemonset, job or pod is not ready: its
rollout; its pods' phases and conditions; containers that crash (with the last
exit code), cannot pull their image, run out of memory or miss a ConfigMap or
Secret; failing readiness and liveness probes; pods no node can take, and why;
volume claims stuck in Pending; and recent Warning events. Each finding has a
severity and a next step. why only reads. It exits 1 when the workload is not
healthy, as doctor does when a check fails, so scripts can wait on it.`,
		Example: `  tug why api
  tug why sts/db -n data
  tug why job/migrate -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("why supports -o table, json or yaml")
			}
			ctx := cmd.Context()
			env, err := a.opsEnv(ctx, o.namespace)
			if err != nil {
				return err
			}
			t, err := a.resolveTarget(ctx, env, args[0])
			if err != nil {
				return err
			}
			d, err := ops.Why(ctx, env.cs, t, ops.WhyOptions{
				Now:               a.now,
				ExplicitNamespace: env.namespace != env.session.State.Namespace,
			})
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				err = render.Structured(a.stdout, format, d)
			} else {
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
	return cmd
}

func (a *app) printWhy(d *ops.Diagnosis) error {
	c := a.colors()
	w := a.stdout
	state := c.Red("not ready")
	if d.Healthy {
		state = c.Green("healthy")
	}
	fmt.Fprintf(w, "%s in %s is %s: %s\n", c.Bold(d.Target), d.Namespace, state, d.Summary)

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

	if len(d.Findings) > 0 {
		fmt.Fprintln(w)
	}
	const indent = "         " // under the severity column
	for _, f := range d.Findings {
		subject := objectList(f.Objects)
		if f.Container != "" {
			subject += ", container " + f.Container
		}
		fmt.Fprintf(w, "%s %s: %s\n", severity(c, f.Severity), subject, c.Bold(f.Reason))
		fmt.Fprintf(w, "%s%s\n", indent, f.Message)
		if f.Next != "" {
			fmt.Fprintf(w, "%s%s %s\n", indent, c.Dim("next:"), f.Next)
		}
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
