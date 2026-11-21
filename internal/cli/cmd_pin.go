package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/pin"
	"github.com/paingha/tugcli/internal/session"
)

func newPinCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pin",
		Short: "Pin a directory to a cluster and namespace with .tug.yaml",
		Long: `A .tug.yaml in a directory pins it, and every directory below it, to a
cluster and namespace. When a terminal with the shell hook moves into the
directory, its session connects as the file says; when it leaves, the
session goes back to where it was. Connecting by hand ends the pin.

A pin file someone else wrote could switch your terminal on its own, so tug
uses one only after tug pin allow, and asks again whenever the file changes.
Set TUG_PIN_DISABLE=1 to turn pins off in a shell.

.tug.yaml:
  cluster: payments-staging   # a name, prefix or fuzzy match, as for tug connect
  namespace: payments
  labels: {region: eu-west-1} # optional, filters first`,
	}
	cmd.AddCommand(newPinSetCmd(a), newPinAllowCmd(a), newPinRevokeCmd(a), newPinShowCmd(a))
	return cmd
}

func newPinSetCmd(a *app) *cobra.Command {
	var namespace, dir string
	cmd := &cobra.Command{
		Use:   "set <cluster> [label=value...]",
		Short: "Write .tug.yaml here, allow it and apply it to this terminal",
		Example: `  tug pin set payments-staging -n payments
  tug pin set gpu=true region=us-east --dir ~/src/trainer`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := a.loadCatalog()
			if err != nil {
				return err
			}
			cl, err := a.chooseCluster(cat, args)
			if err != nil {
				return err
			}
			found, err := pin.Write(dir, pin.File{Cluster: cl.Name, Namespace: namespace})
			if err != nil {
				return err
			}
			if err := pin.NewTrust(a.paths.Data).Allow(found); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Wrote %s: %s\n", found.Path, describePin(found.File))
			return a.applyPinHere(cmd.Context(), dir)
		},
		ValidArgsFunction: a.completeClusters,
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace (default: the cluster's)")
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to pin")
	return cmd
}

func newPinAllowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "allow [dir]",
		Short: "Trust the .tug.yaml that applies to a directory, as it is now",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := argOr(args, ".")
			found, err := findPin(dir)
			if err != nil {
				return err
			}
			if err := pin.NewTrust(a.paths.Data).Allow(found); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Allowed %s: %s\n", found.Path, describePin(found.File))
			return a.applyPinHere(cmd.Context(), dir)
		},
	}
}

func newPinRevokeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "revoke [dir]",
		Aliases: []string{"deny"},
		Short:   "Stop trusting the .tug.yaml that applies to a directory",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			found, err := findPin(argOr(args, "."))
			if err != nil {
				return err
			}
			had, err := pin.NewTrust(a.paths.Data).Revoke(found.Path)
			if err != nil {
				return err
			}
			if !had {
				_, err = fmt.Fprintf(a.stdout, "%s was not allowed\n", found.Path)
				return err
			}
			fmt.Fprintf(a.stdout, "Revoked %s\n", found.Path)
			if s, err := a.sessions().Current(); err == nil && s.State.Pin != nil && s.State.Pin.File == found.Path {
				return a.unpin(cmd.Context(), s)
			}
			return nil
		},
	}
}

func newPinShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show [dir]",
		Short: "Show the .tug.yaml that applies to a directory and whether it is allowed",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			found, err := findPin(argOr(args, "."))
			if err != nil {
				return err
			}
			state := "not allowed; tug pin allow uses it"
			if pin.NewTrust(a.paths.Data).Allowed(found) {
				state = "allowed"
			}
			if s, err := a.sessions().Current(); err == nil && s.State.Pin != nil && s.State.Pin.File == found.Path {
				state += ", and this terminal is pinned by it"
			}
			_, err = fmt.Fprintf(a.stdout, "%s\n  pins:  %s\n  state: %s\n", found.Path, describePin(found.File), state)
			return err
		},
	}
}

// newSessionPinCmd is what the shell hook runs when the directory changes.
func newSessionPinCmd(a *app) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "pin",
		Short: "Apply the .tug.yaml for a directory to this terminal's session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.sessions().Current()
			if err != nil {
				return nil // no session, nothing to pin
			}
			// A cd must never stop at a question: an ambiguous pin fails
			// with a message instead.
			a.stdin = &bytes.Buffer{}
			return a.applyPin(cmd.Context(), s, dir)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "the shell's working directory")
	return cmd
}

// applyPinHere applies dir's pin to this terminal's session, if it has one.
func (a *app) applyPinHere(ctx context.Context, dir string) error {
	s, err := a.sessions().Current()
	if err != nil {
		return nil // outside a session the file waits for the next cd
	}
	return a.applyPin(ctx, s, dir)
}

// applyPin connects s as the pin file for dir says, or puts back the
// connection from before the pin when dir has none.
func (a *app) applyPin(ctx context.Context, s *session.Session, dir string) error {
	found, err := pin.Find(dir)
	if err != nil {
		return err
	}
	st := s.State
	switch {
	case found == nil && st.Pin == nil:
		return nil
	case found == nil:
		return a.unpin(ctx, s)
	case st.Pin != nil && st.Pin.File == found.Path && st.Pin.Hash == found.Hash:
		return nil // still inside the same pinned tree
	}
	if !pin.NewTrust(a.paths.Data).Allowed(found) {
		noticed := found.Path + "@" + found.Hash
		if st.PinNoticed != noticed {
			fmt.Fprintf(a.stderr, "tug: %s pins %s; run `tug pin allow` to use it\n", found.Path, describePin(found.File))
			if err := s.NotePin(noticed); err != nil {
				return err
			}
		}
		return nil
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	cl, err := a.chooseCluster(cat, found.File.Args())
	if err != nil {
		return fmt.Errorf("%s: %w", found.Path, err)
	}
	// Moving from one pinned tree to another keeps the connection from
	// before the first.
	previous := st.Current()
	if st.Pin != nil {
		previous = st.Pin.Previous
	}
	readOnly, _, err := a.connectTo(ctx, s, cl, connectOptions{namespace: found.File.Namespace})
	if err != nil {
		return err
	}
	if err := s.SetPin(&session.Pin{File: found.Path, Hash: found.Hash, Previous: previous}); err != nil {
		return err
	}
	mode := ""
	if readOnly {
		mode = " (read-only)"
	}
	fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf("pinned by %s: %s, namespace %s%s", found.Path, cl.Name, s.State.Namespace, mode)))
	return nil
}

// unpin puts back the connection from before the pin.
func (a *app) unpin(ctx context.Context, s *session.Session) error {
	prev := s.State.Pin.Previous
	if prev != nil {
		cat, err := a.loadCatalog()
		if err != nil {
			return err
		}
		if cl, err := cat.Get(prev.ClusterID); err == nil {
			o := connectOptions{namespace: prev.Namespace, readOnly: prev.ReadOnly, readWrite: !prev.ReadOnly, proxy: prev.Proxy}
			if _, _, err := a.connectTo(ctx, s, cl, o); err != nil {
				return err
			}
			if err := s.SetPin(nil); err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, a.colors().Dim(fmt.Sprintf("left the pinned directory: back to %s, namespace %s", cl.Name, s.State.Namespace)))
			return nil
		}
	}
	if err := a.disconnect(ctx, s); err != nil {
		return err
	}
	fmt.Fprintln(a.stderr, a.colors().Dim("left the pinned directory: disconnected"))
	return nil
}

func findPin(dir string) (*pin.Found, error) {
	found, err := pin.Find(dir)
	if err != nil {
		return nil, err
	}
	if found == nil {
		abs, _ := os.Getwd()
		if dir != "." {
			abs = dir
		}
		return nil, fmt.Errorf("no %s in %s or its parents; tug pin set writes one", pin.FileName, abs)
	}
	return found, nil
}

func describePin(f pin.File) string {
	parts := f.Args()
	what := strings.Join(parts, " ")
	if f.Namespace != "" {
		what += ", namespace " + f.Namespace
	}
	return what
}

func argOr(args []string, def string) string {
	if len(args) > 0 {
		return args[0]
	}
	return def
}
