package diff

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// elemKind says how an Elem steps into a value.
type elemKind uint8

const (
	fieldElem elemKind = iota // into a map, by key
	nameElem                  // into a list, to the item with this name
	indexElem                 // into a list, to the item at this position
)

// Elem is one step of a Path: into a map by key, or into a list to an item
// found by its name field or by its position.
type Elem struct {
	kind  elemKind
	key   string
	index int
}

// Field is the step into a map's key.
func Field(key string) Elem { return Elem{kind: fieldElem, key: key} }

// Named is the step into a list to the item whose name field is name, as
// for containers, volumes and env.
func Named(name string) Elem { return Elem{kind: nameElem, key: name} }

// Index is the step into a list to the item at position i.
func Index(i int) Elem { return Elem{kind: indexElem, index: i} }

// Path is where a field is in an object.
type Path []Elem

// String writes the path as server-side apply writes field paths in
// conflict messages, without the leading dot:
// spec.template.spec.containers[name="api"].image.
func (p Path) String() string {
	var b strings.Builder
	for _, e := range p {
		switch e.kind {
		case fieldElem:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(e.key)
		case nameElem:
			q, _ := json.Marshal(e.key) // a string always marshals
			b.WriteString(`[name=`)
			b.Write(q)
			b.WriteByte(']')
		case indexElem:
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(e.index))
			b.WriteByte(']')
		}
	}
	return b.String()
}

// MarshalText writes the path as String does, for JSON and YAML output.
func (p Path) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText reads a path as String writes it. A key with a dot in it,
// such as an annotation's, comes back as several steps, which String
// writes the same way.
func (p *Path) UnmarshalText(b []byte) error {
	s := string(b)
	var out Path
	for s != "" {
		switch {
		case s[0] == '.':
			s = s[1:]
		case s[0] == '[':
			end := strings.IndexByte(s, ']')
			if strings.HasPrefix(s, `[name="`) {
				// The name is a JSON string, which may hold a "]".
				var name string
				dec := json.NewDecoder(strings.NewReader(s[len(`[name=`):]))
				if err := dec.Decode(&name); err != nil {
					return fmt.Errorf("path %q: %w", b, err)
				}
				end = len(`[name=`) + int(dec.InputOffset())
				if end >= len(s) || s[end] != ']' {
					return fmt.Errorf("path %q: no ] after a name", b)
				}
				out = append(out, Named(name))
			} else {
				if end < 0 {
					return fmt.Errorf("path %q: no ] after [", b)
				}
				i, err := strconv.Atoi(s[1:end])
				if err != nil {
					return fmt.Errorf("path %q: %w", b, err)
				}
				out = append(out, Index(i))
			}
			s = s[end+1:]
		default:
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			out = append(out, Field(s[:end]))
			s = s[end:]
		}
	}
	*p = out
	return nil
}

// child returns a copy of p with e appended, so that paths handed out never
// share a backing array.
func (p Path) child(e Elem) Path {
	c := make(Path, len(p), len(p)+1)
	copy(c, p)
	return append(c, e)
}

// itemIndex finds where in list the step e leads, or -1.
func itemIndex(list []any, e Elem) int {
	switch e.kind {
	case indexElem:
		if e.index >= 0 && e.index < len(list) {
			return e.index
		}
	case nameElem:
		for i, it := range list {
			if m, ok := it.(map[string]any); ok && m["name"] == e.key {
				return i
			}
		}
	}
	return -1
}

// lookup returns the value at p in v.
func lookup(v any, p Path) (any, bool) {
	for _, e := range p {
		switch e.kind {
		case fieldElem:
			m, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if v, ok = m[e.key]; !ok {
				return nil, false
			}
		default:
			list, ok := v.([]any)
			if !ok {
				return nil, false
			}
			i := itemIndex(list, e)
			if i < 0 {
				return nil, false
			}
			v = list[i]
		}
	}
	return v, true
}

// remove takes the value at p out of v and returns what is left. Removing
// a list item shifts the items after it, so callers remove the items of one
// list from the last to the first.
func remove(v any, p Path) any {
	if len(p) == 0 {
		return v
	}
	e := p[0]
	switch e.kind {
	case fieldElem:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		if len(p) == 1 {
			delete(m, e.key)
			return m
		}
		if child, ok := m[e.key]; ok {
			m[e.key] = remove(child, p[1:])
		}
		return m
	default:
		list, ok := v.([]any)
		if !ok {
			return v
		}
		i := itemIndex(list, e)
		if i < 0 {
			return v
		}
		if len(p) == 1 {
			return append(list[:i:i], list[i+1:]...)
		}
		list[i] = remove(list[i], p[1:])
		return list
	}
}
