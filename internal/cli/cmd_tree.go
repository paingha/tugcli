package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

type treeOptions struct {
	namespace string
	output    string
}

func newTreeCmd(a *app) *cobra.Command {
	var o treeOptions
	cmd := &cobra.Command{
		Use:   "tree <target>",
		Short: "Show a workload's ownership tree with the health of each part",
		Long: `Show what a workload is made of, as k9s's xray does: its ReplicaSets (for a
deployment) or ControllerRevisions (for a statefulset or daemonset), their
pods and containers, the Services that select the pods, and the ConfigMaps,
Secrets and PersistentVolumeClaims the pods use. Each object is marked ready
(✓), not ready (✗) or unknown (?) with a short status. tree only reads, and
shows only the key names of Secrets.`,
		Example: `  tug tree api
  tug tree sts/db -o yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(o.output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("tree supports -o table, json or yaml")
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
			root, err := ops.Tree(ctx, env.cs, t)
			if err != nil {
				return err
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, root)
			}
			tree := a.treeLines(*root)
			tree.Label = a.colors().Bold(tree.Label)
			return render.Tree(a.stdout, tree)
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "output format: table, json or yaml")
	return cmd
}

// treeKinds are the short names tree labels objects with.
var treeKinds = map[string]string{
	"Deployment":            "deploy",
	"StatefulSet":           "sts",
	"DaemonSet":             "ds",
	"Job":                   "job",
	"ReplicaSet":            "rs",
	"ControllerRevision":    "controllerrevision",
	"Pod":                   "pod",
	"Service":               "svc",
	"ConfigMap":             "cm",
	"Secret":                "secret",
	"PersistentVolumeClaim": "pvc",
}

// treeLines turns a workload's tree into lines with a health marker.
func (a *app) treeLines(n ops.TreeNode) render.TreeNode {
	c := a.colors()
	var label string
	switch n.Kind {
	case ops.KindContainer:
		label = "container " + n.Name
	case ops.KindInitContainer:
		label = "init container " + n.Name
	case ops.KindEphemeralContainer:
		label = "ephemeral container " + n.Name
	default:
		label = treeKinds[n.Kind] + "/" + n.Name
	}
	marker := c.Yellow("?")
	switch n.Health {
	case ops.HealthReady:
		marker = c.Green("✓")
	case ops.HealthNotReady:
		marker = c.Red("✗")
	}
	out := render.TreeNode{Label: label, Detail: marker + " " + n.Status}
	for _, child := range n.Children {
		out.Children = append(out.Children, a.treeLines(child))
	}
	return out
}
