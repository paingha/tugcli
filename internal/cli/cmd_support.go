package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/paingha/tugcli/internal/bundle"
	"github.com/paingha/tugcli/internal/doctor"
	"github.com/paingha/tugcli/internal/render"
)

type supportBundleOptions struct {
	namespace      string
	since          time.Duration
	output         string
	preview        bool
	include        []string
	exclude        []string
	logsTail       int64
	redactPatterns []string
}

func newSupportCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "support",
		Short: "Gather evidence about a problem to share with whoever helps you",
	}
	cmd.AddCommand(newSupportBundleCmd(a))
	return cmd
}

func newSupportBundleCmd(a *app) *cobra.Command {
	var o supportBundleOptions
	cmd := &cobra.Command{
		Use:   "bundle <target>",
		Short: "Collect diagnostic evidence about an object or a namespace into a redacted archive",
		Long: `Collect what someone helping you needs to see about an object, into one
redacted .tar.gz archive to attach to a ticket: the object and everything it
owns as YAML, tug why's diagnosis and tug tree's tree as JSON, events, the logs
of its pods' containers (the previous run's too, when one restarted), the
conditions of the nodes they run on, the cluster's version and resources, tug
doctor's checks, a manifest.json of every file and where it came from, and a
README that says what was redacted and how to review it.

The target is any object, named as kubectl names it, such as deploy/api or a
custom resource, or namespace/<name> for everything in a namespace.

Redaction is on, always: Secrets are copied from the metadata API, so no
Secret's data is in the bundle; environment variable values, ConfigMap values,
fields named like a credential and kubectl's last-applied annotation are
replaced with <redacted>; and bearer tokens, passwords in URLs and other common
credential patterns are redacted in logs, events and every other text. Add
your own patterns with --redact-pattern. Review the bundle before you share it:
redaction cannot know everything your applications treat as private.

The archive is readable by you only. support bundle only reads from the
cluster.`,
		Example: `  tug support bundle deploy/api
  tug support bundle certificate/web-tls -n ingress
  tug support bundle namespace/shop --since 6h           # everything in the namespace
  tug support bundle api --preview                       # list the files and their sizes; write nothing
  tug support bundle api --exclude configmap --redact-pattern 'cust-[0-9]+'
  tug support bundle api --output case-1234.tar.gz`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSupportBundle(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: the session's)")
	f.DurationVar(&o.since, "since", time.Hour, "how far back events and logs go")
	f.StringVar(&o.output, "output", "", "file to write the archive to, or - for stdout (default: ./tug-bundle-<cluster>-<target>-<time>.tar.gz)")
	f.BoolVar(&o.preview, "preview", false, "list the files that would be written, with their sizes, and write nothing")
	f.StringArrayVar(&o.include, "include", nil, "collect only objects of this kind, such as deploy or certificates.cert-manager.io (repeatable)")
	f.StringArrayVar(&o.exclude, "exclude", nil, "leave objects of this kind out (repeatable)")
	f.Int64Var(&o.logsTail, "logs-tail", 1000, "most lines to keep from the end of each container's log; 0 leaves logs out")
	f.StringArrayVar(&o.redactPatterns, "redact-pattern", nil, "also redact whatever this regular expression matches (repeatable)")
	return cmd
}

func (a *app) runSupportBundle(ctx context.Context, arg string, o supportBundleOptions) error {
	if o.since <= 0 {
		return errors.New("--since must be more than 0")
	}
	if o.logsTail < 0 {
		return errors.New("--logs-tail must be 0 or more")
	}
	redactor, err := bundle.NewRedactor(o.redactPatterns)
	if err != nil {
		return err
	}
	env, err := a.opsEnv(ctx, o.namespace)
	if err != nil {
		return err
	}
	clients, mapper, err := a.objectClients(env)
	if err != nil {
		return err
	}
	target, err := a.resolveObject(ctx, env, clients, mapper, arg)
	if err != nil {
		return err
	}

	cluster := bundle.ClusterInfo{Name: env.session.State.ClusterName}
	if cat, err := a.loadCatalog(); err == nil {
		if cl, err := cat.Get(env.session.State.ClusterID); err == nil {
			cluster.Provider, cluster.Region = cl.Provider, cl.Region
		}
	}
	name := bundle.DefaultName(cluster.Name, target.String(), a.now())
	file, archive := o.output, name+bundle.Suffix
	switch file {
	case "":
		file = archive
	case "-": // stdout, under the default name
	default:
		name, archive = bundle.NameOf(file), filepath.Base(file)
	}
	if !o.preview {
		if file == "-" && a.stdoutTerminal() {
			return errors.New("--output - would write the archive to this terminal; redirect it to a file, or name one with --output")
		}
		if file != "-" {
			if _, err := os.Lstat(file); err == nil {
				return fmt.Errorf("%s already exists; remove it, or choose another file with --output", file)
			}
		}
	}

	c := a.colors()
	b, err := bundle.Collect(ctx, clients, target, bundle.Options{
		Name: name, Archive: archive, Cluster: cluster,
		Since: o.since, LogsTail: o.logsTail, Include: o.include, Exclude: o.exclude,
		Redactor: redactor, Now: a.now,
		Progress: func(step string) { fmt.Fprintln(a.stderr, c.Dim(step+"…")) },
		Doctor: func(ctx context.Context) (doctor.Report, error) {
			cfg, err := a.doctorConfig(nil, "")
			if err != nil {
				return doctor.Report{}, err
			}
			return doctor.Run(ctx, cfg), nil
		},
	})
	if err != nil {
		return err
	}
	for _, p := range b.Problems {
		fmt.Fprintf(a.stderr, "%s %s\n", c.Yellow("warning:"), p)
	}
	size := humanBytes(b.Size())

	if o.preview {
		rows := make([][]string, 0, len(b.Files))
		for _, f := range b.Files {
			rows = append(rows, []string{f.Path, strconv.Itoa(len(f.Data)), f.Source})
		}
		if err := render.Table(a.stdout, []string{"FILE", "BYTES", "SOURCE"}, rows); err != nil {
			return err
		}
		fmt.Fprintf(a.stderr, "%d files, %s before compression, under %s/ in the archive; nothing written\n", len(b.Files), size, b.Name)
		return nil
	}
	if file == "-" {
		if _, err := b.WriteTo(a.stdout); err != nil {
			return err
		}
		fmt.Fprintf(a.stderr, "wrote %d files (%s before compression); review the bundle before you share it: its README.md says what was redacted\n", len(b.Files), size)
		return nil
	}
	if err := b.WriteFile(file); err != nil {
		if errors.Is(err, bundle.ErrExists) {
			return fmt.Errorf("%s already exists; remove it, or choose another file with --output", file)
		}
		return err
	}
	fmt.Fprintln(a.stdout, file)
	fmt.Fprintf(a.stderr, "wrote %d files (%s before compression); review the bundle before you share it: its README.md says what was redacted\n", len(b.Files), size)
	return nil
}

// stdoutTerminal reports whether stdout is a terminal.
func (a *app) stdoutTerminal() bool {
	f, ok := a.stdout.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// humanBytes writes a size as people read it: 512 B, 1.2 KiB, 3.4 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
