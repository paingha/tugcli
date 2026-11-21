// Package manifest finds the files in a source repository that define a
// live Kubernetes object: from what kustomize, Flux, Argo CD and Helm leave
// on the objects they apply, and otherwise by searching the repository's
// YAML for the object's kind and name. It only reads files.
package manifest

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Object is what is known of a live object.
type Object struct {
	APIVersion  string
	Kind        string
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
}

// Location is a place in a file that defines an object.
type Location struct {
	// Path is the file's path, under the repository's root.
	Path string `json:"path"`
	// Line is where the object's document starts, from 1.
	Line int `json:"line"`
	// Via says how tug found it, such as "Flux Kustomization flux-system/apps".
	Via string `json:"via"`
}

// String is the location as an editor-agnostic link: "path:line".
func (l Location) String() string { return fmt.Sprintf("%s:%d", l.Path, l.Line) }

// Kinds of the GitOps objects that apply others.
const (
	FluxKustomization = "Kustomization"
	FluxHelmRelease   = "HelmRelease"
	ArgoApplication   = "Application"
)

// Source is a GitOps object that applied a live one: a Flux Kustomization
// or HelmRelease, or an Argo CD Application.
type Source struct {
	Kind      string
	Namespace string
	Name      string
}

// String names the source as "Flux Kustomization flux-system/apps".
func (s Source) String() string {
	tool := "Flux"
	if s.Kind == ArgoApplication {
		tool = "Argo CD"
	}
	return fmt.Sprintf("%s %s %s/%s", tool, s.Kind, s.Namespace, s.Name)
}

// The labels and annotations GitOps tools, kustomize and Helm set.
const (
	fluxKustomizationName      = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizationNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	fluxHelmReleaseName        = "helm.toolkit.fluxcd.io/name"
	fluxHelmReleaseNamespace   = "helm.toolkit.fluxcd.io/namespace"
	argoTrackingID             = "argocd.argoproj.io/tracking-id"
	argoInstance               = "argocd.argoproj.io/instance"
	instanceLabel              = "app.kubernetes.io/instance"
	helmReleaseName            = "meta.helm.sh/release-name"
	helmChart                  = "helm.sh/chart"
	kustomizeOrigin            = "config.kubernetes.io/origin"
)

// argoNamespace is where Argo CD keeps its Applications unless an object's
// tracking ID says otherwise.
const argoNamespace = "argocd"

// Sources returns the GitOps objects that an object's labels and
// annotations say applied it.
func Sources(o Object) []Source {
	var out []Source
	if name := o.Labels[fluxKustomizationName]; name != "" {
		out = append(out, Source{Kind: FluxKustomization, Namespace: o.Labels[fluxKustomizationNamespace], Name: name})
	}
	if name := o.Labels[fluxHelmReleaseName]; name != "" {
		out = append(out, Source{Kind: FluxHelmRelease, Namespace: o.Labels[fluxHelmReleaseNamespace], Name: name})
	}
	// Argo CD tracks by annotation, "<app>:<group>/<kind>:<namespace>/<name>",
	// where an Application outside Argo CD's namespace is "<namespace>_<app>",
	// or by a label naming the Application. app.kubernetes.io/instance is
	// also Helm's, so it counts only on objects Helm did not install.
	app := ""
	switch {
	case o.Annotations[argoTrackingID] != "":
		app, _, _ = strings.Cut(o.Annotations[argoTrackingID], ":")
	case o.Labels[argoInstance] != "":
		app = o.Labels[argoInstance]
	case o.Labels[instanceLabel] != "" && o.Annotations[helmReleaseName] == "":
		app = o.Labels[instanceLabel]
	}
	if app != "" {
		ns, name, ok := strings.Cut(app, "_")
		if !ok {
			ns, name = argoNamespace, app
		}
		out = append(out, Source{Kind: ArgoApplication, Namespace: ns, Name: name})
	}
	return out
}

// SourcePaths returns the paths in its repository that a GitOps object
// applies from, read from the object itself: a Flux Kustomization's
// spec.path, a Flux HelmRelease's chart when it is a path, or an Argo CD
// Application's source paths.
func SourcePaths(kind string, obj map[string]any) []string {
	str := func(m map[string]any, fields ...string) string {
		for _, f := range fields[:len(fields)-1] {
			next, ok := m[f].(map[string]any)
			if !ok {
				return ""
			}
			m = next
		}
		s, _ := m[fields[len(fields)-1]].(string)
		return s
	}
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	switch kind {
	case FluxKustomization:
		add(cmp.Or(str(obj, "spec", "path"), "./"))
	case FluxHelmRelease:
		if chart := str(obj, "spec", "chart", "spec", "chart"); strings.Contains(chart, "/") {
			add(chart)
		}
	case ArgoApplication:
		add(str(obj, "spec", "source", "path"))
		spec, _ := obj["spec"].(map[string]any)
		sources, _ := spec["sources"].([]any)
		for _, s := range sources {
			if m, ok := s.(map[string]any); ok {
				add(str(m, "path"))
			}
		}
	}
	return out
}

// Finder looks for the files that define objects in one repository.
type Finder struct {
	// Root is the repository's top directory.
	Root string
	// SourcePaths returns the paths, relative to Root, that a GitOps
	// object applies from, or none when it cannot tell. It may be nil.
	SourcePaths func(ctx context.Context, s Source) []string
}

// Find returns where an object is defined, best first: the file named by
// kustomize's origin annotation, then the files in the paths its GitOps
// source applies from, then the templates of its Helm chart. Only when
// those find nothing does it search the whole repository for a document of
// the object's kind and name. A document with another namespace never
// matches.
func (f *Finder) Find(ctx context.Context, o Object) ([]Location, error) {
	var out []Location
	add := func(locs ...Location) {
		for _, l := range locs {
			if !slices.ContainsFunc(out, func(m Location) bool { return m.Path == l.Path && m.Line == l.Line }) {
				out = append(out, l)
			}
		}
	}
	var dirs []string
	if f.SourcePaths != nil {
		for _, s := range Sources(o) {
			for _, p := range f.SourcePaths(ctx, s) {
				if dir, ok := f.within(p); ok {
					dirs = append(dirs, dir)
					locs, err := f.search(ctx, dir, o, s.String())
					if err != nil {
						return nil, err
					}
					add(locs...)
					if len(locs) == 0 && isChart(dir) {
						locs, err := f.chartTemplates(ctx, dir, o, "Helm chart "+filepath.Base(dir)+" of "+s.String())
						if err != nil {
							return nil, err
						}
						add(locs...)
					}
				}
			}
		}
	}
	origin, err := f.origin(ctx, o, dirs)
	if err != nil {
		return nil, err
	}
	out = slices.Concat(origin, slices.DeleteFunc(out, func(l Location) bool {
		return slices.ContainsFunc(origin, func(m Location) bool { return m.Path == l.Path })
	}))
	if len(out) == 0 {
		locs, err := f.helm(ctx, o)
		if err != nil {
			return nil, err
		}
		add(locs...)
	}
	if len(out) == 0 {
		locs, err := f.search(ctx, f.Root, o, "a search of "+f.Root)
		if err != nil {
			return nil, err
		}
		add(locs...)
	}
	return out, nil
}

// within returns a repository path as a directory under Root, if it is one.
func (f *Finder) within(p string) (string, bool) {
	p = filepath.Join(f.Root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	rel, err := filepath.Rel(f.Root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(p)
	return p, err == nil && fi.IsDir()
}

// origin finds the file kustomize's origin annotation names, as a path
// relative to the kustomization that was built, which may be anywhere in
// the repository: first under the GitOps paths, then anywhere.
func (f *Finder) origin(ctx context.Context, o Object, dirs []string) ([]Location, error) {
	raw := o.Annotations[kustomizeOrigin]
	if raw == "" {
		return nil, nil
	}
	var origin struct {
		Path string `json:"path"`
	}
	if yaml.Unmarshal([]byte(raw), &origin) != nil || origin.Path == "" {
		return nil, nil // a malformed annotation only means no hint
	}
	want := filepath.FromSlash(filepath.Clean(origin.Path))
	var files []string
	for _, dir := range dirs {
		if p := filepath.Join(dir, want); isFile(p) {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		// Without the kustomization's directory, a path that climbs out of
		// it, as ../base/api.yaml does, can only be matched by its end.
		tail := want
		for strings.HasPrefix(tail, ".."+string(filepath.Separator)) {
			tail = tail[3:]
		}
		err := walk(ctx, f.Root, func(path string) {
			if path == filepath.Join(f.Root, tail) || strings.HasSuffix(path, string(filepath.Separator)+tail) {
				files = append(files, path)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	var out []Location
	for _, p := range files {
		docs, err := readDocs(p)
		if err != nil {
			continue
		}
		line := 1
		// kustomize may have renamed the object, with namePrefix for one;
		// its kind still picks the document.
		if i := slices.IndexFunc(docs, func(d doc) bool { return d.matches(o) }); i >= 0 {
			line = docs[i].Line
		} else if i := slices.IndexFunc(docs, func(d doc) bool { return d.Kind == o.Kind }); i >= 0 {
			line = docs[i].Line
		}
		out = append(out, Location{Path: p, Line: line, Via: "kustomize's origin annotation"})
	}
	return out, nil
}

// helm finds the chart of an object Helm installed by its helm.sh/chart
// label, "<chart>-<version>", and the templates in it that make objects of
// its kind. Templates name objects with expressions, so the kind is all
// there is to go on.
func (f *Finder) helm(ctx context.Context, o Object) ([]Location, error) {
	release, chart := o.Annotations[helmReleaseName], o.Labels[helmChart]
	if release == "" || chart == "" {
		return nil, nil
	}
	var charts []string
	err := walk(ctx, f.Root, func(path string) {
		if filepath.Base(path) != "Chart.yaml" {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var c struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if yaml.Unmarshal(b, &c) != nil || c.Name == "" {
			return
		}
		if chart == c.Name+"-"+strings.ReplaceAll(c.Version, "+", "_") || strings.HasPrefix(chart, c.Name+"-") {
			charts = append(charts, filepath.Dir(path))
		}
	})
	if err != nil {
		return nil, err
	}
	var out []Location
	for _, dir := range charts {
		locs, err := f.chartTemplates(ctx, dir, o, fmt.Sprintf("Helm chart %s of release %s", filepath.Base(dir), release))
		if err != nil {
			return nil, err
		}
		out = append(out, locs...)
	}
	return out, nil
}

// chartTemplates finds the documents of an object's kind in a chart's
// templates.
func (f *Finder) chartTemplates(ctx context.Context, chart string, o Object, via string) ([]Location, error) {
	var out []Location
	err := walk(ctx, filepath.Join(chart, "templates"), func(path string) {
		docs, err := readDocs(path)
		if err != nil {
			return
		}
		for _, d := range docs {
			if d.Kind == o.Kind {
				out = append(out, Location{Path: path, Line: d.Line, Via: via})
			}
		}
	})
	return out, err
}

// search finds the documents of an object's kind and name under dir, those
// that also name its namespace first.
func (f *Finder) search(ctx context.Context, dir string, o Object, via string) ([]Location, error) {
	type found struct {
		loc    Location
		sameNS bool
	}
	var all []found
	err := walk(ctx, dir, func(path string) {
		docs, err := readDocs(path)
		if err != nil {
			return
		}
		for _, d := range docs {
			if d.matches(o) {
				all = append(all, found{Location{Path: path, Line: d.Line, Via: via}, d.Namespace != "" && d.Namespace == o.Namespace})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b found) int {
		switch {
		case a.sameNS && !b.sameNS:
			return -1
		case b.sameNS && !a.sameNS:
			return 1
		}
		return cmp.Or(
			cmp.Compare(strings.Count(a.loc.Path, string(filepath.Separator)), strings.Count(b.loc.Path, string(filepath.Separator))),
			cmp.Compare(a.loc.Path, b.loc.Path),
		)
	})
	out := make([]Location, len(all))
	for i, a := range all {
		out[i] = a.loc
	}
	return out, nil
}

// The limits of a search, which keep a run in a huge directory, or one
// that is not a repository at all, short.
const (
	maxFiles    = 50000
	maxFileSize = 4 << 20
)

// skipDirs are never searched.
var skipDirs = []string{"node_modules", "vendor"}

// walk calls fn with each YAML file under dir, leaving out hidden
// directories and skipDirs, until it has seen maxFiles files.
func walk(ctx context.Context, dir string, fn func(path string)) error {
	seen := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return nil // an unreadable directory only hides its files
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(d.Name(), ".") || slices.Contains(skipDirs, d.Name())) {
				return fs.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if seen++; seen > maxFiles {
			return fs.SkipAll
		}
		fn(path)
		return nil
	})
	return err
}

// FindRoot returns the repository dir is in: the nearest directory at or
// above it with a .git, or dir itself when there is none.
func FindRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isChart(dir string) bool { return isFile(filepath.Join(dir, "Chart.yaml")) }

// doc is one YAML document of a file, as far as finding objects needs.
type doc struct {
	// Line is the document's first line that is not blank or a comment.
	Line       int
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// matches reports whether a document defines an object: the same kind in
// the same API group, the same name, and no other namespace.
func (d doc) matches(o Object) bool {
	return d.Kind == o.Kind && d.Name == o.Name && sameGroup(d.APIVersion, o.APIVersion) &&
		(d.Namespace == "" || o.Namespace == "" || d.Namespace == o.Namespace)
}

// sameGroup compares the API groups of two apiVersions, either of which
// may be unknown: "apps/v1" and "apps/v1beta2" are the same group.
func sameGroup(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	group := func(v string) string {
		g, _, ok := strings.Cut(v, "/")
		if !ok {
			return ""
		}
		return g
	}
	return group(a) == group(b)
}

func readDocs(path string) ([]doc, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxFileSize {
		return nil, fmt.Errorf("%s is too big to search", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scanDocs(b), nil
}

// scanDocs reads the top-level apiVersion and kind, and the name and
// namespace under metadata, of each document in a YAML stream, line by
// line rather than by parsing, so that Helm templates, which are not YAML
// until rendered, can be read too.
func scanDocs(b []byte) []doc {
	var out []doc
	var cur doc
	inMeta, metaIndent := false, -1
	flush := func() {
		if cur.Kind != "" {
			out = append(out, cur)
		}
		cur, inMeta, metaIndent = doc{}, false, -1
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), maxFileSize)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "---" || strings.HasPrefix(line, "--- ") || line == "..." {
			flush()
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A document starts at its first line of YAML, after any comments
		// and template directives such as {{- if .Values.enabled }}.
		if cur.Line == 0 && !strings.HasPrefix(trimmed, "{{") {
			cur.Line = n
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			inMeta = false
			key, value := keyValue(line)
			switch key {
			case "apiVersion":
				cur.APIVersion = value
			case "kind":
				cur.Kind = value
			case "metadata":
				inMeta, metaIndent = value == "", -1
			}
			continue
		}
		if !inMeta {
			continue
		}
		if metaIndent < 0 {
			metaIndent = indent
		}
		if indent != metaIndent {
			continue
		}
		switch key, value := keyValue(trimmed); key {
		case "name":
			cur.Name = value
		case "namespace":
			cur.Namespace = value
		}
	}
	flush()
	return out
}

// keyValue splits a "key: value" line, dropping a trailing comment and
// quotes around the value.
func keyValue(line string) (string, string) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", ""
	}
	value = strings.TrimSpace(value)
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return strings.TrimSpace(key), value
}
