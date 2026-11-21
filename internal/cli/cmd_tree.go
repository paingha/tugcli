package cli

import (
	"errors"
	"fmt"
	"strings"

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
		Short: "Show what a workload or any other object owns, with the health of each part",
		Long: `Show what a workload is made of, as k9s's xray does: its ReplicaSets (for a
deployment) or ControllerRevisions (for a statefulset or daemonset), their
pods and containers, the Services that select the pods, and the ConfigMaps,
Secrets and PersistentVolumeClaims the pods use.

Any other kind works too, named as kubectl names it, such as certificate/web-tls
or cronjob/nightly-backup: tree shows everything it owns through
ownerReferences, in its namespace or, for a cluster-scoped object, the whole
cluster, with each workload it owns shown as above. Other objects' health comes
from their status conditions.

Each object is marked ready (✓), not ready (✗), unknown (?) or, when it reports
no health of its own, as a ConfigMap does, with a dot (·), and has a short
status. tree only reads, and shows only the key names of Secrets.`,
		Example: `  tug tree api
  tug tree sts/db -o yaml
  tug tree kustomization.kustomize.toolkit.fluxcd.io/apps`,
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
			clients, mapper, err := a.objectClients(env)
			if err != nil {
				return err
			}
			o, err := a.resolveObject(ctx, env, clients, mapper, args[0])
			if err != nil {
				return err
			}
			root, unlisted, err := ops.ObjectTree(ctx, clients, o)
			if err != nil {
				return err
			}
			if len(unlisted) > 0 {
				fmt.Fprintf(a.stderr, "%s could not list %s, so anything of those kinds it owns is missing\n",
					a.colors().Yellow("warning:"), strings.Join(unlisted, ", "))
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
		kind, ok := treeKinds[n.Kind]
		if !ok {
			kind = strings.ToLower(n.Kind)
		}
		label = kind + "/" + n.Name
	}
	marker := c.Yellow("?")
	switch n.Health {
	case ops.HealthReady:
		marker = c.Green("✓")
	case ops.HealthNotReady:
		marker = c.Red("✗")
	case ops.HealthNone:
		marker = c.Dim("·")
	}
	out := render.TreeNode{Label: label, Detail: marker + " " + n.Status}
	for _, child := range n.Children {
		out.Children = append(out.Children, a.treeLines(child))
	}
	return out
}
