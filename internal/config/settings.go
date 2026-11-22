package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
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

// Setting kinds: what a setting holds.
const (
	KindBool     = "bool"
	KindInt      = "int"
	KindNumber   = "number"
	KindString   = "string"
	KindDuration = "duration"
	// KindList is a list of strings: plugins.sandbox.exclude.
	KindList = "list"
	// KindLabels is a list of label sets: guard.readOnlyLabels.
	KindLabels = "labels"
	// KindEntries is a list of entries with named fields:
	// plugins.indexes, each with a name, url and publicKey.
	KindEntries = "entries"
	// KindMap is a map whose entries are settings of their own, named
	// after it: plugins.timeouts.list.
	KindMap = "map"
)

// Setting is a key in config.yaml that tug settings can read and change: a
// boolean, number, string or duration, a list of strings, label sets or
// entries, or an entry of a map. Lists of entries with lists of their own,
// such as teams, are left to the file and the commands that own them.
type Setting struct {
	// Key is the setting's path as config.yaml spells it:
	// intelligence.enabled, or plugins.timeouts.list for an entry of a map.
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Fields are an entry's fields, for KindEntries, in order.
	Fields []string `json:"fields,omitempty"`
	// Elem is the kind of a map's values, for KindMap.
	Elem string `json:"elem,omitempty"`
	// Env names the variable that overrides the setting, if one does.
	Env string `json:"env,omitempty"`

	index []int
	path  []string
	// mapKey is the entry, for a setting that is one entry of a map.
	mapKey string
	// fieldKinds are the kinds of an entry's fields.
	fieldKinds []string
}

// envOverrides are the settings Load takes from the environment when set.
var envOverrides = map[string]string{
	"keyring.backend": "TUG_KEYRING_BACKEND",
	"kubectl":         "TUG_KUBECTL",
}

var durationType = reflect.TypeFor[Duration]()

// scalarKind is the kind of a field that holds one value, or "".
func scalarKind(t reflect.Type) string {
	if t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Bool {
		t = t.Elem()
	}
	switch {
	case t == durationType:
		return KindDuration
	case t.Kind() == reflect.Bool:
		return KindBool
	case t.Kind() == reflect.Int:
		return KindInt
	case t.Kind() == reflect.Float64:
		return KindNumber
	case t.Kind() == reflect.String:
		return KindString
	}
	return ""
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "-" || !f.IsExported() {
		return ""
	}
	return name
}

// settings are Config's settings, found once from its fields.
var settings = func() []Setting {
	var out []Setting
	var walk func(t reflect.Type, path []string, index []int)
	walk = func(t reflect.Type, path []string, index []int) {
		for i := range t.NumField() {
			f := t.Field(i)
			name := jsonName(f)
			if name == "" {
				continue
			}
			p := append(slices.Clone(path), name)
			s := Setting{Key: strings.Join(p, "."), path: p, index: append(slices.Clone(index), i)}
			s.Env = envOverrides[s.Key]
			ft := f.Type
			switch k := scalarKind(ft); {
			case k != "":
				s.Kind = k
			case ft.Kind() == reflect.Struct:
				walk(ft, p, s.index)
				continue
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String:
				s.Kind = KindList
			case ft.Kind() == reflect.Slice && ft.Elem() == reflect.TypeFor[map[string]string]():
				s.Kind = KindLabels
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
				if !entryFields(ft.Elem(), &s) {
					continue // an entry with lists or entries of its own
				}
				s.Kind = KindEntries
			case ft.Kind() == reflect.Map && ft.Key().Kind() == reflect.String && scalarKind(ft.Elem()) != "":
				s.Kind, s.Elem = KindMap, scalarKind(ft.Elem())
			default:
				continue
			}
			out = append(out, s)
		}
	}
	walk(reflect.TypeFor[Config](), nil, nil)
	return out
}()

// entryFields records an entry's fields, and reports false when one of
// them does not hold a single value.
func entryFields(t reflect.Type, s *Setting) bool {
	for i := range t.NumField() {
		f := t.Field(i)
		name, kind := jsonName(f), scalarKind(f.Type)
		if name == "" {
			continue
		}
		if kind == "" {
			return false
		}
		s.Fields, s.fieldKinds = append(s.Fields, name), append(s.fieldKinds, kind)
	}
	return len(s.Fields) > 0
}

// Settings returns the settings tug settings can change, in config.yaml's
// order. A map, such as plugins.timeouts, stands for its entries.
func Settings() []Setting { return slices.Clone(settings) }

// LookupSetting finds a setting by its key, in any case, or an entry of a
// map by the map's key, a dot and the entry's own key, in its own case:
// plugins.timeouts.checkAccess.
func LookupSetting(key string) (Setting, bool) {
	for _, s := range settings {
		if strings.EqualFold(s.Key, key) {
			return s, true
		}
	}
	for _, s := range settings {
		if s.Kind != KindMap || len(key) <= len(s.Key)+1 || !strings.EqualFold(key[:len(s.Key)+1], s.Key+".") {
			continue
		}
		entry := key[len(s.Key)+1:]
		s.Key, s.Kind, s.Elem = s.Key+"."+entry, s.Elem, ""
		s.path, s.mapKey = append(slices.Clone(s.path), entry), entry
		return s, true
	}
	return Setting{}, false
}

// MapKey is the entry a setting names in its map, or "".
func (s Setting) MapKey() string { return s.mapKey }

// IsList reports whether the setting is a list, which tug settings add and
// remove change one item at a time.
func (s Setting) IsList() bool {
	return s.Kind == KindList || s.Kind == KindLabels || s.Kind == KindEntries
}

// Value returns the setting's value in c as tug settings get writes it, or
// "" when it has none. A list's items are on lines of their own, each as
// tug settings add takes it; a map's entries are key=value on one line.
func (s Setting) Value(c *Config) string {
	return strings.Join(s.Values(c), "\n")
}

// Values returns the setting's value in c: one item for a single value,
// each item of a list, or each key=value of a map, sorted.
func (s Setting) Values(c *Config) []string {
	v := reflect.ValueOf(c).Elem().FieldByIndex(s.index)
	if s.mapKey != "" {
		e := v.MapIndex(reflect.ValueOf(s.mapKey))
		if !e.IsValid() {
			return nil
		}
		return nonEmpty(scalarText(e, s.Kind))
	}
	switch s.Kind {
	case KindList:
		return slices.Clone(v.Interface().([]string))
	case KindLabels:
		var out []string
		for _, m := range v.Interface().([]map[string]string) {
			out = append(out, labelsText(m))
		}
		return out
	case KindEntries:
		var out []string
		for i := range v.Len() {
			e := v.Index(i)
			var parts []string
			for j, name := range s.Fields {
				if t := scalarText(e.Field(fieldIndex(e.Type(), name)), s.fieldKinds[j]); t != "" {
					parts = append(parts, name+"="+t)
				}
			}
			out = append(out, strings.Join(parts, " "))
		}
		return out
	case KindMap:
		var out []string
		for _, k := range mapKeysOf(v) {
			out = append(out, k+"="+scalarText(v.MapIndex(reflect.ValueOf(k)), s.Elem))
		}
		return out
	}
	return nonEmpty(scalarText(v, s.Kind))
}

// mapKeysOf lists a map's keys, sorted, whatever its values' type.
func mapKeysOf(v reflect.Value) []string {
	var keys []string
	for _, k := range v.MapKeys() {
		keys = append(keys, k.String())
	}
	slices.Sort(keys)
	return keys
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func fieldIndex(t reflect.Type, name string) int {
	for i := range t.NumField() {
		if jsonName(t.Field(i)) == name {
			return i
		}
	}
	return -1
}

// scalarText writes one value as tug settings set takes it, or "" when it
// has none. A bool is always written.
func scalarText(v reflect.Value, kind string) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	switch kind {
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

// labelsText writes a label set as tug settings add takes it:
// env=prod tier=critical.
func labelsText(m map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

// parseScalar checks one value of a kind and returns it as YAML should
// hold it. what names it in an error.
func parseScalar(what, kind, value string) (any, error) {
	value = strings.TrimSpace(value)
	bad := func(want string) error { return fmt.Errorf("%s takes %s, not %q", what, want, value) }
	switch kind {
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
			return nil, fmt.Errorf("%s cannot be set to nothing", what)
		}
		return value, nil
	}
}

// pairs reads key=value arguments, each key once.
func pairs(what string, args []string) ([][2]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%s needs key=value arguments", what)
	}
	var out [][2]string
	seen := map[string]bool{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || k == "" || v == "":
			return nil, fmt.Errorf("%s takes key=value arguments, not %q", what, a)
		case seen[k]:
			return nil, fmt.Errorf("%s names %s twice", what, k)
		}
		seen[k] = true
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// Change ops.
const (
	// OpSet sets a setting that holds one value, or an entry of a map.
	OpSet = "set"
	// OpUnset removes a setting, a whole list included.
	OpUnset = "unset"
	// OpAdd adds an item to a list; for a list of entries, it replaces the
	// entry with the same name, model, path or url.
	OpAdd = "add"
	// OpRemove removes an item from a list.
	OpRemove = "remove"
)

// Change is one change to a setting.
type Change struct {
	Setting Setting
	Op      string
	// Value is the new value, for OpSet.
	Value string
	// Item is the item to add or remove: one string for a list of
	// strings, and key=value arguments for a label set or an entry. An
	// entry to remove may also be named by its name, model, path or url
	// alone.
	Item []string
}

// Effects say what a change did.
const (
	EffectSet       = "set"
	EffectAdded     = "added"
	EffectReplaced  = "replaced"
	EffectRemoved   = "removed"
	EffectUnchanged = "unchanged"
)

// EditSettings makes the changes to config.yaml in dir in one write,
// changing only their entries, so the rest of the file, comments included,
// stays as the user wrote it; blank lines between settings do not survive.
// Before writing, it checks that tug can read the result and passes it to
// check, whose error stops the write. It returns each change's effect, in
// order; nothing is written when no change has one.
func EditSettings(dir string, changes []Change, check func(*Config) error) ([]string, error) {
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
	effects := make([]string, len(changes))
	for i, c := range changes {
		effect, err := apply(root, c)
		if err != nil {
			return nil, fmt.Errorf("%s in %s: %w", c.Setting.Key, path, err)
		}
		effects[i] = effect
	}
	if !slices.ContainsFunc(effects, func(e string) bool { return e != EffectUnchanged }) {
		return effects, nil
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
	return effects, WriteFileAtomic(path, out.Bytes(), 0o600)
}

// apply makes one change to the document's settings.
func apply(root *yamlv3.Node, c Change) (string, error) {
	s := c.Setting
	switch c.Op {
	case OpUnset:
		if unsetPath(root, s.path) {
			return EffectRemoved, nil
		}
		return EffectUnchanged, nil
	case OpSet, "":
		if s.IsList() {
			return "", fmt.Errorf("it is a list; add or remove one item at a time")
		}
		if s.Kind == KindMap {
			return "", fmt.Errorf("it is a map; set one of its entries, as %s.<key>", s.Key)
		}
		v, err := parseScalar(s.Key, s.Kind, c.Value)
		if err != nil {
			return "", err
		}
		node, err := encode(v)
		if err != nil {
			return "", err
		}
		return EffectSet, setPath(root, s.path, node)
	case OpAdd, OpRemove:
		if !s.IsList() {
			return "", fmt.Errorf("it is not a list; use set or unset")
		}
		item, err := itemNode(s, c)
		if err != nil {
			return "", err
		}
		if c.Op == OpAdd {
			return addItem(root, s, item)
		}
		return removeItem(root, s, item)
	}
	return "", fmt.Errorf("no such change %q", c.Op)
}

func encode(v any) (*yamlv3.Node, error) {
	var node yamlv3.Node
	if err := node.Encode(v); err != nil {
		return nil, err
	}
	return &node, nil
}

// itemNode builds the item a change adds or removes. An entry to remove
// named by a bare value is a scalar node, which matches an entry with that
// name, model, path or url.
func itemNode(s Setting, c Change) (*yamlv3.Node, error) {
	switch s.Kind {
	case KindList:
		if len(c.Item) != 1 || strings.TrimSpace(c.Item[0]) == "" {
			return nil, errors.New("give one item")
		}
		return encode(strings.TrimSpace(c.Item[0]))
	case KindLabels:
		ps, err := pairs("a label set", c.Item)
		if err != nil {
			return nil, err
		}
		m := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
		for _, p := range ps {
			m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: p[0]},
				&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: p[1]})
		}
		return m, nil
	default: // KindEntries
		if c.Op == OpRemove && len(c.Item) == 1 && !strings.Contains(c.Item[0], "=") {
			return encode(strings.TrimSpace(c.Item[0]))
		}
		ps, err := pairs("an entry", c.Item)
		if err != nil {
			return nil, err
		}
		m := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
		for _, p := range ps {
			i := slices.Index(s.Fields, p[0])
			if i < 0 {
				return nil, fmt.Errorf("an entry has no field %s; its fields are %s", p[0], strings.Join(s.Fields, ", "))
			}
			v, err := parseScalar(p[0], s.fieldKinds[i], p[1])
			if err != nil {
				return nil, err
			}
			node, err := encode(v)
			if err != nil {
				return nil, err
			}
			m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: p[0]}, node)
		}
		return m, nil
	}
}

// identityFields are the fields that name an entry: one with the same
// value in one of these is the same entry.
var identityFields = []string{"name", "model", "path", "url"}

// sameItem reports whether a list's item is the one named.
func sameItem(s Setting, have, want *yamlv3.Node) bool {
	switch s.Kind {
	case KindList:
		return have.Kind == yamlv3.ScalarNode && have.Value == want.Value
	case KindLabels:
		return have.Kind == yamlv3.MappingNode && maps.Equal(fieldsOf(have), fieldsOf(want))
	}
	if have.Kind != yamlv3.MappingNode {
		return false
	}
	h := fieldsOf(have)
	if want.Kind == yamlv3.ScalarNode {
		return slices.ContainsFunc(identityFields, func(f string) bool { return h[f] != "" && h[f] == want.Value })
	}
	w := fieldsOf(want)
	return slices.ContainsFunc(identityFields, func(f string) bool { return w[f] != "" && h[f] == w[f] })
}

func fieldsOf(m *yamlv3.Node) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		out[m.Content[i].Value] = m.Content[i+1].Value
	}
	return out
}

// sequence returns the list at the setting's path, adding it when create
// is set.
func sequence(root *yamlv3.Node, s Setting, create bool) (*yamlv3.Node, error) {
	m := root
	for _, p := range s.path[:len(s.path)-1] {
		if m = mapping(m, p, create); m == nil {
			if create {
				return nil, fmt.Errorf("%s is not a mapping", p)
			}
			return nil, nil
		}
	}
	key := s.path[len(s.path)-1]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		v := m.Content[i+1]
		switch {
		case v.Kind == yamlv3.SequenceNode:
			return v, nil
		case v.Kind == yamlv3.ScalarNode && v.Tag == "!!null" && create:
			m.Content[i+1] = &yamlv3.Node{Kind: yamlv3.SequenceNode, Tag: "!!seq"}
			return m.Content[i+1], nil
		}
		return nil, fmt.Errorf("%s is not a list", key)
	}
	if !create {
		return nil, nil
	}
	seq := &yamlv3.Node{Kind: yamlv3.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}, seq)
	return seq, nil
}

func addItem(root *yamlv3.Node, s Setting, item *yamlv3.Node) (string, error) {
	seq, err := sequence(root, s, true)
	if err != nil {
		return "", err
	}
	for i, have := range seq.Content {
		if !sameItem(s, have, item) {
			continue
		}
		if s.Kind != KindEntries || maps.Equal(fieldsOf(have), fieldsOf(item)) {
			return EffectUnchanged, nil
		}
		seq.Content[i] = item
		return EffectReplaced, nil
	}
	seq.Content = append(seq.Content, item)
	return EffectAdded, nil
}

func removeItem(root *yamlv3.Node, s Setting, item *yamlv3.Node) (string, error) {
	seq, err := sequence(root, s, false)
	if err != nil || seq == nil {
		return EffectUnchanged, err
	}
	n := len(seq.Content)
	seq.Content = slices.DeleteFunc(seq.Content, func(have *yamlv3.Node) bool { return sameItem(s, have, item) })
	if len(seq.Content) == n {
		return EffectUnchanged, nil
	}
	if len(seq.Content) == 0 {
		unsetPath(root, s.path) // an empty list is the default again
	}
	return EffectRemoved, nil
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

// SetInFile returns the keys of the settings config.yaml in dir sets, with
// each entry of a map as its own setting.
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
		for _, p := range s.path[:len(s.path)-1] {
			m = mapping(m, p, false)
		}
		if m == nil {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value != s.path[len(s.path)-1] {
				continue
			}
			set[s.Key] = true
			if v := m.Content[i+1]; s.Kind == KindMap && v.Kind == yamlv3.MappingNode {
				for j := 0; j+1 < len(v.Content); j += 2 {
					set[s.Key+"."+v.Content[j].Value] = true
				}
			}
		}
	}
	return set, nil
}
