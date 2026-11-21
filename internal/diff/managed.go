package diff

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Owner is a field manager that owns a field.
type Owner struct {
	Manager string `json:"manager"`
	// Operation is Apply for a server-side apply and Update for anything
	// else: create, update, patch or client-side kubectl apply.
	Operation string `json:"operation"`
	// Subresource is set when the manager wrote through one, such as
	// status or scale.
	Subresource string `json:"subresource,omitempty"`
}

// Manager is one entry of an object's managedFields: who wrote, how, when,
// and the fields they own.
type Manager struct {
	Owner
	APIVersion string    `json:"apiVersion,omitempty"`
	Time       time.Time `json:"time,omitzero"`
	// Fields are the paths of the fields the manager owns, as String
	// writes a Path, with sets written [="value"].
	Fields []string `json:"fields"`

	set map[string]any
}

// Managers reads an object's managedFields. Entries in a format other than
// FieldsV1, which no API server writes today, are skipped.
func Managers(obj map[string]any) ([]Manager, error) {
	md, _ := obj["metadata"].(map[string]any)
	entries, _ := md["managedFields"].([]any)
	out := make([]Manager, 0, len(entries))
	for i, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("managedFields[%d] is not an object", i)
		}
		if t, _ := m["fieldsType"].(string); t != "" && t != "FieldsV1" {
			continue
		}
		set, _ := m["fieldsV1"].(map[string]any)
		mgr := Manager{
			Owner: Owner{
				Manager:     str(m["manager"]),
				Operation:   str(m["operation"]),
				Subresource: str(m["subresource"]),
			},
			APIVersion: str(m["apiVersion"]),
			Fields:     fieldPaths(set),
			set:        set,
		}
		if s := str(m["time"]); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, fmt.Errorf("managedFields[%d] of %s: time %q: %w", i, mgr.Manager, s, err)
			}
			mgr.Time = t
		}
		out = append(out, mgr)
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Owners returns the managers of obj that own the field at p, or anything
// beneath it, in managedFields order. obj is the object as the API server
// returned it, with its managedFields; p is found in it as Compare found
// it, so list items are matched however the field set keys them.
func Owners(obj map[string]any, p Path) []Owner {
	mgrs, err := Managers(obj)
	if err != nil {
		return nil
	}
	return ownersIn(mgrs, obj, p)
}

func ownersIn(mgrs []Manager, obj map[string]any, p Path) []Owner {
	var out []Owner
	for _, m := range mgrs {
		if owns(m.set, obj, p) && !slices.Contains(out, m.Owner) {
			out = append(out, m.Owner)
		}
	}
	return out
}

// owns reports whether a FieldsV1 set holds the field at p in v. The set
// mirrors the object: "f:name" for a field, "k:{...}" for a list item by
// its keys, "v:..." for an item of a set, "i:n" for an item by position,
// and "." for the item itself. A field whose node in the set is empty is
// owned whole, atomically, with everything beneath it.
func owns(set map[string]any, v any, p Path) bool {
	if len(set) == 0 {
		return false
	}
	node := set
	for _, e := range p {
		var child any
		switch e.kind {
		case fieldElem:
			child = node["f:"+e.key]
			m, _ := v.(map[string]any)
			v = m[e.key]
		default:
			list, _ := v.([]any)
			i := itemIndex(list, e)
			if i < 0 {
				return false
			}
			v = list[i]
			child = itemNode(node, v, i)
		}
		next, ok := child.(map[string]any)
		if !ok {
			return false
		}
		if len(next) == 0 {
			return true
		}
		node = next
	}
	return true
}

// itemNode finds the node of a set that stands for the list item at
// position i.
func itemNode(node map[string]any, item any, i int) any {
	for k, child := range node {
		switch {
		case strings.HasPrefix(k, "k:"):
			keys, ok := decode([]byte(k[2:])).(map[string]any)
			m, isMap := item.(map[string]any)
			if !ok || !isMap {
				continue
			}
			match := true
			for name, want := range keys {
				if got, ok := m[name]; !ok || !jsonEqual(got, want) {
					match = false
					break
				}
			}
			if match {
				return child
			}
		case strings.HasPrefix(k, "v:"):
			if jsonEqual(decode([]byte(k[2:])), item) {
				return child
			}
		case strings.HasPrefix(k, "i:"):
			if n, err := strconv.Atoi(k[2:]); err == nil && n == i {
				return child
			}
		}
	}
	return nil
}

// fieldPaths lists the leaves of a FieldsV1 set: the fields a manager
// owns, sorted.
func fieldPaths(set map[string]any) []string {
	var out []string
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		children := 0
		for k, child := range node {
			if k == "." {
				continue
			}
			children++
			c, _ := child.(map[string]any)
			walk(prefix+pathStep(k), c)
		}
		if children == 0 && prefix != "" {
			out = append(out, strings.TrimPrefix(prefix, "."))
		}
	}
	walk("", set)
	slices.Sort(out)
	return out
}

// pathStep writes one key of a FieldsV1 set as a step of a path.
func pathStep(k string) string {
	switch {
	case strings.HasPrefix(k, "f:"):
		return "." + k[2:]
	case strings.HasPrefix(k, "k:"):
		keys, ok := decode([]byte(k[2:])).(map[string]any)
		if !ok {
			return "[" + k[2:] + "]"
		}
		names := make([]string, 0, len(keys))
		for name := range keys {
			names = append(names, name)
		}
		slices.Sort(names)
		parts := make([]string, len(names))
		for i, name := range names {
			v, _ := json.Marshal(keys[name])
			parts[i] = name + "=" + string(v)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case strings.HasPrefix(k, "v:"):
		return "[=" + k[2:] + "]"
	case strings.HasPrefix(k, "i:"):
		return "[" + k[2:] + "]"
	default:
		return "." + k
	}
}

// clusterManager reports whether a field manager is one of Kubernetes' own
// components. What they write, such as a Deployment's revision annotation
// or the CA bundle in kube-root-ca.crt, differs between clusters by design.
func clusterManager(name string) bool {
	return strings.HasPrefix(name, "kube-") || name == "kubelet"
}

// setByCluster reports whether a field's owners, on one side, show that the
// cluster set it itself: nobody owns it, so the API server defaulted or
// allocated it, or only Kubernetes' own components do.
func setByCluster(owners []Owner) bool {
	for _, o := range owners {
		if !clusterManager(o.Manager) {
			return false
		}
	}
	return true
}

// managerNames lists the distinct managers of owners.
func managerNames(owners []Owner) []string {
	var out []string
	for _, o := range owners {
		if !slices.Contains(out, o.Manager) {
			out = append(out, o.Manager)
		}
	}
	return out
}
