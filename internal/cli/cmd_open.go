package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/editor"
	"github.com/paingha/tugcli/internal/manifest"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

type openOptions struct {
	namespace string
	repo      string
	print     bool
}

func newOpenCmd(a *app) *cobra.Command {
	var o openOptions
	cmd := &cobra.Command{
		Use:   "open <target>",
		Short: "Open the file that defines an object in your editor, or else its live YAML",
		Long: `Find the file in this repository that defines an object, print where as
path:line, and open it in $VISUAL or $EDITOR. tug looks first where the tools
that applied the object say it came from: kustomize's config.kubernetes.io/origin
annotation, the path of the Flux Kustomization or Argo CD Application that
applied it, or the templates of its Helm chart. Only then does it search the
repository's YAML for a document of the object's kind and name.

When no file defines the object, tug writes its live YAML to a read-only
temporary file and opens that. A Secret's data is never read or written.

The editor opens only on a terminal. With --print, or without a terminal, tug
prints the path:line links alone, which any editor or terminal can follow.`,
		Example: `  tug open deploy/api
  tug open kustomization.kustomize.toolkit.fluxcd.io/apps -n flux-system
  tug open certificate/web-tls --print     # just the path:line links
  tug open api --repo ~/src/platform       # look in another repository`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			live, err := liveObject(ctx, clients, obj)
			if err != nil {
				return err
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo := manifest.FindRoot(wd)
			if o.repo != "" {
				if repo, err = filepath.Abs(o.repo); err != nil {
					return err
				}
			}
			f := &manifest.Finder{Root: repo, SourcePaths: a.gitopsPaths(clients, mapper)}
			locs, err := f.Find(ctx, manifestObject(live))
			if err != nil {
				return fmt.Errorf("looking for %s in %s: %w", obj, repo, err)
			}
			c := a.colors()
			if len(locs) == 0 {
				path, err := a.writeLiveYAML(env.session.State.ClusterName, obj, live, repo)
				if err != nil {
					return err
				}
				fmt.Fprintln(a.stderr, c.Dim(fmt.Sprintf("No file in %s defines %s; here is its live YAML, read-only", repo, obj)))
				locs = []manifest.Location{{Path: path, Line: 1}}
			} else {
				fmt.Fprintln(a.stderr, c.Dim(fmt.Sprintf("%s, found through %s:", obj, locs[0].Via)))
			}
			for _, l := range locs {
				fmt.Fprintf(a.stdout, "%s:%d\n", shownPath(wd, l.Path), l.Line)
			}
			if o.print || !a.interactive() {
				return nil
			}
			return a.edit(ctx, locs[0])
		},
	}
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	cmd.Flags().StringVar(&o.repo, "repo", "", "repository to look in (default: the one the current directory is in)")
	cmd.Flags().BoolVar(&o.print, "print", false, "print the path:line links without opening an editor")
	return cmd
}

// edit opens a location in the user's editor on the terminal. The editor
// draws on stderr, which is a terminal whenever tug is interactive, so that
// a pipe on stdout gets only the links.
func (a *app) edit(ctx context.Context, l manifest.Location) error {
	cmd, err := editor.Command(ctx, os.Getenv, l.Path, l.Line)
	if errors.Is(err, editor.ErrNoEditor) {
		fmt.Fprintln(a.stderr, a.colors().Dim("Set $VISUAL or $EDITOR for tug open to open it there"))
		return nil
	}
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stderr, a.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", strings.Join(cmd.Args, " "), err)
	}
	return nil
}

// shownPath shortens a path under the working directory to a relative one.
func shownPath(wd, path string) string {
	rel, err := filepath.Rel(wd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

var secrets = schema.GroupResource{Resource: "secrets"}

// liveObject reads an object as the API server has it. A Secret is read as
// metadata only, so its data never reaches tug.
func liveObject(ctx context.Context, c ops.Clients, o resolve.Object) (*unstructured.Unstructured, error) {
	if o.Resource.GroupResource() != secrets {
		u, err := c.Dynamic.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", o, err)
		}
		return u, nil
	}
	p, err := c.Metadata.Resource(o.Resource).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting %s: %w", o, err)
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&p.ObjectMeta)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: map[string]any{"metadata": m}}
	u.SetAPIVersion(o.Resource.GroupVersion().String())
	u.SetKind(o.Kind)
	return u, nil
}

// manifestObject is what finding a live object's file goes on.
func manifestObject(u *unstructured.Unstructured) manifest.Object {
	return manifest.Object{
		APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Name: u.GetName(), Namespace: u.GetNamespace(),
		Labels: u.GetLabels(), Annotations: u.GetAnnotations(),
	}
}

// gitopsResources are the resources of the GitOps objects that apply
// others, as kubectl names them.
var gitopsResources = map[string]string{
	manifest.FluxKustomization: "kustomizations.kustomize.toolkit.fluxcd.io",
	manifest.FluxHelmRelease:   "helmreleases.helm.toolkit.fluxcd.io",
	manifest.ArgoApplication:   "applications.argoproj.io",
}

// gitopsPaths reads, from the session's cluster, the paths a GitOps object
// applies from. One tug cannot read, as when Argo CD runs on another
// cluster, has none.
func (a *app) gitopsPaths(c ops.Clients, mapper meta.RESTMapper) func(context.Context, manifest.Source) []string {
	return func(ctx context.Context, s manifest.Source) []string {
		m, err := resolve.FindResource(mapper, gitopsResources[s.Kind])
		if err != nil {
			a.log.Debug("finding the source of an object", "source", s.String(), "err", err)
			return nil
		}
		u, err := c.Dynamic.Resource(m.Resource).Namespace(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
		if err != nil {
			a.log.Debug("finding the source of an object", "source", s.String(), "err", err)
			return nil
		}
		return manifest.SourcePaths(s.Kind, u.Object)
	}
}

// openFilesAge is how long the live YAML tug open writes is kept. Editors
// that return at once, as GUI editors may, read it after tug has ended, so
// it cannot be removed when the editor returns.
const openFilesAge = 24 * time.Hour

// writeLiveYAML writes an object's live YAML, without its managed fields,
// to a read-only file in tug's private runtime directory, and removes the
// ones written there before openFilesAge ago.
func (a *app) writeLiveYAML(cluster string, o resolve.Object, live *unstructured.Unstructured, repo string) (string, error) {
	dir := filepath.Join(a.paths.Runtime, "open")
	if err := config.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	a.pruneOpenFiles(dir)
	u := live.DeepCopy()
	unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
	body, err := yaml.Marshal(u.Object)
	if err != nil {
		return "", err
	}
	where := ""
	if o.Namespace != "" {
		where = " in " + o.Namespace
	}
	header := fmt.Sprintf("# Live %s%s on %s, read at %s.\n# No file in %s defines it. This copy is read-only; changing it changes nothing.\n",
		o, where, cluster, a.now().UTC().Format(time.RFC3339), repo)
	if o.Resource.GroupResource() == secrets {
		header += "# tug leaves a Secret's data out.\n"
	}
	name := fmt.Sprintf("%s-%s-%s.yaml", strings.ToLower(o.Kind), o.Name, strconv.FormatInt(a.now().UnixNano(), 36))
	path := filepath.Join(dir, name)
	if err := config.WriteFileAtomic(path, append([]byte(header), body...), 0o400); err != nil {
		return "", fmt.Errorf("writing the live YAML of %s: %w", o, err)
	}
	return path, nil
}

// pruneOpenFiles removes the live YAML written more than openFilesAge ago.
func (a *app) pruneOpenFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || a.now().Sub(fi.ModTime()) < openFilesAge {
			continue
		}
		p := filepath.Join(dir, e.Name())
		_ = os.Chmod(p, 0o600) // Windows will not remove a read-only file
		if err := os.Remove(p); err != nil {
			a.log.Debug("removing old live YAML", "path", p, "err", err)
		}
	}
}
