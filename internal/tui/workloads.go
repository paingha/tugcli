package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// workloadsMsg brings the workloads of a namespace.
type workloadsMsg struct {
	cluster   string
	namespace string
	// kube is the client the load made, when the model had none.
	kube  kubernetes.Interface
	items []ops.WorkloadState
	err   error
}

// tickMsg says it is time to reload the workloads view.
type tickMsg struct{ gen int }

// ticker reloads the workloads view on an interval while it is shown. Each
// run of it has a generation, so a tick from an earlier run is ignored, and
// a channel that is closed when the run ends, so its timer stops at once.
type ticker struct {
	gen  int
	done chan struct{}
}

func (t *ticker) stop() {
	if t.done != nil {
		close(t.done)
		t.done = nil
	}
	t.gen++
}

// workloadList is the workloads view: the session namespace's workloads,
// filtered by name.
type workloadList struct {
	items   []ops.WorkloadState
	loaded  bool
	loading bool
	err     error
	updated time.Time

	filter    string
	filtering bool
	shown     []int // indexes into items
	cursor    int
	offset    int
	rows      int
}

// enterWorkloads shows the workloads view, loads it and reloads it on the
// refresh interval until the view ends.
func (m *model) enterWorkloads() tea.Cmd {
	m.view = viewWorkloads
	m.ticker.stop()
	cmds := []tea.Cmd{m.loadWorkloads()}
	if m.refresh > 0 {
		m.ticker.done = make(chan struct{})
		cmds = append(cmds, m.nextTick())
	}
	return tea.Batch(cmds...)
}

// enterClusters shows the clusters view with nothing typed, and stops the
// refresh timer.
func (m *model) enterClusters() tea.Cmd {
	m.ticker.stop()
	m.view = viewClusters
	m.clusters.filter, m.clusters.filtering = "", false
	m.clusters.apply(m.clusters.current)
	return m.loadCatalog()
}

// nextTick waits for the next reload of the current run of the ticker.
func (m *model) nextTick() tea.Cmd {
	gen, done, d := m.ticker.gen, m.ticker.done, m.refresh
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return nil
		case <-t.C:
			return tickMsg{gen: gen}
		}
	})
}

func (m *model) tick(msg tickMsg) tea.Cmd {
	if msg.gen != m.ticker.gen || m.ticker.done == nil {
		return nil // from a run that has ended
	}
	cmds := []tea.Cmd{m.nextTick()}
	// Nothing reloads under a pane or the picker, or while a load is still
	// on its way.
	if m.pane == nil && m.picker == nil && !m.workloads.loading {
		cmds = append(cmds, m.loadWorkloads())
	}
	return tea.Batch(cmds...)
}

// loadWorkloads reads the session namespace's workloads, making the
// client first when there is none for the session's cluster.
func (m *model) loadWorkloads() tea.Cmd {
	if !m.state.Connected() {
		return nil
	}
	cluster, ns := m.state.ClusterID, m.state.Namespace
	kube := m.kube
	if m.kubeFor != cluster {
		kube = nil
	}
	m.workloads.loading = true
	b := m.b
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		msg := workloadsMsg{cluster: cluster, namespace: ns}
		if kube == nil {
			k, err := b.Kube(ctx, false)
			if err != nil {
				msg.err = err
				return msg
			}
			kube, msg.kube = k, k
		}
		msg.items, msg.err = ops.Workloads(ctx, kube, ns)
		return msg
	})
}

func (m *model) loadedWorkloads(msg workloadsMsg) {
	if msg.cluster != m.state.ClusterID {
		return // the session has moved to another cluster since
	}
	if msg.kube != nil && m.kubeFor != msg.cluster {
		m.kube, m.kubeFor = msg.kube, msg.cluster
	}
	if msg.namespace != m.state.Namespace {
		return
	}
	l := &m.workloads
	l.loading = false
	l.err = msg.err
	if msg.err != nil {
		return // keep showing what was there, under the error
	}
	was := l.selectedName()
	l.items, l.loaded, l.updated = msg.items, true, m.now()
	l.apply(was)
}

// selectedName names the selected workload, or is "" when there is none.
func (l *workloadList) selectedName() string {
	if w := l.selected(); w != nil {
		return w.Target.String()
	}
	return ""
}

// apply filters the workloads again and puts the cursor on the workload
// named was when it is still shown.
func (l *workloadList) apply(was string) {
	l.shown = l.shown[:0]
	if l.filter == "" {
		for i := range l.items {
			l.shown = append(l.shown, i)
		}
	} else {
		names := make([]string, len(l.items))
		index := map[string]int{}
		for i, w := range l.items {
			names[i] = w.Target.String()
			index[names[i]] = i
		}
		for _, match := range resolve.All(l.filter, names) {
			l.shown = append(l.shown, index[match.Value])
		}
	}
	l.cursor = min(l.cursor, max(len(l.shown)-1, 0))
	for i, idx := range l.shown {
		if l.items[idx].Target.String() == was {
			l.cursor = i
		}
	}
	l.scroll(l.rows)
}

func (l *workloadList) selected() *ops.WorkloadState {
	if l.cursor < 0 || l.cursor >= len(l.shown) {
		return nil
	}
	return &l.items[l.shown[l.cursor]]
}

func (l *workloadList) move(by int) {
	l.cursor = max(min(l.cursor+by, len(l.shown)-1), 0)
	l.scroll(l.rows)
}

func (l *workloadList) scroll(rows int) {
	l.rows = rows
	l.offset = scrolled(l.cursor, l.offset, rows, len(l.shown))
}

// moveKey moves a list's cursor for the movement keys, and reports
// whether the key was one of them.
func (m *model) moveKey(msg tea.KeyMsg, move func(int), rows, total int) bool {
	k := m.keys
	switch {
	case key.Matches(msg, k.Up):
		move(-1)
	case key.Matches(msg, k.Down):
		move(1)
	case key.Matches(msg, k.PageUp):
		move(-rows)
	case key.Matches(msg, k.PageDown):
		move(rows)
	case key.Matches(msg, k.Top):
		move(-total)
	case key.Matches(msg, k.Bottom):
		move(total)
	default:
		return false
	}
	return true
}

// workloadsKey handles a key in the workloads view.
func (m *model) workloadsKey(msg tea.KeyMsg) tea.Cmd {
	l := &m.workloads
	if l.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			l.filtering = false
		case tea.KeyEsc:
			l.filter, l.filtering = "", false
			l.apply(l.selectedName())
		case tea.KeyUp:
			l.move(-1)
		case tea.KeyDown:
			l.move(1)
		default:
			if edit(&l.filter, msg) {
				l.apply(l.selectedName())
			}
		}
		return nil
	}
	k := m.keys
	switch {
	case m.moveKey(msg, l.move, l.rows, len(l.shown)):
		return nil
	case key.Matches(msg, k.Quit):
		return m.quit()
	case key.Matches(msg, k.Help):
		m.openHelp()
	case key.Matches(msg, k.Back):
		if l.filter != "" {
			l.filter = ""
			l.apply(l.selectedName())
		}
	case key.Matches(msg, k.Filter):
		l.filtering = true
	case key.Matches(msg, k.Refresh):
		return m.loadWorkloads()
	case key.Matches(msg, k.Namespace):
		return m.openNamespaces()
	case key.Matches(msg, k.Clusters):
		return m.enterClusters()
	default:
		if w := l.selected(); w != nil {
			return m.act(msg, *w)
		}
	}
	return nil
}

// workloadsView draws the session namespace's workloads.
func (m *model) workloadsView(height int) string {
	l := &m.workloads
	var b strings.Builder
	info := ""
	switch {
	case l.filtering || l.filter != "":
		info = "Filter: " + l.filter
		if l.filtering {
			info += "▏"
		}
	case l.loading && !l.loaded:
		info = "Loading…"
	case l.loaded:
		info = fmt.Sprintf("%d workloads on %s, updated %s", len(l.items), m.state.ClusterName, l.updated.Format(time.TimeOnly))
	}
	if l.err != nil {
		b.WriteString(m.st.red.Render(truncate(errorText(l.err), m.width)) + "\n")
	} else {
		b.WriteString(m.st.dim.Render(info) + "\n")
	}
	switch {
	case !l.loaded:
		return strings.TrimSuffix(b.String(), "\n")
	case len(l.items) == 0:
		fmt.Fprintf(&b, "Nothing runs in namespace %s. n switches namespace.", m.state.Namespace)
		return b.String()
	case len(l.shown) == 0:
		b.WriteString("Nothing matches. esc clears the filter.")
		return b.String()
	}
	now := m.now()
	rows := make([][]string, len(l.shown))
	for i, idx := range l.shown {
		w := l.items[idx]
		mark := "✓"
		if w.Health != ops.HealthReady {
			mark = "✗"
		}
		age := "-"
		if !w.Created.IsZero() {
			age = duration.HumanDuration(now.Sub(w.Created))
		}
		rows[i] = []string{mark, w.Target.String(), fmt.Sprintf("%d/%d", w.Ready, w.Desired), w.Status, fmt.Sprint(w.Restarts), age}
	}
	t := table{
		columns: []column{{"", 1}, {"NAME", 60}, {"READY", 7}, {"STATUS", 40}, {"RESTARTS", 8}, {"AGE", 0}},
		rows:    rows,
		style: func(row, col int, cell string) string {
			healthy := rows[row][0] == "✓"
			switch {
			case col == 0 && healthy:
				return m.st.green.Render(cell)
			case col == 0 || (col == 3 && !healthy):
				return m.st.red.Render(cell)
			}
			return cell
		},
	}
	ws := t.widths(m.width - 2)
	b.WriteString("  " + m.st.header.Render(t.headingLine(ws)) + "\n")
	l.scroll(height - 2)
	end := min(l.offset+height-2, len(rows))
	for i := l.offset; i < end; i++ {
		b.WriteString(t.row(m.st, ws, i, i == l.cursor) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
