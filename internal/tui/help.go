package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// openHelp shows every key over the view.
func (m *model) openHelp() {
	m.showHelp = true
	m.helpVP = viewport.New()
	m.helpVP.KeyMap = viewportKeys(m.keys)
	m.resizeHelp()
}

// resizeHelp lays the help out again for the window.
func (m *model) resizeHelp() {
	if h := m.bodyHeight(); m.helpVP.Width() != m.width || m.helpVP.Height() != h {
		m.helpVP.SetWidth(m.width)
		m.helpVP.SetHeight(h)
		m.helpVP.SetContent(m.helpText())
	}
}

// helpKey scrolls the help when it is longer than the screen; any other
// key closes it.
func (m *model) helpKey(msg tea.KeyPressMsg) tea.Cmd {
	if scrolls(msg, m.helpVP.KeyMap) {
		m.helpVP, _ = m.helpVP.Update(msg)
		return nil
	}
	m.showHelp = false
	return nil
}

// helpText lists every key, grouped by where it works: in two columns when
// they fit side by side.
func (m *model) helpText() string {
	k := m.keys
	type group struct {
		title    string
		bindings []key.Binding
	}
	left := []group{
		{"The selected workload, in the list or its panes", k.actions()},
		{"Workloads", []key.Binding{k.Namespace, k.Clusters, k.Filter, k.Refresh}},
	}
	right := []group{
		{"Clusters", []key.Binding{k.TypeToFind, k.Connect}},
		{"Moving", []key.Binding{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom}},
		{"Everywhere", []key.Binding{k.Back, k.Help, k.Quit, k.ForceQuit}},
	}
	column := func(groups []group) string {
		var b strings.Builder
		for i, g := range groups {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(m.st.header.Render(g.title) + "\n")
			for _, kb := range g.bindings {
				fmt.Fprintf(&b, "  %s  %s\n", m.st.key.Render(pad(kb.Help().Key, 8)), kb.Help().Desc)
			}
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	l, r := column(left), column(right)
	var body string
	if lipgloss.Width(l)+lipgloss.Width(r)+4 <= m.width {
		body = lipgloss.JoinHorizontal(lipgloss.Top, l, "    ", r)
	} else {
		body = l + "\n\n" + r
	}
	note := "On a read-only session, shells, restarts and scaling ask first and are refused otherwise, as the commands are."
	return body + "\n\n" + m.st.dim.Render(note)
}
