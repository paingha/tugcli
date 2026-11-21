package diff

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Manifest is one object read from a file.
type Manifest struct {
	// Source is the file it was read from, or "-" for stdin.
	Source string
	Object map[string]any
}

// manifestExts are the files a directory's manifests are read from.
var manifestExts = []string{".yaml", ".yml", ".json"}

// ReadFiles reads the objects in files, directories and, for "-", stdin, as
// kubectl apply -f does: YAML or JSON, several documents to a file, and
// the items of a List. A directory's .yaml, .yml and .json files are read
// in name order; its subdirectories are not.
func ReadFiles(paths []string, stdin io.Reader) ([]Manifest, error) {
	var out []Manifest
	for _, p := range paths {
		if p == "-" {
			objs, err := Manifests(stdin)
			if err != nil {
				return nil, fmt.Errorf("reading stdin: %w", err)
			}
			out = appendManifests(out, "-", objs)
			continue
		}
		files, err := manifestFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			objs, err := readFile(f)
			if err != nil {
				return nil, err
			}
			out = appendManifests(out, f, objs)
		}
	}
	return out, nil
}

func appendManifests(out []Manifest, source string, objs []map[string]any) []Manifest {
	for _, o := range objs {
		out = append(out, Manifest{Source: source, Object: o})
	}
	return out
}

// manifestFiles returns p, or the manifests in p when it is a directory.
func manifestFiles(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && slices.Contains(manifestExts, strings.ToLower(filepath.Ext(e.Name()))) {
			out = append(out, filepath.Join(p, e.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no .yaml, .yml or .json files", p)
	}
	return out, nil
}

func readFile(name string) ([]map[string]any, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	objs, err := Manifests(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return objs, nil
}

// Manifests reads the objects in a stream of YAML or JSON documents.
// Empty documents are skipped, and a List stands for its items. Every
// object needs an apiVersion, a kind and a name.
func Manifests(r io.Reader) ([]map[string]any, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(r), 4096)
	var out []map[string]any
	for doc := 1; ; doc++ {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", doc, err)
		}
		if len(obj) == 0 {
			continue
		}
		objs := []map[string]any{obj}
		if items, ok := obj["items"].([]any); ok && strings.HasSuffix(str(obj["kind"]), "List") {
			objs = objs[:0]
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					objs = append(objs, m)
				}
			}
		}
		for _, o := range objs {
			if err := checkManifest(o); err != nil {
				return nil, fmt.Errorf("document %d: %w", doc, err)
			}
			out = append(out, o)
		}
	}
}

func checkManifest(o map[string]any) error {
	md, _ := o["metadata"].(map[string]any)
	switch {
	case str(o["apiVersion"]) == "":
		return errors.New("an object has no apiVersion")
	case str(o["kind"]) == "":
		return errors.New("an object has no kind")
	case str(md["name"]) == "" && str(md["generateName"]) != "":
		return fmt.Errorf("%s has only a generateName, so there is no live object to compare it with", o["kind"])
	case str(md["name"]) == "":
		return fmt.Errorf("a %s has no name", o["kind"])
	}
	return nil
}
