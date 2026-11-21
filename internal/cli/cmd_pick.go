package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/alias"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/tui"
)

// objectActions are what tug pick and tug bookmark open run on an object,
// in the order the picker offers them.
var objectActions = []string{"why", "tree", "logs", "describe", "pf"}

// objectActionKeys choose an action in the picker.
var objectActionKeys = map[string]string{"why": "w", "tree": "t", "logs": "l", "describe": "d", "pf": "p"}

// objectAction returns the command line that runs an action on an object,
// in the object's namespace, with extra words after it: tug why, tree,
// logs or pf, or kubectl describe. The object is named so that it resolves
// to itself and nothing else.
func objectAction(action string, o resolve.Object, extra []string) ([]string, error) {
	var line []string
	switch action {
	case "why", "tree", "describe":
		line = []string{action, o.Ref()}
	case "logs", "pf":
		t, ok := o.Workload()
		if !ok {
			return nil, fmt.Errorf("%s works on deployments, statefulsets, daemonsets, jobs and pods, not on %s", action, o)
		}
		line = []string{action, t.String()}
	default:
		return nil, fmt.Errorf("unknown action %q; choose %s", action, joinOr(objectActions))
	}
	if o.Namespace != "" {
		line = append(line, "-n", o.Namespace)
	}
	return append(line, extra...), nil
}

// joinOr lists items as "a, b or c".
func joinOr(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// pickKinds are what tug pick lists without a kind.
var pickKinds = []string{"deployments", "statefulsets", "daemonsets", "cronjobs", "jobs", "pods"}

type pickOptions struct {
	namespace     string
	allNamespaces bool
	action        string
	query         string
}

func newPickCmd(a *app) *cobra.Command {
	var o pickOptions
	cmd := &cobra.Command{
		Use:   "pick [kind]",
		Short: "Fuzzy-pick an object, then run why, tree, logs, describe or pf on it",
		Long: `Fuzzy-pick an object from a full-screen list of the namespace's objects, with
each one's kind, namespace, name, status, age and owner, and the cluster and
namespace always at the top. Without a kind, tug pick lists deployments,
statefulsets, daemonsets, cronjobs, jobs and pods; any kind works, named as
kubectl names it.

Type to filter: each word must match the name as tug's targets match, or be
found in the kind, namespace, status or owner, so "api crash" finds the api
pods in CrashLoopBackOff. enter chooses an object, then w, t, l, d or p runs
tug why, tree, logs, kubectl describe or tug pf on it.

Without a terminal, --query must match exactly one object, which --action
(default why) then runs on; otherwise tug pick lists the matches and fails,
as other commands do when a target is ambiguous.`,
		Example: `  tug pick
  tug pick pods -A
  tug pick certificates --action why
  tug pick deploy --query api --action logs   # works in scripts too`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.action != "" && !slices.Contains(objectActions, o.action) {
				return fmt.Errorf("unknown action %q; choose %s", o.action, joinOr(objectActions))
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
			ns := env.namespace
			if o.allNamespaces {
				ns = ""
			}
			kinds := pickKinds
			if len(args) == 1 {
				kinds = args
			}
			var objects []ops.ObjectState
			var unlisted []error
			for _, kind := range kinds {
				m, err := resolve.FindResource(mapper, kind)
				if err == nil {
					var listed []ops.ObjectState
					if listed, err = ops.ListObjects(ctx, clients, m, ns); err == nil {
						objects = append(objects, listed...)
						continue
					}
					err = fmt.Errorf("listing %s: %w", m.Resource.GroupResource(), err)
				}
				if len(args) == 1 {
					return err
				}
				a.log.Debug("leaving a kind out of tug pick", "kind", kind, "err", err)
				unlisted = append(unlisted, err)
			}
			where := "namespace " + env.namespace
			if o.allNamespaces {
				where = "any namespace"
			}
			if len(objects) == 0 {
				if len(unlisted) == len(kinds) {
					return errors.Join(unlisted...) // nothing could be listed, which says why
				}
				if o.allNamespaces {
					return fmt.Errorf("nothing to pick in %s on %s", where, env.session.State.ClusterName)
				}
				return fmt.Errorf("nothing to pick in %s on %s; use -n for another namespace, or -A for every one", where, env.session.State.ClusterName)
			}

			var picked tui.Picked
			if a.interactive() {
				picked, err = a.pickInteractively(cmd, env.session.State.ClusterName, ns, objects, o)
				if errors.Is(err, tui.ErrPickCancelled) {
					return nil
				}
			} else {
				picked, err = a.pickByQuery(where, objects, o)
			}
			if err != nil {
				return err
			}
			line, err := objectAction(picked.Action, picked.Object.Object, nil)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim("tug "+alias.Join(line)))
			return a.execute(ctx, line)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "list objects in every namespace")
	f.StringVar(&o.action, "action", "", "run this on the object chosen: why, tree, logs, describe or pf (default: ask; why without a terminal)")
	f.StringVar(&o.query, "query", "", "start with this filter; without a terminal, it must match one object")
	return cmd
}

// pickInteractively shows the picker on the terminal, on stderr as prompts
// are.
func (a *app) pickInteractively(cmd *cobra.Command, cluster, ns string, objects []ops.ObjectState, o pickOptions) (tui.Picked, error) {
	names := objectActions
	if o.action != "" {
		names = []string{o.action}
	}
	actions := make([]tui.PickAction, len(names))
	for i, n := range names {
		actions[i] = tui.PickAction{Key: objectActionKeys[n], Name: n}
	}
	return tui.Pick(cmd.Context(), a.stdin, a.stderr, tui.PickOptions{
		Cluster: cluster, Namespace: ns, Objects: objects, Actions: actions, Query: o.query, Now: a.now,
	})
}

// pickByQuery chooses without a terminal: the one object --query matches.
// Several matches fail as an ambiguous target does.
func (a *app) pickByQuery(where string, objects []ops.ObjectState, o pickOptions) (tui.Picked, error) {
	idx := tui.FilterObjects(objects, o.query)
	if len(idx) == 0 {
		return tui.Picked{}, fmt.Errorf("nothing in %s matches %q", where, o.query)
	}
	labels := make([]string, len(idx))
	for i, j := range idx {
		labels[i] = tui.ObjectLabel(objects[j])
		if o.allNamespaces && objects[j].Object.Namespace != "" {
			labels[i] += " -n " + objects[j].Object.Namespace
		}
	}
	title := "Objects in " + where
	if o.query != "" {
		title = fmt.Sprintf("Objects in %s matching %q", where, o.query)
	}
	i, err := a.pick(title, labels)
	if err != nil {
		return tui.Picked{}, err
	}
	action := o.action
	if action == "" {
		action = "why"
	}
	return tui.Picked{Object: objects[idx[i]], Action: action}, nil
}
