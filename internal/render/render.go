// Package render writes tables, trees, structured output and colour for the
// CLI.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"
)

// Format is an output format chosen with -o.
type Format string

// Output formats.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatName  Format = "name"
)

// ParseFormat validates a -o value. "" means table.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(s)); f {
	case "", FormatTable:
		return FormatTable, nil
	case FormatJSON, FormatYAML, FormatName:
		return f, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table, json, yaml or name)", s)
	}
}

// Structured writes v as JSON or YAML.
func Structured(w io.Writer, f Format, v any) error {
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case FormatYAML:
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	default:
		return fmt.Errorf("format %q is not structured", f)
	}
}

// Table writes rows under a header, aligned with spaces.
func Table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// TreeNode is one line of a tree drawn by Tree: a label, a detail shown in a
// column after the labels, and the nodes beneath it.
type TreeNode struct {
	Label    string
	Detail   string
	Children []TreeNode
}

// Tree draws a tree with box-drawing characters, its details lined up in a
// column after the widest label. Labels and details may carry colour.
func Tree(w io.Writer, root TreeNode) error {
	type line struct{ head, detail string }
	var lines []line
	var walk func(n TreeNode, prefix, indent string)
	walk = func(n TreeNode, prefix, indent string) {
		lines = append(lines, line{prefix + n.Label, n.Detail})
		for i, c := range n.Children {
			if i == len(n.Children)-1 {
				walk(c, indent+"└── ", indent+"    ")
			} else {
				walk(c, indent+"├── ", indent+"│   ")
			}
		}
	}
	walk(root, "", "")

	width := 0
	for _, l := range lines {
		width = max(width, visibleWidth(l.head))
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.head)
		if l.detail != "" {
			b.WriteString(strings.Repeat(" ", width-visibleWidth(l.head)+2))
			b.WriteString(l.detail)
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// visibleWidth counts the characters of s that a terminal shows, leaving out
// ANSI escape sequences.
func visibleWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++ // parameter and intermediate bytes
			}
			i++ // the final byte
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// Colors applies ANSI colours when enabled.
type Colors struct{ Enabled bool }

// ColorsFor enables colour when w is a terminal and NO_COLOR is unset.
func ColorsFor(w io.Writer) Colors {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Colors{}
	}
	f, ok := w.(*os.File)
	return Colors{Enabled: ok && term.IsTerminal(int(f.Fd()))}
}

// Width returns the width of the terminal w writes to, in columns, or 0
// when w is not a terminal, so that output to files and pipes is never
// wrapped.
func Width(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}

// Wrap breaks s into lines of at most width visible characters, at spaces.
// A word wider than width gets a line of its own rather than being split.
// With a width of 0 or less, s is returned as one line.
func Wrap(s string, width int) []string {
	if width <= 0 || visibleWidth(s) <= width {
		return []string{s}
	}
	var lines []string
	var line strings.Builder
	n := 0
	for _, word := range strings.Fields(s) {
		ww := visibleWidth(word)
		if n > 0 && n+1+ww > width {
			lines = append(lines, line.String())
			line.Reset()
			n = 0
		}
		if n > 0 {
			line.WriteByte(' ')
			n++
		}
		line.WriteString(word)
		n += ww
	}
	return append(lines, line.String())
}

func (c Colors) wrap(code, s string) string {
	if !c.Enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Red is used for read-only production clusters.
func (c Colors) Red(s string) string { return c.wrap("1;31", s) }

// Yellow is used for warnings and unlocked sessions.
func (c Colors) Yellow(s string) string { return c.wrap("33", s) }

// Green is used for success.
func (c Colors) Green(s string) string { return c.wrap("32", s) }

// Dim is used for secondary information.
func (c Colors) Dim(s string) string { return c.wrap("2", s) }

// Bold emphasises names.
func (c Colors) Bold(s string) string { return c.wrap("1", s) }

// Cyan marks commands to run, in the colour of the session prompt.
func (c Colors) Cyan(s string) string { return c.wrap("36", s) }

// palette holds distinguishable foreground colours for log prefixes.
var palette = []string{"36", "33", "32", "35", "34", "96", "93", "92", "95", "94"}

// Pick colours s with a palette colour chosen by key, so the same pod keeps
// the same colour for the whole stream.
func (c Colors) Pick(key, s string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return c.wrap(palette[h.Sum32()%uint32(len(palette))], s)
}

// PrettyJSON indents a line that is a JSON object; other lines are returned
// unchanged.
func PrettyJSON(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return line
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(trimmed), "", "  "); err != nil {
		return line
	}
	return buf.String()
}
