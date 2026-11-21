package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/render"
)

// paneKind is what a pane shows.
type paneKind int

const (
	paneLogs paneKind = iota
	paneWhy
	paneTree
)

const (
	// logTail is how many lines each container's log starts with.
	logTail = 100
	// maxPaneLines is how many lines a log pane keeps; older ones go.
	maxPaneLines = 5000
	// maxLogBatch is how many log lines one message brings at most.
	maxLogBatch = 500
)

// logEvent is one thing a log stream says.
type logEvent struct {
	line ops.LogLine
	// warn is a container whose stream failed, while the others go on.
	warn string
	// err ends the stream.
	err error
}

// logsMsg brings log lines to the pane with ID id.
type logsMsg struct {
	id     int
	events []logEvent
	done   bool
}

// paneMsg brings a diagnosis or tree to the pane with ID id.
type paneMsg struct {
	id   int
	why  *ops.Diagnosis
	tree *ops.TreeNode
	err  error
}

// pane shows one workload's logs, diagnosis or tree over the list, and
// scrolls. A log pane owns its stream and ends it when it closes.
type pane struct {
	id      int
	kind    paneKind
	w       ops.WorkloadState
	lines   []string
	vp      viewport.Model
	loading bool
	err     error

	// follow keeps the newest log line in view; scrolling up stops it.
	follow bool
	done   bool
	events <-chan logEvent
	stop   func()
}

func (p *pane) title() string {
	t := p.w.Target.String()
	switch p.kind {
	case paneWhy:
		return "Why " + t
	case paneTree:
		return "Tree of " + t
	default:
		return "Logs of " + t
	}
}

// resize gives the viewport the body less the pane's status line.
func (p *pane) resize(width, height int) {
	height = max(height-1, 1)
	if p.vp.Width() == width && p.vp.Height() == height {
		return
	}
	p.vp.SetWidth(width)
	p.vp.SetHeight(height)
	p.show()
}

// show puts the lines in the viewport, at the bottom while following.
func (p *pane) show() {
	p.vp.SetContent(strings.Join(p.lines, "\n"))
	if p.follow {
		p.vp.GotoBottom()
	}
}

// viewportKeys scroll a pane. l and h are left to the workload's actions;
// the arrows scroll sideways.
func viewportKeys(k keyMap) viewport.KeyMap {
	return viewport.KeyMap{
		Up:           k.Up,
		Down:         k.Down,
		PageUp:       k.PageUp,
		PageDown:     k.PageDown,
		HalfPageUp:   key.NewBinding(key.WithKeys("u", "ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("d", "ctrl+d")),
		Left:         key.NewBinding(key.WithKeys("left")),
		Right:        key.NewBinding(key.WithKeys("right")),
	}
}

// openPane replaces any open pane with a new one for w.
func (m *model) openPane(kind paneKind, w ops.WorkloadState) *pane {
	m.closePane()
	m.panes++
	p := &pane{id: m.panes, kind: kind, w: w, loading: true}
	p.vp = viewport.New()
	p.vp.KeyMap = viewportKeys(m.keys)
	p.vp.SetHorizontalStep(8)
	p.resize(m.width, m.bodyHeight())
	m.pane = p
	return p
}

// closePane closes the pane, ending its log stream.
func (m *model) closePane() {
	if m.pane != nil && m.pane.stop != nil {
		m.pane.stop()
	}
	m.pane = nil
}

// openLogs streams logs from every pod and container of w through
// ops.Logs, following new lines and new pods.
func (m *model) openLogs(w ops.WorkloadState) tea.Cmd {
	kube := m.kube
	p := m.openPane(paneLogs, w)
	p.follow = true
	events := make(chan logEvent, 256)
	p.events = events
	t := w.Target
	p.stop = m.tasks.start(func(ctx context.Context) {
		defer close(events)
		send := func(ev logEvent) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}
		err := ops.Logs(ctx, kube, t, ops.LogOptions{Follow: true, Tail: logTail}, ops.LogSink{
			Line: func(l ops.LogLine) { send(logEvent{line: l}) },
			Error: func(pod, container string, err error) {
				if pod == "" {
					send(logEvent{warn: err.Error()})
					return
				}
				send(logEvent{warn: fmt.Sprintf("%s/%s: %v", pod, container, err)})
			},
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			send(logEvent{err: err})
		}
	})
	return m.waitLogs(p)
}

// waitLogs waits for the next log lines, and takes every line that has
// come since, up to a batch. The stream closes its channel when it ends,
// pane closed or not, so this never waits for ever.
func (m *model) waitLogs(p *pane) tea.Cmd {
	id, events := p.id, p.events
	return m.tasks.cmd(func(context.Context) tea.Msg {
		ev, ok := <-events
		if !ok {
			return logsMsg{id: id, done: true}
		}
		msg := logsMsg{id: id, events: []logEvent{ev}}
		for len(msg.events) < maxLogBatch {
			select {
			case ev, ok := <-events:
				if !ok {
					msg.done = true
					return msg
				}
				msg.events = append(msg.events, ev)
			default:
				return msg
			}
		}
		return msg
	})
}

func (m *model) logs(msg logsMsg) tea.Cmd {
	p := m.pane
	if p == nil || p.id != msg.id {
		return nil // from a pane that has closed
	}
	p.loading = false
	for _, ev := range msg.events {
		switch {
		case ev.err != nil:
			p.err = ev.err
		case ev.warn != "":
			p.lines = append(p.lines, m.st.yellow.Render("warning: "+clean(ev.warn)))
		default:
			l := ev.line
			p.lines = append(p.lines, m.st.pickFor(l.Pod, l.Pod+"/"+l.Container)+" "+clean(l.Text))
		}
	}
	if n := len(p.lines) - maxPaneLines; n > 0 {
		p.lines = append(p.lines[:0], p.lines[n:]...)
	}
	p.show()
	if msg.done {
		p.done = true
		return nil
	}
	return m.waitLogs(p)
}

// controls matches what a log line must not send to the terminal: escape
// sequences, which could move the cursor or clear the screen, and other
// control characters.
var controls = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?|[\x00-\x08\x0b-\x1f\x7f]`)

// clean makes a log line safe to draw: tabs become spaces and control
// characters go.
func clean(s string) string {
	return controls.ReplaceAllString(strings.ReplaceAll(s, "\t", "    "), "")
}

// openReport shows why w is not ready, or its tree.
func (m *model) openReport(kind paneKind, w ops.WorkloadState) tea.Cmd {
	m.openPane(kind, w)
	return m.loadReport()
}

// loadReport reads the open pane's diagnosis or tree.
func (m *model) loadReport() tea.Cmd {
	p := m.pane
	p.loading, p.err = true, nil
	id, kind, t, kube, now := p.id, p.kind, p.w.Target, m.kube, m.now
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		if kind == paneWhy {
			d, err := ops.Why(ctx, kube, t, ops.WhyOptions{Now: now})
			return paneMsg{id: id, why: d, err: err}
		}
		root, err := ops.Tree(ctx, kube, t)
		return paneMsg{id: id, tree: root, err: err}
	})
}

func (m *model) paneLoaded(msg paneMsg) {
	p := m.pane
	if p == nil || p.id != msg.id {
		return
	}
	p.loading, p.err = false, msg.err
	switch {
	case msg.why != nil:
		p.lines = m.whyLines(msg.why)
	case msg.tree != nil:
		p.lines = m.treeLines(*msg.tree)
	}
	p.show()
}

// paneKey handles a key while a pane is open. The workload's actions work
// on the pane's workload, so l, w and t switch between its panes.
func (m *model) paneKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.pane
	k := m.keys
	switch {
	case key.Matches(msg, k.Back):
		m.closePane()
		return m.loadWorkloads()
	case key.Matches(msg, k.Quit):
		return m.quit()
	case key.Matches(msg, k.Help):
		m.openHelp()
	case key.Matches(msg, k.Refresh):
		if p.kind == paneLogs {
			return m.openLogs(p.w)
		}
		return m.loadReport()
	case key.Matches(msg, k.Top):
		p.follow = false
		p.vp.GotoTop()
	case key.Matches(msg, k.Bottom):
		p.follow = p.kind == paneLogs
		p.vp.GotoBottom()
	case scrolls(msg, p.vp.KeyMap):
		p.vp, _ = p.vp.Update(msg)
		p.follow = p.kind == paneLogs && p.vp.AtBottom()
	default:
		return m.act(msg, p.w)
	}
	return nil
}

// scrolls reports whether a key scrolls a viewport.
func scrolls(msg tea.KeyPressMsg, k viewport.KeyMap) bool {
	return key.Matches(msg, k.Up, k.Down, k.PageUp, k.PageDown, k.HalfPageUp, k.HalfPageDown, k.Left, k.Right)
}

// paneView draws the pane's status line over its lines.
func (m *model) paneView() string {
	p := m.pane
	var status string
	switch {
	case p.err != nil:
		status = m.st.red.Render(truncate(errorText(p.err), m.width))
	case p.loading:
		status = m.st.dim.Render("Loading…")
	case p.kind == paneLogs:
		state := "following new lines and new pods; scroll up to stop, G to follow again"
		switch {
		case p.done:
			state = "the stream has ended; ctrl+r starts it again"
		case !p.follow:
			state = "paused; G follows new lines again"
		}
		status = m.st.dim.Render(fmt.Sprintf("%d lines, %s", len(p.lines), state))
	default:
		status = m.st.dim.Render("ctrl+r reloads")
	}
	return status + "\n" + p.vp.View()
}

// whyLines lays out a diagnosis as tug why prints it.
func (m *model) whyLines(d *ops.Diagnosis) []string {
	var b strings.Builder
	state := m.st.red.Render("not ready")
	if d.Healthy {
		state = m.st.green.Render("healthy")
	}
	fmt.Fprintf(&b, "%s in %s is %s: %s\n", m.st.bold.Render(d.Target), d.Namespace, state, d.Summary)
	now := m.now()
	if len(d.Pods) > 0 {
		b.WriteByte('\n')
		rows := make([][]string, 0, len(d.Pods))
		for _, p := range d.Pods {
			rows = append(rows, []string{
				p.Name, p.Status, fmt.Sprintf("%d/%d", p.ReadyContainers, p.Containers),
				strconv.Itoa(int(p.Restarts)), dash(p.Node), duration.HumanDuration(now.Sub(p.Created)),
			})
		}
		_ = render.Table(&b, []string{"POD", "STATUS", "READY", "RESTARTS", "NODE", "AGE"}, rows) // a Builder does not fail
	}
	if len(d.Findings) > 0 {
		b.WriteByte('\n')
	}
	const indent = "         " // under the severity column
	for _, f := range d.Findings {
		subject := strings.Join(f.Objects, ", ")
		if f.Container != "" {
			subject += ", container " + f.Container
		}
		fmt.Fprintf(&b, "%s %s: %s\n", m.severity(f.Severity), subject, m.st.bold.Render(f.Reason))
		fmt.Fprintf(&b, "%s%s\n", indent, f.Message)
		if f.Next != "" {
			fmt.Fprintf(&b, "%s%s %s\n", indent, m.st.dim.Render("next:"), f.Next)
		}
	}
	if d.Healthy && len(d.Findings) == 0 {
		fmt.Fprintf(&b, "\n%s\n", m.st.green.Render("Nothing wrong found."))
	}
	if len(d.Events) > 0 {
		fmt.Fprintf(&b, "\n%s\n", m.st.bold.Render("Recent warning events:"))
		rows := make([][]string, 0, len(d.Events))
		for _, e := range d.Events {
			seen := duration.HumanDuration(now.Sub(e.LastSeen))
			if e.Count > 1 {
				seen += fmt.Sprintf(" (x%d)", e.Count)
			}
			rows = append(rows, []string{seen, e.Object, e.Reason, e.Message})
		}
		_ = render.Table(&b, []string{"LAST SEEN", "OBJECT", "REASON", "MESSAGE"}, rows) // a Builder does not fail
	}
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// severity pads and colours a finding's severity for the column it starts.
func (m *model) severity(s ops.Severity) string {
	label := fmt.Sprintf("%-8s", s)
	switch s {
	case ops.SeverityError:
		return m.st.red.Render(label)
	case ops.SeverityWarning:
		return m.st.yellow.Render(label)
	default:
		return m.st.dim.Render(label)
	}
}

// treeLines draws a tree as tug tree does, each part marked ready (✓), not
// ready (✗) or unknown (?).
func (m *model) treeLines(root ops.TreeNode) []string {
	var walk func(n ops.TreeNode) render.TreeNode
	walk = func(n ops.TreeNode) render.TreeNode {
		var label string
		switch n.Kind {
		case ops.KindContainer:
			label = "container " + n.Name
		case ops.KindInitContainer:
			label = "init container " + n.Name
		case ops.KindEphemeralContainer:
			label = "ephemeral container " + n.Name
		default:
			label = ops.ShortKind(n.Kind) + "/" + n.Name
		}
		marker := m.st.yellow.Render("?")
		switch n.Health {
		case ops.HealthReady:
			marker = m.st.green.Render("✓")
		case ops.HealthNotReady:
			marker = m.st.red.Render("✗")
		}
		out := render.TreeNode{Label: label, Detail: marker + " " + n.Status}
		for _, c := range n.Children {
			out.Children = append(out.Children, walk(c))
		}
		return out
	}
	tree := walk(root)
	tree.Label = m.st.bold.Render(tree.Label)
	var b strings.Builder
	_ = render.Tree(&b, tree) // a Builder does not fail
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}
