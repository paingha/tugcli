package apiinfo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Schema is a node of a CRD version's openAPIV3Schema, as much of it as tug
// shows.
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	// Items is the schema of an array's items.
	Items *Schema `json:"items,omitempty"`
	// AdditionalProperties is the schema of a map's values: an empty
	// Schema when the map takes any values, nil when it is not a map.
	AdditionalProperties *Schema           `json:"additionalProperties,omitempty"`
	Enum                 []json.RawMessage `json:"enum,omitempty"`
	Default              json.RawMessage   `json:"default,omitempty"`
	Nullable             bool              `json:"nullable,omitempty"`
	// The Kubernetes extensions that change what a field holds.
	PreserveUnknownFields bool `json:"x-kubernetes-preserve-unknown-fields,omitempty"`
	IntOrString           bool `json:"x-kubernetes-int-or-string,omitempty"`
	EmbeddedResource      bool `json:"x-kubernetes-embedded-resource,omitempty"`
}

// UnmarshalJSON reads a schema whose items may be a list of schemas and
// whose additionalProperties may be a boolean, as JSON Schema allows.
func (s *Schema) UnmarshalJSON(b []byte) error {
	type plain Schema
	var raw struct {
		plain
		Items                json.RawMessage `json:"items"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = Schema(raw.plain)
	s.Items, s.AdditionalProperties = nil, nil
	switch items := bytes.TrimSpace(raw.Items); {
	case len(items) == 0, bytes.Equal(items, []byte("null")):
	case items[0] == '[':
		var list []*Schema
		if err := json.Unmarshal(items, &list); err != nil {
			return err
		}
		if len(list) > 0 {
			s.Items = list[0]
		}
	default:
		if err := json.Unmarshal(items, &s.Items); err != nil {
			return err
		}
	}
	switch ap := bytes.TrimSpace(raw.AdditionalProperties); {
	case len(ap) == 0, bytes.Equal(ap, []byte("null")), bytes.Equal(ap, []byte("false")):
	case bytes.Equal(ap, []byte("true")):
		s.AdditionalProperties = &Schema{}
	default:
		if err := json.Unmarshal(ap, &s.AdditionalProperties); err != nil {
			return err
		}
	}
	return nil
}

// TypeName says what a field holds, in the style of Go types: string,
// []string, map[string]string, object, int-or-string, or any.
func (s *Schema) TypeName() string {
	switch {
	case s == nil:
		return "any"
	case s.IntOrString:
		return "int-or-string"
	case s.EmbeddedResource:
		return "object (a Kubernetes object)"
	case s.Type == "array":
		return "[]" + s.Items.TypeName()
	case s.Type == "object" && s.AdditionalProperties != nil && len(s.Properties) == 0:
		return "map[string]" + s.AdditionalProperties.TypeName()
	case s.Type == "":
		return "any"
	case s.Format != "":
		return s.Type + " (" + s.Format + ")"
	}
	return s.Type
}

// fields returns the schema whose properties are a field's children,
// looking through arrays and maps to what they hold.
func (s *Schema) fields() *Schema {
	for s != nil {
		switch {
		case len(s.Properties) > 0:
			return s
		case s.Items != nil:
			s = s.Items
		case s.AdditionalProperties != nil:
			s = s.AdditionalProperties
		default:
			return s
		}
	}
	return nil
}

// Field is one field of a schema, flattened for display.
type Field struct {
	// Path is where the field is in an object, such as
	// spec.issuerRef.name. Arrays and maps are looked through, so the
	// fields of a list's items follow the list's own name.
	Path string `json:"path"`
	// Depth is how far below the listing's starting point the field is,
	// from 0.
	Depth       int      `json:"-"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// Fields flattens a schema depth-first, each field's children after it in
// name order. With a path, such as spec.issuerRef, it lists that field and
// then what is below it; without one, the top-level fields and what is
// below them. A maxDepth above 0 lists only that many levels of fields
// below the starting point.
func Fields(root *Schema, path string, maxDepth int) ([]Field, error) {
	if root == nil {
		return nil, fmt.Errorf("the definition has no schema for this version")
	}
	var out []Field
	var walk func(parent *Schema, prefix string, depth, level int)
	walk = func(parent *Schema, prefix string, depth, level int) {
		holder := parent.fields()
		if holder == nil || (maxDepth > 0 && level > maxDepth) {
			return
		}
		for _, name := range slices.Sorted(maps.Keys(holder.Properties)) {
			child := holder.Properties[name]
			f := fieldOf(name, prefix+name, child, slices.Contains(holder.Required, name))
			f.Depth = depth
			out = append(out, f)
			walk(child, f.Path+".", depth+1, level+1)
		}
	}
	if path == "" {
		walk(root, "", 0, 1)
		return out, nil
	}
	node, path, required, err := lookup(root, path)
	if err != nil {
		return nil, err
	}
	out = append(out, fieldOf(path[strings.LastIndex(path, ".")+1:], path, node, required))
	walk(node, path+".", 1, 1)
	return out, nil
}

// lookup finds the field at a dotted path, the path as the schema spells
// it, and whether the field's parent requires it.
func lookup(root *Schema, path string) (*Schema, string, bool, error) {
	node, required := root, false
	walked := ""
	for _, seg := range strings.Split(path, ".") {
		holder := node.fields()
		name, child := holder.property(seg)
		if child == nil {
			where := "at the top level"
			if walked != "" {
				where = "under " + walked
			}
			if holder == nil || len(holder.Properties) == 0 {
				return nil, "", false, fmt.Errorf("no field %q %s, which has no fields of its own", seg, where)
			}
			return nil, "", false, fmt.Errorf("no field %q %s; it has %s", seg, where, strings.Join(slices.Sorted(maps.Keys(holder.Properties)), ", "))
		}
		required = slices.Contains(holder.Required, name)
		node = child
		walked = strings.TrimPrefix(walked+"."+name, ".")
	}
	return node, walked, required, nil
}

// property returns a named property and its name, ignoring case only when
// no property matches exactly.
func (s *Schema) property(name string) (string, *Schema) {
	if s == nil {
		return "", nil
	}
	if p, ok := s.Properties[name]; ok {
		return name, p
	}
	for _, k := range slices.Sorted(maps.Keys(s.Properties)) {
		if strings.EqualFold(k, name) {
			return k, s.Properties[k]
		}
	}
	return "", nil
}

func fieldOf(name, path string, s *Schema, required bool) Field {
	f := Field{Path: path, Name: name, Type: s.TypeName(), Required: required}
	if s == nil {
		return f
	}
	f.Description = strings.TrimSpace(s.Description)
	for _, e := range s.Enum {
		f.Enum = append(f.Enum, jsonScalar(e))
	}
	if len(s.Default) > 0 {
		f.Default = jsonScalar(s.Default)
	}
	return f
}

// jsonScalar writes a JSON value for people: strings without their quotes,
// anything else as compact JSON.
func jsonScalar(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}
