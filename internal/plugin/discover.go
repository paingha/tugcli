package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// PathEnv lists development plugin directories, searched first.
const PathEnv = "TUG_PLUGIN_PATH"

// Dirs locates installed and linked plugins.
type Dirs struct {
	// Installed is <data>/plugins; each plugin lives in <name>/<version>/.
	Installed string
}

// NewDirs returns the plugin directories under tug's data directory.
func NewDirs(dataDir string) Dirs { return Dirs{Installed: filepath.Join(dataDir, "plugins")} }

func (d Dirs) linksFile() string { return filepath.Join(d.Installed, "links.yaml") }

// Problem is a plugin that was found but could not be loaded.
type Problem struct {
	Location string
	Err      error
}

// Discover finds plugins in precedence order: $TUG_PLUGIN_PATH, linked
// plugins, then installed plugins (highest version of each). Installed
// plugins must match their manifest's sha256.
func (d Dirs) Discover() ([]*Plugin, []Problem) {
	var (
		out      []*Plugin
		problems []Problem
		seen     = map[string]bool{}
	)
	add := func(p *Plugin, err error, loc string) {
		if err != nil {
			problems = append(problems, Problem{Location: loc, Err: err})
			return
		}
		if seen[p.Name()] {
			return
		}
		seen[p.Name()] = true
		out = append(out, p)
	}

	for _, dir := range filepath.SplitList(os.Getenv(PathEnv)) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			problems = append(problems, Problem{Location: dir, Err: err})
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
			p, err := Load(dir, true)
			add(p, err, dir)
			continue
		}
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(sub, ManifestFile)); err == nil {
				p, err := Load(sub, true)
				add(p, err, sub)
			}
		}
	}

	links, err := d.Links()
	if err != nil {
		problems = append(problems, Problem{Location: d.linksFile(), Err: err})
	}
	for _, name := range sortedKeys(links) {
		p, err := Load(links[name], true)
		if err == nil && p.Name() != name {
			err = fmt.Errorf("linked as %q but the manifest names %q", name, p.Name())
		}
		add(p, err, links[name])
	}

	names, _ := os.ReadDir(d.Installed)
	for _, n := range names {
		if !n.IsDir() || strings.HasPrefix(n.Name(), ".") { // skip staging dirs
			continue
		}
		dir, err := latestVersionDir(filepath.Join(d.Installed, n.Name()))
		if err != nil {
			problems = append(problems, Problem{Location: filepath.Join(d.Installed, n.Name()), Err: err})
			continue
		}
		p, err := Load(dir, false)
		add(p, err, dir)
	}
	return out, problems
}

// Load reads a plugin directory. Dev plugins skip the checksum check.
func Load(dir string, dev bool) (*Plugin, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	plat, err := m.Platform()
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, filepath.FromSlash(plat.Bin))
	st, err := os.Stat(bin)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Metadata.Name, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("plugin %s: %s is a directory", m.Metadata.Name, bin)
	}
	if !dev {
		if err := VerifyChecksum(bin, plat.SHA256); err != nil {
			return nil, fmt.Errorf("plugin %s: %w", m.Metadata.Name, err)
		}
	}
	return &Plugin{Manifest: m, Dir: dir, Bin: bin, Dev: dev}, nil
}

// Links returns linked development plugins by name.
func (d Dirs) Links() (map[string]string, error) {
	b, err := os.ReadFile(d.linksFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	links := map[string]string{}
	if err := yaml.Unmarshal(b, &links); err != nil {
		return nil, err
	}
	return links, nil
}

// Link registers a development plugin directory under its manifest name.
func (d Dirs) Link(dir string) (*Plugin, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	p, err := Load(abs, true)
	if err != nil {
		return nil, err
	}
	links, err := d.Links()
	if err != nil {
		return nil, err
	}
	links[p.Name()] = abs
	return p, d.saveLinks(links)
}

// Unlink removes a linked plugin.
func (d Dirs) Unlink(name string) error {
	links, err := d.Links()
	if err != nil {
		return err
	}
	if _, ok := links[name]; !ok {
		return fmt.Errorf("no linked plugin named %q", name)
	}
	delete(links, name)
	return d.saveLinks(links)
}

func (d Dirs) saveLinks(links map[string]string) error {
	b, err := yaml.Marshal(links)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(d.linksFile(), b, 0o600)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// latestVersionDir picks the highest version directory under a plugin's
// install directory.
func latestVersionDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", errors.New("no versions installed")
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
	return filepath.Join(dir, versions[0]), nil
}

// compareVersions compares dotted numeric versions such as 0.3.0 or v1.2;
// non-numeric parts compare as strings.
func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil && xi != yi:
			if xi < yi {
				return -1
			}
			return 1
		case (xerr != nil || yerr != nil) && x != y:
			return strings.Compare(x, y)
		}
	}
	return 0
}
