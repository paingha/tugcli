// Package alias keeps the user's command aliases, in aliases.yaml in tug's
// config directory, and expands them in a command line.
//
// An alias stands for the words it expands to, followed by whatever came
// after it on the command line, as git's aliases do. Only the first word of
// a command line is expanded, and only once: an expansion is never expanded
// again, so aliases cannot loop. A name that tug itself has as a command is
// never expanded, so an alias can never shadow one of tug's commands.
package alias

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/passthrough"
)

// ErrNotFound is returned for an alias name that is not set.
var ErrNotFound = errors.New("no such alias")

// file is aliases.yaml.
type file struct {
	Aliases map[string][]string `json:"aliases"`
}

// Store reads and writes aliases.yaml.
type Store struct{ path string }

// New returns the store in tug's config directory.
func New(configDir string) *Store { return &Store{path: filepath.Join(configDir, "aliases.yaml")} }

// Path is the file the store keeps aliases in.
func (s *Store) Path() string { return s.path }

// Load returns every alias by name. A missing file has none.
func (s *Store) Load() (map[string][]string, error) {
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return map[string][]string{}, nil
	case err != nil:
		return nil, fmt.Errorf("reading aliases: %w", err)
	}
	var f file
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if f.Aliases == nil {
		f.Aliases = map[string][]string{}
	}
	return f.Aliases, nil
}

// Set saves an alias, replacing any of the same name. The caller has
// checked that the name is not one of tug's commands.
func (s *Store) Set(name string, words []string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if len(words) == 0 || words[0] == "" {
		return fmt.Errorf("alias %s needs a command to stand for", name)
	}
	all, err := s.Load()
	if err != nil {
		return err
	}
	all[name] = slices.Clone(words)
	return s.save(all)
}

// Remove deletes the named aliases and returns the names it removed. Names
// that are not set make it return ErrNotFound, after removing the rest.
func (s *Store) Remove(names ...string) ([]string, error) {
	all, err := s.Load()
	if err != nil {
		return nil, err
	}
	var removed, missing []string
	for _, n := range names {
		if _, ok := all[n]; !ok {
			missing = append(missing, n)
			continue
		}
		delete(all, n)
		removed = append(removed, n)
	}
	if len(removed) > 0 {
		if err := s.save(all); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		return removed, fmt.Errorf("%w %s", ErrNotFound, strings.Join(missing, ", "))
	}
	return removed, nil
}

func (s *Store) save(all map[string][]string) error {
	b, err := yaml.Marshal(file{Aliases: all})
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(s.path, b, 0o600); err != nil {
		return fmt.Errorf("saving aliases: %w", err)
	}
	return nil
}

// Names returns the aliases' names in order.
func Names(all map[string][]string) []string { return slices.Sorted(maps.Keys(all)) }

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName returns an error unless name can name an alias: one word of
// letters, digits, dots, dashes and underscores that does not start like a
// flag.
func ValidName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("alias name %q: use letters, digits, '.', '-' and '_', starting with a letter or digit", name)
	}
	return nil
}

// Expansion is a command line with an alias expanded.
type Expansion struct {
	// Name is the alias.
	Name string
	// Words are what the alias stands for.
	Words []string
	// Args is the whole command line after expansion.
	Args []string
}

// Expand expands the alias that starts a command line, if one does. The
// first word is found as kubectl would find it, after any flags and their
// values. builtin reports whether a word is one of tug's own commands; those
// are never expanded, whatever aliases.yaml says.
func Expand(args []string, aliases map[string][]string, builtin func(string) bool) (Expansion, bool) {
	at := firstWord(args)
	if at < 0 || builtin(args[at]) {
		return Expansion{}, false
	}
	words, ok := aliases[args[at]]
	if !ok || len(words) == 0 {
		return Expansion{}, false
	}
	out := make([]string, 0, len(args)+len(words))
	out = append(out, args[:at]...)
	out = append(out, words...)
	out = append(out, args[at+1:]...)
	return Expansion{Name: args[at], Words: slices.Clone(words), Args: out}, true
}

// Candidate returns the word that starts a command line when it could be an
// alias: when it is not one of tug's own commands. Callers use it to read
// aliases.yaml only when there may be an alias to expand.
func Candidate(args []string, builtin func(string) bool) (string, bool) {
	at := firstWord(args)
	if at < 0 || builtin(args[at]) {
		return "", false
	}
	return args[at], true
}

// firstWord returns the index of the first word of args that is neither a
// flag nor a flag's value, as passthrough.Words judges them, or -1.
func firstWord(args []string) int {
	for i := range args {
		if len(passthrough.Words(args[:i+1])) == 1 {
			return i
		}
	}
	return -1
}

// Split splits an expansion typed as one string into words, as a POSIX
// shell would without expanding anything: single quotes keep everything,
// double quotes keep everything but backslash escapes of `\`, `"`, `$` and
// "`", and a backslash outside quotes keeps the next character.
func Split(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case ' ', '\t', '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\\':
			inWord = true
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			}
		case '\'':
			inWord = true
			end := slices.Index(runes[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated ' in the expansion")
			}
			cur.WriteString(string(runes[i+1 : i+1+end]))
			i += end + 1
		case '"':
			inWord = true
			closed := false
			for i++; i < len(runes); i++ {
				c := runes[i]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' && i+1 < len(runes) && strings.ContainsRune("\\\"$`", runes[i+1]) {
					i++
					c = runes[i]
				}
				cur.WriteRune(c)
			}
			if !closed {
				return nil, errors.New(`unterminated " in the expansion`)
			}
		default:
			inWord = true
			cur.WriteRune(r)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// Join writes words as a shell would need them typed, quoting any that
// hold spaces or characters a shell treats specially.
func Join(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = quote(w)
	}
	return strings.Join(out, " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func quote(w string) string {
	if plainWord.MatchString(w) {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
