package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// SetPluginPin pins the named plugin to version in config.yaml in dir, or
// removes its pin when version is empty. It changes only that entry, so the
// rest of the file, comments included, stays as the user wrote it. It
// returns the version the plugin was pinned to before, if any.
func SetPluginPin(dir, name, version string) (string, error) {
	path := filepath.Join(dir, "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var doc yamlv3.Node
	if len(bytes.TrimSpace(b)) > 0 {
		if err := yamlv3.Unmarshal(b, &doc); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	}
	// A file of comments alone parses to nothing; they are kept by
	// writing the settings after them.
	var out bytes.Buffer
	if len(doc.Content) == 0 {
		doc = yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{{Kind: yamlv3.MappingNode, Tag: "!!map"}}}
		if len(bytes.TrimSpace(b)) > 0 {
			out.Write(bytes.TrimRight(b, "\n"))
			out.WriteByte('\n')
		}
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yamlv3.MappingNode {
		return "", fmt.Errorf("%s is not a mapping of settings; pin %s by hand under plugins.pins", path, name)
	}
	set := version != ""
	pins := mapping(mapping(doc.Content[0], "plugins", set), "pins", set)
	switch {
	case pins == nil && set:
		return "", fmt.Errorf("plugins or plugins.pins in %s is not a mapping; pin %s by hand", path, name)
	case pins == nil:
		return "", nil // nothing to remove
	}
	before := ""
	for i := 0; i+1 < len(pins.Content); i += 2 {
		if pins.Content[i].Value != name {
			continue
		}
		before = pins.Content[i+1].Value
		if version == "" {
			pins.Content = append(pins.Content[:i], pins.Content[i+2:]...)
		} else {
			// Quoted, so that a version such as 1.10 stays a string.
			pins.Content[i+1] = &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: version, Style: yamlv3.DoubleQuotedStyle}
		}
		version = ""
		break
	}
	if version != "" {
		pins.Content = append(pins.Content,
			&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: name},
			&yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: version, Style: yamlv3.DoubleQuotedStyle})
	}
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	// Refuse to write a file tug would then refuse to read.
	if err := yaml.UnmarshalStrict(out.Bytes(), &Config{}); err != nil {
		return "", fmt.Errorf("pinning %s would leave %s unreadable: %w", name, path, err)
	}
	return before, WriteFileAtomic(path, out.Bytes(), 0o600)
}

// mapping returns the mapping under key in m, adding an empty one when
// create is set and the key is missing or empty. It returns nil when there
// is none and create is not set.
func mapping(m *yamlv3.Node, key string, create bool) *yamlv3.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		v := m.Content[i+1]
		if v.Kind == yamlv3.MappingNode {
			return v
		}
		if !create || v.Kind != yamlv3.ScalarNode || v.Tag != "!!null" {
			return nil
		}
		m.Content[i+1] = &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
		return m.Content[i+1]
	}
	if !create {
		return nil
	}
	v := &yamlv3.Node{Kind: yamlv3.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}
