package bundle

import (
	"bytes"
	"cmp"
	_ "embed"
	"fmt"
	"text/template"
	"time"

	"github.com/paingha/tugcli/internal/version"
)

//go:embed readme.md.tmpl
var readmeTemplate string

var readmeTmpl = template.Must(template.New("README.md").Parse(readmeTemplate))

// readme writes the bundle's README: what it holds, what was redacted and
// how to review it before sharing it.
func (col *collector) readme() ([]byte, error) {
	var b bytes.Buffer
	err := readmeTmpl.Execute(&b, map[string]any{
		"Name":        col.opts.Name,
		"Archive":     cmp.Or(col.opts.Archive, col.opts.Name+Suffix),
		"Target":      col.target.String(),
		"Namespace":   col.target.Namespace,
		"IsNamespace": col.target.Resource.GroupResource() == namespaces,
		"Cluster":     col.opts.Cluster.Name,
		"Version":     version.String(),
		"Created":     col.now.UTC().Format(time.RFC3339),
		"Since":       formatDuration(col.opts.Since),
		"LogsTail":    col.opts.LogsTail,
		"Patterns":    col.red.Patterns(),
		"Problems":    len(col.problems),
		"MaxLogPods":  maxLogPods,
		"Include":     col.opts.Include,
		"Exclude":     col.opts.Exclude,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the README: %w", err)
	}
	return b.Bytes(), nil
}

// formatDuration writes a window as people do: 1h, 30m, 1h30m.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0 && d > time.Hour:
		return fmt.Sprintf("%dh%dm", d/time.Hour, (d%time.Hour)/time.Minute)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
