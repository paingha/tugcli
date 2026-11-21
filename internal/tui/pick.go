package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// PickAction is something the picker can run on the object chosen.
type PickAction struct {
	// Key chooses the action in the picker's menu, such as "l".
	Key string
	// Name is the action, such as "logs".
	Name string
}

// PickOptions say what Pick offers.
type PickOptions struct {
	// Cluster and Namespace head the picker, so it is always plain where
	// the objects are. Namespace "" means every namespace.
	Cluster   string
	Namespace string
	Objects   []ops.ObjectState
	// Actions are offered, in order, once an object is chosen. With one
	// action, enter runs it straight away.
	Actions []PickAction
	// Query is the filter the picker starts with.
	Query string
	Now   func() time.Time
}

// Picked is what the user chose.
type Picked struct {
	Object ops.ObjectState
	Action string
}

// ErrPickCancelled is returned when the user leaves the picker without
// choosing.
var ErrPickCancelled = errors.New("nothing picked")

// Pick shows a full-screen fuzzy picker over objects on a terminal, and
// returns the object and action the user chose. Typing filters the list as
// FilterObjects does; enter chooses an object and then an action. Leaving
// with esc or Ctrl-C returns ErrPickCancelled, as does ctx ending.
func Pick(ctx context.Context, in io.Reader, out io.Writer, o PickOptions) (Picked, error) {
	m := newPickModel(o, newStyles())
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		// tug's own signal handling cancels ctx.
		tea.WithoutSignalHandler(),
	)
	if _, err := p.Run(); err != nil {
		if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
			return Picked{}, ErrPickCancelled
		}
		return Picked{}, err
	}
	if m.picked == nil {
		return Picked{}, ErrPickCancelled
	}
	return *m.picked, nil
}

// pickTerms split a filter into the words that must each match a row.
func pickTerms(query string) []string { return strings.Fields(strings.ToLower(query)) }

// ObjectLabel names an object in the picker as tug prints objects, with
// kubectl's short kind: "deploy/api-server".
func ObjectLabel(o ops.ObjectState) string { return ops.ShortKind(o.Object.Kind) + "/" + o.Object.Name }

// FilterObjects returns the indexes of the objects that match a filter,
// best first. The filter is words separated by spaces, each of which must
// match the object's name as tug resolves names, from an exact match down to
// a fuzzy one, or else be found in its kind, namespace, status or owner:
// "api crash" finds the api pods in CrashLoopBackOff. Objects whose names
// match better come first; ties keep the objects' order.
func FilterObjects(objects []ops.ObjectState, query string) []int {
	terms := pickTerms(query)
	type scored struct {
		i     int
		score int
	}
	var out []scored
	for i, o := range objects {
		total, ok := 0, true
		for _, t := range terms {
			s, matched := termScore(o, t)
			if !matched {
				ok = false
				break
			}
			total += s
		}
		if ok {
			out = append(out, scored{i, total})
		}
	}
	slices.SortStableFunc(out, func(a, b scored) int { return cmp.Compare(a.score, b.score) })
	idx := make([]int, len(out))
	for i, s := range out {
		idx[i] = s.i
	}
	return idx
}

// otherField scores a term found only outside the name, after every match
// on the name.
const otherField = 100

// termScore scores how well one term matches an object; lower is better.
func termScore(o ops.ObjectState, term string) (int, bool) {
	for _, name := range []string{o.Object.Name, ObjectLabel(o)} {
		if m := resolve.All(term, []string{name}); len(m) == 1 {
			return int(m[0].Rank), true
		}
	}
	others := []string{strings.ToLower(o.Object.Kind), o.Object.Namespace, strings.ToLower(o.Status), strings.ToLower(o.Owner)}
	for _, f := range others {
		if f != "" && strings.Contains(f, term) {
			return otherField, true
		}
	}
	return 0, false
}

// pickModel is the picker's Bubble Tea model.
type pickModel struct {
	o      PickOptions
	st     styles
	filter string
	shown  []int
	cursor int
	offset int
	width  int
	height int
	// menu is true while the user chooses an action for the selected
	// object; choice is the action under the cursor then.
	menu   bool
	choice int
	picked *Picked
}

func newPickModel(o PickOptions, st styles) *pickModel {
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &pickModel{o: o, st: st, filter: o.Query, width: 80, height: 24}
	m.apply()
	return m
}

func (m *pickModel) apply() {
	m.shown = FilterObjects(m.o.Objects, m.filter)
	m.cursor, m.offset = 0, 0
}

func (m *pickModel) move(by int) {
	m.cursor = max(min(m.cursor+by, len(m.shown)-1), 0)
	m.offset = scrolled(m.cursor, m.offset, m.rows(), len(m.shown))
}

// rows is how many objects fit: the screen less the title, filter, header
// and help lines.
func (m *pickModel) rows() int { return max(m.height-4, 1) }

func (m *pickModel) Init() tea.Cmd { return nil }

func (m *pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = cmp.Or(msg.Width, 80), cmp.Or(msg.Height, 24)
		m.offset = scrolled(m.cursor, m.offset, m.rows(), len(m.shown))
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.PasteMsg:
		if !m.menu {
			m.filter += strings.Join(strings.Fields(msg.Content), " ")
			m.apply()
		}
	}
	return m, nil
}

func (m *pickModel) key(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	if m.menu {
		return m.menuKey(msg)
	}
	switch {
	case k == "esc":
		if m.filter == "" {
			return tea.Quit
		}
		m.filter = ""
		m.apply()
	case k == "enter":
		if len(m.shown) == 0 || len(m.o.Actions) == 0 {
			return nil
		}
		if len(m.o.Actions) == 1 {
			return m.choose(0)
		}
		m.menu, m.choice = true, 0
	case k == "up" || k == "ctrl+p":
		m.move(-1)
	case k == "down" || k == "ctrl+n":
		m.move(1)
	case k == "pgup":
		m.move(-m.rows())
	case k == "pgdown":
		m.move(m.rows())
	case edit(&m.filter, msg):
		m.apply()
	}
	return nil
}

func (m *pickModel) menuKey(msg tea.KeyPressMsg) tea.Cmd {
	switch k := msg.String(); k {
	case "esc", "backspace":
		m.menu = false
	case "enter":
		return m.choose(m.choice)
	case "left", "up", "shift+tab", "ctrl+p":
		m.choice = (m.choice + len(m.o.Actions) - 1) % len(m.o.Actions)
	case "right", "down", "tab", "ctrl+n":
		m.choice = (m.choice + 1) % len(m.o.Actions)
	default:
		if i := slices.IndexFunc(m.o.Actions, func(a PickAction) bool { return a.Key == k }); i >= 0 {
			return m.choose(i)
		}
	}
	return nil
}

func (m *pickModel) choose(action int) tea.Cmd {
	m.picked = &Picked{Object: m.o.Objects[m.shown[m.cursor]], Action: m.o.Actions[action].Name}
	return tea.Quit
}

func (m *pickModel) View() tea.View {
	v := tea.NewView(m.screen())
	v.AltScreen = true
	return v
}

// pickHeader are the picker's columns.
var pickHeader = []string{"KIND/NAME", "NAMESPACE", "STATUS", "AGE", "OWNER"}

// maxPickStatus is the most of a status the picker shows.
const maxPickStatus = 40

// screen draws where the objects are, the filter, the objects and the keys.
func (m *pickModel) screen() string {
	var b strings.Builder
	where := "every namespace"
	if m.o.Namespace != "" {
		where = "namespace " + m.o.Namespace
	}
	b.WriteString(truncate(m.st.title.Render(m.o.Cluster)+m.st.dim.Render(" · ")+m.st.bold.Render(where)+
		m.st.dim.Render(fmt.Sprintf("  %d of %d", len(m.shown), len(m.o.Objects))), m.width) + "\n")
	b.WriteString(m.st.dim.Render("Filter: ") + m.filter + "▏\n")

	rows := make([][]string, 0, len(m.shown))
	end := min(m.offset+m.rows(), len(m.shown))
	now := m.o.Now()
	for _, i := range m.shown[m.offset:end] {
		o := m.o.Objects[i]
		age := "-"
		if !o.Created.IsZero() {
			age = duration.HumanDuration(now.Sub(o.Created))
		}
		// A long status, such as a condition's message, would push the age
		// and owner off the screen.
		status := truncate(cmp.Or(o.Status, "-"), maxPickStatus)
		rows = append(rows, []string{ObjectLabel(o), cmp.Or(o.Object.Namespace, "-"), status, age, cmp.Or(o.Owner, "-")})
	}
	widths := make([]int, len(pickHeader))
	for c, h := range pickHeader {
		widths[c] = len(h)
		for _, r := range rows {
			widths[c] = max(widths[c], lipgloss.Width(r[c]))
		}
	}
	line := func(cells []string) string {
		parts := make([]string, len(cells))
		for c, cell := range cells {
			parts[c] = pad(cell, widths[c])
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	b.WriteString(m.st.header.Render(truncate("  "+line(pickHeader), m.width)) + "\n")
	written := len(rows)
	if len(m.shown) == 0 {
		b.WriteString(m.st.dim.Render("  Nothing matches; esc clears the filter.") + "\n")
		written = 1
	}
	for j, r := range rows {
		text := truncate(line(r), m.width-2)
		if m.offset+j == m.cursor {
			b.WriteString(m.st.selected.Render("›") + " " + m.st.selected.Render(text) + "\n")
			continue
		}
		b.WriteString("  " + m.colorByHealth(m.o.Objects[m.shown[m.offset+j]], text) + "\n")
	}
	for range m.rows() - written {
		b.WriteString("\n")
	}
	b.WriteString(m.help())
	return b.String()
}

// colorByHealth colours a row by the object's health.
func (m *pickModel) colorByHealth(o ops.ObjectState, text string) string {
	switch o.Health {
	case ops.HealthNotReady:
		return m.st.red.Render(text)
	case ops.HealthUnknown:
		return m.st.yellow.Render(text)
	default:
		return text
	}
}

// help is the bottom line: the actions for the selected object while
// choosing one, else the keys of the list.
func (m *pickModel) help() string {
	if !m.menu {
		return m.st.dim.Render(truncate("type to filter · ↑↓ move · enter choose · esc quit", m.width))
	}
	parts := make([]string, len(m.o.Actions))
	for i, a := range m.o.Actions {
		label := a.Key + " " + a.Name
		if i == m.choice {
			label = m.st.selected.Render(label)
		}
		parts[i] = label
	}
	label := ObjectLabel(m.o.Objects[m.shown[m.cursor]])
	return truncate(m.st.bold.Render(label)+": "+strings.Join(parts, "  ")+m.st.dim.Render("  · esc back"), m.width)
}
