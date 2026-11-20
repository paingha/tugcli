// Package pin reads .tug.yaml files, which pin a directory tree to a
// cluster and namespace, and remembers which of them the user trusts. A
// repository someone else wrote could otherwise switch the terminal to a
// cluster on its own, so a pin file is used only once it is allowed, and
// again only after a change is allowed too.
package pin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// FileName is the pin file looked for in a directory and its parents.
const FileName = ".tug.yaml"

// File is .tug.yaml.
type File struct {
	// Cluster is a cluster name, prefix or fuzzy match, as tug connect
	// takes it.
	Cluster string `json:"cluster,omitempty"`
	// Labels narrow the clusters first, as label=value does for tug connect.
	Labels    map[string]string `json:"labels,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
}

// Args returns the arguments tug connect would take for the pin.
func (f File) Args() []string {
	var args []string
	if f.Cluster != "" {
		args = append(args, f.Cluster)
	}
	keys := make([]string, 0, len(f.Labels))
	for k := range f.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, k+"="+f.Labels[k])
	}
	return args
}

// Found is a pin file on disk.
type Found struct {
	Path string
	// Hash is the sha256 of the file, which trust is tied to.
	Hash string
	File File
}

// Find returns the pin file in dir or the nearest parent, or nil when
// there is none.
func Find(dir string) (*Found, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		path := filepath.Join(dir, FileName)
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			return parse(path, b)
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

func parse(path string, b []byte) (*Found, error) {
	var f File
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Cluster == "" && len(f.Labels) == 0 {
		return nil, fmt.Errorf("%s names no cluster; set cluster, labels or both", path)
	}
	sum := sha256.Sum256(b)
	return &Found{Path: path, Hash: hex.EncodeToString(sum[:]), File: f}, nil
}

// Write saves f as dir's pin file and returns it as found.
func Write(dir string, f File) (*Found, error) {
	b, err := yaml.Marshal(f)
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	// A pin file belongs to the project and is often committed, so it is
	// readable like other project files.
	if err := config.WriteFileAtomic(path, b, 0o644); err != nil {
		return nil, err
	}
	return parse(path, b)
}

// Trust records which pin files, by path and content, the user allowed.
type Trust struct{ path string }

// NewTrust keeps the record under tug's data directory.
func NewTrust(dataDir string) *Trust { return &Trust{path: filepath.Join(dataDir, "pins.yaml")} }

func (t *Trust) load() (map[string]string, error) {
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", t.path, err)
	}
	return m, nil
}

func (t *Trust) save(m map[string]string) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(t.path, b, 0o600)
}

// Allowed reports whether f, as it is now, was allowed.
func (t *Trust) Allowed(f *Found) bool {
	m, err := t.load()
	return err == nil && m[f.Path] == f.Hash
}

// Allow trusts f as it is now.
func (t *Trust) Allow(f *Found) error {
	m, err := t.load()
	if err != nil {
		return err
	}
	m[f.Path] = f.Hash
	return t.save(m)
}

// Revoke forgets the trust in the pin file at path and reports whether
// there was any.
func (t *Trust) Revoke(path string) (bool, error) {
	m, err := t.load()
	if err != nil {
		return false, err
	}
	if _, ok := m[path]; !ok {
		return false, nil
	}
	delete(m, path)
	return true, t.save(m)
}

// Paths lists the allowed pin files.
func (t *Trust) Paths() ([]string, error) {
	m, err := t.load()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, nil
}
