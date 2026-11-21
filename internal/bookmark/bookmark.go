// Package bookmark keeps the user's named bookmarks: an object on a cluster,
// or a cluster and namespace, saved under a short name in bookmarks.yaml in
// tug's config directory.
package bookmark

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// ErrNotFound is returned for a bookmark name that is not saved.
var ErrNotFound = errors.New("no such bookmark")

// ErrExists is returned when adding a bookmark under a name already taken.
var ErrExists = errors.New("bookmark already exists")

// Bookmark is a named place: a cluster and namespace, and usually an object
// in them.
type Bookmark struct {
	Name string `json:"name"`
	// Cluster is the catalog name of the bookmark's cluster.
	Cluster string `json:"cluster"`
	// Namespace is "" for a cluster-scoped object.
	Namespace string `json:"namespace,omitempty"`
	// Target is the object; nil for a bookmark of a cluster and namespace.
	Target  *Target   `json:"target,omitempty"`
	Created time.Time `json:"created"`
}

// Target is a bookmarked object, saved exactly as it resolved when the
// bookmark was added, so that opening it never has to guess.
type Target struct {
	// Resource is the object's resource and group as kubectl accepts it,
	// such as "deployments.apps", or "pods" for the core group.
	Resource string `json:"resource"`
	// Kind is the object's kind, such as Deployment.
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// String names the object as tug prints objects: "deployment/api-server".
func (t Target) String() string { return strings.ToLower(t.Kind) + "/" + t.Name }

// Ref names the object so that kubectl and tug resolve it to its resource
// whatever else the cluster serves: "deployments.apps/api-server".
func (t Target) Ref() string { return t.Resource + "/" + t.Name }

// Where describes the bookmark's place: "deployment/api-server in shop on
// payments-prod".
func (b Bookmark) Where() string {
	var s string
	if b.Target != nil {
		s = b.Target.String()
	}
	if b.Namespace != "" {
		s += " in " + b.Namespace
	}
	return strings.TrimSpace(s + " on " + b.Cluster)
}

// file is bookmarks.yaml.
type file struct {
	Bookmarks []Bookmark `json:"bookmarks"`
}

// Store reads and writes bookmarks.yaml.
type Store struct{ path string }

// New returns the store in tug's config directory.
func New(configDir string) *Store { return &Store{path: filepath.Join(configDir, "bookmarks.yaml")} }

// Path is the file the store keeps bookmarks in.
func (s *Store) Path() string { return s.path }

// List returns every bookmark, sorted by name.
func (s *Store) List() ([]Bookmark, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Bookmarks, nil
}

// Get returns the bookmark with a name.
func (s *Store) Get(name string) (Bookmark, error) {
	f, err := s.load()
	if err != nil {
		return Bookmark{}, err
	}
	i := slices.IndexFunc(f.Bookmarks, func(b Bookmark) bool { return b.Name == name })
	if i < 0 {
		return Bookmark{}, fmt.Errorf("%w %q", ErrNotFound, name)
	}
	return f.Bookmarks[i], nil
}

// Add saves a bookmark. A bookmark of the same name is replaced only when
// replace is true; otherwise Add returns ErrExists.
func (s *Store) Add(b Bookmark, replace bool) error {
	if err := ValidName(b.Name); err != nil {
		return err
	}
	if b.Cluster == "" {
		return errors.New("a bookmark needs a cluster")
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(f.Bookmarks, func(o Bookmark) bool { return o.Name == b.Name }); i >= 0 {
		if !replace {
			return fmt.Errorf("%w: %s is %s", ErrExists, b.Name, f.Bookmarks[i].Where())
		}
		f.Bookmarks[i] = b
	} else {
		f.Bookmarks = append(f.Bookmarks, b)
	}
	return s.save(f)
}

// Remove deletes the named bookmarks and returns the ones it removed. Names
// that are not saved make it return ErrNotFound, after removing the rest.
func (s *Store) Remove(names ...string) ([]Bookmark, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	var removed []Bookmark
	var missing []string
	for _, name := range names {
		i := slices.IndexFunc(f.Bookmarks, func(b Bookmark) bool { return b.Name == name })
		if i < 0 {
			missing = append(missing, name)
			continue
		}
		removed = append(removed, f.Bookmarks[i])
		f.Bookmarks = slices.Delete(f.Bookmarks, i, i+1)
	}
	if len(removed) > 0 {
		if err := s.save(f); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		return removed, fmt.Errorf("%w %s", ErrNotFound, strings.Join(quoteAll(missing), ", "))
	}
	return removed, nil
}

func (s *Store) load() (*file, error) {
	f := &file{}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return f, nil
	case err != nil:
		return nil, fmt.Errorf("reading bookmarks: %w", err)
	}
	if err := yaml.UnmarshalStrict(b, f); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	sortByName(f.Bookmarks)
	return f, nil
}

func (s *Store) save(f *file) error {
	sortByName(f.Bookmarks)
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(s.path, b, 0o600); err != nil {
		return fmt.Errorf("saving bookmarks: %w", err)
	}
	return nil
}

func sortByName(bs []Bookmark) {
	slices.SortFunc(bs, func(a, b Bookmark) int { return strings.Compare(a.Name, b.Name) })
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName returns an error unless name can name a bookmark: letters,
// digits, dots, dashes and underscores, not starting with a dot or dash.
func ValidName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("bookmark name %q: use letters, digits, '.', '-' and '_', starting with a letter or digit", name)
	}
	return nil
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}
