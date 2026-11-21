// Package profile reads and writes working-environment profiles: YAML files
// in tug's config directory that bundle a cluster, a namespace, a safety
// mode, command aliases and named port-forwards. The user writes them and
// names one to use it, so unlike a directory pin (internal/pin) a profile
// needs no separate trust step, and nothing chooses one on its own.
package profile

import (
	"bytes"
	_ "embed" // for the new-profile template
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// Safety modes a profile may ask for. There is deliberately no read-write
// mode: a profile can make a session stricter than the cluster's labels,
// never looser.
const (
	// SafetyAuto lets the cluster's labels decide, as tug connect does.
	SafetyAuto = "auto"
	// SafetyReadOnly opens the session read-only whatever the labels say.
	SafetyReadOnly = "read-only"
)

// ErrNotFound is returned for a profile that has no file.
var ErrNotFound = errors.New("no such profile")

// ErrReadWrite is returned for a profile that asks to loosen the guard.
var ErrReadWrite = errors.New("a profile can make a session read-only but never read-write")

// validName is a profile, forward or alias name. tugd names a profile's
// forwards <profile>.<forward>, so neither part may contain a dot, and the
// two together stay within tugd's 63 characters.
var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,29}[a-z0-9])?$`)

// ValidName reports whether name can name a profile, a forward or an alias.
func ValidName(name string) bool { return validName.MatchString(name) }

// Profile is one profile file.
type Profile struct {
	// Name is the file's name without .yaml; the file does not repeat it.
	Name string `json:"-"`
	// Cluster is a name, prefix or fuzzy match, as tug connect takes it.
	Cluster string `json:"cluster,omitempty"`
	// Labels narrow the clusters first, as label=value does for tug connect.
	Labels    map[string]string `json:"labels,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	// Safety is auto (the default) or read-only.
	Safety string `json:"safety,omitempty"`
	// Proxy sends the session through tugd, so the guard applies to every
	// client and not only to tug's own commands.
	Proxy bool `json:"proxy,omitempty"`
	// Aliases are short names for tug command lines, run with
	// tug profile run <alias> [args...].
	Aliases map[string]string `json:"aliases,omitempty"`
	// Forwards are the named port-forwards tug profile up starts.
	Forwards []Forward `json:"forwards,omitempty"`
}

// Forward is one port-forward of a profile.
type Forward struct {
	Name string `json:"name"`
	// Target is a workload or pod, as tug pf takes it: api, sts/db.
	Target string `json:"target"`
	// Namespace defaults to the profile's.
	Namespace string `json:"namespace,omitempty"`
	// Ports are as tug pf takes them; none forwards every declared port.
	Ports Ports `json:"ports,omitempty"`
	// Addresses to listen on. Default: localhost.
	Addresses []string `json:"addresses,omitempty"`
}

// Ports are port-forward specs: 8080, 9000:http, ":80". YAML may write a
// bare port as a number.
type Ports []string

// UnmarshalJSON accepts each port as a string or a number.
func (p *Ports) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New(`ports must be a list, such as [5432, "9000:http"]`)
	}
	out := make(Ports, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out = append(out, s)
			continue
		}
		var n uint16
		if err := json.Unmarshal(r, &n); err != nil {
			return fmt.Errorf(`port %s must be a port number or a string such as "9000:http"`, r)
		}
		out = append(out, strconv.Itoa(int(n)))
	}
	*p = out
	return nil
}

// ForceReadOnly reports whether the profile opens sessions read-only
// whatever the cluster's labels say.
func (p *Profile) ForceReadOnly() bool { return p.Safety == SafetyReadOnly }

// Args returns the arguments tug connect would take for the profile's
// cluster.
func (p *Profile) Args() []string {
	var args []string
	if p.Cluster != "" {
		args = append(args, p.Cluster)
	}
	keys := make([]string, 0, len(p.Labels))
	for k := range p.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, k+"="+p.Labels[k])
	}
	return args
}

// ForwardName is the name tugd knows forward f of the profile by, so that
// two profiles can each have a forward called db.
func (p *Profile) ForwardName(f Forward) string { return p.Name + "." + f.Name }

// AliasNames lists the profile's aliases in order.
func (p *Profile) AliasNames() []string {
	names := make([]string, 0, len(p.Aliases))
	for n := range p.Aliases {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Expand returns the tug command line alias name stands for, followed by
// extra.
func (p *Profile) Expand(name string, extra []string) ([]string, error) {
	line, ok := p.Aliases[name]
	if !ok {
		if len(p.Aliases) == 0 {
			return nil, fmt.Errorf("profile %s has no aliases; add some under aliases: with `tug profile edit %s`", p.Name, p.Name)
		}
		return nil, fmt.Errorf("profile %s has no alias %q (it has %s)", p.Name, name, strings.Join(p.AliasNames(), ", "))
	}
	words, err := Words(line)
	if err != nil {
		return nil, fmt.Errorf("alias %s: %w", name, err)
	}
	return append(words, extra...), nil
}

func (p *Profile) validate() error {
	var errs []error
	if p.Cluster == "" && len(p.Labels) == 0 {
		errs = append(errs, errors.New("names no cluster; set cluster, labels or both"))
	}
	switch p.Safety {
	case SafetyAuto, SafetyReadOnly:
	case "read-write", "readwrite", "rw", "write", "unlocked":
		errs = append(errs, fmt.Errorf("safety: %s: %w; on a cluster whose labels make it read-only, "+
			"use `tug unlock` for a while, or `tug connect --read-write`", p.Safety, ErrReadWrite))
	default:
		errs = append(errs, fmt.Errorf("safety: %q is not auto or read-only", p.Safety))
	}
	for _, name := range p.AliasNames() {
		if !ValidName(name) {
			errs = append(errs, fmt.Errorf("alias %q: names are lower-case letters, digits, dashes and underscores, up to 31", name))
			continue
		}
		words, err := Words(p.Aliases[name])
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("alias %s: %w", name, err))
		case len(words) == 0:
			errs = append(errs, fmt.Errorf("alias %s is empty", name))
		case len(words) > 1 && words[0] == "profile" && words[1] == "run":
			// One alias running another could loop forever.
			errs = append(errs, fmt.Errorf("alias %s runs another alias; write out the command instead", name))
		}
	}
	seen := map[string]bool{}
	for i, f := range p.Forwards {
		switch {
		case !ValidName(f.Name):
			errs = append(errs, fmt.Errorf("forwards[%d]: name %q: names are lower-case letters, digits, dashes and underscores, up to 31", i, f.Name))
		case seen[f.Name]:
			errs = append(errs, fmt.Errorf("forwards[%d]: a second forward named %s", i, f.Name))
		}
		seen[f.Name] = true
		if f.Target == "" {
			errs = append(errs, fmt.Errorf("forwards[%d] (%s) has no target", i, f.Name))
		}
		for _, port := range f.Ports {
			if port == "" || strings.Count(port, ":") > 1 {
				errs = append(errs, fmt.Errorf("forwards[%d] (%s): port %q is not like 8080, 9000:http or :80", i, f.Name, port))
			}
		}
	}
	return errors.Join(errs...)
}

// Parse reads profile name from the bytes of its file.
func Parse(name string, b []byte) (*Profile, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("profile name %q: names are lower-case letters, digits, dashes and underscores, up to 31", name)
	}
	p := &Profile{}
	if err := yaml.UnmarshalStrict(b, p); err != nil {
		return nil, err
	}
	p.Name = name
	switch p.Safety = strings.ToLower(strings.TrimSpace(p.Safety)); p.Safety {
	case "":
		p.Safety = SafetyAuto
	case "readonly", "ro":
		p.Safety = SafetyReadOnly
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Store keeps profiles in a directory, one <name>.yaml each.
type Store struct{ dir string }

// NewStore keeps profiles under tug's config directory.
func NewStore(configDir string) *Store { return &Store{dir: filepath.Join(configDir, "profiles")} }

// Dir is the directory that holds the profiles.
func (s *Store) Dir() string { return s.dir }

// Path is the file of profile name.
func (s *Store) Path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("profile name %q: names are lower-case letters, digits, dashes and underscores, up to 31", name)
	}
	return filepath.Join(s.dir, name+".yaml"), nil
}

// Load reads profile name.
func (s *Store) Load(name string) (*Profile, error) {
	path, err := s.Path(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w %s; `tug profile edit %s` writes one, `tug profile ls` lists them", ErrNotFound, name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading profile %s: %w", name, err)
	}
	p, err := Parse(name, b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// List reads every profile in order of name. A file that does not parse
// is left out and reported in the error, which is returned with the
// profiles that did.
func (s *Store) List() ([]*Profile, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing profiles: %w", err)
	}
	var out []*Profile
	var errs []error
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() || !ValidName(name) {
			continue
		}
		p, err := s.Load(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

// Save checks b as profile name and writes it as the profile's file, as
// given, so comments are kept.
func (s *Store) Save(name string, b []byte) (*Profile, error) {
	path, err := s.Path(name)
	if err != nil {
		return nil, err
	}
	p, err := Parse(name, b)
	if err != nil {
		return nil, err
	}
	if err := config.WriteFileAtomic(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("writing profile %s: %w", name, err)
	}
	return p, nil
}

//go:embed template.yaml
var templateText string

var tmpl = template.Must(template.New("profile").Parse(templateText))

// Template is the starting text of a new profile, for the cluster and
// namespace given, or placeholders when they are empty.
func Template(name, cluster, namespace string) []byte {
	if cluster == "" {
		cluster = "my-cluster"
	}
	if namespace == "" {
		namespace = "default"
	}
	var buf bytes.Buffer
	// The template is fixed and its data are strings, so it cannot fail.
	_ = tmpl.Execute(&buf, struct{ Name, Cluster, Namespace string }{name, quote(cluster), quote(namespace)})
	return buf.Bytes()
}

// quote writes s as a YAML string that reads back as s.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// Words splits an alias into words as a shell would, without expanding
// anything: whitespace separates words, single quotes keep everything
// literally, double quotes keep everything but \" and \\, and a backslash
// outside quotes keeps the next character.
func Words(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	const (
		bare = iota
		single
		double
	)
	state := bare
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case single:
			if c == '\'' {
				state = bare
			} else {
				cur.WriteByte(c)
			}
		case double:
			switch {
			case c == '"':
				state = bare
			case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		default:
			switch c {
			case ' ', '\t', '\n', '\r':
				if inWord {
					words = append(words, cur.String())
					cur.Reset()
					inWord = false
				}
				continue
			case '\'':
				state = single
			case '"':
				state = double
			case '\\':
				if i+1 == len(s) {
					return nil, errors.New("ends with a backslash")
				}
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		}
		inWord = true
	}
	if state != bare {
		return nil, errors.New("has a quote that is not closed")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
