package diff

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// LineOp marks a line of a unified diff.
type LineOp byte

// The marks of a unified diff's lines.
const (
	LineSame    LineOp = ' '
	LineRemoved LineOp = '-'
	LineAdded   LineOp = '+'
)

// Line is one line of a unified diff.
type Line struct {
	Op   LineOp
	Text string
}

// Hunk is one group of changed lines with the lines around them, as a
// unified diff writes it after its @@ header.
type Hunk struct {
	FromLine, FromCount int
	ToLine, ToCount     int
	Lines               []Line
}

// Header writes the hunk's @@ line.
func (h Hunk) Header() string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.FromLine, h.FromCount, h.ToLine, h.ToCount)
}

// maxCells caps the table that finds the longest common run of lines. Two
// objects that differ in more than about 2,000 lines each, which no real
// object does once their common start and end are taken off, are shown as
// one removed and one added block instead.
const maxCells = 4 << 20

// Unified compares two texts line by line and returns the hunks of a
// unified diff with context lines around each change.
func Unified(from, to []string, context int) []Hunk {
	script := edits(from, to)
	type span struct{ start, end int }
	var spans []span
	for i, l := range script {
		if l.Op == LineSame {
			continue
		}
		s, e := max(0, i-context), min(len(script), i+1+context)
		if n := len(spans); n > 0 && s <= spans[n-1].end {
			spans[n-1].end = e
			continue
		}
		spans = append(spans, span{s, e})
	}
	hunks := make([]Hunk, 0, len(spans))
	fromLine, toLine, pos := 1, 1, 0
	for _, sp := range spans {
		for ; pos < sp.start; pos++ {
			fromLine, toLine = advance(script[pos].Op, fromLine, toLine)
		}
		h := Hunk{FromLine: fromLine, ToLine: toLine, Lines: script[sp.start:sp.end]}
		for _, l := range h.Lines {
			if l.Op != LineAdded {
				h.FromCount++
			}
			if l.Op != LineRemoved {
				h.ToCount++
			}
		}
		// An empty side starts at the line before, as diff -u writes it.
		if h.FromCount == 0 {
			h.FromLine--
		}
		if h.ToCount == 0 {
			h.ToLine--
		}
		hunks = append(hunks, h)
	}
	return hunks
}

func advance(op LineOp, fromLine, toLine int) (int, int) {
	if op != LineAdded {
		fromLine++
	}
	if op != LineRemoved {
		toLine++
	}
	return fromLine, toLine
}

// edits returns the lines of both texts in order, marked as in both, only
// in from or only in to, using a longest common subsequence of the lines
// between their common start and end.
func edits(from, to []string) []Line {
	pre := 0
	for pre < len(from) && pre < len(to) && from[pre] == to[pre] {
		pre++
	}
	suf := 0
	for suf < len(from)-pre && suf < len(to)-pre && from[len(from)-1-suf] == to[len(to)-1-suf] {
		suf++
	}
	out := make([]Line, 0, len(from)+len(to))
	for _, s := range from[:pre] {
		out = append(out, Line{LineSame, s})
	}
	a, b := from[pre:len(from)-suf], to[pre:len(to)-suf]
	n, m := len(a), len(b)
	if (n+1)*(m+1) > maxCells {
		for _, s := range a {
			out = append(out, Line{LineRemoved, s})
		}
		for _, s := range b {
			out = append(out, Line{LineAdded, s})
		}
	} else {
		// lcs[i*(m+1)+j] is the length of the longest common subsequence
		// of a[i:] and b[j:].
		lcs := make([]int32, (n+1)*(m+1))
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if a[i] == b[j] {
					lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
				} else {
					lcs[i*(m+1)+j] = max(lcs[(i+1)*(m+1)+j], lcs[i*(m+1)+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n && j < m {
			switch {
			case a[i] == b[j]:
				out = append(out, Line{LineSame, a[i]})
				i++
				j++
			case lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]:
				out = append(out, Line{LineRemoved, a[i]})
				i++
			default:
				out = append(out, Line{LineAdded, b[j]})
				j++
			}
		}
		for ; i < n; i++ {
			out = append(out, Line{LineRemoved, a[i]})
		}
		for ; j < m; j++ {
			out = append(out, Line{LineAdded, b[j]})
		}
	}
	for _, s := range from[len(from)-suf:] {
		out = append(out, Line{LineSame, s})
	}
	return out
}

// yamlLines writes a normalised object as YAML, with sorted keys, one
// string per line. A nil object has no lines.
func yamlLines(obj map[string]any) ([]string, error) {
	if obj == nil {
		return nil, nil
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), nil
}

// FormatUnified writes hunks as the text of a unified diff between two
// labelled sides, without colour.
func FormatUnified(fromLabel, toLabel string, hunks []Hunk) string {
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", fromLabel, toLabel)
	for _, h := range hunks {
		b.WriteString(h.Header())
		b.WriteByte('\n')
		for _, l := range h.Lines {
			b.WriteByte(byte(l.Op))
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
