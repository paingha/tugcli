package tui

import (
	"hash/fnv"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// styles are the TUI's colours. They come from a renderer for the output,
// which leaves colour out when it is not a terminal, NO_COLOR is set or
// TERM=dumb, as render.Colors does for the CLI. Only the basic ANSI colours
// are used, so the terminal's theme decides the shades and nothing has to
// ask the terminal for its background.
type styles struct {
	title    lipgloss.Style
	header   lipgloss.Style
	selected lipgloss.Style
	dim      lipgloss.Style
	bold     lipgloss.Style
	red      lipgloss.Style
	yellow   lipgloss.Style
	green    lipgloss.Style
	status   lipgloss.Style
	key      lipgloss.Style
	// pick are distinguishable colours for log prefixes.
	pick []lipgloss.Style
}

func newStyles(out io.Writer) styles {
	r := lipgloss.NewRenderer(out)
	var pick []lipgloss.Style
	for _, c := range []string{"6", "3", "2", "5", "4", "14", "11", "10", "13", "12"} {
		pick = append(pick, r.NewStyle().Foreground(lipgloss.Color(c)))
	}
	return styles{
		pick:     pick,
		title:    r.NewStyle().Bold(true),
		header:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("8")),
		selected: r.NewStyle().Reverse(true),
		dim:      r.NewStyle().Foreground(lipgloss.Color("8")),
		bold:     r.NewStyle().Bold(true),
		red:      r.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
		yellow:   r.NewStyle().Foreground(lipgloss.Color("3")),
		green:    r.NewStyle().Foreground(lipgloss.Color("2")),
		status:   r.NewStyle().Reverse(true),
		key:      r.NewStyle().Bold(true),
	}
}

// pickFor colours text with a palette colour chosen by key, so the same pod
// keeps the same colour for the whole log stream, as tug logs does.
func (s styles) pickFor(key, text string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.pick[h.Sum32()%uint32(len(s.pick))].Render(text)
}

// truncate shortens s to width cells, ending it with an ellipsis.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// pad fills s with spaces to width cells.
func pad(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// column is one column of a table: its heading, its cells and how wide it
// may grow. The last column takes what is left of the line.
type column struct {
	heading string
	max     int
}

// table lays out rows under headings, each cell cut to its column. style,
// when set, colours a cell after it has been laid out, so colour never
// throws the columns out.
type table struct {
	columns []column
	rows    [][]string
	style   func(row, col int, cell string) string
}

// widths returns each column's width for a line of width cells.
func (t table) widths(width int) []int {
	ws := make([]int, len(t.columns))
	for i, c := range t.columns {
		ws[i] = lipgloss.Width(c.heading)
		for _, r := range t.rows {
			if i < len(r) {
				ws[i] = max(ws[i], lipgloss.Width(r[i]))
			}
		}
		if c.max > 0 {
			ws[i] = min(ws[i], c.max)
		}
	}
	used := 0
	for i := range len(ws) - 1 {
		used += ws[i] + 2
	}
	if last := len(ws) - 1; last >= 0 {
		ws[last] = max(min(ws[last], width-used), 0)
	}
	return ws
}

// line lays out one row of cells.
func (t table) line(ws []int, row int, cells []string) string {
	var b strings.Builder
	for i, w := range ws {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		cell = pad(truncate(cell, w), w)
		if t.style != nil && row >= 0 {
			cell = t.style(row, i, cell)
		}
		b.WriteString(cell)
		if i < len(ws)-1 {
			b.WriteString("  ")
		}
	}
	return b.String()
}

// row draws row i, marked and in reverse video when it is selected. A
// selected row is drawn without its cells' colours, which would end the
// reverse video part way along.
func (t table) row(s styles, ws []int, i int, selected bool) string {
	if selected {
		return s.selected.Render("›") + " " + s.selected.Render(t.line(ws, -1, t.rows[i]))
	}
	return "  " + t.line(ws, i, t.rows[i])
}

// headingLine lays out the headings.
func (t table) headingLine(ws []int) string {
	cells := make([]string, len(t.columns))
	for i, c := range t.columns {
		cells[i] = c.heading
	}
	return t.line(ws, -1, cells)
}
