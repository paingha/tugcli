package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/paingha/tugcli/internal/bookmark"
	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
)

func newBookmarkCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bookmark",
		Short: "Save objects and places under short names, and open them from any terminal",
		Long: `Save an object, or a cluster and namespace, under a short name, and open it
later from any terminal: tug bookmark open runs why, tree, logs, describe or
pf on a bookmarked object, on its own cluster, without changing the
terminal's session. Opening a bookmark on another cluster reads it through a
temporary read-only session.

Bookmarks are kept in bookmarks.yaml in tug's config directory.`,
		Example: `  tug bookmark add api deploy/api-server -n shop
  tug bookmark add certs certificate/web-tls --cluster payments-prod
  tug bookmark add edge -n ingress --cluster edge-eu   # a place, no object
  tug bookmark open api                # tug why on it
  tug bookmark open api logs -- -f     # flags after -- go to the action
  tug bookmark open edge               # connect this terminal there`,
	}
	cmd.AddCommand(newBookmarkAddCmd(a), newBookmarkLsCmd(a), newBookmarkRmCmd(a), newBookmarkOpenCmd(a))
	return cmd
}

type bookmarkAddOptions struct {
	namespace string
	cluster   string
	replace   bool
}

func newBookmarkAddCmd(a *app) *cobra.Command {
	var o bookmarkAddOptions
	cmd := &cobra.Command{
		Use:   "add <name> [target]",
		Short: "Bookmark an object, or with no target a cluster and namespace",
		Long: `Bookmark an object under a name. The target is resolved now, as tug why
resolves it, and saved exactly, so the bookmark always opens that object.
Without a target, the bookmark is of the cluster and namespace.`,
		Example: `  tug bookmark add api deploy/api-server -n shop
  tug bookmark add orders db/orders --cluster payments-staging
  tug bookmark add edge -n ingress --cluster edge-eu`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := bookmark.ValidName(args[0]); err != nil {
				return err
			}
			ctx := cmd.Context()
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, s, err := a.bookmarkCluster(cat, o.cluster)
			if err != nil {
				return err
			}
			b := bookmark.Bookmark{Name: args[0], Cluster: cl.Name, Namespace: o.namespace, Created: a.now().UTC()}
			if len(args) == 1 {
				if b.Namespace == "" && s != nil && s.State.ClusterID == cl.ID {
					b.Namespace = s.State.Namespace
				}
				b.Namespace = cmp.Or(b.Namespace, cl.DefaultNamespace, "default")
			} else {
				err := a.inCluster(ctx, cl, false, func() error {
					obj, err := a.resolveIn(ctx, o.namespace, args[1])
					if err != nil {
						return err
					}
					b.Namespace = obj.Namespace
					b.Target = &bookmark.Target{Resource: obj.Resource.GroupResource().String(), Kind: obj.Kind, Name: obj.Name}
					return nil
				})
				if err != nil {
					return err
				}
			}
			if err := bookmark.New(a.paths.Config).Add(b, o.replace); err != nil {
				if errors.Is(err, bookmark.ErrExists) {
					return fmt.Errorf("%w; use --replace to change it", err)
				}
				return err
			}
			_, err = fmt.Fprintf(a.stdout, "Bookmarked %s: %s\n", b.Name, b.Where())
			return err
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringVar(&o.cluster, "cluster", "", "cluster, by name, prefix or fuzzy match (default: this terminal's)")
	cmd.Flags().BoolVar(&o.replace, "replace", false, "replace a bookmark of the same name")
	_ = cmd.RegisterFlagCompletionFunc("cluster", a.completeClusters)
	return cmd
}

// bookmarkCluster returns the catalog cluster a bookmark is added on: the
// one named, else this terminal's. It also returns the terminal's session
// when there is one.
func (a *app) bookmarkCluster(cat *catalog.Catalog, name string) (*catalog.Cluster, *session.Session, error) {
	s, err := a.session()
	switch {
	case errors.Is(err, session.ErrNoSession):
		s = nil
	case err != nil:
		return nil, nil, err
	}
	if name != "" {
		cl, err := a.chooseCluster(cat, []string{name})
		return cl, s, err
	}
	if s == nil || !s.State.Connected() {
		return nil, nil, errors.New("this terminal is not connected; name the cluster with --cluster, or run `tug connect` first")
	}
	cl, err := cat.Get(s.State.ClusterID)
	if err != nil {
		return nil, nil, fmt.Errorf("session cluster: %w; run `tug connect` again", err)
	}
	return cl, s, nil
}

// resolveIn resolves a target as why does, in a namespace or the session's.
func (a *app) resolveIn(ctx context.Context, namespace, target string) (resolve.Object, error) {
	env, err := a.opsEnv(ctx, namespace)
	if err != nil {
		return resolve.Object{}, err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return resolve.Object{}, err
	}
	return a.resolveObject(ctx, env, clients, mapper, target)
}

// inCluster runs fn with commands reaching cl: in this terminal's session
// when it is on cl, else in a temporary read-only session on cl that is
// removed afterwards, so the terminal stays where it is. With kubectl, the
// temporary session also suits kubectl, as a run across clusters does.
func (a *app) inCluster(ctx context.Context, cl *catalog.Cluster, kubectl bool, fn func() error) error {
	if s, err := a.session(); err == nil && s.State.Connected() && s.State.ClusterID == cl.ID {
		return fn()
	}
	tmp, err := a.fanoutSession(ctx, cl, kubectl)
	if err != nil {
		return err
	}
	defer a.endFanoutSession(tmp)
	prev := a.fixed
	a.fixed = tmp
	defer func() { a.fixed = prev }()
	return fn()
}

func newBookmarkLsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List bookmarks",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			bs, err := bookmark.New(a.paths.Config).List()
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON, render.FormatYAML:
				return render.Structured(a.stdout, format, bs)
			case render.FormatName:
				for _, b := range bs {
					fmt.Fprintln(a.stdout, b.Name)
				}
				return nil
			}
			if len(bs) == 0 {
				fmt.Fprintln(a.stderr, "No bookmarks; add one with `tug bookmark add <name> <target>`")
				return nil
			}
			rows := make([][]string, 0, len(bs))
			for _, b := range bs {
				target := "-"
				if b.Target != nil {
					target = b.Target.String()
				}
				rows = append(rows, []string{b.Name, target, cmp.Or(b.Namespace, "-"), b.Cluster})
			}
			return render.Table(a.stdout, []string{"NAME", "TARGET", "NAMESPACE", "CLUSTER"}, rows)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func newBookmarkRmCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name>...",
		Aliases:           []string{"remove"},
		Short:             "Remove bookmarks",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeBookmarks,
		RunE: func(_ *cobra.Command, args []string) error {
			removed, err := bookmark.New(a.paths.Config).Remove(args...)
			for _, b := range removed {
				fmt.Fprintf(a.stdout, "Removed bookmark %s (%s)\n", b.Name, b.Where())
			}
			return err
		},
	}
}

// bookmarkConnect is the action that points this terminal at a bookmark's
// cluster and namespace, and what opening a bookmark without an object does.
const bookmarkConnect = "connect"

func newBookmarkOpenCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "open <name> [action] [args...]",
		Short: "Run why, tree, logs, describe or pf on a bookmark, or connect to it",
		Long: `Run an action on a bookmarked object: why (the default), tree, logs, describe
or pf. Words after the action go to it; put flags after --. The action runs
on the bookmark's cluster and in its namespace; when this terminal is on
another cluster, it runs in a temporary read-only session there, and the
terminal stays where it is.

connect points this terminal at the bookmark's cluster and namespace, and is
what opening a bookmark without an object does.`,
		Example: `  tug bookmark open api
  tug bookmark open api tree
  tug bookmark open api logs -- -f --since 10m
  tug bookmark open db pf 5432
  tug bookmark open api connect`,
		Args: cobra.MinimumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return a.completeBookmarks(cmd, args, toComplete)
			case 1:
				return slices.Concat(objectActions, []string{bookmarkConnect}), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bookmark.New(a.paths.Config).Get(args[0])
			if err != nil {
				if errors.Is(err, bookmark.ErrNotFound) {
					return fmt.Errorf("%w; `tug bookmark ls` lists them", err)
				}
				return err
			}
			action := bookmarkConnect
			if b.Target != nil {
				action = "why"
			}
			if len(args) > 1 {
				action = args[1]
			}
			var extra []string
			if len(args) > 2 {
				extra = args[2:]
			}
			return a.openBookmark(cmd.Context(), b, action, extra)
		},
	}
}

// openBookmark runs an action on a bookmark.
func (a *app) openBookmark(ctx context.Context, b bookmark.Bookmark, action string, extra []string) error {
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	cl, err := cat.Get(b.Cluster)
	if errors.Is(err, catalog.ErrNotFound) {
		return fmt.Errorf("bookmark %s is on %s, which is not in the catalog; `tug ls` lists the clusters", b.Name, b.Cluster)
	}
	if err != nil {
		return err
	}
	if action == bookmarkConnect {
		line := []string{"connect", cl.Name}
		if b.Namespace != "" {
			line = append(line, "-n", b.Namespace)
		}
		return a.execute(ctx, append(line, extra...))
	}
	if b.Target == nil {
		return fmt.Errorf("bookmark %s is a cluster and namespace, with no object to %s; open it without an action to connect there", b.Name, action)
	}
	line, err := objectAction(action, bookmarkObject(b), extra)
	if err != nil {
		return err
	}
	if s, err := a.session(); err != nil || s.State.ClusterID != cl.ID {
		fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf("%s is on %s; reading it there in a temporary read-only session", b.Target, cl.Name)))
	}
	return a.inCluster(ctx, cl, action == "describe", func() error { return a.execute(ctx, line) })
}

// bookmarkObject returns the object a bookmark saved.
func bookmarkObject(b bookmark.Bookmark) resolve.Object {
	gr := schema.ParseGroupResource(b.Target.Resource)
	return resolve.Object{
		Resource:  schema.GroupVersionResource{Group: gr.Group, Resource: gr.Resource},
		Kind:      b.Target.Kind,
		Name:      b.Target.Name,
		Namespace: b.Namespace,
	}
}

// completeBookmarks completes bookmark names.
func (a *app) completeBookmarks(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if a.cfg == nil {
		if err := a.setup(false); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
	}
	bs, err := bookmark.New(a.paths.Config).List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, b := range bs {
		if strings.HasPrefix(b.Name, toComplete) {
			out = append(out, b.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
