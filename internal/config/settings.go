package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// Setting kinds: what a setting's value is.
const (
	KindBool     = "bool"
	KindInt      = "int"
	KindNumber   = "number"
	KindString   = "string"
	KindDuration = "duration"
)

// Setting is a key in config.yaml whose value is one boolean, number,
// string or duration, so that tug settings can read and change it. Lists and
// maps, such as plugins.indexes, are edited in the file or by the commands
// that own them.
type Setting struct {
	// Key is the setting's path as config.yaml spells it:
	// intelligence.enabled.
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Env names the variable that overrides the setting, if one does.
	Env string `json:"env,omitempty"`

	index []int
}

// envOverrides are the settings Load takes from the environment when set.
var envOverrides = map[string]string{
	"keyring.backend": "TUG_KEYRING_BACKEND",
	"kubectl":         "TUG_KUBECTL",
}

var durationType = reflect.TypeFor[Duration]()

// settings are Config's settings, found once from its fields.
var settings = func() []Setting {
	var out []Setting
	var walk func(t reflect.Type, prefix string, index []int)
	walk = func(t reflect.Type, prefix string, index []int) {
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" || !f.IsExported() {
				continue
			}
			key := prefix + name
			idx := append(slices.Clone(index), i)
			ft := f.Type
			if ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Bool {
				ft = ft.Elem()
			}
			kind := ""
			switch {
			case ft == durationType:
				kind = KindDuration
			case ft.Kind() == reflect.Struct:
				walk(ft, key+".", idx)
				continue
			case ft.Kind() == reflect.Bool:
				kind = KindBool
			case ft.Kind() == reflect.Int:
				kind = KindInt
			case ft.Kind() == reflect.Float64:
				kind = KindNumber
			case ft.Kind() == reflect.String:
				kind = KindString
			default:
				continue // a list or a map
			}
			out = append(out, Setting{Key: key, Kind: kind, Env: envOverrides[key], index: idx})
		}
	}
	walk(reflect.TypeFor[Config](), "", nil)
	return out
}()

// Settings returns the settings tug settings can change, in config.yaml's
// order.
func Settings() []Setting { return slices.Clone(settings) }

// LookupSetting finds a setting by its key, in any case.
func LookupSetting(key string) (Setting, bool) {
	for _, s := range settings {
		if strings.EqualFold(s.Key, key) {
			return s, true
		}
	}
	return Setting{}, false
}

// Value returns the setting's value in c, written as tug settings set takes
// it, or "" when it has none.
func (s Setting) Value(c *Config) string {
	v := reflect.ValueOf(c).Elem().FieldByIndex(s.index)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	switch s.Kind {
	case KindDuration:
		if d := v.Interface().(Duration); d.Duration != 0 {
			return d.String()
		}
		return ""
	case KindBool:
		return strconv.FormatBool(v.Bool())
	case KindInt:
		if v.Int() == 0 {
			return ""
		}
		return strconv.FormatInt(v.Int(), 10)
	case KindNumber:
		if v.Float() == 0 {
			return ""
		}
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	default:
		return v.String()
	}
}

// parse checks a value for the setting and returns it as YAML should hold
// it.
func (s Setting) parse(value string) (any, error) {
	value = strings.TrimSpace(value)
	bad := func(want string) error { return fmt.Errorf("%s takes %s, not %q", s.Key, want, value) }
	switch s.Kind {
	case KindBool:
		switch strings.ToLower(value) {
		case "true", "yes", "on":
			return true, nil
		case "false", "no", "off":
			return false, nil
		}
		return nil, bad("true or false")
	case KindInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, bad("a whole number")
		}
		return n, nil
	case KindNumber:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, bad("a number")
		}
		return f, nil
	case KindDuration:
		if _, err := time.ParseDuration(value); err != nil {
			return nil, bad(`a duration such as "30s" or "5m"`)
		}
		return value, nil
	default:
		if value == "" {
			return nil, fmt.Errorf("%s cannot be set to nothing; use tug settings unset %s", s.Key, s.Key)
		}
		return value, nil
	}
}

// Change is one change to a setting: a new value, or with Unset, its
// removal.
type Change struct {
	Setting Setting
	Value   string
	Unset   bool
}

// EditSettings makes the changes to config.yaml in dir in one write,
// changing only their entries, so the rest of the file, comments included,
// stays as the user wrote it. Before writing, it checks that tug can read
// the result and passes it to check, whose error stops the write. It
// returns the settings that were removed, leaving out those that were not
// set.
func EditSettings(dir string, changes []Change, check func(*Config) error) (removed []Setting, err error) {
	path := filepath.Join(dir, "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var doc yamlv3.Node
	if len(bytes.TrimSpace(b)) > 0 {
		if err := yamlv3.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// A file of comments alone parses to nothing; they are kept by writing
	// the settings after them.
	var out bytes.Buffer
	if len(doc.Content) == 0 {
		doc = yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{{Kind: yamlv3.MappingNode, Tag: "!!map"}}}
		if len(bytes.TrimSpace(b)) > 0 {
			out.Write(bytes.TrimRight(b, "\n"))
			out.WriteByte('\n')
		}
	}
	root := doc.Content[0]
	if len(doc.Content) != 1 || root.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("%s is not a mapping of settings; edit it by hand", path)
	}
	for _, c := range changes {
		parts := strings.Split(c.Setting.Key, ".")
		if c.Unset {
			if unsetPath(root, parts) {
				removed = append(removed, c.Setting)
			}
			continue
		}
		v, err := c.Setting.parse(c.Value)
		if err != nil {
			return nil, err
		}
		var node yamlv3.Node
		if err := node.Encode(v); err != nil {
			return nil, err
		}
		if err := setPath(root, parts, &node); err != nil {
			return nil, fmt.Errorf("%s in %s: %w; edit it by hand", c.Setting.Key, path, err)
		}
	}
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	// Refuse to write a file tug would then refuse to read.
	var cfg Config
	if err := yaml.UnmarshalStrict(out.Bytes(), &cfg); err != nil {
		return nil, fmt.Errorf("the change would leave %s unreadable: %w", path, err)
	}
	if check != nil {
		if err := check(&cfg); err != nil {
			return nil, err
		}
	}
	return removed, WriteFileAtomic(path, out.Bytes(), 0o600)
}

// setPath puts value at the path under m, adding the mappings on the way.
func setPath(m *yamlv3.Node, parts []string, value *yamlv3.Node) error {
	for _, p := range parts[:len(parts)-1] {
		if m = mapping(m, p, true); m == nil {
			return fmt.Errorf("%s is not a mapping", p)
		}
	}
	key := parts[len(parts)-1]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// Keep a comment written after the old value.
			value.LineComment = m.Content[i+1].LineComment
			m.Content[i+1] = value
			return nil
		}
	}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}, value)
	return nil
}

// unsetPath removes the path under m, and any mapping it leaves empty that
// has no comment of its own. It reports whether the path was there.
func unsetPath(m *yamlv3.Node, parts []string) bool {
	if len(parts) > 1 {
		child := mapping(m, parts[0], false)
		if child == nil || !unsetPath(child, parts[1:]) {
			return false
		}
		if len(child.Content) == 0 {
			for i := 0; i+1 < len(m.Content); i += 2 {
				if m.Content[i+1] == child && m.Content[i].HeadComment == "" && m.Content[i].LineComment == "" {
					m.Content = slices.Delete(m.Content, i, i+2)
					break
				}
			}
		}
		return true
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == parts[0] {
			m.Content = slices.Delete(m.Content, i, i+2)
			return true
		}
	}
	return false
}

// SetInFile returns the keys of the settings config.yaml in dir sets.
func SetInFile(dir string) (map[string]bool, error) {
	path := filepath.Join(dir, "config.yaml")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	set := map[string]bool{}
	if len(doc.Content) != 1 {
		return set, nil
	}
	for _, s := range settings {
		m := doc.Content[0]
		parts := strings.Split(s.Key, ".")
		for _, p := range parts[:len(parts)-1] {
			m = mapping(m, p, false)
		}
		if m == nil {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == parts[len(parts)-1] {
				set[s.Key] = true
			}
		}
	}
	return set, nil
}
