package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/paingha/tugcli/internal/resolve"
	"github.com/paingha/tugcli/internal/session"
)

// namespacesMsg brings the cluster's namespaces.
type namespacesMsg struct {
	names []string
	err   error
}

// namespaceMsg says how switching the session's namespace went.
type namespaceMsg struct {
	state session.State
	err   error
}

// picker chooses a namespace: typing filters the list as tug ns matches,
// and when listing namespaces is not allowed, enter takes the typed name,
// as tug ns trusts the name it is given then.
type picker struct {
	title   string
	current string
	all     []string
	loaded  bool
	err     error
	filter  string
	shown   []string
	cursor  int
	offset  int
	rows    int
}

func (m *model) openNamespaces() tea.Cmd {
	m.picker = &picker{title: "Namespaces on " + m.state.ClusterName, current: m.state.Namespace}
	m.picker.scroll(m.pickerRows())
	b := m.b
	return m.tasks.cmd(func(ctx context.Context) tea.Msg {
		names, err := b.Namespaces(ctx)
		return namespacesMsg{names: names, err: err}
	})
}

func (p *picker) load(names []string, err error) {
	p.all, p.err, p.loaded = names, err, true
	p.apply()
	for i, n := range p.shown {
		if n == p.current {
			p.cursor = i
		}
	}
	p.scroll(p.rows)
}

func (p *picker) apply() {
	p.shown = p.shown[:0]
	if p.filter == "" {
		p.shown = append(p.shown, p.all...)
	} else {
		for _, m := range resolve.All(p.filter, p.all) {
			p.shown = append(p.shown, m.Value)
		}
	}
	p.cursor = 0
	p.scroll(p.rows)
}

func (p *picker) move(by int) {
	p.cursor = max(min(p.cursor+by, len(p.shown)-1), 0)
	p.scroll(p.rows)
}

func (p *picker) scroll(rows int) {
	p.rows = rows
	p.offset = scrolled(p.cursor, p.offset, rows, len(p.shown))
}

// choice is the namespace enter picks: the selected one, or what was typed
// when nothing is listed.
func (p *picker) choice() string {
	if p.cursor < len(p.shown) {
		return p.shown[p.cursor]
	}
	return strings.TrimSpace(p.filter)
}

// pickerRows is how many namespaces fit under the picker's filter line.
func (m *model) pickerRows() int { return m.bodyHeight() - 1 }

// pickerKey handles a key while the picker is open; every printable key
// types into its filter.
func (m *model) pickerKey(msg tea.KeyMsg) tea.Cmd {
	p := m.picker
	switch {
	case msg.Type == tea.KeyEsc:
		m.picker = nil
	case msg.Type == tea.KeyEnter:
		ns := p.choice()
		if ns == "" {
			return nil
		}
		m.picker = nil
		return m.setNamespace(ns)
	case edit(&p.filter, msg):
		p.apply()
	case msg.Type == tea.KeyUp || msg.Type == tea.KeyCtrlP:
		p.move(-1)
	case msg.Type == tea.KeyDown || msg.Type == tea.KeyCtrlN:
		p.move(1)
	case msg.Type == tea.KeyPgUp:
		p.move(-p.rows)
	case msg.Type == tea.KeyPgDown:
		p.move(p.rows)
	}
	return nil
}

func (m *model) setNamespace(ns string) tea.Cmd {
	b := m.b
	return m.tasks.cmd(func(context.Context) tea.Msg {
		st, err := b.SetNamespace(ns)
		return namespaceMsg{state: st, err: err}
	})
}

func (m *model) namespaceSet(msg namespaceMsg) tea.Cmd {
	if msg.err != nil {
		m.setError(msg.err)
		return nil
	}
	m.state = msg.state
	m.workloads = workloadList{}
	m.setInfo("Namespace %s on %s", msg.state.Namespace, msg.state.ClusterName)
	return m.enterWorkloads()
}

// pickerView draws the namespaces.
func (m *model) pickerView(height int) string {
	p := m.picker
	var b strings.Builder
	b.WriteString(m.st.dim.Render("Filter: "+p.filter+"▏") + "\n")
	switch {
	case !p.loaded:
		b.WriteString("Loading namespaces…")
		return b.String()
	case p.err != nil:
		b.WriteString(m.st.red.Render(truncate(errorText(p.err), m.width)) + "\n")
		b.WriteString("Type a namespace's name and press enter to use it anyway.")
		return b.String()
	case len(p.shown) == 0:
		fmt.Fprintf(&b, "No namespace matches; enter switches to %q.", strings.TrimSpace(p.filter))
		return b.String()
	}
	p.scroll(height - 1)
	end := min(p.offset+height-1, len(p.shown))
	for i := p.offset; i < end; i++ {
		name, note := p.shown[i], ""
		if name == p.current {
			note = "  (current)"
		}
		if i == p.cursor {
			b.WriteString(m.st.selected.Render("›") + " " + m.st.selected.Render(name+note) + "\n")
			continue
		}
		b.WriteString("  " + name + m.st.dim.Render(note) + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
