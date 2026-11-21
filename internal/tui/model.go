package tui

import (
	"cmp"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	// exec hands the terminal to a command while the TUI is suspended. It
	// is tea.Exec; tests run the command themselves.
	exec func(tea.ExecCommand, tea.ExecCallback) tea.Cmd

	st   styles
	keys keyMap
	help help.Model

	width, height int
	view          view
	showHelp      bool
	helpVP        viewport.Model
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
	prompt    *prompt
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
		b: b, tasks: t, now: s.now, refresh: s.refresh, exec: tea.Exec,
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
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that does not say its size gets the classic one.
		m.width, m.height = cmp.Or(msg.Width, 80), cmp.Or(msg.Height, 24)
	case tea.KeyPressMsg:
		cmd = m.key(msg)
	case tea.PasteMsg:
		cmd = m.paste(msg)
	default:
		cmd = m.handle(msg)
	}
	// A prompt or message that opened or closed changes the room the body
	// has.
	m.resize()
	return m, cmd
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
	case shellPodMsg:
		return m.shellPod(msg)
	case shellDoneMsg:
		return m.shellDone(msg)
	case forwardMsg:
		m.forwarded(msg)
	case writeMsg:
		return m.written(msg)
	}
	return nil
}

// key handles a key press: the open prompt, picker or help takes it first,
// then the pane, then the view.
func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, m.keys.ForceQuit) {
		return m.quit()
	}
	switch {
	case m.prompt != nil:
		return m.promptKey(msg)
	case m.picker != nil:
		return m.pickerKey(msg)
	case m.showHelp:
		return m.helpKey(msg)
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

// edit applies a key to a line of typed text: printable keys add to it and
// backspace takes the last character away. It reports whether the key was
// for the line.
func edit(line *string, msg tea.KeyPressMsg) bool {
	switch {
	case msg.Code == tea.KeyBackspace:
		if r := []rune(*line); len(r) > 0 {
			*line = string(r[:len(r)-1])
		}
	case msg.Text != "" && !msg.Mod.Contains(tea.ModCtrl) && !msg.Mod.Contains(tea.ModAlt):
		*line += msg.Text
	default:
		return false
	}
	return true
}

// paste types pasted text into whatever takes typing: a prompt's field or a
// filter, as one line. A paste is not a key press, so it never runs an
// action.
func (m *model) paste(msg tea.PasteMsg) tea.Cmd {
	text := strings.Join(strings.Fields(msg.Content), " ")
	switch {
	case m.prompt != nil:
		if len(m.prompt.fields) == 0 {
			return nil
		}
		f := &m.prompt.fields[m.prompt.focus]
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(tea.PasteMsg{Content: text})
		return cmd
	case m.picker != nil:
		m.picker.filter += text
		m.picker.apply()
	case m.showHelp || m.pane != nil:
	case m.view == viewClusters:
		m.clusters.filter += text
		m.clusters.apply("")
	case m.workloads.filtering:
		m.workloads.filter += text
		m.workloads.apply(m.workloads.selectedName())
	}
	return nil
}

// resize fits what scrolls to the window. The lists have a line of
// information and a line of headings above their rows.
func (m *model) resize() {
	h := m.bodyHeight()
	m.clusters.scroll(h - 2)
	m.workloads.scroll(h - 2)
	if m.showHelp {
		m.resizeHelp()
	}
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

// View draws the screen on the terminal's alternate screen.
func (m *model) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	return v
}

// screen draws a title line, the body, then the prompt or flash message
// and the status bar.
func (m *model) screen() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	footer := m.footer()
	h := max(m.height-2-lineCount(footer), 1)
	var body string
	switch {
	case m.showHelp:
		body = m.helpVP.View()
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
	m.help.SetWidth(max(m.width-lipgloss.Width(title)-1, 0))
	short := m.help.ShortHelpView(hints(keys...))
	gap := max(m.width-lipgloss.Width(title)-lipgloss.Width(short), 1)
	return title + strings.Repeat(" ", gap) + short
}

// footer is the open prompt, or else the flash message.
func (m *model) footer() string {
	if m.prompt != nil {
		return m.promptView()
	}
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
