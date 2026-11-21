package diff

import (
	"bytes"
	"encoding/json"
)

// NormaliseOptions say what Normalise keeps.
type NormaliseOptions struct {
	// Status keeps the object's status, which is otherwise left out: it
	// is what controllers report, not what anyone asked for.
	Status bool
	// AcrossClusters also leaves out what differs between clusters by
	// design for the same object: the UIDs in ownerReferences.
	AcrossClusters bool
}

// serverMetadata are the metadata fields the API server writes itself.
var serverMetadata = []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink"}

// lastApplied is the annotation client-side kubectl apply keeps the applied
// manifest in. It repeats the object, so it only adds noise; for a Secret
// it would also hold the Secret's data.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Normalise returns a deep copy of obj without the fields the API server
// generates: uid, resourceVersion, generation, creationTimestamp,
// managedFields and selfLink, the last-applied annotation, and status
// unless opts asks for it. Numbers are kept as json.Number, so that values
// read from YAML and from the API server compare equal. A nil obj stays
// nil.
func Normalise(obj map[string]any, opts NormaliseOptions) map[string]any {
	if obj == nil {
		return nil
	}
	out, ok := deepCopy(obj).(map[string]any)
	if !ok {
		return nil
	}
	if !opts.Status {
		delete(out, "status")
	}
	md, ok := out["metadata"].(map[string]any)
	if !ok {
		return out
	}
	for _, f := range serverMetadata {
		delete(md, f)
	}
	if ann, ok := md["annotations"].(map[string]any); ok {
		delete(ann, lastApplied)
		if len(ann) == 0 {
			delete(md, "annotations")
		}
	}
	if opts.AcrossClusters {
		if refs, ok := md["ownerReferences"].([]any); ok {
			for _, r := range refs {
				if m, ok := r.(map[string]any); ok {
					delete(m, "uid")
				}
			}
		}
	}
	return out
}

// deepCopy copies a JSON-like value through encoding/json, turning every
// number into a json.Number.
func deepCopy(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return decode(b)
}

// decode reads JSON with numbers as json.Number.
func decode(b []byte) any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	if err := d.Decode(&out); err != nil {
		return nil
	}
	return out
}

// jsonEqual reports whether two JSON-like values are the same. It compares
// their encodings, so an int64 from the API server equals the json.Number
// a manifest gave, and maps compare whatever their order.
func jsonEqual(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(x, y)
}
