package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"k8s.io/client-go/kubernetes"

	"github.com/paingha/tugcli/internal/session"
	"github.com/paingha/tugcli/internal/tunnel"
	"github.com/paingha/tugcli/pkg/provider"
)

// view is the list the TUI shows.
type view int

const (
	viewClusters view = iota
	viewWorkloads
)

// level is how much a flash message matters.
type level int

const (
	levelInfo level = iota
	levelWarn
	levelError
)

// flash is the one-line message above the status bar.
type flash struct {
	text  string
	level level
}

// model is the TUI's state. Bubble Tea calls Update and View on one
// goroutine; work that blocks runs in commands through tasks and comes back
// as messages, so nothing else touches the model.
type model struct {
	b       Backend
	tasks   *tasks
	now     func() time.Time
	refresh time.Duration

	st   styles
	keys keyMap
	help help.Model

	width, height int
	view          view
	showHelp      bool
	flash         flash

	// state is the session as last read.
	state session.State
	// clusters is the catalog as last read; the status bar finds the
	// session's tunnel in it.
	clusters clusterList
	// kube reads the session's cluster; it is made once per cluster.
	// Writes get a client of their own from the Backend.
	kube    kubernetes.Interface
	kubeFor string

	workloads workloadList
	ticker    ticker
	picker    *picker
	pane      *pane
	panes     int // the last pane's ID
}

func newModel(b Backend, t *tasks, st session.State, sty styles, opts ...Option) *model {
	s := settings{refresh: DefaultRefresh, now: time.Now}
	for _, o := range opts {
		o(&s)
	}
	h := help.New()
	h.Styles = help.Styles{
		ShortKey: sty.key, ShortDesc: sty.dim, ShortSeparator: sty.dim, Ellipsis: sty.dim,
		FullKey: sty.key, FullDesc: sty.dim, FullSeparator: sty.dim,
	}
	h.ShortSeparator = "  "
	m := &model{
		b: b, tasks: t, now: s.now, refresh: s.refresh,
		st: sty, keys: newKeyMap(), help: h,
		state: st, view: viewClusters,
	}
	if st.Connected() {
		m.view = viewWorkloads
	}
	return m
}

// Init loads the catalog and, when connected, the workloads.
func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadCatalog()}
	if m.view == viewWorkloads {
		cmds = append(cmds, m.enterWorkloads())
	}
	return tea.Batch(cmds...)
}

// Update handles one message.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, m.handle(msg)
}

// handle routes the messages that commands send back.
func (m *model) handle(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case catalogMsg:
		m.clusters.load(msg, m.state.ClusterID)
		if msg.err != nil {
			m.setError(msg.err)
		}
	case connectedMsg:
		return m.connected(msg)
	case workloadsMsg:
		m.loadedWorkloads(msg)
	case tickMsg:
		return m.tick(msg)
	case namespacesMsg:
		if m.picker != nil {
			m.picker.load(msg.names, msg.err)
		}
	case namespaceMsg:
		return m.namespaceSet(msg)
	case logsMsg:
		return m.logs(msg)
	case paneMsg:
		m.paneLoaded(msg)
	}
	return nil
}

// key handles a key press: the open picker or help takes it first, then
// the pane, then the view.
func (m *model) key(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, m.keys.ForceQuit) {
		return m.quit()
	}
	switch {
	case m.picker != nil:
		return m.pickerKey(msg)
	case m.showHelp:
		m.showHelp = false // any key closes it
		return nil
	case m.pane != nil:
		return m.paneKey(msg)
	case m.view == viewClusters:
		return m.clustersKey(msg)
	default:
		return m.workloadsKey(msg)
	}
}

// quit stops what the views run and ends the program.
func (m *model) quit() tea.Cmd {
	m.closePane()
	m.ticker.stop()
	return tea.Quit
}

func (m *model) setInfo(format string, args ...any) {
	m.flash = flash{text: fmt.Sprintf(format, args...), level: levelInfo}
}

func (m *model) setWarning(format string, args ...any) {
	m.flash = flash{text: fmt.Sprintf(format, args...), level: levelWarn}
}

func (m *model) setError(err error) {
	m.flash = flash{text: errorText(err), level: levelError}
}

// errorText is an error as the CLI prints it, with a provider's hint.
func errorText(err error) string {
	text := err.Error()
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Hint != "" {
		text += "; hint: " + pe.Hint
	}
	return text
}

// edit applies a key to a line of typed text: runes add to it and backspace
// takes the last one away. It reports whether the key was for the line.
func edit(line *string, msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace:
		*line += string(msg.Runes)
	case tea.KeyBackspace:
		if r := []rune(*line); len(r) > 0 {
			*line = string(r[:len(r)-1])
		}
	default:
		return false
	}
	return true
}

// resize fits what scrolls to the window. The lists have a line of
// information and a line of headings above their rows.
func (m *model) resize() {
	h := m.bodyHeight()
	m.clusters.scroll(h - 2)
	m.workloads.scroll(h - 2)
	if m.pane != nil {
		m.pane.resize(m.width, h)
	}
	if m.picker != nil {
		m.picker.scroll(m.pickerRows())
	}
}

// bodyHeight is the height between the title line and the footer.
func (m *model) bodyHeight() int {
	return max(m.height-2-lineCount(m.footer()), 1)
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// View draws the screen: a title line, the body, then the prompt or flash
// message and the status bar.
func (m *model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	footer := m.footer()
	h := max(m.height-2-lineCount(footer), 1)
	var body string
	switch {
	case m.showHelp:
		body = m.helpView()
	case m.picker != nil:
		body = m.pickerView(h)
	case m.pane != nil:
		body = m.paneView()
	case m.view == viewClusters:
		body = m.clustersView(h)
	default:
		body = m.workloadsView(h)
	}
	lines := strings.Split(body, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	parts := []string{m.titleLine(), strings.Join(lines, "\n")}
	if footer != "" {
		parts = append(parts, footer)
	}
	parts = append(parts, m.statusBar())
	return strings.Join(parts, "\n")
}

// titleLine names what is on screen, with the keys that matter there.
func (m *model) titleLine() string {
	k := m.keys
	var title string
	var keys []key.Binding
	switch {
	case m.showHelp:
		title, keys = "Help", []key.Binding{k.Back}
	case m.picker != nil:
		title, keys = m.picker.title, []key.Binding{k.Connect, k.Back}
	case m.pane != nil:
		title, keys = m.pane.title(), append(k.actions(), k.Back, k.Help, k.Quit)
	case m.view == viewClusters:
		title, keys = "Clusters", []key.Binding{k.Connect, k.Back, k.Help, k.Quit}
	default:
		title = "Workloads in " + m.state.Namespace
		keys = append(k.actions(), k.Namespace, k.Clusters, k.Filter, k.Help, k.Quit)
	}
	title = m.st.title.Render(" " + title + " ")
	m.help.Width = max(m.width-lipgloss.Width(title)-1, 0)
	short := m.help.ShortHelpView(hints(keys...))
	gap := max(m.width-lipgloss.Width(title)-lipgloss.Width(short), 1)
	return title + strings.Repeat(" ", gap) + short
}

// footer is the flash message.
func (m *model) footer() string {
	if m.flash.text == "" {
		return ""
	}
	text := truncate(m.flash.text, m.width-1)
	switch m.flash.level {
	case levelError:
		return m.st.red.Render(text)
	case levelWarn:
		return m.st.yellow.Render(text)
	default:
		return text
	}
}

// statusBar shows the session: cluster, namespace, whether writes are
// allowed and how tug reaches the cluster.
func (m *model) statusBar() string {
	st := m.state
	base := m.st.status
	type segment struct {
		text  string
		style lipgloss.Style
	}
	segments := []segment{{"not connected", base}}
	if st.Connected() {
		mode, colour := m.mode()
		segments = []segment{
			{st.ClusterName, base.Bold(true)},
			{"namespace " + st.Namespace, base},
			{mode, base.Inherit(colour)},
			{m.route(), base},
		}
	}
	// Each part is coloured on its own, so the bar's reverse video runs
	// unbroken from one end to the other.
	var b strings.Builder
	left := m.width
	for i, s := range segments {
		sep := "  │  "
		if i == 0 {
			sep = " "
		}
		if left <= lipgloss.Width(sep) {
			break
		}
		b.WriteString(base.Render(sep))
		left -= lipgloss.Width(sep)
		text := truncate(s.text, left)
		b.WriteString(s.style.Render(text))
		left -= lipgloss.Width(text)
	}
	if left > 0 {
		b.WriteString(base.Render(strings.Repeat(" ", left)))
	}
	return b.String()
}

// mode says whether the session may write, as tug status does, and the
// colour to say it in.
func (m *model) mode() (string, lipgloss.Style) {
	st := m.state
	switch {
	case st.ReadOnly && m.now().Before(st.UnlockedUntil):
		return "unlocked until " + st.UnlockedUntil.Local().Format(time.Kitchen), m.st.yellow
	case st.ReadOnly:
		return "read-only", m.st.red
	default:
		return "read-write", m.st.green
	}
}

// route says how tug reaches the session's cluster: through its tunnel,
// through tugd's proxy, or directly.
func (m *model) route() string {
	var parts []string
	if cl := m.clusters.byID(m.state.ClusterID); cl != nil && cl.Tunnel != "" {
		parts = append(parts, "tunnel "+tunnel.Redacted(cl.Tunnel))
	}
	if m.state.ProxyURL != "" {
		parts = append(parts, "via tugd")
	}
	if len(parts) == 0 {
		return "direct"
	}
	return strings.Join(parts, " ")
}

// helpView lists every key, grouped by where it works.
func (m *model) helpView() string {
	k := m.keys
	groups := []struct {
		title    string
		bindings []key.Binding
	}{
		{"The selected workload, in the list or its panes", k.actions()},
		{"Workloads", []key.Binding{k.Namespace, k.Clusters, k.Filter, k.Refresh}},
		{"Clusters", []key.Binding{k.TypeToFind, k.Connect}},
		{"Moving", []key.Binding{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom}},
		{"Everywhere", []key.Binding{k.Back, k.Help, k.Quit, k.ForceQuit}},
	}
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
	return b.String()
}
