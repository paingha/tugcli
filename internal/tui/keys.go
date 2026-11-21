package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every key binding, so the help screen and the handlers
// cannot disagree.
type keyMap struct {
	Quit      key.Binding
	ForceQuit key.Binding
	Help      key.Binding
	Back      key.Binding

	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding
	Filter   key.Binding
	Refresh  key.Binding

	Connect    key.Binding
	TypeToFind key.Binding

	Logs    key.Binding
	Why     key.Binding
	Tree    key.Binding
	Shell   key.Binding
	Forward key.Binding
	Restart key.Binding
	Scale   key.Binding

	Namespace key.Binding
	Clusters  key.Binding
}

func newKeyMap() keyMap {
	b := func(keys []string, help, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return keyMap{
		Quit:      b([]string{"q"}, "q", "quit"),
		ForceQuit: b([]string{"ctrl+c"}, "ctrl+c", "quit from anywhere"),
		Help:      b([]string{"?"}, "?", "show this help; any key closes it"),
		Back:      b([]string{"esc"}, "esc", "back, or clear the filter"),

		Up:       b([]string{"up", "k", "ctrl+p"}, "↑/k", "up"),
		Down:     b([]string{"down", "j", "ctrl+n"}, "↓/j", "down"),
		PageUp:   b([]string{"pgup", "b", "ctrl+b"}, "pgup/b", "page up"),
		PageDown: b([]string{"pgdown", "f", "ctrl+f", " "}, "pgdn/f", "page down"),
		Top:      b([]string{"home", "g"}, "g/home", "top"),
		Bottom:   b([]string{"end", "G"}, "G/end", "bottom; in logs, follow new lines again"),
		Filter:   b([]string{"/"}, "/", "filter by name"),
		Refresh:  b([]string{"ctrl+r"}, "ctrl+r", "reload now"),

		Connect:    b([]string{"enter"}, "enter", "connect this terminal to the cluster"),
		TypeToFind: b([]string{"/"}, "type", "filter: names, label values, label=value; / to type q or ?"),

		Logs:    b([]string{"l"}, "l", "stream logs from every pod, following new ones"),
		Why:     b([]string{"w"}, "w", "why it is not ready, and what to do next"),
		Tree:    b([]string{"t"}, "t", "what it is made of, and the health of each part"),
		Shell:   b([]string{"s"}, "s", "shell in a running pod; the TUI comes back after"),
		Forward: b([]string{"p"}, "p", "port-forward through tugd, as tug pf --name"),
		Restart: b([]string{"r"}, "r", "rolling restart, after asking"),
		Scale:   b([]string{"S"}, "S", "scale to a number of replicas"),

		Namespace: b([]string{"n"}, "n", "switch the session's namespace"),
		Clusters:  b([]string{"c"}, "c", "clusters"),
	}
}

// hintNames are the one-word names the title line gives keys.
var hintNames = map[string]string{
	"q": "quit", "?": "help", "esc": "back", "enter": "choose", "/": "filter",
	"n": "namespace", "c": "clusters", "l": "logs", "w": "why", "t": "tree",
	"s": "shell", "p": "forward", "r": "restart", "S": "scale",
}

// actions are the bindings that act on a workload, from its row in the list
// or from one of its panes.
func (k keyMap) actions() []key.Binding {
	return []key.Binding{k.Logs, k.Why, k.Tree, k.Shell, k.Forward, k.Restart, k.Scale}
}

// hints turns bindings into the title line's short hints.
func hints(bs ...key.Binding) []key.Binding {
	out := make([]key.Binding, len(bs))
	for i, b := range bs {
		h := b.Help()
		name, ok := hintNames[h.Key]
		if !ok {
			name = h.Desc
		}
		out[i] = key.NewBinding(key.WithKeys(b.Keys()...), key.WithHelp(h.Key, name))
	}
	return out
}
