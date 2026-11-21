package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/fanout"
	"github.com/paingha/tugcli/internal/passthrough"
	"github.com/paingha/tugcli/internal/render"
	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/tunnel"
	"github.com/paingha/tugcli/pkg/provider"
)

// The global flags that run one command on many clusters.
const (
	flagClusterSelector = "--cluster-selector"
	flagParallel        = "--parallel"
)

// annotationAcrossClusters marks the tug commands that may run on many
// clusters at once. They only read. Its value says how they reach a
// cluster.
const annotationAcrossClusters = "tug.across-clusters"

// How a command marked with annotationAcrossClusters reaches a cluster.
const (
	// acrossInProcess commands use tug's own clients.
	acrossInProcess = "in-process"
	// acrossKubectl commands run kubectl with the session's kubeconfig.
	acrossKubectl = "kubectl"
)

// acrossClusters is the annotation of a command that may run on many
// clusters at once, reaching each as how says.
func acrossClusters(how string) map[string]string {
	return map[string]string{annotationAcrossClusters: how}
}

// fanoutOptions say which clusters a command runs on and how many at once.
type fanoutOptions struct {
	selector map[string]string
	parallel int
}

// splitFanoutFlags takes --cluster-selector and --parallel out of a command
// line. execute does this before cobra parses anything, because kubectl
// commands never reach cobra. Without --cluster-selector it returns nil and
// args as they are, leaving a --parallel to cobra or kubectl.
func splitFanoutFlags(args []string) (*fanoutOptions, []string, error) {
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return nil, args, nil // completion knows the flags from the root command
	}
	if !slices.ContainsFunc(args[:argsEnd(args)], isSelectorFlag) {
		return nil, args, nil
	}
	o := &fanoutOptions{selector: map[string]string{}, parallel: fanout.DefaultParallel}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name != flagClusterSelector && name != flagParallel {
			rest = append(rest, arg)
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == flagParallel {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, nil, fmt.Errorf("%s wants a number from 1, got %q", flagParallel, value)
			}
			o.parallel = n
			continue
		}
		k, v, ok := strings.Cut(value, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, nil, fmt.Errorf("%s wants label=value, got %q; repeat the flag for each label", flagClusterSelector, value)
		}
		if prev, dup := o.selector[k]; dup && prev != v {
			return nil, nil, fmt.Errorf("%s asks for %s=%s and %s=%s; a cluster has one value per label", flagClusterSelector, k, prev, k, v)
		}
		o.selector[k] = v
	}
	return o, rest, nil
}

// argsEnd is where tug's flags end: at "--", after which the words belong
// to a command run in a pod.
func argsEnd(args []string) int {
	if i := slices.Index(args, "--"); i >= 0 {
		return i
	}
	return len(args)
}

func isSelectorFlag(arg string) bool {
	return arg == flagClusterSelector || strings.HasPrefix(arg, flagClusterSelector+"=")
}

// selectorString writes a selector as label=value pairs in a stable order.
func selectorString(sel map[string]string) string {
	pairs := make([]string, 0, len(sel))
	for k, v := range sel {
		pairs = append(pairs, k+"="+v)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ", ")
}

// fanoutFunc runs a command line in one temporary session.
type fanoutFunc func(ctx context.Context, s *session.Session, stdout, stderr io.Writer) error

// fanout runs a command line on every catalog cluster that o selects, a
// bounded number at a time, with each output line prefixed by its cluster.
// Only commands that read run: kubectl commands as passthrough.IsRead
// judges them, and tug commands marked with annotationAcrossClusters.
// Nothing runs when the command may write.
func (a *app) fanout(ctx context.Context, root *cobra.Command, args []string, o *fanoutOptions) error {
	if err := a.setup(false); err != nil {
		return err
	}
	run, kubectl, err := a.fanoutRunner(root, args)
	if err != nil {
		return err
	}
	cat, err := a.loadCatalog()
	if err != nil {
		return err
	}
	clusters := cat.Select(o.selector)
	if len(clusters) == 0 {
		return fmt.Errorf("no cluster has labels %s; `tug ls` shows each cluster's labels", selectorString(o.selector))
	}
	byName := make(map[string]*catalog.Cluster, len(clusters))
	names := make([]string, 0, len(clusters))
	for _, cl := range clusters {
		byName[cl.Name] = cl
		names = append(names, cl.Name)
	}
	results := fanout.Run(ctx, names, o.parallel, a.fanoutSink(names),
		func(ctx context.Context, name string, stdout, stderr io.Writer) error {
			s, err := a.fanoutSession(ctx, byName[name], kubectl)
			if err != nil {
				return err
			}
			defer a.endFanoutSession(s)
			return run(ctx, s, stdout, stderr)
		})
	return a.fanoutSummary(names, results)
}

// fanoutRunner returns how to run args on one cluster and whether that runs
// kubectl, or refuses a command that may write or that tug cannot run
// across clusters.
func (a *app) fanoutRunner(root *cobra.Command, args []string) (fanoutFunc, bool, error) {
	if isPassthrough(root, args) {
		if !passthrough.IsRead(args) {
			return nil, false, fmt.Errorf("refused: `kubectl %s` may write, and %s runs only commands that read; "+
				"to write, connect to one cluster with `tug connect`", strings.Join(args, " "), flagClusterSelector)
		}
		if _, err := passthrough.LookPath(a.cfg.Kubectl); err != nil {
			return nil, false, err
		}
		return func(ctx context.Context, s *session.Session, stdout, stderr io.Writer) error {
			return a.fanoutKubectl(ctx, s, args, stdout, stderr)
		}, true, nil
	}
	cmd, _, err := root.Find(args)
	if err != nil || cmd == root {
		return nil, false, fmt.Errorf("name a command to run on each cluster, for example `tug get pods %s env=staging`", flagClusterSelector)
	}
	how := cmd.Annotations[annotationAcrossClusters]
	if how == "" {
		var can []string
		for _, c := range root.Commands() {
			if c.Annotations[annotationAcrossClusters] != "" {
				can = append(can, "tug "+c.Name())
			}
		}
		return nil, false, fmt.Errorf("%s does not run across clusters; %s works with kubectl commands that read and with %s",
			cmd.CommandPath(), flagClusterSelector, joinAnd(can))
	}
	return func(ctx context.Context, s *session.Session, stdout, stderr io.Writer) error {
		return a.fanoutTug(ctx, s, args, stdout, stderr)
	}, how == acrossKubectl, nil
}

// joinAnd lists items as "a, b and c".
func joinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// fanoutSession makes the temporary session a run on cl uses: read-only,
// in the cluster's default namespace, with a kubeconfig whose user runs
// `tug auth token` as every session kubeconfig does, so no credential is
// written. For a run of kubectl, a tunnelled cluster is reached through
// this process's tunnel, which kubectl takes as a SOCKS5 proxy-url. That
// URL carries the random password the tunnel's loopback proxy asks for; it
// is worth nothing once this process ends, and the session, in tug's
// private runtime directory, is removed when the run does. tug's own
// commands open the tunnel themselves.
func (a *app) fanoutSession(ctx context.Context, cl *catalog.Cluster, kubectl bool) (*session.Session, error) {
	target := *cl
	if tun := cl.EffectiveTunnel(); tun != "" && kubectl {
		route, err := a.tunnels().Route(ctx, tun)
		if err != nil {
			return nil, fmt.Errorf("opening the tunnel to %s: %w", cl.Name, err)
		}
		if route.Endpoint != nil {
			// A local proxy such as tsh's wants a client key, which no
			// kubeconfig tug writes may hold.
			return nil, fmt.Errorf("kubectl cannot reach %s through %s without its client key on disk; "+
				"`tug connect %s` reaches it through tugd", cl.Name, tunnel.Redacted(tun), cl.Name)
		}
		target.ProxyURL = route.Proxy.String()
	}
	mgr := a.sessions()
	s, err := mgr.Create(os.Getpid(), "")
	if err != nil {
		return nil, fmt.Errorf("creating a session for %s: %w", cl.Name, err)
	}
	if err := s.Connect(&target, "", true, session.AuthCommand{Command: selfPath()}); err != nil {
		a.endFanoutSession(s)
		return nil, fmt.Errorf("writing a kubeconfig for %s: %w", cl.Name, err)
	}
	return s, nil
}

// endFanoutSession removes a temporary session and its kubeconfig.
func (a *app) endFanoutSession(s *session.Session) {
	if err := a.sessions().Remove(s.State.ID); err != nil {
		a.log.Warn("removing a temporary session", "session", s.State.ID, "err", err)
	}
}

// fanoutKubectl runs kubectl with a temporary session's kubeconfig.
// Cancelling ctx interrupts it.
func (a *app) fanoutKubectl(ctx context.Context, s *session.Session, args []string, stdout, stderr io.Writer) error {
	cmd, err := passthrough.CommandContext(ctx, a.cfg.Kubectl, args, s.KubeconfigPath())
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return fmt.Errorf("kubectl exited with %d", ee.ExitCode())
	case err != nil:
		return fmt.Errorf("running kubectl: %w", err)
	}
	return nil
}

// fanoutTug runs one of tug's commands in a temporary session, in this
// process. The run has no terminal, so it never prompts, and the session's
// read-only guard refuses any write.
func (a *app) fanoutTug(ctx context.Context, s *session.Session, args []string, stdout, stderr io.Writer) error {
	stdin := strings.NewReader("")
	run := &app{
		stdin:  stdin,
		in:     bufio.NewReader(stdin),
		stdout: stdout,
		stderr: stderr,
		now:    a.now,
		debug:  a.debug,
		fixed:  s,
		// Shared, so each tunnel opens once; a closes it when tug ends.
		tunnelPool: a.tunnels(),
	}
	defer run.closePlugins()
	return run.execute(ctx, args)
}

// fanoutSink prefixes each line with its cluster's name, coloured, and
// lines the names up.
func (a *app) fanoutSink(names []string) fanout.Sink {
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}
	outColors, errColors := a.colors(), render.ColorsFor(a.stderr)
	return fanout.Sink{Line: func(cluster string, stream fanout.Stream, text string) {
		w, c := a.stdout, outColors
		if stream == fanout.Stderr {
			w, c = a.stderr, errColors
		}
		fmt.Fprintf(w, "%s%s | %s\n", c.Pick(cluster, cluster), strings.Repeat(" ", width-len(cluster)), text)
	}}
}

// fanoutSummary lists, on stderr, the clusters where the command failed or
// never ran, and makes tug exit 1 when there are any. Runs that Ctrl-C cut
// short are not failures, as for any command that streams.
func (a *app) fanoutSummary(names []string, results []fanout.Result) error {
	var failed []fanout.Result
	var skipped []string
	for _, r := range results {
		switch {
		case r.Failed():
			failed = append(failed, r)
		case r.State == fanout.Skipped:
			skipped = append(skipped, r.Cluster)
		}
	}
	if len(failed) == 0 && len(skipped) == 0 {
		return nil
	}
	c := render.ColorsFor(a.stderr)
	width := 0
	for _, r := range failed {
		width = max(width, len(r.Cluster))
	}
	if len(failed) > 0 {
		fmt.Fprintf(a.stderr, "%s on %d of %d clusters:\n", c.Red("Failed"), len(failed), len(names))
	}
	for _, r := range failed {
		fmt.Fprintf(a.stderr, "  %s%s  %v\n", c.Bold(r.Cluster), strings.Repeat(" ", width-len(r.Cluster)), r.Err)
		var pe *provider.Error
		if errors.As(r.Err, &pe) && pe.Hint != "" {
			fmt.Fprintf(a.stderr, "  %s  hint: %s\n", strings.Repeat(" ", width), pe.Hint)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(a.stderr, "%s before it ran on %s\n", c.Yellow("Interrupted"), strings.Join(skipped, ", "))
	}
	return &exitError{code: 1}
}
