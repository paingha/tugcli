// Package ini reads the simple INI files cloud CLIs keep their settings in,
// such as ~/.aws/config and gcloud's configurations.
package ini

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// File maps section names to their keys and values.
type File map[string]map[string]string

// Parse reads INI text. Keys are case-sensitive and comments start with # or
// ;. A line indented more deeply than the key before it continues that key's
// value, as the AWS CLI writes nested settings; lines at the same indentation
// are sibling keys.
func Parse(r io.Reader) (File, error) {
	f := File{}
	section := ""
	var (
		lastKey    string
		lastIndent int
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSpace(line[1 : len(line)-1])
			if f[section] == nil {
				f[section] = map[string]string{}
			}
			lastKey = ""
		case lastKey != "" && indent(raw) > lastIndent:
			f[section][lastKey] += "\n" + line
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if f[section] == nil {
				f[section] = map[string]string{}
			}
			lastKey, lastIndent = strings.TrimSpace(k), indent(raw)
			f[section][lastKey] = strings.TrimSpace(v)
		}
	}
	return f, sc.Err()
}

// indent counts a line's leading spaces and tabs.
func indent(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// ParseFile reads an INI file. A missing file is an empty File.
func ParseFile(path string) (File, error) {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return Parse(fh)
}

// Get returns a value, or "".
func (f File) Get(section, key string) string { return f[section][key] }
