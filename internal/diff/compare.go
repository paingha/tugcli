package diff

import (
	"slices"
)

// Op is what a Change does to a field.
type Op string

// What a Change does.
const (
	OpAdd    Op = "add"    // the field is only on the To side
	OpRemove Op = "remove" // the field is only on the From side
	OpChange Op = "change" // the field has a different value on each side
)

// Change is one field that differs between two objects.
type Change struct {
	Path Path `json:"path"`
	Op   Op   `json:"op"`
	// From and To are the values on each side; the one an add or a remove
	// lacks is left out.
	From any `json:"from,omitempty"`
	To   any `json:"to,omitempty"`
	// FromOwners and ToOwners are the field managers that own the field on
	// each side, from managedFields.
	FromOwners []Owner `json:"fromOwners,omitempty"`
	ToOwners   []Owner `json:"toOwners,omitempty"`
	// Origin says, when comparing with a manifest, what set the To value:
	// OriginManifest, OriginAdmission or OriginDefault.
	Origin string `json:"origin,omitempty"`
	// Conflicts are the field managers an apply of the To value would
	// conflict with.
	Conflicts []string `json:"conflicts,omitempty"`
}

// Compare lists the fields that differ between from and to, which are
// normalised objects, in a stable order. Maps are compared key by key.
// Lists whose items all have a distinct name, such as containers, env and
// volumes, are compared item by item by name; other lists by position. A
// nil object has no fields, so comparing with one lists the other whole.
func Compare(from, to map[string]any) []Change {
	var out []Change
	switch {
	case from == nil && to == nil:
	case from == nil:
		out = append(out, Change{Op: OpAdd, To: to})
	case to == nil:
		out = append(out, Change{Op: OpRemove, From: from})
	default:
		compare(nil, from, to, &out)
	}
	return out
}

func compare(p Path, a, b any, out *[]Change) {
	switch av := a.(type) {
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			compareMaps(p, av, bv, out)
			return
		}
	case []any:
		if bv, ok := b.([]any); ok {
			compareLists(p, av, bv, out)
			return
		}
	}
	if !jsonEqual(a, b) {
		*out = append(*out, Change{Path: p, Op: OpChange, From: a, To: b})
	}
}

func compareMaps(p Path, a, b map[string]any, out *[]Change) {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		av, inA := a[k]
		bv, inB := b[k]
		switch {
		case !inB:
			*out = append(*out, Change{Path: p.child(Field(k)), Op: OpRemove, From: av})
		case !inA:
			*out = append(*out, Change{Path: p.child(Field(k)), Op: OpAdd, To: bv})
		default:
			compare(p.child(Field(k)), av, bv, out)
		}
	}
}

func compareLists(p Path, a, b []any, out *[]Change) {
	an, aok := itemNames(a)
	bn, bok := itemNames(b)
	if !aok || !bok {
		for i := range max(len(a), len(b)) {
			switch {
			case i >= len(b):
				*out = append(*out, Change{Path: p.child(Index(i)), Op: OpRemove, From: a[i]})
			case i >= len(a):
				*out = append(*out, Change{Path: p.child(Index(i)), Op: OpAdd, To: b[i]})
			default:
				compare(p.child(Index(i)), a[i], b[i], out)
			}
		}
		return
	}
	for i, name := range an {
		j := slices.Index(bn, name)
		if j < 0 {
			*out = append(*out, Change{Path: p.child(Named(name)), Op: OpRemove, From: a[i]})
			continue
		}
		compare(p.child(Named(name)), a[i], b[j], out)
	}
	for j, name := range bn {
		if !slices.Contains(an, name) {
			*out = append(*out, Change{Path: p.child(Named(name)), Op: OpAdd, To: b[j]})
		}
	}
}

// itemNames returns the name of each item of a list, and whether every item
// is a map with a name that no other item has. An empty list counts, so
// that adding the first container is matched by name too.
func itemNames(list []any) ([]string, bool) {
	names := make([]string, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := m["name"].(string)
		if !ok || slices.Contains(names, name) {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}
