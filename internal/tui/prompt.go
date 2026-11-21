package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// prompt asks a question above the status bar: yes or no, or for a few
// values. While it is open every key goes to it.
type prompt struct {
	question string
	// fields are what it asks for; without any it asks yes or no, and no
	// is the default, as the CLI's questions have it.
	fields []field
	focus  int
	err    string
	// yes runs on y, or on enter in the last field, with the fields'
	// values. An error keeps the prompt open and shows the error in it.
	yes func(values []string) (tea.Cmd, error)
	// no runs on anything else, or on esc for a prompt with fields.
	no func() tea.Cmd
}

// field is one value a prompt asks for.
type field struct {
	label string
	input textinput.Model
}

// newField makes a field with a value to start from and a hint shown while
// it is empty. Its cursor does not blink, so nothing ticks while it waits.
func newField(label, value, hint string) field {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = hint
	in.CharLimit = 256
	in.SetWidth(60)
	s := in.Styles()
	s.Cursor.Blink = false
	in.SetStyles(s)
	in.SetValue(value)
	return field{label: label, input: in}
}

// ask asks a yes or no question.
func (m *model) ask(question string, yes, no func() tea.Cmd) {
	m.prompt = &prompt{
		question: question + " [y/N]",
		yes:      func([]string) (tea.Cmd, error) { return yes(), nil },
		no:       no,
	}
}

// askFor asks for values; esc cancels without a word.
func (m *model) askFor(question string, fields []field, yes func(values []string) (tea.Cmd, error)) {
	p := &prompt{question: question, fields: fields, yes: yes, no: func() tea.Cmd { return nil }}
	p.focusOn(0)
	m.prompt = p
}

func (p *prompt) focusOn(i int) {
	p.fields[p.focus].input.Blur()
	p.focus = (i + len(p.fields)) % len(p.fields)
	p.fields[p.focus].input.Focus()
}

func (p *prompt) values() []string {
	out := make([]string, len(p.fields))
	for i, f := range p.fields {
		out[i] = f.input.Value()
	}
	return out
}

// promptKey hands a key to the open prompt. The prompt closes before yes
// or no runs, so they may open the next one.
func (m *model) promptKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.prompt
	if len(p.fields) == 0 {
		m.prompt = nil
		if strings.EqualFold(msg.String(), "y") {
			cmd, _ := p.yes(nil)
			return cmd
		}
		return p.no()
	}
	switch msg.String() {
	case "esc":
		m.prompt = nil
		return p.no()
	case "tab", "down":
		p.focusOn(p.focus + 1)
	case "shift+tab", "up":
		p.focusOn(p.focus - 1)
	case "enter":
		if p.focus < len(p.fields)-1 {
			p.focusOn(p.focus + 1)
			return nil
		}
		m.prompt = nil
		cmd, err := p.yes(p.values())
		if err != nil {
			p.err = err.Error()
			m.prompt = p
			return nil
		}
		return cmd
	default:
		var cmd tea.Cmd
		p.fields[p.focus].input, cmd = p.fields[p.focus].input.Update(msg)
		return cmd
	}
	return nil
}

// promptView draws the open prompt: its question, its fields and what the
// keys do.
func (m *model) promptView() string {
	p := m.prompt
	lines := []string{m.st.bold.Render("? " + truncate(p.question, m.width-2))}
	width := 0
	for _, f := range p.fields {
		width = max(width, len(f.label))
	}
	for _, f := range p.fields {
		lines = append(lines, "  "+m.st.dim.Render(pad(f.label, width))+"  "+f.input.View())
	}
	if p.err != "" {
		lines = append(lines, "  "+m.st.red.Render(truncate(p.err, m.width-2)))
	}
	if len(p.fields) > 0 {
		keys := "enter: go on  esc: cancel"
		if len(p.fields) > 1 {
			keys = "enter: next, then go on  tab: next field  esc: cancel"
		}
		lines = append(lines, "  "+m.st.dim.Render(keys))
	}
	return strings.Join(lines, "\n")
}
