// Package diff compares Kubernetes objects: the live object with a manifest
// through a server-side dry-run apply, the same object on two clusters, and
// whole namespaces and API surfaces across clusters. It leaves out what the
// API server generates, attributes each differing field to the field
// managers that own it, from managedFields, and flags the fields an apply
// would conflict with another manager over. It never reads a Secret's data.
package diff

import (
	"slices"
)

// Origins of a field's value after a dry-run apply.
const (
	// OriginManifest values are what the manifest says.
	OriginManifest = "manifest"
	// OriginAdmission values were set by admission, such as a mutating
	// webhook, for the applying field manager.
	OriginAdmission = "admission"
	// OriginDefault values were defaulted by the API server: no field
	// manager owns them.
	OriginDefault = "default"
)

// Options say how to compare.
type Options struct {
	// Status compares status too.
	Status bool
	// FieldManager is the manager a dry run applies as, and whose
	// conflicts with other managers are flagged; without one, none are.
	FieldManager string
	// AllFields keeps, when comparing clusters, the fields that only the
	// clusters set themselves.
	AllFields bool
}

// Result is the comparison of one object's two sides.
type Result struct {
	// Object is the kind in lower case and the name: deployment/api.
	Object    string `json:"object"`
	Namespace string `json:"namespace,omitempty"`
	// From and To say what each side is, such as a cluster's name.
	From string `json:"from"`
	To   string `json:"to"`
	// Missing names the side the object does not exist on, From or To.
	Missing string   `json:"missing,omitempty"`
	Same    bool     `json:"same"`
	Changes []Change `json:"changes"`
	// Conflicts are what the API server said an apply as the field manager
	// would conflict over. The dry run was then forced, to show what the
	// apply would make of the object anyway.
	Conflicts []Conflict `json:"conflicts,omitempty"`
	// Omitted counts the differing fields left out because only the
	// clusters set them, such as allocated IPs.
	Omitted int `json:"omitted,omitempty"`
	// Diff is the unified diff of the normalised objects as YAML.
	Diff  string `json:"diff"`
	Hunks []Hunk `json:"-"`
}

// Label names the two sides, and writes the unified diff with them in its
// header lines.
func (r *Result) Label(from, to string) {
	r.From, r.To = from, to
	r.Diff = FormatUnified(from, to, r.Hunks)
}

// Conflict is one field an apply would take from another field manager.
type Conflict struct {
	Manager string `json:"manager"`
	// Field is the field's path as the API server wrote it.
	Field   string `json:"field"`
	Message string `json:"message"`
}

// sides is what compareSides compares: each side as the API server has it,
// with managedFields, or nil when it does not exist.
type sides struct {
	from, to map[string]any
	// manifest, when set, is what was applied to make to.
	manifest map[string]any
	// acrossClusters leaves out what each cluster sets itself.
	acrossClusters bool
	// text asks for the unified diff as well as the changes.
	text bool
}

// compareSides compares two sides of an object and attributes each
// difference to its owners.
func compareSides(s sides, opts Options) (*Result, error) {
	nopts := NormaliseOptions{Status: opts.Status, AcrossClusters: s.acrossClusters}
	from, to := Normalise(s.from, nopts), Normalise(s.to, nopts)
	changes := Compare(from, to)

	fromMgrs, err := Managers(s.from)
	if err != nil {
		return nil, err
	}
	toMgrs, err := Managers(s.to)
	if err != nil {
		return nil, err
	}
	r := &Result{}
	switch {
	case s.from == nil:
		r.Missing = "from"
	case s.to == nil:
		r.Missing = "to"
	}
	kept := make([]Change, 0, len(changes))
	var omitted []Change
	for _, c := range changes {
		if c.Op != OpAdd {
			c.FromOwners = ownersIn(fromMgrs, s.from, c.Path)
		}
		if c.Op != OpRemove {
			c.ToOwners = ownersIn(toMgrs, s.to, c.Path)
		}
		if s.acrossClusters && !opts.AllFields && r.Missing == "" && clusterSet(c, len(fromMgrs) > 0, len(toMgrs) > 0) {
			omitted = append(omitted, c)
			continue
		}
		if s.manifest != nil {
			c.Origin = origin(c, s.manifest, opts.FieldManager)
		}
		if s.acrossClusters && c.Op != OpRemove && opts.FieldManager != "" {
			c.Conflicts = conflictsOver(c.FromOwners, opts.FieldManager)
		}
		kept = append(kept, c)
	}
	r.Changes, r.Omitted = kept, len(omitted)
	// Take what was left out off both sides, so the text shows only what
	// was kept. A list loses its items from the last to the first.
	for _, c := range slices.Backward(omitted) {
		if c.Op != OpAdd {
			from, _ = remove(from, c.Path).(map[string]any)
		}
		if c.Op != OpRemove {
			to, _ = remove(to, c.Path).(map[string]any)
		}
	}
	if s.text {
		a, err := yamlLines(from)
		if err != nil {
			return nil, err
		}
		b, err := yamlLines(to)
		if err != nil {
			return nil, err
		}
		r.Hunks = Unified(a, b, 3)
	}
	r.Same = len(r.Changes) == 0 && len(r.Hunks) == 0
	return r, nil
}

// clusterSet reports whether a difference is in a field that only the
// clusters set, on every side the field is on. Without managedFields on a
// side, nothing can be told, and the difference is kept.
func clusterSet(c Change, fromManaged, toManaged bool) bool {
	switch c.Op {
	case OpAdd:
		return toManaged && setByCluster(c.ToOwners)
	case OpRemove:
		return fromManaged && setByCluster(c.FromOwners)
	default:
		return fromManaged && toManaged && setByCluster(c.FromOwners) && setByCluster(c.ToOwners)
	}
}

// origin says what set a field's value after applying manifest as manager.
func origin(c Change, manifest map[string]any, manager string) string {
	want, inManifest := lookup(Normalise(manifest, NormaliseOptions{Status: true}), c.Path)
	if c.Op == OpRemove {
		if !inManifest && slices.ContainsFunc(c.FromOwners, func(o Owner) bool { return o.Manager == manager }) {
			return OriginManifest // the manifest no longer sets it, so the apply takes it out
		}
		return ""
	}
	_, isMap := want.(map[string]any)
	_, isList := want.([]any)
	switch {
	case inManifest && (isMap || isList || jsonEqual(want, c.To)):
		// The manifest sets the field; beneath a map or list, the API
		// server may have added defaults too.
		return OriginManifest
	case slices.ContainsFunc(c.ToOwners, func(o Owner) bool { return o.Manager == manager }):
		return OriginAdmission
	case len(c.ToOwners) == 0:
		return OriginDefault
	}
	return ""
}

// conflictsOver lists the managers other than manager that own a field, and
// so would conflict with an apply of another value for it.
func conflictsOver(owners []Owner, manager string) []string {
	var out []string
	for _, name := range managerNames(owners) {
		if name != manager {
			out = append(out, name)
		}
	}
	return out
}

// markConflicts adds the conflicts the API server reported to the changes
// at or beneath their fields.
func markConflicts(r *Result) {
	for _, cf := range r.Conflicts {
		for i := range r.Changes {
			c := &r.Changes[i]
			p := c.Path.String()
			if (p == cf.Field || hasPathPrefix(p, cf.Field)) && !slices.Contains(c.Conflicts, cf.Manager) {
				c.Conflicts = append(c.Conflicts, cf.Manager)
			}
		}
	}
}

// hasPathPrefix reports whether path p lies beneath the path prefix.
func hasPathPrefix(p, prefix string) bool {
	if len(p) <= len(prefix) || p[:len(prefix)] != prefix {
		return false
	}
	return p[len(prefix)] == '.' || p[len(prefix)] == '['
}
