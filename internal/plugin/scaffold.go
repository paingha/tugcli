package plugin

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"
)

// Templates use [[ ]] delimiters so that GitHub Actions' ${{ }} expressions
// pass through untouched.
//
//go:embed templates
var templateFS embed.FS

// Lang is a scaffold language.
type Lang string

// Supported scaffold languages.
const (
	LangGo     Lang = "go"
	LangPython Lang = "python"
)

// ParseLang validates a --lang value.
func ParseLang(s string) (Lang, error) {
	switch Lang(strings.ToLower(s)) {
	case LangGo, "golang":
		return LangGo, nil
	case LangPython, "py":
		return LangPython, nil
	default:
		return "", fmt.Errorf("unsupported language %q (want go or python)", s)
	}
}

// ScaffoldOptions describe a new plugin.
type ScaffoldOptions struct {
	Name string
	Lang Lang
	// Dir to create; defaults to ./tug-provider-<name>.
	Dir string
	// Module is the Go module path; defaults to example.com/tug-provider-<name>.
	Module string
	// SDKReplace points the Go module at a local tugcli checkout, for working
	// on the SDK itself.
	SDKReplace string
}

type scaffoldFile struct {
	tmpl string
	out  string
	mode os.FileMode
}

func scaffoldFiles(lang Lang, name string) []scaffoldFile {
	bin := BinaryPrefix + name
	switch lang {
	case LangPython:
		return []scaffoldFile{
			{"python/plugin.py.tmpl", bin, 0o755},
			{"python/plugin.yaml.tmpl", ManifestFile, 0o644},
			{"python/test_plugin.py.tmpl", "test_plugin.py", 0o644},
			{"python/README.md.tmpl", "README.md", 0o644},
		}
	default:
		return []scaffoldFile{
			{"go/go.mod.tmpl", "go.mod", 0o644},
			{"go/main.go.tmpl", "main.go", 0o644},
			{"go/main_test.go.tmpl", "main_test.go", 0o644},
			{"go/plugin.yaml.tmpl", ManifestFile, 0o644},
			{"go/Makefile.tmpl", "Makefile", 0o644},
			{"go/gitignore.tmpl", ".gitignore", 0o644},
			{"go/github-release.yml.tmpl", filepath.Join(".github", "workflows", "release.yml"), 0o644},
			{"go/README.md.tmpl", "README.md", 0o644},
		}
	}
}

// Scaffold writes a new plugin project and returns the files it created.
func Scaffold(o ScaffoldOptions) ([]string, error) {
	if !validName.MatchString(o.Name) {
		return nil, fmt.Errorf("plugin name %q must be lower-case letters, digits and dashes", o.Name)
	}
	if o.Lang == "" {
		o.Lang = LangGo
	}
	if o.Dir == "" {
		o.Dir = BinaryPrefix + o.Name
	}
	if o.Module == "" {
		o.Module = "example.com/" + BinaryPrefix + o.Name
	}
	if o.SDKReplace != "" {
		abs, err := filepath.Abs(o.SDKReplace)
		if err != nil {
			return nil, err
		}
		o.SDKReplace = filepath.ToSlash(abs)
	}
	if entries, err := os.ReadDir(o.Dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s already exists and is not empty", o.Dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	data := struct {
		Name, Ident, Module, SDKReplace string
	}{o.Name, goIdent(o.Name) + "Provider", o.Module, o.SDKReplace}

	var written []string
	for _, f := range scaffoldFiles(o.Lang, o.Name) {
		src, err := templateFS.ReadFile("templates/" + f.tmpl)
		if err != nil {
			return written, err
		}
		tmpl, err := template.New(f.tmpl).Delims("[[", "]]").Option("missingkey=error").Parse(string(src))
		if err != nil {
			return written, fmt.Errorf("template %s: %w", f.tmpl, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return written, fmt.Errorf("template %s: %w", f.tmpl, err)
		}
		dest := filepath.Join(o.Dir, f.out)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(dest, buf.Bytes(), f.mode); err != nil {
			return written, err
		}
		written = append(written, dest)
	}
	return written, nil
}

// goIdent turns a plugin name such as "my-cloud" into "myCloud".
func goIdent(name string) string {
	var b strings.Builder
	upper := false
	for _, r := range name {
		switch {
		case r == '-':
			upper = true
		case upper:
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || unicode.IsDigit(rune(s[0])) {
		s = "p" + s
	}
	return s
}
