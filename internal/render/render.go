// Package render writes tables, structured output and colour for the CLI.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

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
