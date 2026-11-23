package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/daemon"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// shellPodMsg names the pod and container a shell opens in.
type shellPodMsg struct {
	req ShellRequest
	err error
}

// shellDoneMsg says how a shell ended.
type shellDoneMsg struct {
	req ShellRequest
	err error
}

// forwardMsg says how starting a named forward went.
type forwardMsg struct {
	state daemon.ForwardState
	err   error
}

// writeMsg says how a restart or scale went.
type writeMsg struct {
	done string
	err  error
}

// act runs the action a key asks for on a workload, from its row in the
// list or from one of its panes. Other keys do nothing.
func (m *model) act(msg tea.KeyPressMsg, w ops.WorkloadState) tea.Cmd {
	k := m.keys
	if m.kube == nil && key.Matches(msg, k.actions()...) {
		m.setWarning("Not connected to %s yet", m.state.ClusterName)
		return nil
	}
	switch {
	case key.Matches(msg, k.Logs):
		return m.openLogs(w)
	case key.Matches(msg, k.Why):
		return m.openReport(paneWhy, w)
	case key.Matches(msg, k.Tree):
		return m.openReport(paneTree, w)
	case key.Matches(msg, k.Shell):
		return m.shell(w)
	case key.Matches(msg, k.Forward):
		m.forward(w)
	case key.Matches(msg, k.Restart):
		return m.restart(w)
	case key.Matches(msg, k.Scale):
		m.scale(w)
	}
	return nil
}

func (m *model) writable() bool { return m.state.Writable(m.now()) }

// guarded runs a write at once on a writable session. On a read-only one
// it asks first, as the CLI's ensureWritable does, and refuses the write
// when the answer is no. The client the write gets allows writes only when
// the user said yes.
func (m *model) guarded(action string, run func(allowWrites bool) tea.Cmd) tea.Cmd {
	if m.writable() {
		return run(false)
	}
	cluster := m.state.ClusterName
	m.ask(fmt.Sprintf("%s is read-only. %s anyway?", cluster, capitalise(action)),
		func() tea.Cmd { return run(true) },
		func() tea.Cmd {
			m.setError(&guard.DeniedError{Cluster: cluster, Action: action, Unlock: m.unlock})
			return nil
		})
	return nil
}

// confirmed runs a write that a single key starts after asking: on a
// read-only session the guard's question, and otherwise question.
func (m *model) confirmed(action, question string, run func(allowWrites bool) tea.Cmd) tea.Cmd {
	if !m.writable() {
		return m.guarded(action, run)
	}
	m.ask(question,
		func() tea.Cmd { return run(false) },
		func() tea.Cmd {
			m.setInfo("Nothing changed")
			return nil
		})
	return nil
}

func capitalise(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// write changes the cluster with a client of its own, which allows writes
// on a read-only session only when the user agreed.
func (m *model) write(allowWrites bool, done string, fn func(ctx context.Context, kube kubernetes.Interface) error) tea.Cmd {
	b := m.b
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		kube, err := b.Kube(ctx, allowWrites)
		if err == nil {
			err = fn(ctx, kube)
		}
		return writeMsg{done: done, err: err}
	})
}

func (m *model) written(msg writeMsg) tea.Cmd {
	if msg.err != nil {
		m.setError(msg.err)
		return nil
	}
	m.setInfo("%s", msg.done)
	return m.loadWorkloads()
}

// restart restarts a workload, as tug restart does, after asking.
func (m *model) restart(w ops.WorkloadState) tea.Cmd {
	t := w.Target
	if err := ops.CheckRestart(t); err != nil {
		m.setError(err)
		return nil
	}
	now := m.now()
	return m.confirmed("restart "+t.String(), fmt.Sprintf("Restart %s?", t), func(allow bool) tea.Cmd {
		return m.write(allow, "Restarted "+t.String(), func(ctx context.Context, kube kubernetes.Interface) error {
			return ops.Restart(ctx, kube, t, now)
		})
	})
}

// scale asks for a replica count and scales a workload, as tug scale does.
func (m *model) scale(w ops.WorkloadState) {
	t := w.Target
	if err := ops.CheckScale(t); err != nil {
		m.setError(err)
		return
	}
	fields := []field{newField("replicas", strconv.Itoa(int(w.Desired)), "")}
	m.askFor(fmt.Sprintf("Scale %s to how many replicas?", t), fields, func(v []string) (tea.Cmd, error) {
		n, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("replicas must be a number from 0, got %q", v[0])
		}
		return m.guarded("scale "+t.String(), func(allow bool) tea.Cmd {
			return m.write(allow, fmt.Sprintf("Scaled %s to %d", t, n), func(ctx context.Context, kube kubernetes.Interface) error {
				return ops.Scale(ctx, kube, t, int32(n))
			})
		}), nil
	})
}

// forward asks for ports and a name, then has tugd run the forward, as tug
// pf --name does. Port-forwards are reads, so the guard does not ask.
func (m *model) forward(w ops.WorkloadState) {
	t := w.Target
	fields := []field{
		newField("ports", "", "8080, 9000:http or :80, several with spaces; empty for every declared port"),
		newField("name", t.Name, ""),
	}
	b := m.b
	m.askFor(fmt.Sprintf("Port-forward %s through tugd, which keeps it open across pod restarts", t), fields,
		func(v []string) (tea.Cmd, error) {
			name := strings.TrimSpace(v[1])
			if name == "" {
				return nil, errors.New("name the forward; tug pf stop <name> ends it")
			}
			ports := strings.FieldsFunc(v[0], func(r rune) bool { return r == ' ' || r == ',' })
			m.setInfo("Starting the forward %s…", name)
			return m.tasks.cmd(func(ctx context.Context) tea.Msg {
				st, err := b.Forward(ctx, t, name, ports)
				return forwardMsg{state: st, err: err}
			}), nil
		})
}

func (m *model) forwarded(msg forwardMsg) {
	if msg.err != nil {
		m.setError(msg.err)
		return
	}
	st := msg.state
	ports := make([]string, len(st.Bound))
	for i, b := range st.Bound {
		local, remote, _ := strings.Cut(b, ":")
		ports[i] = "localhost:" + local + " -> " + remote
	}
	m.setInfo("Forwarding %s to %s (pod/%s) as %s; tug pf ls shows it and tug pf stop %s ends it",
		strings.Join(ports, ", "), st.Target, st.Pod, st.Name, st.Name)
}

// shell finds the pod a shell opens in, as tug sh does: a running pod of
// the workload, and its default container.
func (m *model) shell(w ops.WorkloadState) tea.Cmd {
	t, kube := w.Target, m.kube
	m.setInfo("Finding a running pod of %s…", t)
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		pod, err := resolve.RunningPod(ctx, kube, t)
		if err != nil {
			return shellPodMsg{err: err}
		}
		return shellPodMsg{req: ShellRequest{Namespace: t.Namespace, Pod: pod.Name, Container: resolve.DefaultContainer(pod)}}
	})
}

// shellPod opens the shell, after the guard's question on a read-only
// session: running a command in a pod is a write.
func (m *model) shellPod(msg shellPodMsg) tea.Cmd {
	if msg.err != nil {
		m.setError(msg.err)
		return nil
	}
	m.flash = flash{}
	req := msg.req
	return m.guarded("Run a command in pod/"+req.Pod, func(allow bool) tea.Cmd {
		req.AllowWrites = allow
		return m.runShell(req)
	})
}

// shellCommand runs a shell while Bubble Tea has handed the terminal back.
type shellCommand struct {
	run    func(stdin io.Reader, stdout, stderr io.Writer) error
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

var _ tea.ExecCommand = (*shellCommand)(nil)

func (c *shellCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *shellCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *shellCommand) SetStderr(w io.Writer) { c.stderr = w }
func (c *shellCommand) Run() error            { return c.run(c.stdin, c.stdout, c.stderr) }

// runShell suspends the TUI, runs the shell on the terminal and comes back
// when it ends.
func (m *model) runShell(req ShellRequest) tea.Cmd {
	b, t := m.b, m.tasks
	c := &shellCommand{run: func(stdin io.Reader, stdout, stderr io.Writer) error {
		return t.do(func(ctx context.Context) error { return b.Shell(ctx, req, stdin, stdout, stderr) })
	}}
	return m.exec(c, func(err error) tea.Msg { return shellDoneMsg{req: req, err: err} })
}

// shellDone says how the shell ended. A container without a shell gets
// the offer tug sh makes: a debug container beside it, which is a write.
func (m *model) shellDone(msg shellDoneMsg) tea.Cmd {
	req := msg.req
	pod := "pod/" + req.Pod
	code, exited := ops.ExitCode(msg.err)
	switch {
	case msg.err == nil:
		m.setInfo("Left the shell in %s", pod)
	case exited:
		m.setInfo("The shell in %s exited with status %d", pod, code)
	case ops.NoShell(msg.err) && !req.Debug:
		m.ask(fmt.Sprintf("Container %s of %s has no shell. Add an ephemeral debug container (%s) to the pod?", req.Container, pod, ops.DefaultDebugImage),
			func() tea.Cmd {
				return m.guarded("Add a debug container to "+pod, func(allow bool) tea.Cmd {
					debug := req
					debug.Debug, debug.AllowWrites = true, allow
					return m.runShell(debug)
				})
			},
			func() tea.Cmd {
				m.setError(fmt.Errorf("container %s of %s has no shell; s again, then y, adds an ephemeral debug container that has one", req.Container, pod))
				return nil
			})
		return nil
	default:
		m.setError(msg.err)
	}
	return m.loadWorkloads()
}
